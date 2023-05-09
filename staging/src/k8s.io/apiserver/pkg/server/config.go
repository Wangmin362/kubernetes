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

package server

import (
	"fmt"
	"net"
	"net/http"
	goruntime "runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/google/uuid"
	oteltrace "go.opentelemetry.io/otel/trace"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/sets"
	utilwaitgroup "k8s.io/apimachinery/pkg/util/waitgroup"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/authenticatorfactory"
	authenticatorunion "k8s.io/apiserver/pkg/authentication/request/union"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/authorization/authorizerfactory"
	authorizerunion "k8s.io/apiserver/pkg/authorization/union"
	"k8s.io/apiserver/pkg/endpoints/discovery"
	"k8s.io/apiserver/pkg/endpoints/filterlatency"
	genericapifilters "k8s.io/apiserver/pkg/endpoints/filters"
	apiopenapi "k8s.io/apiserver/pkg/endpoints/openapi"
	apirequest "k8s.io/apiserver/pkg/endpoints/request"
	genericfeatures "k8s.io/apiserver/pkg/features"
	genericregistry "k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	"k8s.io/apiserver/pkg/server/egressselector"
	genericfilters "k8s.io/apiserver/pkg/server/filters"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/apiserver/pkg/server/routes"
	serverstore "k8s.io/apiserver/pkg/server/storage"
	"k8s.io/apiserver/pkg/storageversion"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	utilflowcontrol "k8s.io/apiserver/pkg/util/flowcontrol"
	flowcontrolrequest "k8s.io/apiserver/pkg/util/flowcontrol/request"
	"k8s.io/client-go/informers"
	restclient "k8s.io/client-go/rest"
	"k8s.io/component-base/logs"
	"k8s.io/klog/v2"
	openapicommon "k8s.io/kube-openapi/pkg/common"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/utils/clock"
	utilsnet "k8s.io/utils/net"

	// install apis
	_ "k8s.io/apiserver/pkg/apis/apiserver/install"
)

const (
	// DefaultLegacyAPIPrefix is where the legacy APIs will be located.
	// 所有的core API 都是装载到了这个路径下， 所谓的Legecy，实际上就是因为core资源实现的比较早，因此称之为Legacy
	DefaultLegacyAPIPrefix = "/api"

	// APIGroupPrefix is where non-legacy API group will be located.
	APIGroupPrefix = "/apis"
)

