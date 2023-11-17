/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apiserver

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/httpstream"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/proxy"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	endpointmetrics "k8s.io/apiserver/pkg/endpoints/metrics"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	utilflowcontrol "k8s.io/apiserver/pkg/util/flowcontrol"
	"k8s.io/apiserver/pkg/util/x509metrics"
	"k8s.io/client-go/transport"
	"k8s.io/klog/v2"
	apiregistrationv1api "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	apiregistrationv1apihelper "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1/helper"
)

const (
	aggregatorComponent string = "aggregator"

	aggregatedDiscoveryTimeout = 5 * time.Second
)

type certKeyFunc func() ([]byte, []byte)

// proxyHandler provides a http.Handler which will proxy traffic to locations
// specified by items implementing Redirector.
type proxyHandler struct {
	// localDelegate is used to satisfy local APIServices
	// 其实就是APIServer
	localDelegate http.Handler

	// proxyCurrentCertKeyContent holds the client cert used to identify this proxy. Backing APIServices use this to confirm the proxy's identity
	// 看到这里，突然明白了代理证书的意义之所在，之所以需要代理证书，是因为AggregatorServer的存在。通过AggregatorServer,用户可以
	// 自定义Aggregator，当真正访问用户子当以Aggregator时，AggregatorServer实际上就是一个代理，它负责把流量代理到用户自定义的Aggregator,
	// 而K8S中，所有的HTTPS服务必须是双向认证，因此访问用户自定义的Aggregator，肯定是需要给AggregatorServer配置代理证书的。此证书专门用于
	// 访问用户自定义Aggregator。
	proxyCurrentCertKeyContent certKeyFunc
	// 用于访问用户自定义的Aggregator
	proxyTransportDial *transport.DialHolder

	// Endpoints based routing to map from cluster IP to routable IP
	// Service解析器，用于把一个Service解析为一个合法的URL
	serviceResolver ServiceResolver

	// 用于存储 proxyHandlingInfo
	handlingInfo atomic.Value

	// reject to forward redirect response
	rejectForwardingRedirects bool
}

type proxyHandlingInfo struct {
	// local indicates that this APIService is locally satisfied
	// 如果为true，表示当前APIService用于本地服务，在K8S当中，本地服务只有APIServer以及ExtensionServer
	local bool

	// name is the name of the APIService
	// APIService的名字
	name string
	// transportConfig holds the information for building a roundtripper
	// 用于配置TLS
	transportConfig *transport.Config
	// transportBuildingError is an error produced while building the transport.  If this
	// is non-nil, it will be reported to clients.
	transportBuildingError error
	// proxyRoundTripper is the re-useable portion of the transport.  It does not vary with any request.
	proxyRoundTripper http.RoundTripper
	// serviceName is the name of the service this handler proxies to
	// 服务名
	serviceName string
	// namespace is the namespace the service lives in
	// 名称空间
	serviceNamespace string
	// serviceAvailable indicates this APIService is available or not
	// 服务是否可用 K8S起了一个controller定期判断服务是否可用
	serviceAvailable bool
	// servicePort is the port of the service this handler proxies to
	// 服务端口
	servicePort int32
}

func proxyError(w http.ResponseWriter, req *http.Request, error string, code int) {
	http.Error(w, error, code)

	ctx := req.Context()
	info, ok := genericapirequest.RequestInfoFrom(ctx)
	if !ok {
		klog.Warning("no RequestInfo found in the context")
		return
	}
	// TODO: record long-running request differently? The long-running check func does not necessarily match the one of the aggregated apiserver
	endpointmetrics.RecordRequestTermination(req, info, aggregatorComponent, code)
}

func (r *proxyHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// 加载handlingInfor信息
	value := r.handlingInfo.Load()
	if value == nil {
		// 如果没有找到代理信息，那么直接把请求转发给APIServer
		r.localDelegate.ServeHTTP(w, req)
		return
	}
	handlingInfo := value.(proxyHandlingInfo)
	if handlingInfo.local {
		// 所有APIServer所有的组都是local=true，所有都是直接代理给APIServer
		if r.localDelegate == nil {
			http.Error(w, "", http.StatusNotFound)
			return
		}
		r.localDelegate.ServeHTTP(w, req)
		return
	}

	if !handlingInfo.serviceAvailable {
		proxyError(w, req, "service unavailable", http.StatusServiceUnavailable)
		return
	}

	if handlingInfo.transportBuildingError != nil {
		proxyError(w, req, handlingInfo.transportBuildingError.Error(), http.StatusInternalServerError)
		return
	}

	// 必须要从请求当中获取到用户信息
	user, ok := genericapirequest.UserFrom(req.Context())
	if !ok {
		proxyError(w, req, "missing user", http.StatusInternalServerError)
		return
	}

	// write a new location based on the existing request pointed at the target service
	location := &url.URL{}
	location.Scheme = "https"
	rloc, err := r.serviceResolver.ResolveEndpoint(handlingInfo.serviceNamespace, handlingInfo.serviceName, handlingInfo.servicePort)
	if err != nil {
		klog.Errorf("error resolving %s/%s: %v", handlingInfo.serviceNamespace, handlingInfo.serviceName, err)
		proxyError(w, req, "service unavailable", http.StatusServiceUnavailable)
		return
	}
	location.Host = rloc.Host
	location.Path = req.URL.Path                 // 原始请求的路径
	location.RawQuery = req.URL.Query().Encode() // 原始请求的参数

	// 构造一个新的请求，此请求一会儿会发送给APIService指向的服务
	newReq, cancelFn := newRequestForProxy(location, req)
	defer cancelFn()

	if handlingInfo.proxyRoundTripper == nil {
		proxyError(w, req, "", http.StatusNotFound)
		return
	}

	proxyRoundTripper := handlingInfo.proxyRoundTripper
	upgrade := httpstream.IsUpgradeRequest(req)

	proxyRoundTripper = transport.NewAuthProxyRoundTripper(user.GetName(), user.GetGroups(), user.GetExtra(), proxyRoundTripper)

	// If we are upgrading, then the upgrade path tries to use this request with the TLS config we provide, but it does
	// NOT use the proxyRoundTripper.  It's a direct dial that bypasses the proxyRoundTripper.  This means that we have to
	// attach the "correct" user headers to the request ahead of time.
	if upgrade {
		transport.SetAuthProxyHeaders(newReq, user.GetName(), user.GetGroups(), user.GetExtra())
	}

	// TODO 分析这里
	handler := proxy.NewUpgradeAwareHandler(location, proxyRoundTripper, true, upgrade, &responder{w: w})
	if r.rejectForwardingRedirects {
		handler.RejectForwardingRedirects = true
	}
	utilflowcontrol.RequestDelegated(req.Context())
	// 发起真正请求
	handler.ServeHTTP(w, newReq)
}

