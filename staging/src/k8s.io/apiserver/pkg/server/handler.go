/*
Copyright 2017 The Kubernetes Authors.

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

package server

import (
	"bytes"
	"fmt"
	"net/http"
	rt "runtime"
	"sort"
	"strings"

	"github.com/emicklei/go-restful/v3"
	"k8s.io/klog/v2"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/apiserver/pkg/server/mux"
)

// APIServerHandler holds the different http.Handlers used by the API server.
// This includes the full handler chain, the director (which chooses between gorestful and nonGoRestful,
// the gorestful handler (used for the API) which falls through to the nonGoRestful handler on unregistered paths,
// and the nonGoRestful handler (which can contain a fallthrough of its own)
// FullHandlerChain -> Director -> {GoRestfulContainer,NonGoRestfulMux} based on inspection of registered web services
// 1、APIServerHandler本质上就是要一个http.Handler，用于处理HTTP请求
// 2、本质上APIServer, ExtensionServer, AggregatorServer都是Web服务，因此肯定需要一个http.Handler处理HTTP请求。所以K8S就需要一个
// 地方可以用来保存合法的路由以及对应的处理器。APIServerHandler的设计就是如此。只不过路由被分别放在了两个地方保存，同时根据Delegator的设计
// 思路，分别设计了一个需要鉴权、认证、审计的http.Handler，以及不需要认证、鉴权、审计的http.Handler。

type APIServerHandler struct {
	// FullHandlerChain is the one that is eventually served with.  It should include the full filter
	// chain and then call the Director.
	// 需要经过认证、鉴权、审计、流程的Handler
	FullHandlerChain http.Handler
	// The registered APIs.  InstallAPIs uses this.  Other servers probably shouldn't access this directly.
	// 1、一个Container就是一个Web服务，里面包含了这个Web服务的路由信息
	// 2、注册的路由有：/versions, /apis, /apis/<group>, /apis/<group>/<version>, /apis/<group>/<version>/<resource>, 不包含核心资源
	// 除了核心资源意外的其余资源增删改查也是在这里注册的, /logs, /.well-known/openid-configuration, /openid/v1/jwks路由, /api
	GoRestfulContainer *restful.Container
	// NonGoRestfulMux is the final HTTP handler in the chain.
	// It comes after all filters and the API handling
	// This is where other servers can attach handler to various parts of the chain.
	// TODO k8s为什么需要自定义PathRecorderMux?
	// 1、NonGoRestfulMux其实一个多路复用器，里面包含了很多路由
	// 2、添加的路由有：/, /index.html, /debug/pprof, /debug/pprof/, /debug/pprof/profile， /debug/pprof/symbol,
	// /debug/pprof/trace, /metrics, /metrics/slis, /debug/api_priority_and_fairness/dump_priority_levels,
	// /debug/api_priority_and_fairness/dump_queues, /debug/api_priority_and_fairness/dump_requests, /healthz,
	// /livez, /readyz, /openapi/v2, /openapi/v3, APIService的定义，譬如/apis/<group>/<version>
	NonGoRestfulMux *mux.PathRecorderMux

	// Director is here so that we can properly handle fall through and proxy cases.
	// This looks a bit bonkers, but here's what's happening.  We need to have /apis handling registered in gorestful in order to have
	// swagger generated for compatibility.  Doing that with `/apis` as a webservice, means that it forcibly 404s (no defaulting allowed)
	// all requests which are not /apis or /apis/.  We need those calls to fall through behind goresful for proper delegation.  Trying to
	// register for a pattern which includes everything behind it doesn't work because gorestful negotiates for verbs and content encoding
	// and all those things go crazy when gorestful really just needs to pass through.  In addition, openapi enforces unique verb constraints
	// which we don't fit into and it still muddies up swagger.  Trying to switch the webservices into a route doesn't work because the
	//  containing webservice faces all the same problems listed above.
	// This leads to the crazy thing done here.  Our mux does what we need, so we'll place it in front of gorestful.  It will introspect to
	// decide if the route is likely to be handled by goresful and route there if needed.  Otherwise, it goes to NonGoRestfulMux mux in
	// order to handle "normal" paths and delegation. Hopefully no API consumers will ever have to deal with this level of detail.  I think
	// we should consider completely removing gorestful.
	// Other servers should only use this opaquely to delegate to an API server.
	// 1、直接处理请求，无需再经过认证、鉴权、审计、限流等处理器
	// 2、为什么需要这么一个Director呢？是因为当有多个GenericServer级联时，只需要第一个GenericServer完成认证、鉴权、审计、限流等操作即可。
	// 后续的GenericServer收到请求的时候直接处理请求即可，不需要再次执行相同的动作，完全没有必要。
	Director http.Handler
}

// HandlerChainBuilderFn is used to wrap the GoRestfulContainer handler using the provided handler chain.
// It is normally used to apply filtering like authentication and authorization
type HandlerChainBuilderFn func(apiHandler http.Handler) http.Handler

func NewAPIServerHandler(
	name string, // 当前处理器的名字
	s runtime.NegotiatedSerializer, // 序列化器用于响应请求的时候把go结构体序列化响应请求
	handlerChainBuilder HandlerChainBuilderFn, // 默认的请求处理链
	notFoundHandler http.Handler, // 如果自己处理不了这个请求，就需要把请求委派给NotFoundHandler
) *APIServerHandler {
	nonGoRestfulMux := mux.NewPathRecorderMux(name)
	if notFoundHandler != nil {
		nonGoRestfulMux.NotFoundHandler(notFoundHandler)
	}

	gorestfulContainer := restful.NewContainer()
	gorestfulContainer.ServeMux = http.NewServeMux()
	gorestfulContainer.Router(restful.CurlyRouter{}) // e.g. for proxy/{kind}/{name}/{*}
	gorestfulContainer.RecoverHandler(func(panicReason interface{}, httpWriter http.ResponseWriter) {
		logStackOnRecover(s, panicReason, httpWriter)
	})
	gorestfulContainer.ServiceErrorHandler(func(serviceErr restful.ServiceError, request *restful.Request, response *restful.Response) {
		serviceErrorHandler(s, serviceErr, request, response)
	})

	director := director{
		name:               name,
		goRestfulContainer: gorestfulContainer,
		nonGoRestfulMux:    nonGoRestfulMux,
	}

	return &APIServerHandler{
		FullHandlerChain:   handlerChainBuilder(director), // 请求调用链
		GoRestfulContainer: gorestfulContainer,
		NonGoRestfulMux:    nonGoRestfulMux,
		Director:           director,
	}
}

// ListedPaths returns the paths that should be shown under /
// 列出所有的路由
func (a *APIServerHandler) ListedPaths() []string {
	var handledPaths []string
	// Extract the paths handled using restful.WebService
	for _, ws := range a.GoRestfulContainer.RegisteredWebServices() {
		handledPaths = append(handledPaths, ws.RootPath())
	}
	handledPaths = append(handledPaths, a.NonGoRestfulMux.ListedPaths()...)
	sort.Strings(handledPaths)

	return handledPaths
}

type director struct {
	name               string
	goRestfulContainer *restful.Container
	nonGoRestfulMux    *mux.PathRecorderMux
}

func (d director) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	path := req.URL.Path

	// check to see if our webservices want to claim this path
	//先看看是不是资源请求，如果是处理请求，如果不是，那么看看是不是非资源请求
	for _, ws := range d.goRestfulContainer.RegisteredWebServices() {
		switch {
		case ws.RootPath() == "/apis":
			// if we are exactly /apis or /apis/, then we need special handling in loop.
			// normally these are passed to the nonGoRestfulMux, but if discovery is enabled, it will go directly.
			// We can't rely on a prefix match since /apis matches everything (see the big comment on Director above)
			if path == "/apis" || path == "/apis/" {
				klog.V(5).Infof("%v: %v %q satisfied by gorestful with webservice %v", d.name, req.Method, path, ws.RootPath())
				// don't use servemux here because gorestful servemuxes get messed up when removing webservices
				// TODO fix gorestful, remove TPRs, or stop using gorestful
				d.goRestfulContainer.Dispatch(w, req)
				return
			}

		case strings.HasPrefix(path, ws.RootPath()):
			// ensure an exact match or a path boundary match
			if len(path) == len(ws.RootPath()) || path[len(ws.RootPath())] == '/' {
				klog.V(5).Infof("%v: %v %q satisfied by gorestful with webservice %v", d.name, req.Method, path, ws.RootPath())
				// don't use servemux here because gorestful servemuxes get messed up when removing webservices
				// TODO fix gorestful, remove TPRs, or stop using gorestful
				d.goRestfulContainer.Dispatch(w, req)
				return
			}
		}
	}

	// if we didn't find a match, then we just skip gorestful altogether
	klog.V(5).Infof("%v: %v %q satisfied by nonGoRestful", d.name, req.Method, path)
	d.nonGoRestfulMux.ServeHTTP(w, req)
}

// TODO: Unify with RecoverPanics?
func logStackOnRecover(s runtime.NegotiatedSerializer, panicReason interface{}, w http.ResponseWriter) {
	var buffer bytes.Buffer
	buffer.WriteString(fmt.Sprintf("recover from panic situation: - %v\r\n", panicReason))
	for i := 2; ; i++ {
		_, file, line, ok := rt.Caller(i)
		if !ok {
			break
		}
		buffer.WriteString(fmt.Sprintf("    %s:%d\r\n", file, line))
	}
	klog.Errorln(buffer.String())

	headers := http.Header{}
	if ct := w.Header().Get("Content-Type"); len(ct) > 0 {
		headers.Set("Accept", ct)
	}
	responsewriters.ErrorNegotiated(apierrors.NewGenericServerResponse(http.StatusInternalServerError, "", schema.GroupResource{}, "", "", 0, false), s, schema.GroupVersion{}, w, &http.Request{Header: headers})
}

func serviceErrorHandler(s runtime.NegotiatedSerializer, serviceErr restful.ServiceError, request *restful.Request, resp *restful.Response) {
	responsewriters.ErrorNegotiated(
		apierrors.NewGenericServerResponse(serviceErr.Code, "", schema.GroupResource{}, "", serviceErr.Message, 0, false),
		s,
		schema.GroupVersion{},
		resp,
		request.Request,
	)
}

// ServeHTTP makes it an http.Handler
func (a *APIServerHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.FullHandlerChain.ServeHTTP(w, r)
}