// Config is a structure used to configure a GenericAPIServer.
// Its members are sorted roughly in order of importance for composers.
// TODO generic server config
// TODO k8s认为一个GenericServer应该需要哪些配置呢？ K8S是如何进行抽象的？
type Config struct {
	// SecureServing is required to serve https
	// 安全信息，实际上就是用于指定HTTPS启用哪个端口？ 证书是啥？ 加密Cihper选择啥？
	// GenericServer作为一个WEB Server，肯定是需要接受用户请求的，因此需要绑定端口，如果是HTTP协议
	// 那显然不需要这些信息，而如果启用HTTPS协议，肯定是需要告诉GenericServer TLS相关的信息的
	SecureServing *SecureServingInfo

	// Authentication is the configuration for authentication
	// 1、Authentication中包含APIAudiences以及一个Authenticator
	// 2、K8S支持多种认证方式，譬如Token认证、匿名认证、OIDC认证、CA认证、Webhook认证、RequestHeader认证、Service认证
	// 3、K8S会为每一种认证方式实例化一个认证器，也就是说这里的Authentication.Authenticator是一个认证器数组，并且这个数组实现了 authenticator.Request 接口，
	// 所以这个认证器数组还是一个认证器，只不过在真正认证一个HTTP请求时，认证器数组会依次遍历内部的认证器进行认证
	// 4、TODO 认证器发生的时间
	Authentication AuthenticationInfo

	// Authorization is the configuration for authorization
	// 1、K8S有AlwaysAllow,AlwaysDeny,ABAC,Webhook,RBAC,Node这几种模式，用户可以选择配置其中的一个或者几个，甚至可以全部配置；
	// 2、AuthorizationInfo本质上就是Authorizer，也就是是我们所谓的鉴权器。
	// 3、GenericServer的鉴权器本质上是一个[]Authorizer，也就是一个鉴权器数组。这个鉴权器数组实现了Authorize方法，也就是说鉴权其数组实际上
	// 就是一个鉴权器，只不过在判断一个HTTP请求是否有权限访问K8S资源的时候鉴权器数组会依次委托给内部的鉴权器
	// 4、从第三点可以看出，鉴权器数组的顺序非常重要，前面的鉴权器只要做除了鉴权决定吗，后面的鉴权器将永远不会得到执行
	// 5、TODO K8S鉴权发生的时间
	// 6、TODO K8S支持的集中鉴权模式有何区别？分别适用于什么场景？
	Authorization AuthorizationInfo

	// LoopbackClientConfig is a config for a privileged loopback connection to the API server
	// This is required for proper functioning of the PostStartHooks on a GenericAPIServer
	// TODO: move into SecureServing(WithLoopback) as soon as insecure serving is gone
	// TODO 什么叫做Loopback客户端？ 它应该有什么样的功能？ 实际上就是K8S的客户端工具的配置
	// 这个属性是什么时候初始化的？ 答：buildGenericConfig函数的时候进行了一些初始化
	LoopbackClientConfig *restclient.Config

	// EgressSelector provides a lookup mechanism for dialing outbound connections.
	// It does so based on a EgressSelectorConfiguration which was read at startup.
	// TODO K8S有master, worker两种节点，他们之间的通信需要安全加密 而Konnectivity就是用于解决这个问题的
	// egressorselector就是在配置Konnectivity
	// 参考：https://kubernetes.io/zh-cn/docs/tasks/extend-kubernetes/setup-konnectivity/
	// https://www.jianshu.com/p/d1528a9d3cfa
	EgressSelector *egressselector.EgressSelector

	// RuleResolver is required to get the list of rules that apply to a given user
	// in a given namespace
	RuleResolver authorizer.RuleResolver
	// AdmissionControl performs deep inspection of a given request (including content)
	// to set values and determine whether its allowed
	// 准入控制，实际上AdmissionControl是一个准入控制插件链，简单来说就是一个准入控制插件数组，实现了Admit以及Validate方法
	// 当请求到来时，会一次执行每个准入控制插件
	AdmissionControl admission.Interface
	// TODO CORS控制
	CorsAllowedOriginList []string
	// TODO 啥是HSTS 答：Strict-Transport-Security
	// 参考：golang.org/issue/26162
	HSTSDirectives []string
	// FlowControl, if not nil, gives priority and fairness to request handling
	// TODO apiserver的流控到底是怎么设计的? apisix限速
	FlowControl utilflowcontrol.Interface

	// 这玩意有啥用？和Informer是否有关？ 答：和informer并不相关 这里的index指的是K8S首页面
	EnableIndex bool
	// 什么叫做Profiling?  实际上就是开启了/debug/pprof/相关的api
	EnableProfiling bool
	// TODO 发现是发现的啥？
	EnableDiscovery bool
	// Requires generic profiling enabled
	// TODO 什么叫做内容Profiling 答：Profiling主要用于K8S性能分析
	EnableContentionProfiling bool
	EnableMetrics             bool

	// TODO 禁用的后置处理器的名称集合
	DisabledPostStartHooks sets.String
	// done values in this values for this map are ignored.
	// TODO 后置处理器，发生在哪个时间点？
	PostStartHooks map[string]PostStartHookConfigEntry

	// Version will enable the /version endpoint if non-nil
	// 构建K8S所需工具的版本信息，譬如K8S的版本信息、go的版本信息、Arch， Git版本信息
	Version *version.Info
	// AuditBackend is where audit events are sent to.
	// TODO K8S中的审计是如何设计的？
	AuditBackend audit.Backend
	// AuditPolicyRuleEvaluator makes the decision of whether and how to audit log a request.
	AuditPolicyRuleEvaluator audit.PolicyRuleEvaluator
	// ExternalAddress is the host name to use for external (public internet) facing URLs (e.g. Swagger)
	// Will default to a value based on secure serving info and available ipv4 IPs.
	ExternalAddress string

	// TracerProvider can provide a tracer, which records spans for distributed tracing.
	TracerProvider oteltrace.TracerProvider

	//===========================================================================
	// Fields you probably don't care about changing
	//===========================================================================

	// BuildHandlerChainFunc allows you to build custom handler chains by decorating the apiHandler.
	// TODO 构建请求链，主要是认证、限速、鉴权、审计、CORS、日志
	BuildHandlerChainFunc func(apiHandler http.Handler, c *Config) (secure http.Handler)
	// HandlerChainWaitGroup allows you to wait for all chain handlers exit after the server shutdown.
	// TODO 如何理解这玩意？为啥需要？干嘛用的？
	HandlerChainWaitGroup *utilwaitgroup.SafeWaitGroup
	// DiscoveryAddresses is used to build the IPs pass to discovery. If nil, the ExternalAddress is
	// always reported
	// TODO 什么叫做DiscoveryAddress?
	DiscoveryAddresses discovery.Addresses
	// The default set of healthz checks. There might be more added via AddHealthChecks dynamically.
	HealthzChecks []healthz.HealthChecker
	// The default set of livez checks. There might be more added via AddHealthChecks dynamically.
	LivezChecks []healthz.HealthChecker
	// The default set of readyz-only checks. There might be more added via AddReadyzChecks dynamically.
	ReadyzChecks []healthz.HealthChecker
	// LegacyAPIGroupPrefixes is used to set up URL parsing for authorization and for validating requests
	// to InstallLegacyAPIGroup. New API servers don't generally have legacy groups at all.
	// TODO 目前看来 Legacy只有 /api前缀，不会再有其它前缀了
	LegacyAPIGroupPrefixes sets.String
	// RequestInfoResolver is used to assign attributes (used by admission and authorization) based on a request URL.
	// Use-cases that are like kubelets may need to customize this.
	// TODO 用于解析标注的HTTP请求为RequestInfo信息
	RequestInfoResolver apirequest.RequestInfoResolver
	// Serializer is required and provides the interface for serializing and converting objects to and from the wire
	// The default (api.Codecs) usually works fine.
	// 资源的序列化、反序列化，因为数据最终是需要存储到ETCD的，所以必须要把资源数据系列化为byte数组
	Serializer runtime.NegotiatedSerializer
	// OpenAPIConfig will be used in generating OpenAPI spec. This is nil by default. Use DefaultOpenAPIConfig for "working" defaults.
	OpenAPIConfig *openapicommon.Config
	// OpenAPIV3Config will be used in generating OpenAPI V3 spec. This is nil by default. Use DefaultOpenAPIV3Config for "working" defaults.
	OpenAPIV3Config *openapicommon.Config
	// SkipOpenAPIInstallation avoids installing the OpenAPI handler if set to true.
	SkipOpenAPIInstallation bool

	// RESTOptionsGetter is used to construct RESTStorage types via the generic registry.
	// 通过RESTOptionsGetter可以获取到每个资源的后端存储，主要包含两个方面：
	// 1、后端存储的存储配置，具体来说包含：如何序列化、反序列化、存储前缀、使用哪种类型的后端存储、转化器、租约管理器
	// 2、后端存储，简单来说就是ETCD
	RESTOptionsGetter genericregistry.RESTOptionsGetter

	// If specified, all requests except those which match the LongRunningFunc predicate will timeout
	// after this duration.
	RequestTimeout time.Duration
	// If specified, long running requests such as watch will be allocated a random timeout between this value, and
	// twice this value.  Note that it is up to the request handlers to ignore or honor this timeout. In seconds.
	MinRequestTimeout int

	// This represents the maximum amount of time it should take for apiserver to complete its startup
	// sequence and become healthy. From apiserver's start time to when this amount of time has
	// elapsed, /livez will assume that unfinished post-start hooks will complete successfully and
	// therefore return true.
	LivezGracePeriod time.Duration
	// ShutdownDelayDuration allows to block shutdown for some time, e.g. until endpoints pointing to this API server
	// have converged on all node. During this time, the API server keeps serving, /healthz will return 200,
	// but /readyz will return failure.
	ShutdownDelayDuration time.Duration

	// The limit on the total size increase all "copy" operations in a json
	// patch may cause.
	// This affects all places that applies json patch in the binary.
	JSONPatchMaxCopyBytes int64
	// The limit on the request size that would be accepted and decoded in a write request
	// 0 means no limit.
	MaxRequestBodyBytes int64
	// MaxRequestsInFlight is the maximum number of parallel non-long-running requests. Every further
	// request has to wait. Applies only to non-mutating requests.
	// TODO apiserver限速相关
	MaxRequestsInFlight int
	// MaxMutatingRequestsInFlight is the maximum number of parallel mutating requests. Every further
	// request has to wait.
	// TODO apiserver限速相关
	MaxMutatingRequestsInFlight int
	// Predicate which is true for paths of long-running http requests
	// 譬如log, watch, exec, proxy等等操作
	LongRunningFunc apirequest.LongRunningRequestCheck

	// GoawayChance is the probability that send a GOAWAY to HTTP/2 clients. When client received
	// GOAWAY, the in-flight requests will not be affected and new requests will use
	// a new TCP connection to triggering re-balancing to another server behind the load balance.
	// Default to 0, means never send GOAWAY. Max is 0.02 to prevent break the apiserver.
	// TODO 主要作用是啥？
	// 参考文章：https://www.kubernetes.org.cn/6898.html
	GoawayChance float64

	// MergedResourceConfig indicates which groupVersion enabled and its resources enabled/disabled.
	// This is composed of genericapiserver defaultAPIResourceConfig and those parsed from flags.
	// If not specify any in flags, then genericapiserver will only enable defaultAPIResourceConfig.
	// 用于标识K8S资源或者子资源的启用或者禁用
	MergedResourceConfig *serverstore.ResourceConfig

	// lifecycleSignals provides access to the various signals
	// that happen during lifecycle of the apiserver.
	// it's intentionally marked private as it should never be overridden.
	lifecycleSignals lifecycleSignals

	// StorageObjectCountTracker is used to keep track of the total number of objects
	// in the storage per resource, so we can estimate width of incoming requests.
	// TODO 似乎是和流控相关
	StorageObjectCountTracker flowcontrolrequest.StorageObjectCountTracker

	// ShutdownSendRetryAfter dictates when to initiate shutdown of the HTTP
	// Server during the graceful termination of the apiserver. If true, we wait
	// for non longrunning requests in flight to be drained and then initiate a
	// shutdown of the HTTP Server. If false, we initiate a shutdown of the HTTP
	// Server as soon as ShutdownDelayDuration has elapsed.
	// If enabled, after ShutdownDelayDuration elapses, any incoming request is
	// rejected with a 429 status code and a 'Retry-After' response.
	ShutdownSendRetryAfter bool

	//===========================================================================
	// values below here are targets for removal
	//===========================================================================

	// PublicAddress is the IP address where members of the cluster (kubelet,
	// kube-proxy, services, etc.) can reach the GenericAPIServer.
	// If nil or 0.0.0.0, the host's default interface will be used.
	PublicAddress net.IP

	// EquivalentResourceRegistry provides information about resources equivalent to a given resource,
	// and the kind associated with a given resource. As resources are installed, they are registered here.
	// TODO 等效资源应该指的是同一个资源，由于历史原因，出现在了两个group当中
	EquivalentResourceRegistry runtime.EquivalentResourceRegistry

	// APIServerID is the ID of this API server
	// TODO API的ID是用来干嘛的？
	APIServerID string

	// StorageVersionManager holds the storage versions of the API resources installed by this server.
	// TODO 这玩意干嘛的？
	StorageVersionManager storageversion.Manager
}