// newRequestForProxy returns a shallow copy of the original request with a context that may include a timeout for discovery requests
func newRequestForProxy(location *url.URL, req *http.Request) (*http.Request, context.CancelFunc) {
	newCtx := req.Context()
	cancelFn := func() {}

	if requestInfo, ok := genericapirequest.RequestInfoFrom(req.Context()); ok {
		// trim leading and trailing slashes. Then "/apis/group/version" requests are for discovery, so if we have exactly three
		// segments that we are going to proxy, we have a discovery request.
		if !requestInfo.IsResourceRequest && len(strings.Split(strings.Trim(requestInfo.Path, "/"), "/")) == 3 {
			// discovery requests are used by kubectl and others to determine which resources a server has.  This is a cheap call that
			// should be fast for every aggregated apiserver.  Latency for aggregation is expected to be low (as for all extensions)
			// so forcing a short timeout here helps responsiveness of all clients.
			newCtx, cancelFn = context.WithTimeout(newCtx, aggregatedDiscoveryTimeout)
		}
	}

	// WithContext creates a shallow clone of the request with the same context.
	newReq := req.WithContext(newCtx)
	newReq.Header = utilnet.CloneHeader(req.Header)
	newReq.URL = location
	newReq.Host = location.Host

	// If the original request has an audit ID, let's make sure we propagate this
	// to the aggregated server.
	if auditID, found := audit.AuditIDFrom(req.Context()); found {
		newReq.Header.Set(auditinternal.HeaderAuditID, string(auditID))
	}

	return newReq, cancelFn
}

// responder implements rest.Responder for assisting a connector in writing objects or errors.
type responder struct {
	w http.ResponseWriter
}

// TODO this should properly handle content type negotiation
// if the caller asked for protobuf and you write JSON bad things happen.
func (r *responder) Object(statusCode int, obj runtime.Object) {
	responsewriters.WriteRawJSON(statusCode, obj, r.w)
}

func (r *responder) Error(_ http.ResponseWriter, _ *http.Request, err error) {
	http.Error(r.w, err.Error(), http.StatusServiceUnavailable)
}

// these methods provide locked access to fields

// Sets serviceAvailable value on proxyHandler
// not thread safe
func (r *proxyHandler) setServiceAvailable() {
	info := r.handlingInfo.Load().(proxyHandlingInfo)
	info.serviceAvailable = true
	r.handlingInfo.Store(info)
}

// 根据指定的APIService更新它的Handler，主要是重新获取代理证书、私钥以及TLS配置
func (r *proxyHandler) updateAPIService(apiService *apiregistrationv1api.APIService) {
	// 如果Service为空，说明是APIServer或者是ExtensionServer
	if apiService.Spec.Service == nil {
		r.handlingInfo.Store(proxyHandlingInfo{local: true})
		return
	}

	// 获取代理证书、以及私钥
	proxyClientCert, proxyClientKey := r.proxyCurrentCertKeyContent()

	transportConfig := &transport.Config{
		TLS: transport.TLSConfig{
			Insecure:   apiService.Spec.InsecureSkipTLSVerify,
			ServerName: apiService.Spec.Service.Name + "." + apiService.Spec.Service.Namespace + ".svc",
			CertData:   proxyClientCert,
			KeyData:    proxyClientKey,
			CAData:     apiService.Spec.CABundle,
		},
		DialHolder: r.proxyTransportDial,
	}
	transportConfig.Wrap(x509metrics.NewDeprecatedCertificateRoundTripperWrapperConstructor(
		x509MissingSANCounter,
		x509InsecureSHA1Counter,
	))

	newInfo := proxyHandlingInfo{
		name:             apiService.Name,
		transportConfig:  transportConfig,
		serviceName:      apiService.Spec.Service.Name,
		serviceNamespace: apiService.Spec.Service.Namespace,
		servicePort:      *apiService.Spec.Service.Port,
		serviceAvailable: apiregistrationv1apihelper.IsAPIServiceConditionTrue(apiService, apiregistrationv1api.Available),
	}
	newInfo.proxyRoundTripper, newInfo.transportBuildingError = transport.New(newInfo.transportConfig)
	if newInfo.transportBuildingError != nil {
		klog.Warning(newInfo.transportBuildingError.Error())
	}
	r.handlingInfo.Store(newInfo)
}
