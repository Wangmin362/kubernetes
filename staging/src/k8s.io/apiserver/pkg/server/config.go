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
	"context"
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"net"
	"net/http"
	"os"
	goruntime "runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/google/uuid"
	"golang.org/x/crypto/cryptobyte"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
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
	"k8s.io/apiserver/pkg/endpoints/discovery"
	discoveryendpoint "k8s.io/apiserver/pkg/endpoints/discovery/aggregated"
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
	"k8s.io/component-base/metrics/features"
	"k8s.io/component-base/metrics/prometheus/slis"
	"k8s.io/component-base/tracing"
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
	// 核心资源使用的前缀，核心资源也被称之为Legacy资源
	DefaultLegacyAPIPrefix = "/api"

	// APIGroupPrefix is where non-legacy API group will be located.
	// 除了核心资源其余资源都是以/apis打头
	APIGroupPrefix = "/apis"
)

// Config is a structure used to configure a GenericAPIServer.
// Its members are sorted roughly in order of importance for composers.
// 1、GenericAPIServer的配置  TODO 思考K8S开发者认为GenericAPIServer应该有哪些能力？对于普通开发者我们可以通过GenericAPIServer干什么？
// 2、此配置当中包含HTTPS服务设置（监听地址、端口、证书）、认证器、鉴权器、回环网卡客户端配置、准入控制、流控（也就是限速）、审计、链路追踪配置
type Config struct {
	// SecureServing is required to serve https
	// 提供HTTPS服务配置，主要有监听地址，监听端口、证书
	SecureServing *SecureServingInfo

	// Authentication is the configuration for authentication
	// 认证相关配置
	Authentication AuthenticationInfo

	// Authorization is the configuration for authorization
	// 鉴权相关配置
	Authorization AuthorizationInfo

	// LoopbackClientConfig is a config for a privileged loopback connection to the API server
	// This is required for proper functioning of the PostStartHooks on a GenericAPIServer
	// TODO: move into SecureServing(WithLoopback) as soon as insecure serving is gone
	// 1、可以理解为是KubeConfig文件，其目的就是告诉客户端如何连接服务器，以及以什么样的身份连接服务器
	// 2、这里的Loopback实际上指的是机器的回环网卡，也就是说这里的配置主要用于向本地的回环网卡发送数据
	// TODO 3、K8S用这个客户端做了什么事情？
	LoopbackClientConfig *restclient.Config

	// EgressSelector provides a lookup mechanism for dialing outbound connections.
	// It does so based on a EgressSelectorConfiguration which was read at startup.
	EgressSelector *egressselector.EgressSelector

	// RuleResolver is required to get the list of rules that apply to a given user
	// in a given namespace
	RuleResolver authorizer.RuleResolver
	// AdmissionControl performs deep inspection of a given request (including content)
	// to set values and determine whether its allowed
	AdmissionControl      admission.Interface
	CorsAllowedOriginList []string
	HSTSDirectives        []string
	// FlowControl, if not nil, gives priority and fairness to request handling
	FlowControl utilflowcontrol.Interface

	// 1、这里的Index，并所值开启索引，而是是否开启网站的/index.html路由以及/路由
	// 2、开启之后，GenericServer在启动的时候会注册/路由以及/index.html路由，我们可以通过kubectl get --raw=/ 或者是通过
	// kubectl get --raw=/index.html来获取Server支持的路由
	// 3、默认都是启用Index页面的
	EnableIndex bool
	// 1、开启profile之后，会想GenericServer当中注册/debug/pprof, /debug/pprof/, /debug/pprof/profile， /debug/pprof/symbol,
	// /debug/pprof/trace路由
	// 2、我们可以通过kubectl get --raw=/debug/pprof来测试接口
	EnableProfiling bool
	DebugSocketPath string // TODO 分析DebugSocket的作用
	EnableDiscovery bool   // TODO 这玩意干嘛的？

	// Requires generic profiling enabled
	EnableContentionProfiling bool
	EnableMetrics             bool

	// TODO PostStartHook在什么时候被调用？一般如何使用？最佳实践是什么？
	DisabledPostStartHooks sets.String
	// done values in this values for this map are ignored.
	// TODO GenericServer配置在初始化的时候一般会添加哪些PostStartHook，APIServer, AggregatorServer, ExtensionServer都添加了哪些PostStartHook?
	PostStartHooks map[string]PostStartHookConfigEntry

	// Version will enable the /version endpoint if non-nil
	Version *version.Info
	// AuditBackend is where audit events are sent to.
	AuditBackend audit.Backend
	// AuditPolicyRuleEvaluator makes the decision of whether and how to audit log a request.
	AuditPolicyRuleEvaluator audit.PolicyRuleEvaluator
	// ExternalAddress is the host name to use for external (public internet) facing URLs (e.g. Swagger)
	// Will default to a value based on secure serving info and available ipv4 IPs.
	ExternalAddress string

	// TracerProvider can provide a tracer, which records spans for distributed tracing.
	TracerProvider tracing.TracerProvider

	//===========================================================================
	// Fields you probably don't care about changing
	//===========================================================================

	// BuildHandlerChainFunc allows you to build custom handler chains by decorating the apiHandler.
	// 用于构建请求处理链，默认的请求处理链所在位置为：staging/src/k8s.io/apiserver/pkg/server/config.go
	BuildHandlerChainFunc func(apiHandler http.Handler, c *Config) (secure http.Handler)
	// NonLongRunningRequestWaitGroup allows you to wait for all chain
	// handlers associated with non long-running requests
	// to complete while the server is shuting down.
	NonLongRunningRequestWaitGroup *utilwaitgroup.SafeWaitGroup
	// WatchRequestWaitGroup allows us to wait for all chain
	// handlers associated with active watch requests to
	// complete while the server is shuting down.
	WatchRequestWaitGroup *utilwaitgroup.RateLimitedSafeWaitGroup
	// DiscoveryAddresses is used to build the IPs pass to discovery. If nil, the ExternalAddress is
	// always reported
	DiscoveryAddresses discovery.Addresses
	// The default set of healthz checks. There might be more added via AddHealthChecks dynamically.
	HealthzChecks []healthz.HealthChecker
	// The default set of livez checks. There might be more added via AddHealthChecks dynamically.
	LivezChecks []healthz.HealthChecker
	// The default set of readyz-only checks. There might be more added via AddReadyzChecks dynamically.
	ReadyzChecks []healthz.HealthChecker
	// LegacyAPIGroupPrefixes is used to set up URL parsing for authorization and for validating requests
	// to InstallLegacyAPIGroup. New API servers don't generally have legacy groups at all.
	LegacyAPIGroupPrefixes sets.String
	// RequestInfoResolver is used to assign attributes (used by admission and authorization) based on a request URL.
	// Use-cases that are like kubelets may need to customize this.
	RequestInfoResolver apirequest.RequestInfoResolver
	// Serializer is required and provides the interface for serializing and converting objects to and from the wire
	// The default (api.Codecs) usually works fine.
	// 1、所谓的协商序列化器，实际上就是调用方先执行SupportedMediaTypes函数，获取当前序列化器支持的媒体类型，然后根据自己的需要媒体类型调用
	// EncoderForVersion完成编码，调用DecoderToVersion解码
	// 2、简单来说，这玩意支持对于任何资源的JSON, YAML, Protobuf解码
	Serializer runtime.NegotiatedSerializer
	// OpenAPIConfig will be used in generating OpenAPI spec. This is nil by default. Use DefaultOpenAPIConfig for "working" defaults.
	OpenAPIConfig *openapicommon.Config
	// OpenAPIV3Config will be used in generating OpenAPI V3 spec. This is nil by default. Use DefaultOpenAPIV3Config for "working" defaults.
	OpenAPIV3Config *openapicommon.Config
	// SkipOpenAPIInstallation avoids installing the OpenAPI handler if set to true.
	SkipOpenAPIInstallation bool

	// RESTOptionsGetter is used to construct RESTStorage types via the generic registry.
	// 非常重要的属性，可以用来获取每个资源的存储配置，譬如编解码器，存储后端
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
	MaxRequestsInFlight int
	// MaxMutatingRequestsInFlight is the maximum number of parallel mutating requests. Every further
	// request has to wait.
	MaxMutatingRequestsInFlight int
	// Predicate which is true for paths of long-running http requests
	// 1、用于检测当前的请求是否是一个长时间的请求
	// 2、在K8S当中对于WATCH, PROXY动作都是长时间的请求
	// 3、在K8S当中对于Attach, Exec, Proxy, Log, PortForward命令都是长时间请求
	// TODO K8S对于这些长时间的请求都干了啥？为什么要单独处理？
	LongRunningFunc apirequest.LongRunningRequestCheck

	// GoawayChance is the probability that send a GOAWAY to HTTP/2 clients. When client received
	// GOAWAY, the in-flight requests will not be affected and new requests will use
	// a new TCP connection to triggering re-balancing to another server behind the load balance.
	// Default to 0, means never send GOAWAY. Max is 0.02 to prevent break the apiserver.
	GoawayChance float64

	// MergedResourceConfig indicates which groupVersion enabled and its resources enabled/disabled.
	// This is composed of genericapiserver defaultAPIResourceConfig and those parsed from flags.
	// If not specify any in flags, then genericapiserver will only enable defaultAPIResourceConfig.
	// 1、用于表示GV的启用/禁用，或者是GVR的启用/禁用
	// 2、通过MergedResourceConfig，我们可以知道某个资源是否被启用，还是被禁用
	MergedResourceConfig *serverstore.ResourceConfig

	// lifecycleSignals provides access to the various signals
	// that happen during lifecycle of the apiserver.
	// it's intentionally marked private as it should never be overridden.
	lifecycleSignals lifecycleSignals

	// StorageObjectCountTracker is used to keep track of the total number of objects
	// in the storage per resource, so we can estimate width of incoming requests.
	// TODO 似乎和FlowControl有关
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
	// TODO 这玩意是啥？
	EquivalentResourceRegistry runtime.EquivalentResourceRegistry

	// APIServerID is the ID of this API server
	APIServerID string

	// StorageVersionManager holds the storage versions of the API resources installed by this server.
	StorageVersionManager storageversion.Manager

	// AggregatedDiscoveryGroupManager serves /apis in an aggregated form.
	AggregatedDiscoveryGroupManager discoveryendpoint.ResourceManager

	// ShutdownWatchTerminationGracePeriod, if set to a positive value,
	// is the maximum duration the apiserver will wait for all active
	// watch request(s) to drain.
	// Once this grace period elapses, the apiserver will no longer
	// wait for any active watch request(s) in flight to drain, it will
	// proceed to the next step in the graceful server shutdown process.
	// If set to a positive value, the apiserver will keep track of the
	// number of active watch request(s) in flight and during shutdown
	// it will wait, at most, for the specified duration and allow these
	// active watch requests to drain with some rate limiting in effect.
	// The default is zero, which implies the apiserver will not keep
	// track of active watch request(s) in flight and will not wait
	// for them to drain, this maintains backward compatibility.
	// This grace period is orthogonal to other grace periods, and
	// it is not overridden by any other grace period.
	// TODO
	ShutdownWatchTerminationGracePeriod time.Duration
}