// RecommendedConfig
// TODO 为什么叫做RecommendedConfig,是因为informer的加入，后续的操作都是本地缓存的原因么？
type RecommendedConfig struct {
	Config // generic server config

	// SharedInformerFactory provides shared informers for Kubernetes resources. This value is set by
	// RecommendedOptions.CoreAPI.ApplyTo called by RecommendedOptions.ApplyTo. It uses an in-cluster client config
	// by default, or the kubeconfig given with kubeconfig command line flag.
	// TODO 这个缓存，缓存了啥？
	SharedInformerFactory informers.SharedInformerFactory

	// ClientConfig holds the kubernetes client configuration.
	// This value is set by RecommendedOptions.CoreAPI.ApplyTo called by RecommendedOptions.ApplyTo.
	// By default in-cluster client config is used.
	// restful客户端，用于向APIServer发送HTTP请求
	ClientConfig *restclient.Config
}

type SecureServingInfo struct {
	// Listener is the secure server network listener.
	Listener net.Listener

	// Cert is the main server cert which is used if SNI does not match. Cert must be non-nil and is
	// allowed to be in SNICerts.
	// TODO 如何理解这个属性，这个属性干嘛的？
	Cert dynamiccertificates.CertKeyContentProvider

	// SNICerts are the TLS certificates used for SNI.
	SNICerts []dynamiccertificates.SNICertKeyContentProvider

	// ClientCA is the certificate bundle for all the signers that you'll recognize for incoming client certificates
	// 1、监听client-ca-file参数所指向的证书，如果此证书发生变化，那么CAContentProvider会通知所有对该文件感兴趣的Controller，当然，前提是
	// 这些Controller需要注册到CAContentProvider当中
	// 2、实际上这里的ClientCA参数是一个unionCAContent类型，而unionCAContent是[]CAContentProvider类型。同时unionCAContent实现了
	// CAContentProvider接口，也就是所unionCAContent会监听多个文件，并非监听一个文件
	ClientCA dynamiccertificates.CAContentProvider

	// MinTLSVersion optionally overrides the minimum TLS version supported.
	// Values are from tls package constants (https://golang.org/pkg/crypto/tls/#pkg-constants).
	MinTLSVersion uint16

	// CipherSuites optionally overrides the list of allowed cipher suites for the server.
	// Values are from tls package constants (https://golang.org/pkg/crypto/tls/#pkg-constants).
	// TLS加密套件
	CipherSuites []uint16

	// HTTP2MaxStreamsPerConnection is the limit that the api server imposes on each client.
	// A value of zero means to use the default provided by golang's HTTP/2 support.
	// TODO 这个属性干嘛的？
	HTTP2MaxStreamsPerConnection int

	// DisableHTTP2 indicates that http2 should not be enabled.
	DisableHTTP2 bool
}

type AuthenticationInfo struct {
	// APIAudiences is a list of identifier that the API identifies as. This is
	// used by some authenticators to validate audience bound credentials.
	// TODO 如何理解Audiences?
	APIAudiences authenticator.Audiences
	// Authenticator determines which subject is making the request
	// 1、认证器，用于从HTTP请求当中抽取认证信息
	// 2、K8S支持多种认证方式，譬如Token认证、匿名认证、OIDC认证、CA认证、Webhook认证、RequestHeader认证、Service认证
	// 3、K8S会为每一种认证方式实例化一个认证器，也就是说这里的Authenticator是一个认证器数组，并且这个数组实现了 authenticator.Request 接口，
	// 所以这个认证器数组还是一个认证器，只不过在真正认证一个HTTP请求时，认证器数组会依次遍历内部的认证器进行认证
	Authenticator authenticator.Request
}

type AuthorizationInfo struct {
	// Authorizer determines whether the subject is allowed to make the request based only
	// on the RequestURI
	// 用于判断发起当前HTTP请求的用户是否有权限访问K8S资源
	Authorizer authorizer.Authorizer
}

// NewConfig returns a Config struct with the default values
func NewConfig(codecs serializer.CodecFactory) *Config {
	defaultHealthChecks := []healthz.HealthChecker{healthz.PingHealthz, healthz.LogHealthz}
	var id string
	if utilfeature.DefaultFeatureGate.Enabled(genericfeatures.APIServerIdentity) {
		id = "kube-apiserver-" + uuid.New().String()
	}
	lifecycleSignals := newLifecycleSignals()

	return &Config{
		Serializer:                  codecs,
		BuildHandlerChainFunc:       DefaultBuildHandlerChain, // todo 重点分析
		HandlerChainWaitGroup:       new(utilwaitgroup.SafeWaitGroup),
		LegacyAPIGroupPrefixes:      sets.NewString(DefaultLegacyAPIPrefix),
		DisabledPostStartHooks:      sets.NewString(),
		PostStartHooks:              map[string]PostStartHookConfigEntry{},
		HealthzChecks:               append([]healthz.HealthChecker{}, defaultHealthChecks...),
		ReadyzChecks:                append([]healthz.HealthChecker{}, defaultHealthChecks...),
		LivezChecks:                 append([]healthz.HealthChecker{}, defaultHealthChecks...),
		EnableIndex:                 true,
		EnableDiscovery:             true,
		EnableProfiling:             true,
		EnableMetrics:               true,
		MaxRequestsInFlight:         400, // 限速策略
		MaxMutatingRequestsInFlight: 200, // TODO 这玩意是和mutating webhook相关的配置么?
		RequestTimeout:              time.Duration(60) * time.Second,
		MinRequestTimeout:           1800, // 30分钟？意思是exec, logs这类的操作最长操作30分钟？
		LivezGracePeriod:            time.Duration(0),
		ShutdownDelayDuration:       time.Duration(0),
		// 1.5MB is the default client request size in bytes
		// the etcd server should accept. See
		// https://github.com/etcd-io/etcd/blob/release-3.4/embed/config.go#L56.
		// A request body might be encoded in json, and is converted to
		// proto when persisted in etcd, so we allow 2x as the largest size
		// increase the "copy" operations in a json patch may cause.
		JSONPatchMaxCopyBytes: int64(3 * 1024 * 1024),
		// 1.5MB is the recommended client request size in byte
		// the etcd server should accept. See
		// https://github.com/etcd-io/etcd/blob/release-3.4/embed/config.go#L56.
		// A request body might be encoded in json, and is converted to
		// proto when persisted in etcd, so we allow 2x as the largest request
		// body size to be accepted and decoded in a write request.
		// If this constant is changed, maxRequestSizeBytes in apiextensions-apiserver/third_party/forked/celopenapi/model/schemas.go
		// should be changed to reflect the new value, if the two haven't
		// been wired together already somehow.
		MaxRequestBodyBytes: int64(3 * 1024 * 1024),

		// Default to treating watch as a long-running operation
		// Generic API servers have no inherent long-running subresources
		// TODO 为什么默认只有Watch操作被认为是长时间的请求，为什么没有EXEC? 是因为EXEC可以被禁用？
		LongRunningFunc:           genericfilters.BasicLongRunningRequestCheck(sets.NewString("watch"), sets.NewString()),
		lifecycleSignals:          lifecycleSignals,
		StorageObjectCountTracker: flowcontrolrequest.NewStorageObjectCountTracker(),

		APIServerID:           id,
		StorageVersionManager: storageversion.NewDefaultManager(),
		TracerProvider:        oteltrace.NewNoopTracerProvider(),
	}
}

// NewRecommendedConfig returns a RecommendedConfig struct with the default values
func NewRecommendedConfig(codecs serializer.CodecFactory) *RecommendedConfig {
	return &RecommendedConfig{
		Config: *NewConfig(codecs),
	}
}

// DefaultOpenAPIConfig provides the default OpenAPIConfig used to build the OpenAPI V2 spec
func DefaultOpenAPIConfig(getDefinitions openapicommon.GetOpenAPIDefinitions, defNamer *apiopenapi.DefinitionNamer) *openapicommon.Config {
	return &openapicommon.Config{
		ProtocolList:   []string{"https"},
		IgnorePrefixes: []string{},
		Info: &spec.Info{
			InfoProps: spec.InfoProps{
				Title: "Generic API Server",
			},
		},
		DefaultResponse: &spec.Response{
			ResponseProps: spec.ResponseProps{
				Description: "Default Response.",
			},
		},
		GetOperationIDAndTags: apiopenapi.GetOperationIDAndTags,
		GetDefinitionName:     defNamer.GetDefinitionName,
		GetDefinitions:        getDefinitions,
	}
}

// DefaultOpenAPIV3Config provides the default OpenAPIV3Config used to build the OpenAPI V3 spec
func DefaultOpenAPIV3Config(getDefinitions openapicommon.GetOpenAPIDefinitions, defNamer *apiopenapi.DefinitionNamer) *openapicommon.Config {
	defaultConfig := DefaultOpenAPIConfig(getDefinitions, defNamer)
	defaultConfig.Definitions = getDefinitions(func(name string) spec.Ref {
		defName, _ := defaultConfig.GetDefinitionName(name)
		return spec.MustCreateRef("#/components/schemas/" + openapicommon.EscapeJsonPointer(defName))
	})

	return defaultConfig
}

func (c *AuthenticationInfo) ApplyClientCert(clientCA dynamiccertificates.CAContentProvider, servingInfo *SecureServingInfo) error {
	if servingInfo == nil {
		return nil
	}
	if clientCA == nil {
		return nil
	}
	if servingInfo.ClientCA == nil {
		servingInfo.ClientCA = clientCA
		return nil
	}

	servingInfo.ClientCA = dynamiccertificates.NewUnionCAContentProvider(servingInfo.ClientCA, clientCA)
	return nil
}

type completedConfig struct {
	*Config

	//===========================================================================
	// values below here are filled in during completion
	//===========================================================================

	// SharedInformerFactory provides shared informers for resources
	SharedInformerFactory informers.SharedInformerFactory
}

type CompletedConfig struct {
	// Embed a private pointer that cannot be instantiated outside of this package.
	*completedConfig
}

// AddHealthChecks adds a health check to our config to be exposed by the health endpoints
// of our configured apiserver. We should prefer this to adding healthChecks directly to
// the config unless we explicitly want to add a healthcheck only to a specific health endpoint.
func (c *Config) AddHealthChecks(healthChecks ...healthz.HealthChecker) {
	c.HealthzChecks = append(c.HealthzChecks, healthChecks...)
	c.LivezChecks = append(c.LivezChecks, healthChecks...)
	c.ReadyzChecks = append(c.ReadyzChecks, healthChecks...)
}