// RecommendedConfig TODO 如何里理解这个 “推荐的GenericConfig”?，仅仅是因为多了SharedInformerFactory缓存？
type RecommendedConfig struct {
	Config

	// SharedInformerFactory provides shared informers for Kubernetes resources. This value is set by
	// RecommendedOptions.CoreAPI.ApplyTo called by RecommendedOptions.ApplyTo. It uses an in-cluster client config
	// by default, or the kubeconfig given with kubeconfig command line flag.
	SharedInformerFactory informers.SharedInformerFactory

	// ClientConfig holds the kubernetes client configuration.
	// This value is set by RecommendedOptions.CoreAPI.ApplyTo called by RecommendedOptions.ApplyTo.
	// By default in-cluster client config is used.
	ClientConfig *restclient.Config
}

type SecureServingInfo struct {
	// Listener is the secure server network listener.
	Listener net.Listener

	// Cert is the main server cert which is used if SNI does not match. Cert must be non-nil and is
	// allowed to be in SNICerts.
	Cert dynamiccertificates.CertKeyContentProvider

	// SNICerts are the TLS certificates used for SNI.
	SNICerts []dynamiccertificates.SNICertKeyContentProvider

	// ClientCA is the certificate bundle for all the signers that you'll recognize for incoming client certificates
	ClientCA dynamiccertificates.CAContentProvider

	// MinTLSVersion optionally overrides the minimum TLS version supported.
	// Values are from tls package constants (https://golang.org/pkg/crypto/tls/#pkg-constants).
	MinTLSVersion uint16

	// CipherSuites optionally overrides the list of allowed cipher suites for the server.
	// Values are from tls package constants (https://golang.org/pkg/crypto/tls/#pkg-constants).
	CipherSuites []uint16

	// HTTP2MaxStreamsPerConnection is the limit that the api server imposes on each client.
	// A value of zero means to use the default provided by golang's HTTP/2 support.
	HTTP2MaxStreamsPerConnection int

	// DisableHTTP2 indicates that http2 should not be enabled.
	DisableHTTP2 bool
}