// AddReadyzChecks adds a health check to our config to be exposed by the readyz endpoint
// of our configured apiserver.
func (c *Config) AddReadyzChecks(healthChecks ...healthz.HealthChecker) {
	c.ReadyzChecks = append(c.ReadyzChecks, healthChecks...)
}

// AddPostStartHook allows you to add a PostStartHook that will later be added to the server itself in a New call.
// Name conflicts will cause an error.
func (c *Config) AddPostStartHook(name string, hook PostStartHookFunc) error {
	if len(name) == 0 {
		return fmt.Errorf("missing name")
	}
	if hook == nil {
		return fmt.Errorf("hook func may not be nil: %q", name)
	}
	if c.DisabledPostStartHooks.Has(name) {
		klog.V(1).Infof("skipping %q because it was explicitly disabled", name)
		return nil
	}

	if postStartHook, exists := c.PostStartHooks[name]; exists {
		// this is programmer error, but it can be hard to debug
		return fmt.Errorf("unable to add %q because it was already registered by: %s", name, postStartHook.originatingStack)
	}
	c.PostStartHooks[name] = PostStartHookConfigEntry{hook: hook, originatingStack: string(debug.Stack())}

	return nil
}

// AddPostStartHookOrDie allows you to add a PostStartHook, but dies on failure.
func (c *Config) AddPostStartHookOrDie(name string, hook PostStartHookFunc) {
	if err := c.AddPostStartHook(name, hook); err != nil {
		klog.Fatalf("Error registering PostStartHook %q: %v", name, err)
	}
}

func completeOpenAPI(config *openapicommon.Config, version *version.Info) {
	if config == nil {
		return
	}
	if config.SecurityDefinitions != nil {
		// Setup OpenAPI security: all APIs will have the same authentication for now.
		config.DefaultSecurity = []map[string][]string{}
		keys := []string{}
		for k := range *config.SecurityDefinitions {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			config.DefaultSecurity = append(config.DefaultSecurity, map[string][]string{k: {}})
		}
		if config.CommonResponses == nil {
			config.CommonResponses = map[int]spec.Response{}
		}
		if _, exists := config.CommonResponses[http.StatusUnauthorized]; !exists {
			config.CommonResponses[http.StatusUnauthorized] = spec.Response{
				ResponseProps: spec.ResponseProps{
					Description: "Unauthorized",
				},
			}
		}
	}
	// make sure we populate info, and info.version, if not manually set
	if config.Info == nil {
		config.Info = &spec.Info{}
	}
	if config.Info.Version == "" {
		if version != nil {
			config.Info.Version = strings.Split(version.String(), "-")[0]
		} else {
			config.Info.Version = "unversioned"
		}
	}
}

// DrainedNotify returns a lifecycle signal of genericapiserver already drained while shutting down.
func (c *Config) DrainedNotify() <-chan struct{} {
	return c.lifecycleSignals.InFlightRequestsDrained.Signaled()
}

// Complete fills in any fields not set that are required to have valid data and can be derived
// from other fields. If you're going to `ApplyOptions`, do that first. It's mutating the receiver.
func (c *Config) Complete(informers informers.SharedInformerFactory) CompletedConfig {
	if len(c.ExternalAddress) == 0 && c.PublicAddress != nil {
		c.ExternalAddress = c.PublicAddress.String()
	}

	// if there is no port, and we listen on one securely, use that one
	if _, _, err := net.SplitHostPort(c.ExternalAddress); err != nil {
		if c.SecureServing == nil {
			klog.Fatalf("cannot derive external address port without listening on a secure port.")
		}
		_, port, err := c.SecureServing.HostPort()
		if err != nil {
			klog.Fatalf("cannot derive external address from the secure port: %v", err)
		}
		c.ExternalAddress = net.JoinHostPort(c.ExternalAddress, strconv.Itoa(port))
	}

	completeOpenAPI(c.OpenAPIConfig, c.Version)
	completeOpenAPI(c.OpenAPIV3Config, c.Version)

	if c.DiscoveryAddresses == nil {
		c.DiscoveryAddresses = discovery.DefaultAddresses{DefaultAddress: c.ExternalAddress}
	}

	AuthorizeClientBearerToken(c.LoopbackClientConfig, &c.Authentication, &c.Authorization)

	if c.RequestInfoResolver == nil {
		c.RequestInfoResolver = NewRequestInfoResolver(c)
	}

	if c.EquivalentResourceRegistry == nil {
		if c.RESTOptionsGetter == nil {
			c.EquivalentResourceRegistry = runtime.NewEquivalentResourceRegistry()
		} else {
			c.EquivalentResourceRegistry = runtime.NewEquivalentResourceRegistryWithIdentity(func(groupResource schema.GroupResource) string {
				// use the storage prefix as the key if possible
				if opts, err := c.RESTOptionsGetter.GetRESTOptions(groupResource); err == nil {
					return opts.ResourcePrefix
				}
				// otherwise return "" to use the default key (parent GV name)
				return ""
			})
		}
	}

	return CompletedConfig{&completedConfig{c, informers}}
}

// Complete fills in any fields not set that are required to have valid data and can be derived
// from other fields. If you're going to `ApplyOptions`, do that first. It's mutating the receiver.
func (c *RecommendedConfig) Complete() CompletedConfig {
	return c.Config.Complete(c.SharedInformerFactory)
}