type AuthenticationInfo struct {
	// APIAudiences is a list of identifier that the API identifies as. This is
	// used by some authenticators to validate audience bound credentials.
	// TODO 这个参数是给OIDC这类的认证器使用的
	APIAudiences authenticator.Audiences
	// Authenticator determines which subject is making the request
	// 1、认证器，请求进来时就由这里的认证器完成认证
	// 2、实际上，这里的认证器是一个UnionAuthenticator，K8S对于用户启用的每个认证模式，都会实例化对应的认证器用于认证特定的模式。
	// 请求到来时，我们需要把请求依次给每个认证器认证一次，但凡有一个认证器认证通过，那么就认为当前请求认证通过。
	// 3、认证的核心目标就是判断当前发出请求的用户是否是系统认可的用户，如果认可，认证器需要在请求上下文中添加User, Group信息，方便
	// 后续鉴权器进行鉴权
	Authenticator authenticator.Request

	// 代理认证
	RequestHeaderConfig *authenticatorfactory.RequestHeaderConfig
}

type AuthorizationInfo struct {
	// Authorizer determines whether the subject is allowed to make the request based only
	// on the RequestURI
	Authorizer authorizer.Authorizer
}

func init() {
	utilruntime.Must(features.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))
}

// NewConfig returns a Config struct with the default values
// 1、此配置当中包含HTTPS服务设置（监听地址、端口、证书）、认证器、鉴权器、回环网卡客户端配置、准入控制、流控（也就是限速）、审计、链路追踪配置
// 2、NewConfig在这里主要是为了创建一个默认配置，仅仅会设置一些默认参数，用户配置的参数还没有赋值
func NewConfig(codecs serializer.CodecFactory) *Config {
	// 健康检测，用于检测GenericServer的某些功能是否正常
	defaultHealthChecks := []healthz.HealthChecker{healthz.PingHealthz, healthz.LogHealthz}
	var id string
	// 生成APIServer的ID
	if utilfeature.DefaultFeatureGate.Enabled(genericfeatures.APIServerIdentity) {
		hostname, err := os.Hostname()
		if err != nil {
			klog.Fatalf("error getting hostname for apiserver identity: %v", err)
		}

		// Since the hash needs to be unique across each kube-apiserver and aggregated apiservers,
		// the hash used for the identity should include both the hostname and the identity value.
		// TODO: receive the identity value as a parameter once the apiserver identity lease controller
		// post start hook is moved to generic apiserver.
		b := cryptobyte.NewBuilder(nil)
		b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) {
			b.AddBytes([]byte(hostname))
		})
		b.AddUint16LengthPrefixed(func(b *cryptobyte.Builder) {
			b.AddBytes([]byte("kube-apiserver"))
		})
		hashData, err := b.Bytes()
		if err != nil {
			klog.Fatalf("error building hash data for apiserver identity: %v", err)
		}

		hash := sha256.Sum256(hashData)
		id = "apiserver-" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(hash[:16]))
	}
	// 所谓的生命周期信号，其实就是当GenericServer处于某种状态时需要发出信号，让对该信号感兴趣的组件接收到。然后根据发生的信号，做某些事情。
	// 譬如对于shutdown信号，组件收到之后就应该释放资源，关闭服务。譬如路由注册完成信号，组件收到之后就知道所有的路由已经全部注册，此时可以
	// 正常提供HTTP服务
	lifecycleSignals := newLifecycleSignals()

	// 实例化GenericServer配置
	return &Config{
		Serializer:                     codecs,
		BuildHandlerChainFunc:          DefaultBuildHandlerChain,                  // 构建请求处理链
		NonLongRunningRequestWaitGroup: new(utilwaitgroup.SafeWaitGroup),          // TODO
		WatchRequestWaitGroup:          &utilwaitgroup.RateLimitedSafeWaitGroup{}, // TODO
		LegacyAPIGroupPrefixes:         sets.NewString(DefaultLegacyAPIPrefix),    // Legacy资源使用的请求前缀
		DisabledPostStartHooks:         sets.NewString(),                          // 禁用的PostStartHook
		PostStartHooks:                 map[string]PostStartHookConfigEntry{},
		HealthzChecks:                  append([]healthz.HealthChecker{}, defaultHealthChecks...), // 健康检测，当执行/healthyz时会执行这些健康检测回调
		ReadyzChecks:                   append([]healthz.HealthChecker{}, defaultHealthChecks...), // 就绪检测，当执行/readyz时会执行这些健康检测回调
		LivezChecks:                    append([]healthz.HealthChecker{}, defaultHealthChecks...), // 存活检测，当执行/livez时会执行这些回调
		// 1、开启APIServer的/index.html路由，开启之后我们可以直接访问https://172.30.3.130:6443/index.html或者是
		// https://172.30.3.130:6443/获取当前Server支持的所有路由，当然前提是kubernetes.svc开启了NodePort，否则只能用
		// kubectl get --raw=/  或者 kubectl get --raw=/index.html来测试
		EnableIndex:                 true,                            // 是否允许注册 /, 以及 /index.html路由
		EnableDiscovery:             true,                            // TODO
		EnableProfiling:             true,                            // 开启后会向GenericServer中注册注册/debug/pprof, /debug/pprof/, /debug/pprof/profile， /debug/pprof/symbol, /debug/pprof/trace路由
		DebugSocketPath:             "",                              // TODO
		EnableMetrics:               true,                            // TODO
		MaxRequestsInFlight:         400,                             // 默认APIServer每秒钟最多只能同时处理400个请求
		MaxMutatingRequestsInFlight: 200,                             // 默认APIServer每秒钟最多只能同时处理200个写请求，这里的Mutating指的是修改、创建、删除动作
		RequestTimeout:              time.Duration(60) * time.Second, // 默认请求超时时间为60秒
		MinRequestTimeout:           1800,                            // TODO
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
		// If this constant is changed, DefaultMaxRequestSizeBytes in k8s.io/apiserver/pkg/cel/limits.go
		// should be changed to reflect the new value, if the two haven't
		// been wired together already somehow.
		MaxRequestBodyBytes: int64(3 * 1024 * 1024),

		// Default to treating watch as a long-running operation
		// Generic API servers have no inherent long-running subresources
		LongRunningFunc:                     genericfilters.BasicLongRunningRequestCheck(sets.NewString("watch"), sets.NewString()),
		lifecycleSignals:                    lifecycleSignals,
		StorageObjectCountTracker:           flowcontrolrequest.NewStorageObjectCountTracker(),
		ShutdownWatchTerminationGracePeriod: time.Duration(0),

		APIServerID: id,
		// TODO StorageVersionManager干嘛用的？用于解决什么问题？
		StorageVersionManager: storageversion.NewDefaultManager(),
		// TODO 链路跟踪
		TracerProvider: tracing.NewNoopTracerProvider(),
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
// 如果注册不了后置处理器，就直接认为Fatal, 那么Kube-APIServer启动将会失败
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
// 1、此信号标识APIServer已经处理完成所有的请求了
// 2、为什么需要这个信号呢？ 因为这个信号是用在APIServer关机的时候，我们需要等待APIServer正常响应当前所有已经进来的请求，当要求关机之后，
// APIServer将不再接受新的请求，同时需要把当前已经进来的请求处理完成。当所有的请求处理完成之后，InFlightRequestsDrained信号将会被
// 发出，表示所有的请求已经处理完毕。关心这个信号的处理器可以进行后续APIServer关机的后续操作
func (c *Config) DrainedNotify() <-chan struct{} {
	return c.lifecycleSignals.InFlightRequestsDrained.Signaled()
}

// Complete fills in any fields not set that are required to have valid data and can be derived
// from other fields. If you're going to `ApplyOptions`, do that first. It's mutating the receiver.
// 1、用于根据目前的配置，填充一些配置，或者给某些配置设置默认值
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
// 1、实例化APIServerHandler，APIServerHandler可以理解为请求的处理函数
// 2、根据GenericConfig配置实例化GenericServer TODO 具体分析GenericServer
// 3、把Delegator的PostStartHook以及PreShutdownHook拷贝到刚才实例化的GenericServer当汇总，因为最终只有ServerChain的最后一个Server
// 会被启动，其余的Server不会被启动，因此需要在最后一个Server当中把之前所有Server的PostStartHook以及PreShutdownHook添加进来，从而
// 间接启动ServerChain中除最后一个Server之前的所有Server。
// 4、添加名为generic-apiserver-start-informers的PostStartHook TODO 分析作用
// 5、添加名为priority-and-fairness-config-consumer的PostStartHook TODO 分析作用
// 6、如果启用了FlowControl，那么添加名为priority-and-fairness-filter的PostStartHook，如果没有启用FlowControl，那么添加名为
// max-in-flight-filter的PostStartHook TODO 分析作用
// 7、如果启用了StorageObjectCountTracker特性，那么添加名为storage-object-count-tracker-hook的PostStartHook，TODO 分析作用
// 8、拷贝Delegator的HealthzCheck添加到前面实例化的GenericServer当中
// 9、注册销毁函数
// 10、安装API，其中包括：
// 10.1、NonGoRestfulMux添加/, /index.html, /debug/pprof, /debug/pprof/, /debug/pprof/profile， /debug/pprof/symbol,
// /debug/pprof/trace, /metrics, /metrics/slis, /debug/api_priority_and_fairness/dump_priority_levels,
// /debug/api_priority_and_fairness/dump_queues, /debug/api_priority_and_fairness/dump_requests路由
// 10.2、GoRestfulContainer添加/versions, /apis, /apis/<group>, /apis/<group>, /apis/<group>/<version>,
// /apis/<group>/<version>/<resource>路由, 当然，这并不包含核心资源路由
func (c completedConfig) New(name string, delegationTarget DelegationTarget) (*GenericAPIServer, error) {
	if c.Serializer == nil {
		return nil, fmt.Errorf("Genericapiserver.New() called with config.Serializer == nil")
	}
	if c.LoopbackClientConfig == nil {
		return nil, fmt.Errorf("Genericapiserver.New() called with config.LoopbackClientConfig == nil")
	}
	if c.EquivalentResourceRegistry == nil {
		return nil, fmt.Errorf("Genericapiserver.New() called with config.EquivalentResourceRegistry == nil")
	}

	// 用于构建请求处理链，实际上这里就是把请求的真正处理放在了请求处理链的最后一个位置
	handlerChainBuilder := func(handler http.Handler) http.Handler {
		return c.BuildHandlerChainFunc(handler, c.Config)
	}

	var debugSocket *routes.DebugSocket
	if c.DebugSocketPath != "" {
		debugSocket = routes.NewDebugSocket(c.DebugSocketPath)
	}

	// TODO 分析APIServerHandler架构
	apiServerHandler := NewAPIServerHandler(
		name,                                  // APIServerHandler名字有何作用？ 打日志？
		c.Serializer,                          // 序列化器
		handlerChainBuilder,                   // 请求处理链
		delegationTarget.UnprotectedHandler(), // 如果自己处理不了，就需要把请求委派给Delegator
	)

	s := &GenericAPIServer{
		discoveryAddresses:             c.DiscoveryAddresses,
		LoopbackClientConfig:           c.LoopbackClientConfig,
		legacyAPIGroupPrefixes:         c.LegacyAPIGroupPrefixes,
		admissionControl:               c.AdmissionControl,
		Serializer:                     c.Serializer,
		AuditBackend:                   c.AuditBackend,
		Authorizer:                     c.Authorization.Authorizer,
		delegationTarget:               delegationTarget,
		EquivalentResourceRegistry:     c.EquivalentResourceRegistry,
		NonLongRunningRequestWaitGroup: c.NonLongRunningRequestWaitGroup,
		WatchRequestWaitGroup:          c.WatchRequestWaitGroup,
		Handler:                        apiServerHandler,
		UnprotectedDebugSocket:         debugSocket,

		listedPathProvider: apiServerHandler,

		minRequestTimeout:                   time.Duration(c.MinRequestTimeout) * time.Second,
		ShutdownTimeout:                     c.RequestTimeout,
		ShutdownDelayDuration:               c.ShutdownDelayDuration,
		ShutdownWatchTerminationGracePeriod: c.ShutdownWatchTerminationGracePeriod,
		SecureServingInfo:                   c.SecureServing,
		ExternalAddress:                     c.ExternalAddress,

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

	// TODO 1.26加入的特性，不过马上又要在1.32被移除
	if utilfeature.DefaultFeatureGate.Enabled(genericfeatures.AggregatedDiscoveryEndpoint) {
		manager := c.AggregatedDiscoveryGroupManager
		if manager == nil {
			manager = discoveryendpoint.NewResourceManager("apis")
		}
		s.AggregatedDiscoveryGroupManager = manager
		s.AggregatedLegacyDiscoveryGroupManager = discoveryendpoint.NewResourceManager("api")
	}
	// TODO 这里是在干嘛？
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

	// first add poststarthooks from delegated targets
	// 这里之所以需要把Delegator的PostStartHook拷贝进来，是因为将来启动只会启动AggregatorServer, APIServer以及ExtensionServer并不会启动
	for k, v := range delegationTarget.PostStartHooks() {
		s.postStartHooks[k] = v
	}

	for k, v := range delegationTarget.PreShutdownHooks() {
		s.preShutdownHooks[k] = v
	}

	// add poststarthooks that were preconfigured.  Using the add method will give us an error if the same name has already been registered.
	for name, preconfiguredPostStartHook := range c.PostStartHooks {
		// 向GenericServer当中添加PostStartHook，被禁用的PostStartHook不允许添加，同名的PostStartHook也不允许添加
		if err := s.AddPostStartHook(name, preconfiguredPostStartHook.hook); err != nil {
			return nil, err
		}
	}

	// register mux signals from the delegated server
	// 这里还需要把路由发现的完成信号拷贝过来，因为最终只会启动AggregatorServer, APIServer以及ExtensionServer并不会启动
	for k, v := range delegationTarget.MuxAndDiscoveryCompleteSignals() {
		if err := s.RegisterMuxAndDiscoveryCompleteSignal(k, v); err != nil {
			return nil, err
		}
	}

	genericApiServerHookName := "generic-apiserver-start-informers"
	if c.SharedInformerFactory != nil {
		// 判断是否注册了名为generic-apiserver-start-informers的PostStartHook
		if !s.isPostStartHookRegistered(genericApiServerHookName) {
			err := s.AddPostStartHook(genericApiServerHookName, func(context PostStartHookContext) error {
				// 启动SharedInformerFactory，缓存K8S各种资源到内存当中
				c.SharedInformerFactory.Start(context.StopCh)
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		// TODO: Once we get rid of /healthz consider changing this to post-start-hook.
		// 用于判断SharedInformerFactory是否准备好，只有当所有的Informer同步完成，才认为SharedInformerFactory同步完成
		err := s.AddReadyzChecks(healthz.NewInformerSyncHealthz(c.SharedInformerFactory))
		if err != nil {
			return nil, err
		}
	}

	const priorityAndFairnessConfigConsumerHookName = "priority-and-fairness-config-consumer"
	if s.isPostStartHookRegistered(priorityAndFairnessConfigConsumerHookName) {
		// 如果名为priority-and-fairness-config-consumer的PostStartHook已经注册，就啥也不干
	} else if c.FlowControl != nil {
		// 如果名为priority-and-fairness-config-consumer的PostStartHook没有注册，并且启用了流控，那么需要注册PostStartHook
		err := s.AddPostStartHook(priorityAndFairnessConfigConsumerHookName, func(context PostStartHookContext) error {
			// 启动流控
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
		// TODO 是不是在FlowControl启用的情况下，全局限流就不起作用了
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
	s.RegisterDestroyFunc(func() {
		if err := c.Config.TracerProvider.Shutdown(context.Background()); err != nil {
			klog.Errorf("failed to shut down tracer provider: %v", err)
		}
	})

	// 列出所有支持的路由，包括Delegator的路由
	s.listedPathProvider = routes.ListedPathProviders{s.listedPathProvider, delegationTarget}

	// 1、NonGoRestfulMux添加/, /index.html, /debug/pprof, /debug/pprof/, /debug/pprof/profile， /debug/pprof/symbol,
	// /debug/pprof/trace, /metrics, /metrics/slis, /debug/api_priority_and_fairness/dump_priority_levels,
	// /debug/api_priority_and_fairness/dump_queues, /debug/api_priority_and_fairness/dump_requests路由
	// 2、GoRestfulContainer添加/versions, /apis, /apis/<group>, /apis/<group>, /apis/<group>/<version>,
	// /apis/<group>/<version>/<resource>路由, 当然，这并不包含核心资源路由
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
	handler := genericapifilters.WithStorageVersionPrecondition(apiHandler, c.StorageVersionManager, c.Serializer)
	return DefaultBuildHandlerChain(handler, c)
}

// DefaultBuildHandlerChain 1、这里的请求就和俄罗斯套娃一毛一样，最先写最后才会执行，最后写的最先执行；所以DefaultBuildHandlerChain
// 正确的看法应该是从下网上看
// 2、apiHandler参数实际上就是要处理的请求，DefaultBuildHandlerChain把这个请求放在了请求处理链的最后，只有前面的Handler处理完成了才会
// 真正调用apiHandler处理请求
func DefaultBuildHandlerChain(apiHandler http.Handler, c *Config) http.Handler {
	// APIServer的鉴权
	handler := filterlatency.TrackCompleted(apiHandler)
	handler = genericapifilters.WithAuthorization(handler, c.Authorization.Authorizer, c.Serializer)
	handler = filterlatency.TrackStarted(handler, c.TracerProvider, "authorization")

	if c.FlowControl != nil {
		workEstimatorCfg := flowcontrolrequest.DefaultWorkEstimatorConfig()
		requestWorkEstimator := flowcontrolrequest.NewWorkEstimator(
			c.StorageObjectCountTracker.Get, c.FlowControl.GetInterestedWatchCount, workEstimatorCfg)
		handler = filterlatency.TrackCompleted(handler)
		handler = genericfilters.WithPriorityAndFairness(handler, c.LongRunningFunc, c.FlowControl, requestWorkEstimator)
		handler = filterlatency.TrackStarted(handler, c.TracerProvider, "priorityandfairness")
	} else {
		handler = genericfilters.WithMaxInFlightLimit(handler, c.MaxRequestsInFlight, c.MaxMutatingRequestsInFlight, c.LongRunningFunc)
	}

	handler = filterlatency.TrackCompleted(handler)
	handler = genericapifilters.WithImpersonation(handler, c.Authorization.Authorizer, c.Serializer)
	handler = filterlatency.TrackStarted(handler, c.TracerProvider, "impersonation")

	// 审计日志
	handler = filterlatency.TrackCompleted(handler)
	handler = genericapifilters.WithAudit(handler, c.AuditBackend, c.AuditPolicyRuleEvaluator, c.LongRunningFunc)
	handler = filterlatency.TrackStarted(handler, c.TracerProvider, "audit")

	// 这里应该是处理认证异常的请求
	failedHandler := genericapifilters.Unauthorized(c.Serializer)
	failedHandler = genericapifilters.WithFailedAuthenticationAudit(failedHandler, c.AuditBackend, c.AuditPolicyRuleEvaluator)

	failedHandler = filterlatency.TrackCompleted(failedHandler)
	// APIServer的认证
	handler = filterlatency.TrackCompleted(handler)
	handler = genericapifilters.WithAuthentication(handler, c.Authentication.Authenticator, failedHandler, c.Authentication.APIAudiences, c.Authentication.RequestHeaderConfig)
	handler = filterlatency.TrackStarted(handler, c.TracerProvider, "authentication")

	handler = genericfilters.WithCORS(handler, c.CorsAllowedOriginList, nil, nil, nil, "true")

	// WithTimeoutForNonLongRunningRequests will call the rest of the request handling in a go-routine with the
	// context with deadline. The go-routine can keep running, while the timeout logic will return a timeout to the client.
	handler = genericfilters.WithTimeoutForNonLongRunningRequests(handler, c.LongRunningFunc)

	handler = genericapifilters.WithRequestDeadline(handler, c.AuditBackend, c.AuditPolicyRuleEvaluator,
		c.LongRunningFunc, c.Serializer, c.RequestTimeout)
	handler = genericfilters.WithWaitGroup(handler, c.LongRunningFunc, c.NonLongRunningRequestWaitGroup)
	if c.ShutdownWatchTerminationGracePeriod > 0 {
		handler = genericfilters.WithWatchTerminationDuringShutdown(handler, c.lifecycleSignals, c.WatchRequestWaitGroup)
	}
	if c.SecureServing != nil && !c.SecureServing.DisableHTTP2 && c.GoawayChance > 0 {
		handler = genericfilters.WithProbabilisticGoaway(handler, c.GoawayChance)
	}
	handler = genericapifilters.WithWarningRecorder(handler)
	handler = genericapifilters.WithCacheControl(handler)
	handler = genericfilters.WithHSTS(handler, c.HSTSDirectives)
	if c.ShutdownSendRetryAfter {
		handler = genericfilters.WithRetryAfter(handler, c.lifecycleSignals.NotAcceptingNewRequest.Signaled())
	}
	handler = genericfilters.WithHTTPLogging(handler)
	if utilfeature.DefaultFeatureGate.Enabled(genericfeatures.APIServerTracing) {
		handler = genericapifilters.WithTracing(handler, c.TracerProvider)
	}
	handler = genericapifilters.WithLatencyTrackers(handler)
	handler = genericapifilters.WithRequestInfo(handler, c.RequestInfoResolver)
	handler = genericapifilters.WithRequestReceivedTimestamp(handler)
	handler = genericapifilters.WithMuxAndDiscoveryComplete(handler, c.lifecycleSignals.MuxAndDiscoveryComplete.Signaled())
	handler = genericfilters.WithPanicRecovery(handler, c.RequestInfoResolver)
	handler = genericapifilters.WithAuditInit(handler)
	return handler
}

// 1、NonGoRestfulMux添加/, /index.html, /debug/pprof, /debug/pprof/, /debug/pprof/profile， /debug/pprof/symbol,
// /debug/pprof/trace, /metrics, /metrics/slis, /debug/api_priority_and_fairness/dump_priority_levels,
// /debug/api_priority_and_fairness/dump_queues, /debug/api_priority_and_fairness/dump_requests路由
// 2、GoRestfulContainer添加/versions, /apis, /apis/<group>, /apis/<group>, /apis/<group>/<version>,
// /apis/<group>/<version>/<resource>路由, 当然，这并不包含核心资源路由
func installAPI(s *GenericAPIServer, c *Config) {
	// 如果启用了Index页面，添加/, /index.html路由
	if c.EnableIndex {
		routes.Index{}.Install(s.listedPathProvider, s.Handler.NonGoRestfulMux)
	}
	// 如果启用了Profiling，那么添加/debug/pprof, /debug/pprof/, /debug/pprof/profile， /debug/pprof/symbol, /debug/pprof/trace路由
	if c.EnableProfiling {
		routes.Profiling{}.Install(s.Handler.NonGoRestfulMux)
		if c.EnableContentionProfiling {
			goruntime.SetBlockProfileRate(1)
		}
		// so far, only logging related endpoints are considered valid to add for these debug flags.
		routes.DebugFlags{}.Install(s.Handler.NonGoRestfulMux, "v", routes.StringFlagPutHandler(logs.GlogSetter))
	}
	// 1、如果启用了DebugSocket特性，那么添加/debug/pprof, /debug/pprof/, /debug/pprof/profile， /debug/pprof/symbol,
	// /debug/pprof/trace路由
	// 2、如果启用了DebugSocket特性，添加/debug/flags, /debug/flags/路由
	if s.UnprotectedDebugSocket != nil {
		s.UnprotectedDebugSocket.InstallProfiling()
		s.UnprotectedDebugSocket.InstallDebugFlag("v", routes.StringFlagPutHandler(logs.GlogSetter))
		if c.EnableContentionProfiling {
			goruntime.SetBlockProfileRate(1)
		}
	}

	// 如果启用了Metric，添加/metrics, /metrics/slis路由
	if c.EnableMetrics {
		if c.EnableProfiling {
			routes.MetricsWithReset{}.Install(s.Handler.NonGoRestfulMux)
			if utilfeature.DefaultFeatureGate.Enabled(features.ComponentSLIs) {
				slis.SLIMetricsWithReset{}.Install(s.Handler.NonGoRestfulMux)
			}
		} else {
			routes.DefaultMetrics{}.Install(s.Handler.NonGoRestfulMux)
			if utilfeature.DefaultFeatureGate.Enabled(features.ComponentSLIs) {
				slis.SLIMetrics{}.Install(s.Handler.NonGoRestfulMux)
			}
		}
	}

	// 添加/version路由
	routes.Version{Version: c.Version}.Install(s.Handler.GoRestfulContainer)

	// 添加/apis, /apis/<group>, /apis/<group>, /apis/<group>/<version>, /apis/<group>/<version>/<resource>路由，并遍历安装所有除了核心资源外的其余资源的路由
	// root@k8s-master1:~# kubectl get --raw=/apis 可以获取到当前所有的
	// {"kind":"APIGroupList","apiVersion":"v1","groups":[{"name":"apiregistration.k8s.io","versions":[{"groupVersion":"apiregistration.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"apiregistration.k8s.io/v1","version":"v1"}},{"name":"apps","versions":[{"groupVersion":"apps/v1","version":"v1"}],"preferredVersion":{"groupVersion":"apps/v1","version":"v1"}},{"name":"events.k8s.io","versions":[{"groupVersion":"events.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"events.k8s.io/v1","version":"v1"}},{"name":"authentication.k8s.io","versions":[{"groupVersion":"authentication.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"authentication.k8s.io/v1","version":"v1"}},{"name":"authorization.k8s.io","versions":[{"groupVersion":"authorization.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"authorization.k8s.io/v1","version":"v1"}},{"name":"autoscaling","versions":[{"groupVersion":"autoscaling/v2","version":"v2"},{"groupVersion":"autoscaling/v1","version":"v1"}],"preferredVersion":{"groupVersion":"autoscaling/v2","version":"v2"}},{"name":"batch","versions":[{"groupVersion":"batch/v1","version":"v1"}],"preferredVersion":{"groupVersion":"batch/v1","version":"v1"}},{"name":"certificates.k8s.io","versions":[{"groupVersion":"certificates.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"certificates.k8s.io/v1","version":"v1"}},{"name":"networking.k8s.io","versions":[{"groupVersion":"networking.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"networking.k8s.io/v1","version":"v1"}},{"name":"policy","versions":[{"groupVersion":"policy/v1","version":"v1"}],"preferredVersion":{"groupVersion":"policy/v1","version":"v1"}},{"name":"rbac.authorization.k8s.io","versions":[{"groupVersion":"rbac.authorization.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"rbac.authorization.k8s.io/v1","version":"v1"}},{"name":"storage.k8s.io","versions":[{"groupVersion":"storage.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"storage.k8s.io/v1","version":"v1"}},{"name":"admissionregistration.k8s.io","versions":[{"groupVersion":"admissionregistration.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"admissionregistration.k8s.io/v1","version":"v1"}},{"name":"apiextensions.k8s.io","versions":[{"groupVersion":"apiextensions.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"apiextensions.k8s.io/v1","version":"v1"}},{"name":"scheduling.k8s.io","versions":[{"groupVersion":"scheduling.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"scheduling.k8s.io/v1","version":"v1"}},{"name":"coordination.k8s.io","versions":[{"groupVersion":"coordination.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"coordination.k8s.io/v1","version":"v1"}},{"name":"node.k8s.io","versions":[{"groupVersion":"node.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"node.k8s.io/v1","version":"v1"}},{"name":"discovery.k8s.io","versions":[{"groupVersion":"discovery.k8s.io/v1","version":"v1"}],"preferredVersion":{"groupVersion":"discovery.k8s.io/v1","version":"v1"}},{"name":"flowcontrol.apiserver.k8s.io","versions":[{"groupVersion":"flowcontrol.apiserver.k8s.io/v1beta3","version":"v1beta3"},{"groupVersion":"flowcontrol.apiserver.k8s.io/v1beta2","version":"v1beta2"}],"preferredVersion":{"groupVersion":"flowcontrol.apiserver.k8s.io/v1beta3","version":"v1beta3"}},{"name":"crd.projectcalico.org","versions":[{"groupVersion":"crd.projectcalico.org/v1","version":"v1"}],"preferredVersion":{"groupVersion":"crd.projectcalico.org/v1","version":"v1"}},{"name":"metrics.k8s.io","versions":[{"groupVersion":"metrics.k8s.io/v1beta1","version":"v1beta1"}],"preferredVersion":{"groupVersion":"metrics.k8s.io/v1beta1","version":"v1beta1"}}]}
	if c.EnableDiscovery {
		if utilfeature.DefaultFeatureGate.Enabled(genericfeatures.AggregatedDiscoveryEndpoint) {
			wrapped := discoveryendpoint.WrapAggregatedDiscoveryToHandler(s.DiscoveryGroupManager, s.AggregatedDiscoveryGroupManager)
			s.Handler.GoRestfulContainer.Add(wrapped.GenerateWebService("/apis", metav1.APIGroupList{}))
		} else {
			s.Handler.GoRestfulContainer.Add(s.DiscoveryGroupManager.WebService())
		}
	}

	// 添加/debug/api_priority_and_fairness/dump_priority_levels, /debug/api_priority_and_fairness/dump_queues,
	// /debug/api_priority_and_fairness/dump_requests路由
	if c.FlowControl != nil && utilfeature.DefaultFeatureGate.Enabled(genericfeatures.APIPriorityAndFairness) {
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
}