// New creates a new server which logically combines the handling chain with the passed server.
// name is used to differentiate for logging. The handler chain in particular can be difficult as it starts delegating.
// delegationTarget may not be nil.
// TODO 实例化一个Generic Server   都做了哪些步骤？
func (c completedConfig) New(name string, delegationTarget DelegationTarget) (*GenericAPIServer, error) {
	if c.Serializer == nil { // 请求Body编解码的处理，没有它是不行的
		return nil, fmt.Errorf("Genericapiserver.New() called with config.Serializer == nil")
	}
	// 这个配置到底如何理解？ 答：实际上这个配置就是实例化clientset需要的配置
	if c.LoopbackClientConfig == nil {
		return nil, fmt.Errorf("Genericapiserver.New() called with config.LoopbackClientConfig == nil")
	}
	// TODO 等效资源注册中心是啥？为啥需要这个东西？
	if c.EquivalentResourceRegistry == nil {
		return nil, fmt.Errorf("Genericapiserver.New() called with config.EquivalentResourceRegistry == nil")
	}

	// handlerChainBuilder类似Java中的Filter，或者Gin中的中间件，实际上就是在请求被真正处理前处理了一道，主要是增加了认证、审计、限速、鉴权等通用工作
	// 真正的资源处理是通过handler放进去的
	handlerChainBuilder := func(handler http.Handler) http.Handler {
		// 请求处理的逻辑，认证、审计、限速等等 extension-apiserver复用了之前的generic-apiserver的初始化逻辑
		return c.BuildHandlerChainFunc(handler, c.Config)
	}

	// TODO APIServerHandler是generic server非常核心的东西，掌握了路由信息
	// 当前创建出来的APIServerHandler是空的，还没有任何的路由信息
	apiServerHandler := NewAPIServerHandler(name, c.Serializer, handlerChainBuilder, delegationTarget.UnprotectedHandler())

	s := &GenericAPIServer{
		discoveryAddresses:         c.DiscoveryAddresses,
		LoopbackClientConfig:       c.LoopbackClientConfig,
		legacyAPIGroupPrefixes:     c.LegacyAPIGroupPrefixes,
		admissionControl:           c.AdmissionControl,
		Serializer:                 c.Serializer,
		AuditBackend:               c.AuditBackend,
		Authorizer:                 c.Authorization.Authorizer,
		delegationTarget:           delegationTarget,
		EquivalentResourceRegistry: c.EquivalentResourceRegistry,
		HandlerChainWaitGroup:      c.HandlerChainWaitGroup,
		Handler:                    apiServerHandler, // 此时的Handler实际上刚刚初始化，并没有任何路由信息

		listedPathProvider: apiServerHandler,

		minRequestTimeout:     time.Duration(c.MinRequestTimeout) * time.Second,
		ShutdownTimeout:       c.RequestTimeout,
		ShutdownDelayDuration: c.ShutdownDelayDuration,
		SecureServingInfo:     c.SecureServing,
		ExternalAddress:       c.ExternalAddress,

		openAPIConfig:           c.OpenAPIConfig,
		openAPIV3Config:         c.OpenAPIV3Config,
		skipOpenAPIInstallation: c.SkipOpenAPIInstallation,

		postStartHooks:         map[string]postStartHookEntry{},
		preShutdownHooks:       map[string]preShutdownHookEntry{},
		disabledPostStartHooks: c.DisabledPostStartHooks,

		healthzChecks:    c.HealthzChecks,
		livezChecks:      c.LivezChecks,
		readyzChecks:     c.ReadyzChecks,
		livezGracePeriod: c.LivezGracePeriod,

		DiscoveryGroupManager: discovery.NewRootAPIsHandler(c.DiscoveryAddresses, c.Serializer),

		maxRequestBodyBytes: c.MaxRequestBodyBytes,
		livezClock:          clock.RealClock{},

		lifecycleSignals:       c.lifecycleSignals,
		ShutdownSendRetryAfter: c.ShutdownSendRetryAfter,

		APIServerID:           c.APIServerID,
		StorageVersionManager: c.StorageVersionManager,

		Version: c.Version,

		muxAndDiscoveryCompleteSignals: map[string]<-chan struct{}{},
	}

	// TODO 为什么这里需要通过CAS设置值？这里为什么会有并发冲突？
	for {
		if c.JSONPatchMaxCopyBytes <= 0 {
			break
		}
		existing := atomic.LoadInt64(&jsonpatch.AccumulatedCopySizeLimit)
		if existing > 0 && existing < c.JSONPatchMaxCopyBytes {
			break
		}
		if atomic.CompareAndSwapInt64(&jsonpatch.AccumulatedCopySizeLimit, existing, c.JSONPatchMaxCopyBytes) {
			break
		}
	}

	// TODO 为什么Delegated的后置处理器需要加进来？
	// 答：因为实际运行的时候，只有最外层的AggregatorServer会被运行，只有它的postStartHooks会被执行，所以在这里再实例化新的
	// GenericServer的时候, 需要把Delegator的postStartHooks添加进来
	for k, v := range delegationTarget.PostStartHooks() {
		s.postStartHooks[k] = v
	}

	for k, v := range delegationTarget.PreShutdownHooks() {
		s.preShutdownHooks[k] = v
	}

	// add poststarthooks that were preconfigured.  Using the add method will give us an error if the same name has already been registered.
	for name, preconfiguredPostStartHook := range c.PostStartHooks {
		if err := s.AddPostStartHook(name, preconfiguredPostStartHook.hook); err != nil {
			return nil, err
		}
	}

	// register mux signals from the delegated server
	for k, v := range delegationTarget.MuxAndDiscoveryCompleteSignals() {
		if err := s.RegisterMuxAndDiscoveryCompleteSignal(k, v); err != nil {
			return nil, err
		}
	}

	genericApiServerHookName := "generic-apiserver-start-informers"
	if c.SharedInformerFactory != nil {
		if !s.isPostStartHookRegistered(genericApiServerHookName) {
			err := s.AddPostStartHook(genericApiServerHookName, func(context PostStartHookContext) error {
				// TODO 启动informer，缓存K8S资源对象
				c.SharedInformerFactory.Start(context.StopCh)
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		// TODO: Once we get rid of /healthz consider changing this to post-start-hook.
		err := s.AddReadyzChecks(healthz.NewInformerSyncHealthz(c.SharedInformerFactory))
		if err != nil {
			return nil, err
		}
	}

	const priorityAndFairnessConfigConsumerHookName = "priority-and-fairness-config-consumer"
	if s.isPostStartHookRegistered(priorityAndFairnessConfigConsumerHookName) {
	} else if c.FlowControl != nil {
		// TODO 啥作用？
		err := s.AddPostStartHook(priorityAndFairnessConfigConsumerHookName, func(context PostStartHookContext) error {
			go c.FlowControl.Run(context.StopCh)
			return nil
		})
		if err != nil {
			return nil, err
		}
		// TODO(yue9944882): plumb pre-shutdown-hook for request-management system?
	} else {
		klog.V(3).Infof("Not requested to run hook %s", priorityAndFairnessConfigConsumerHookName)
	}

	// Add PostStartHooks for maintaining the watermarks for the Priority-and-Fairness and the Max-in-Flight filters.
	if c.FlowControl != nil {
		// TODO 啥作用？
		const priorityAndFairnessFilterHookName = "priority-and-fairness-filter"
		if !s.isPostStartHookRegistered(priorityAndFairnessFilterHookName) {
			err := s.AddPostStartHook(priorityAndFairnessFilterHookName, func(context PostStartHookContext) error {
				genericfilters.StartPriorityAndFairnessWatermarkMaintenance(context.StopCh)
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	} else {
		// TODO apiserver的限速
		const maxInFlightFilterHookName = "max-in-flight-filter"
		if !s.isPostStartHookRegistered(maxInFlightFilterHookName) {
			err := s.AddPostStartHook(maxInFlightFilterHookName, func(context PostStartHookContext) error {
				genericfilters.StartMaxInFlightWatermarkMaintenance(context.StopCh)
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
	}

	// Add PostStartHook for maintenaing the object count tracker.
	if c.StorageObjectCountTracker != nil {
		// TODO 啥作用？
		const storageObjectCountTrackerHookName = "storage-object-count-tracker-hook"
		if !s.isPostStartHookRegistered(storageObjectCountTrackerHookName) {
			if err := s.AddPostStartHook(storageObjectCountTrackerHookName, func(context PostStartHookContext) error {
				go c.StorageObjectCountTracker.RunUntil(context.StopCh)
				return nil
			}); err != nil {
				return nil, err
			}
		}
	}

	for _, delegateCheck := range delegationTarget.HealthzChecks() {
		skip := false
		for _, existingCheck := range c.HealthzChecks {
			if existingCheck.Name() == delegateCheck.Name() {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		s.AddHealthChecks(delegateCheck)
	}

	s.listedPathProvider = routes.ListedPathProviders{s.listedPathProvider, delegationTarget}

	// TODO generic server会安装什么路由呢？
	// 0、添加/ 路由
	// 1、增加/index路由
	// 2、增加/debug/pprof路由
	// 3、增加/debug/flags路由
	// 4、增加/metrics路由
	// 5、增加/version路由
	// 6、增加/apis路由
	// 7、增加/debug/api_priority_and_fairness/dump_priority_levels路由
	installAPI(s, c.Config)

	// use the UnprotectedHandler from the delegation target to ensure that we don't attempt to double authenticator, authorize,
	// or some other part of the filter chain in delegation cases.
	if delegationTarget.UnprotectedHandler() == nil && c.EnableIndex {
		s.Handler.NonGoRestfulMux.NotFoundHandler(routes.IndexLister{
			StatusCode:   http.StatusNotFound,
			PathProvider: s.listedPathProvider,
		})
	}

	return s, nil
}

func BuildHandlerChainWithStorageVersionPrecondition(apiHandler http.Handler, c *Config) http.Handler {
	// WithStorageVersionPrecondition needs the WithRequestInfo to run first
	// TODO StorageVersionPreconditionHandler是干嘛用的？
	handler := genericapifilters.WithStorageVersionPrecondition(apiHandler, c.StorageVersionManager, c.Serializer)
	return DefaultBuildHandlerChain(handler, c)
}

// DefaultBuildHandlerChain 参数apiHandler实际上就是请求的处理逻辑，DefaultBuildHandlerChain的目的是为了在请求被真正
// 处理之前增加一些处理逻辑
func DefaultBuildHandlerChain(apiHandler http.Handler, c *Config) http.Handler {
	// 计算鉴权消耗时间
	// TODO 实际上，apiHandler才是真正的业务处理，也就是K8S的资源处理，只有下面所有的Handler处理完成之后才会执行apiHandler处理器
	// TODO 一旦下面中的任何一个处理拒绝处理请求，apiHandler将不会被执行
	// TODO 准入控制在哪里实现的？

	// TODO 鉴权，如果没有权限直接拒绝
	handler := filterlatency.TrackCompleted(apiHandler)
	handler = genericapifilters.WithAuthorization(handler, c.Authorization.Authorizer, c.Serializer)
	handler = filterlatency.TrackStarted(handler, "authorization")

	// TODO generic server限速相关
	if c.FlowControl != nil {
		workEstimatorCfg := flowcontrolrequest.DefaultWorkEstimatorConfig()
		requestWorkEstimator := flowcontrolrequest.NewWorkEstimator(
			c.StorageObjectCountTracker.Get, c.FlowControl.GetInterestedWatchCount, workEstimatorCfg)
		handler = filterlatency.TrackCompleted(handler)
		handler = genericfilters.WithPriorityAndFairness(handler, c.LongRunningFunc, c.FlowControl, requestWorkEstimator)
		handler = filterlatency.TrackStarted(handler, "priorityandfairness")
	} else {
		// TODO generic server限速的另外一种策略
		handler = genericfilters.WithMaxInFlightLimit(handler, c.MaxRequestsInFlight, c.MaxMutatingRequestsInFlight, c.LongRunningFunc)
	}

	// TODO 这玩意干嘛的？
	handler = filterlatency.TrackCompleted(handler)
	handler = genericapifilters.WithImpersonation(handler, c.Authorization.Authorizer, c.Serializer)
	handler = filterlatency.TrackStarted(handler, "impersonation")

	// TODO 审计
	handler = filterlatency.TrackCompleted(handler)
	handler = genericapifilters.WithAudit(handler, c.AuditBackend, c.AuditPolicyRuleEvaluator, c.LongRunningFunc)
	handler = filterlatency.TrackStarted(handler, "audit")

	failedHandler := genericapifilters.Unauthorized(c.Serializer)
	failedHandler = genericapifilters.WithFailedAuthenticationAudit(failedHandler, c.AuditBackend, c.AuditPolicyRuleEvaluator)

	failedHandler = filterlatency.TrackCompleted(failedHandler)
	handler = filterlatency.TrackCompleted(handler)
	// TODO 认证
	handler = genericapifilters.WithAuthentication(handler, c.Authentication.Authenticator, failedHandler, c.Authentication.APIAudiences)
	handler = filterlatency.TrackStarted(handler, "authentication")

	// 设置Access-Control-Allow-Origin, Access-Control-Allow-Methods, Access-Control-Allow-Headers,
	// Access-Control-Expose-Headers, Access-Control-Allow-Credentials响应头支持CORS
	handler = genericfilters.WithCORS(handler, c.CorsAllowedOriginList, nil, nil, nil, "true")

	// WithTimeoutForNonLongRunningRequests will call the rest of the request handling in a go-routine with the
	// context with deadline. The go-routine can keep running, while the timeout logic will return a timeout to the client.
	// TODO 应该也是超时相关的东西，这个和WithRequestDeadline有啥区别？
	handler = genericfilters.WithTimeoutForNonLongRunningRequests(handler, c.LongRunningFunc)
	// 根据RequestTimeout设置Context的Deadline
	handler = genericapifilters.WithRequestDeadline(handler, c.AuditBackend, c.AuditPolicyRuleEvaluator,
		c.LongRunningFunc, c.Serializer, c.RequestTimeout)
	// TODO 暂时没看懂这玩意干嘛的
	handler = genericfilters.WithWaitGroup(handler, c.LongRunningFunc, c.HandlerChainWaitGroup)
	if c.SecureServing != nil && !c.SecureServing.DisableHTTP2 && c.GoawayChance > 0 {
		// todo probabilistic goaway是啥 参考：https://www.kubernetes.org.cn/6898.html
		handler = genericfilters.WithProbabilisticGoaway(handler, c.GoawayChance)
	}
	// 上下文中设置审计Annotations
	handler = genericapifilters.WithAuditAnnotations(handler, c.AuditBackend, c.AuditPolicyRuleEvaluator)
	// 上下文中设置recorder，用于记录warning信息
	handler = genericapifilters.WithWarningRecorder(handler)
	// 响应头设置Cache-Control
	handler = genericapifilters.WithCacheControl(handler)
	// 响应头设置Strict-Transport-Security
	handler = genericfilters.WithHSTS(handler, c.HSTSDirectives)
	if c.ShutdownSendRetryAfter {
		// 请求重试
		handler = genericfilters.WithRetryAfter(handler, c.lifecycleSignals.NotAcceptingNewRequest.Signaled())
	}
	// HTTP日志
	handler = genericfilters.WithHTTPLogging(handler)
	if utilfeature.DefaultFeatureGate.Enabled(genericfeatures.APIServerTracing) {
		// 对于分布式链路追踪OpenTelemetry的支持
		handler = genericapifilters.WithTracing(handler, c.TracerProvider)
	}
	// 用于响应延迟计算
	handler = genericapifilters.WithLatencyTrackers(handler)
	// 使用RequestInfoResolver解析请求，把请求信息解析为RequestInfo放入到上下文当中，后续处理可以直接使用这个信息，无需再次解析
	handler = genericapifilters.WithRequestInfo(handler, c.RequestInfoResolver)
	// 在上下文中添加请求进来的时间，这个时间将来被用来计算整个请求的处理时间
	handler = genericapifilters.WithRequestReceivedTimestamp(handler)
	// 通过MuxAndDiscoveryComplete chan判断是否Server已经完成了所有API的安装，如果已经完成安装，
	// 那么在上下文当中添加：muxAndDiscoveryIncompleteKey = MuxAndDiscoveryInstallationNotComplete
	handler = genericapifilters.WithMuxAndDiscoveryComplete(handler, c.lifecycleSignals.MuxAndDiscoveryComplete.Signaled())
	handler = genericfilters.WithPanicRecovery(handler, c.RequestInfoResolver)
	// 给请求头和响应头添加审计ID（Audit-ID）
	handler = genericapifilters.WithAuditID(handler)
	return handler
}

func installAPI(s *GenericAPIServer, c *Config) {
	if c.EnableIndex {
		// 安装 /, /index.html路由
		routes.Index{}.Install(s.listedPathProvider, s.Handler.NonGoRestfulMux)
	}
	if c.EnableProfiling {
		routes.Profiling{}.Install(s.Handler.NonGoRestfulMux)
		if c.EnableContentionProfiling {
			goruntime.SetBlockProfileRate(1)
		}
		// so far, only logging related endpoints are considered valid to add for these debug flags.
		routes.DebugFlags{}.Install(s.Handler.NonGoRestfulMux, "v", routes.StringFlagPutHandler(logs.GlogSetter))
	}
	if c.EnableMetrics {
		if c.EnableProfiling {
			routes.MetricsWithReset{}.Install(s.Handler.NonGoRestfulMux)
		} else {
			routes.DefaultMetrics{}.Install(s.Handler.NonGoRestfulMux)
		}
	}

	// 安装/version api
	routes.Version{Version: c.Version}.Install(s.Handler.GoRestfulContainer)

	if c.EnableDiscovery {
		// 安装/apis路由
		s.Handler.GoRestfulContainer.Add(s.DiscoveryGroupManager.WebService())
	}
	if c.FlowControl != nil && utilfeature.DefaultFeatureGate.Enabled(genericfeatures.APIPriorityAndFairness) {
		// 安装 /debug/api_priority_and_fairness/dump_priority_levels API
		c.FlowControl.Install(s.Handler.NonGoRestfulMux)
	}
}

func NewRequestInfoResolver(c *Config) *apirequest.RequestInfoFactory {
	apiPrefixes := sets.NewString(strings.Trim(APIGroupPrefix, "/")) // all possible API prefixes
	legacyAPIPrefixes := sets.String{}                               // APIPrefixes that won't have groups (legacy)
	for legacyAPIPrefix := range c.LegacyAPIGroupPrefixes {
		apiPrefixes.Insert(strings.Trim(legacyAPIPrefix, "/"))
		legacyAPIPrefixes.Insert(strings.Trim(legacyAPIPrefix, "/"))
	}

	return &apirequest.RequestInfoFactory{
		APIPrefixes:          apiPrefixes,
		GrouplessAPIPrefixes: legacyAPIPrefixes,
	}
}

func (s *SecureServingInfo) HostPort() (string, int, error) {
	if s == nil || s.Listener == nil {
		return "", 0, fmt.Errorf("no listener found")
	}
	addr := s.Listener.Addr().String()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, fmt.Errorf("failed to get port from listener address %q: %v", addr, err)
	}
	port, err := utilsnet.ParsePort(portStr, true)
	if err != nil {
		return "", 0, fmt.Errorf("invalid non-numeric port %q", portStr)
	}
	return host, port, nil
}

// AuthorizeClientBearerToken wraps the authenticator and authorizer in loopback authentication logic
// if the loopback client config is specified AND it has a bearer token. Note that if either authn or
// authz is nil, this function won't add a token authenticator or authorizer.
func AuthorizeClientBearerToken(loopback *restclient.Config, authn *AuthenticationInfo, authz *AuthorizationInfo) {
	if loopback == nil || len(loopback.BearerToken) == 0 {
		return
	}
	if authn == nil || authz == nil {
		// prevent nil pointer panic
		return
	}
	if authn.Authenticator == nil || authz.Authorizer == nil {
		// authenticator or authorizer might be nil if we want to bypass authz/authn
		// and we also do nothing in this case.
		return
	}

	privilegedLoopbackToken := loopback.BearerToken
	var uid = uuid.New().String()
	tokens := make(map[string]*user.DefaultInfo)
	tokens[privilegedLoopbackToken] = &user.DefaultInfo{
		Name:   user.APIServerUser,
		UID:    uid,
		Groups: []string{user.SystemPrivilegedGroup},
	}

	tokenAuthenticator := authenticatorfactory.NewFromTokens(tokens, authn.APIAudiences)
	authn.Authenticator = authenticatorunion.New(tokenAuthenticator, authn.Authenticator)

	tokenAuthorizer := authorizerfactory.NewPrivilegedGroups(user.SystemPrivilegedGroup)
	authz.Authorizer = authorizerunion.New(tokenAuthorizer, authz.Authorizer)
}
