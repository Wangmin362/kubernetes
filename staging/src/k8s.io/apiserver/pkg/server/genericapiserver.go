/*
Copyright 2014 The Kubernetes Authors.

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
	"fmt"
	"net/http"
	gpath "path"
	"strings"
	"sync"
	"time"

	systemd "github.com/coreos/go-systemd/v22/daemon"

	"golang.org/x/time/rate"
	apidiscoveryv2beta1 "k8s.io/api/apidiscovery/v2beta1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/managedfields"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	utilwaitgroup "k8s.io/apimachinery/pkg/util/waitgroup"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	genericapi "k8s.io/apiserver/pkg/endpoints"
	"k8s.io/apiserver/pkg/endpoints/discovery"
	discoveryendpoint "k8s.io/apiserver/pkg/endpoints/discovery/aggregated"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/apiserver/pkg/server/routes"
	"k8s.io/apiserver/pkg/storageversion"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	restclient "k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	openapibuilder3 "k8s.io/kube-openapi/pkg/builder3"
	openapicommon "k8s.io/kube-openapi/pkg/common"
	"k8s.io/kube-openapi/pkg/handler"
	"k8s.io/kube-openapi/pkg/handler3"
	openapiutil "k8s.io/kube-openapi/pkg/util"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/utils/clock"
)

// APIGroupInfo Info about an API group.
// TODO APIGroupInfo是如何抽象的？
type APIGroupInfo struct {
	// TODO 什么叫做优先选择的版本  优先选择的版本什么时候用？  为什么是个组数？
	// 1、每个group随着开发迭代，可能会有不同的版本，譬如v1alpha1, v1beta1, v1, v2beta1, v2beta2, v2等等
	// 2、为什么是数组？因为当前结构是抽象的一个组下的所有信息，而一个组先将会有许多不同的资源类型，譬如apps组下有Deployment, StatefulSet,
	// Job, CronJob, DaemonSet等等，那么自然每一个资源都需要一个优先先择的版本，所以是一个数组。
	// TODO 3、为什么是不设计为Map？ 设计为数组的话，在使用的时候就必须遍历到我们需要的资源，才能知道应该优先选择哪个版本。
	// 4、从代码以及当前结构可以看出，APIGroupInfo用于存储当前组下的GroupVersion，就是通过PrioritizedVersions字段，只不过先后顺序
	// 体现了优先级
	// TODO 这里的优先级有啥作用？
	PrioritizedVersions []schema.GroupVersion
	// Info about the resources in this group. It's a map from version to resource to the storage.
	// 1、既然是group信息，那么group肯定是固定的，那么这里的第一级key为version， 第二级key为kind，譬如deployment, deployment/status,
	// daemonset, daemonset/status, job等等资源
	// TODO 2、保存每个组员的存储信息，非常重要
	VersionedResourcesStorageMap map[string]map[string]rest.Storage
	// OptionsExternalVersion controls the APIVersion used for common objects in the
	// schema like api.Status, api.DeleteOptions, and metav1.ListOptions. Other implementors may
	// define a version "v1beta1" but want to use the Kubernetes "v1" internal objects.
	// If nil, defaults to groupMeta.GroupVersion.
	// TODO: Remove this when https://github.com/kubernetes/kubernetes/issues/19018 is fixed.
	// TODO 这玩意是干嘛的，K8S中似乎还有一个InternalVersion
	OptionsExternalVersion *schema.GroupVersion
	// MetaGroupVersion defaults to "meta.k8s.io/v1" and is the scheme group version used to decode
	// common API implementations like ListOptions. Future changes will allow this to vary by group
	// version (for when the inevitable meta/v2 group emerges).
	// 组的元信息，其实就是当前的Group是啥，Version是啥
	MetaGroupVersion *schema.GroupVersion

	// Scheme includes all of the types used by this group and how to convert between them (or
	// to convert objects from outside of this group that are accepted in this API).
	// TODO: replace with interfaces
	Scheme *runtime.Scheme
	// NegotiatedSerializer controls how this group encodes and decodes data
	// TODO 数据编解码器
	NegotiatedSerializer runtime.NegotiatedSerializer
	// ParameterCodec performs conversions for query parameters passed to API calls
	// TODO 一个组下的所有资源的参数解码器都是一样的？
	ParameterCodec runtime.ParameterCodec

	// StaticOpenAPISpec is the spec derived from the definitions of all resources installed together.
	// It is set during InstallAPIGroups, InstallAPIGroup, and InstallLegacyAPIGroup.
	// TODO 这玩意是用来干嘛的？
	StaticOpenAPISpec map[string]*spec.Schema
}

func (a *APIGroupInfo) destroyStorage() {
	for _, stores := range a.VersionedResourcesStorageMap {
		for _, store := range stores {
			store.Destroy()
		}
	}
}

// GenericAPIServer contains state for a Kubernetes cluster api server.
// 1、GenericServer本身就是一个Delegator
type GenericAPIServer struct {
	// discoveryAddresses is used to build cluster IPs for discovery.
	// TODO 用于返回给客户端合适的ServerAddress
	discoveryAddresses discovery.Addresses

	// LoopbackClientConfig is a config for a privileged loopback connection to the API server
	// 用于通过回环网卡访问自己的客户端配置
	LoopbackClientConfig *restclient.Config

	// minRequestTimeout is how short the request timeout can be.  This is used to build the RESTHandlerf
	// TODO  最小请求超时时间，默认1800s，仅用于watch
	minRequestTimeout time.Duration

	// ShutdownTimeout is the timeout used for server shutdown. This specifies the timeout before server
	// gracefully shutdown returns.
	// GenericServer必须在指定的时间之内停止服务
	ShutdownTimeout time.Duration

	// legacyAPIGroupPrefixes is used to set up URL parsing for authorization and for validating requests
	// to InstallLegacyAPIGroup
	// 1、我们一般把Legacy资源称之为核心资源
	// 2、这里虽然是一个数组，但实际上核心资源的前缀只有/api，没有其它
	legacyAPIGroupPrefixes sets.String

	// admissionControl is used to build the RESTStorage that backs an API Group.
	// 1、GenericServer可以配置准入控制插件  TODO 详细分析准入控制插件的生命周期
	// TODO 2、插件初始化器的执行流程我猜测和鉴权器以及认证器应该是一样，也是用一个UnionAdmissionControl插件组装所有的准入控制插件
	// 并且在请求到来时挨个遍历每个准入控制插件
	admissionControl admission.Interface

	// SecureServingInfo holds configuration of the TLS server.
	// 所谓的安全服务信息，其实就是GenericServer启动时为了提供HTTPS服务需要的参数，譬如监听IP地址、端口、证书、TLS加密套件配置等等
	SecureServingInfo *SecureServingInfo

	// ExternalAddress is the address (hostname or IP and port) that should be used in
	// external (public internet) URLs for this GenericAPIServer.
	// TODO 什么叫做ExternalAddress? 难道还有一个InternalAddress？
	ExternalAddress string

	// Serializer controls how common API objects not in a group/version prefix are serialized for this server.
	// Individual APIGroups may define their own serializers.
	// 1、序列化器用于对数据进行序列化
	// 2、反序列的时机：1、请求进来时，需要找一个合适的结构体把请求的Body反序列化到结构体当中 2、从ETCD中获取数据时，也需要把二进制数据反序列化为结构体
	// 3、序列化的时机：把结构体数据持久化到ETCD时，需要进行序列化。序列化的格式可以支持：yaml, json, protobuf
	Serializer runtime.NegotiatedSerializer

	// "Outputs"
	// Handler holds the handlers being used by this API server
	// 1、APIServerHandler实际上就是一个http.Handler，用于处理HTTP请求。
	// 2、ExtensionServer, APIServer, Aggregated在初始化的时候会把相关的路由注册到GenericServer当中
	Handler *APIServerHandler

	// UnprotectedDebugSocket is used to serve pprof information in a unix-domain socket. This socket is
	// not protected by authentication/authorization.
	// 1、调试使用的  TODO 具体怎么使用还需要再研究
	UnprotectedDebugSocket *routes.DebugSocket

	// listedPathProvider is a lister which provides the set of paths to show at /
	// 用于列出当前GenericServer支持的所有路由，也就是URL
	listedPathProvider routes.ListedPathProvider

	// DiscoveryGroupManager serves /apis in an unaggregated form.
	// 1、组管理器可以允许动态的修改已经存在的WebService，支持添加、删除组
	// 2、组管理器的功能非常简单，就是用于维护当前GenericServer所管理的组。组管理器本质上是一个http.Handler，用户通过组管理器可以知道集群中
	// 可以使用的组有哪些。  我们可以通过kubectl get --raw=/apis的方式查询非核心资源意外的所有组。
	// 3、ExtensionServer、AIPServer、AggregatedServer在启动过程当中一定会对组管理器进行初始化，并且把组管理器返回的路由注册到GenericServer
	// 当中，从而支持HTTP请求的查询
	DiscoveryGroupManager discovery.GroupManager

	// AggregatedDiscoveryGroupManager serves /apis in an aggregated form.
	AggregatedDiscoveryGroupManager discoveryendpoint.ResourceManager

	// AggregatedLegacyDiscoveryGroupManager serves /api in an aggregated form.
	AggregatedLegacyDiscoveryGroupManager discoveryendpoint.ResourceManager

	// Enable swagger and/or OpenAPI if these configs are non-nil.
	openAPIConfig *openapicommon.Config

	// Enable swagger and/or OpenAPI V3 if these configs are non-nil.
	openAPIV3Config *openapicommon.Config

	// SkipOpenAPIInstallation indicates not to install the OpenAPI handler
	// during PrepareRun.
	// Set this to true when the specific API Server has its own OpenAPI handler
	// (e.g. kube-aggregator)
	skipOpenAPIInstallation bool

	// OpenAPIVersionedService controls the /openapi/v2 endpoint, and can be used to update the served spec.
	// It is set during PrepareRun if `openAPIConfig` is non-nil unless `skipOpenAPIInstallation` is true.
	OpenAPIVersionedService *handler.OpenAPIService

	// OpenAPIV3VersionedService controls the /openapi/v3 endpoint and can be used to update the served spec.
	// It is set during PrepareRun if `openAPIConfig` is non-nil unless `skipOpenAPIInstallation` is true.
	OpenAPIV3VersionedService *handler3.OpenAPIService

	// StaticOpenAPISpec is the spec derived from the restful container endpoints.
	// It is set during PrepareRun.
	StaticOpenAPISpec *spec.Swagger

	// PostStartHooks are each called after the server has started listening, in a separate go func for each
	// with no guarantee of ordering between them.  The map key is a name used for error reporting.
	// It may kill the process with a panic if it wishes to by returning an error.
	postStartHookLock sync.Mutex
	// 1、PostStartHook,GenericServer启动之后，就会启动所有的PostStartHook
	postStartHooks map[string]postStartHookEntry
	// 1、用于标识postStartHook是否被调用，如果已经被调用，那么不能再向GenericServer添加PostStartHook，PostStartHook被调用，说明了
	// GenericServer基本已经启动完成了，此时添加的PostStartHook极有可能无法被执行，因此是一旦postStartHook被执行，就不再允许添加
	// PostStartHook
	postStartHooksCalled bool
	// 被禁用的PostStartHook的名字
	disabledPostStartHooks sets.String

	preShutdownHookLock sync.Mutex
	// GenericServer停止前需要执行的Hook
	preShutdownHooks       map[string]preShutdownHookEntry
	preShutdownHooksCalled bool

	// healthz checks
	healthzLock            sync.Mutex
	healthzChecks          []healthz.HealthChecker
	healthzChecksInstalled bool
	// livez checks
	livezLock            sync.Mutex
	livezChecks          []healthz.HealthChecker
	livezChecksInstalled bool
	// readyz checks
	readyzLock            sync.Mutex
	readyzChecks          []healthz.HealthChecker
	readyzChecksInstalled bool
	livezGracePeriod      time.Duration
	livezClock            clock.Clock

	// auditing. The backend is started before the server starts listening.
	// 审计配置
	AuditBackend audit.Backend

	// Authorizer determines whether a user is allowed to make a certain request. The Handler does a preliminary
	// authorization check using the request URI but it may be necessary to make additional checks, such as in
	// the create-on-update case
	// 鉴权器
	Authorizer authorizer.Authorizer

	// EquivalentResourceRegistry provides information about resources equivalent to a given resource,
	// and the kind associated with a given resource. As resources are installed, they are registered here.
	// 1、用于缓存子资源到Kind的映射  TODO 为什么需要这种映射关系呢？
	EquivalentResourceRegistry runtime.EquivalentResourceRegistry

	// delegationTarget is the next delegate in the chain. This is never nil.
	// 如果当前的GenericServer处理不了，那么需要委派给下一个Delegator
	delegationTarget DelegationTarget

	// NonLongRunningRequestWaitGroup allows you to wait for all chain
	// handlers associated with non long-running requests
	// to complete while the server is shuting down.
	// TODO 仔细分析
	NonLongRunningRequestWaitGroup *utilwaitgroup.SafeWaitGroup
	// WatchRequestWaitGroup allows us to wait for all chain
	// handlers associated with active watch requests to
	// complete while the server is shuting down.
	// TODO 仔细分析
	WatchRequestWaitGroup *utilwaitgroup.RateLimitedSafeWaitGroup

	// ShutdownDelayDuration allows to block shutdown for some time, e.g. until endpoints pointing to this API server
	// have converged on all node. During this time, the API server keeps serving, /healthz will return 200,
	// but /readyz will return failure.
	// GenericServer关机以前可以继续服务的事件延迟时间
	ShutdownDelayDuration time.Duration

	// The limit on the request body size that would be accepted and decoded in a write request.
	// 0 means no limit.
	// 请求中Body携带的最大字节数
	maxRequestBodyBytes int64

	// APIServerID is the ID of this API server
	// TODO APIServerID有啥用？ 日志和审计？
	APIServerID string

	// StorageVersionManager holds the storage versions of the API resources installed by this server.
	// TODO 分析这玩意的作用
	StorageVersionManager storageversion.Manager

	// Version will enable the /version endpoint if non-nil
	// 此信息用于返回 /version路由信息
	Version *version.Info

	// lifecycleSignals provides access to the various signals that happen during the life cycle of the apiserver.
	// 1、这里所谓的信号，其实就是一个channel，当channel被关闭时，所有关注这个channel的组件都可以感知到，因此被称之为信号
	// 2、不同的信号代表着不同的含义，关注信号的组件会在收到信号之后做特定的事情
	lifecycleSignals lifecycleSignals

	// destroyFns contains a list of functions that should be called on shutdown to clean up resources.
	// GenericServer关闭时应该执行的函数，可以用于释放某些资源
	destroyFns []func()

	// muxAndDiscoveryCompleteSignals holds signals that indicate all known HTTP paths have been registered.
	// it exists primarily to avoid returning a 404 response when a resource actually exists but we haven't installed the path to a handler.
	// it is exposed for easier composition of the individual servers.
	// the primary users of this field are the WithMuxCompleteProtection filter and the NotFoundHandler
	// TODO 用于标识GenericServer路由是否注册完成 仔细分析， 生命周期
	muxAndDiscoveryCompleteSignals map[string]<-chan struct{}

	// ShutdownSendRetryAfter dictates when to initiate shutdown of the HTTP
	// Server during the graceful termination of the apiserver. If true, we wait
	// for non longrunning requests in flight to be drained and then initiate a
	// shutdown of the HTTP Server. If false, we initiate a shutdown of the HTTP
	// Server as soon as ShutdownDelayDuration has elapsed.
	// If enabled, after ShutdownDelayDuration elapses, any incoming request is
	// rejected with a 429 status code and a 'Retry-After' response.
	// TODO 分析
	ShutdownSendRetryAfter bool

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
	// TODO 分析
	ShutdownWatchTerminationGracePeriod time.Duration
}

// DelegationTarget is an interface which allows for composition of API servers with top level handling that works
// as expected.
// 1、DelegationTarget的实现只有GenericServer，用户无法实现这个接口
type DelegationTarget interface {
	// UnprotectedHandler returns a handler that is NOT protected by a normal chain
	// 1、所谓的未保护Handler，其实就是不用经过默认请求链（defaultHandleChain）
	UnprotectedHandler() http.Handler

	// PostStartHooks returns the post-start hooks that need to be combined
	PostStartHooks() map[string]postStartHookEntry

	// PreShutdownHooks returns the pre-stop hooks that need to be combined
	PreShutdownHooks() map[string]preShutdownHookEntry

	// HealthzChecks returns the healthz checks that need to be combined
	HealthzChecks() []healthz.HealthChecker

	// ListedPaths returns the paths for supporting an index
	// 1、列出当前Delegator (其实就是GenericServer）的路由
	ListedPaths() []string

	// NextDelegate returns the next delegationTarget in the chain of delegations
	NextDelegate() DelegationTarget

	// PrepareRun does post API installation setup steps. It calls recursively the same function of the delegates.
	// TODO 从这里可以看出，用户无法实现Delegator,因为这里的返回值时包外不可见的，因此只能使用GenericServer
	// 注册/openapi/v2, /openapi/v3, /healthz, /livez, /readyz路由
	PrepareRun() preparedGenericAPIServer

	// MuxAndDiscoveryCompleteSignals exposes registered signals that indicate if all known HTTP paths have been installed.
	// 用于标识所有已知路由已经注册完成
	MuxAndDiscoveryCompleteSignals() map[string]<-chan struct{}

	// Destroy cleans up its resources on shutdown.
	// Destroy has to be implemented in thread-safe way and be prepared
	// for being called more than once.
	Destroy()
}

func (s *GenericAPIServer) UnprotectedHandler() http.Handler {
	// when we delegate, we need the server we're delegating to choose whether or not to use gorestful
	return s.Handler.Director
}
func (s *GenericAPIServer) PostStartHooks() map[string]postStartHookEntry {
	return s.postStartHooks
}
func (s *GenericAPIServer) PreShutdownHooks() map[string]preShutdownHookEntry {
	return s.preShutdownHooks
}
func (s *GenericAPIServer) HealthzChecks() []healthz.HealthChecker {
	return s.healthzChecks
}
func (s *GenericAPIServer) ListedPaths() []string {
	return s.listedPathProvider.ListedPaths()
}

func (s *GenericAPIServer) NextDelegate() DelegationTarget {
	return s.delegationTarget
}

// RegisterMuxAndDiscoveryCompleteSignal registers the given signal that will be used to determine if all known
// HTTP paths have been registered. It is okay to call this method after instantiating the generic server but before running.
// 注册MuxAndDiscoveryCompleteSignal信号
func (s *GenericAPIServer) RegisterMuxAndDiscoveryCompleteSignal(signalName string, signal <-chan struct{}) error {
	if _, exists := s.muxAndDiscoveryCompleteSignals[signalName]; exists {
		return fmt.Errorf("%s already registered", signalName)
	}
	s.muxAndDiscoveryCompleteSignals[signalName] = signal
	return nil
}

func (s *GenericAPIServer) MuxAndDiscoveryCompleteSignals() map[string]<-chan struct{} {
	return s.muxAndDiscoveryCompleteSignals
}

// RegisterDestroyFunc registers a function that will be called during Destroy().
// The function have to be idempotent and prepared to be called more than once.
func (s *GenericAPIServer) RegisterDestroyFunc(destroyFn func()) {
	s.destroyFns = append(s.destroyFns, destroyFn)
}

// Destroy cleans up all its and its delegation target resources on shutdown.
// It starts with destroying its own resources and later proceeds with
// its delegation target.
func (s *GenericAPIServer) Destroy() {
	for _, destroyFn := range s.destroyFns {
		destroyFn()
	}
	if s.delegationTarget != nil {
		s.delegationTarget.Destroy()
	}
}

type emptyDelegate struct {
	// handler is called at the end of the delegation chain
	// when a request has been made against an unregistered HTTP path the individual servers will simply pass it through until it reaches the handler.
	handler http.Handler
}

func NewEmptyDelegate() DelegationTarget {
	return emptyDelegate{}
}

// NewEmptyDelegateWithCustomHandler allows for registering a custom handler usually for special handling of 404 requests
func NewEmptyDelegateWithCustomHandler(handler http.Handler) DelegationTarget {
	return emptyDelegate{handler}
}

func (s emptyDelegate) UnprotectedHandler() http.Handler {
	return s.handler
}
func (s emptyDelegate) PostStartHooks() map[string]postStartHookEntry {
	return map[string]postStartHookEntry{}
}
func (s emptyDelegate) PreShutdownHooks() map[string]preShutdownHookEntry {
	return map[string]preShutdownHookEntry{}
}
func (s emptyDelegate) HealthzChecks() []healthz.HealthChecker {
	return []healthz.HealthChecker{}
}
func (s emptyDelegate) ListedPaths() []string {
	return []string{}
}
func (s emptyDelegate) NextDelegate() DelegationTarget {
	return nil
}
func (s emptyDelegate) PrepareRun() preparedGenericAPIServer {
	return preparedGenericAPIServer{nil}
}
func (s emptyDelegate) MuxAndDiscoveryCompleteSignals() map[string]<-chan struct{} {
	return map[string]<-chan struct{}{}
}
func (s emptyDelegate) Destroy() {
}

// preparedGenericAPIServer is a private wrapper that enforces a call of PrepareRun() before Run can be invoked.
type preparedGenericAPIServer struct {
	*GenericAPIServer
}

// PrepareRun does post API installation setup steps. It calls recursively the same function of the delegates.
// 1、注册/openapi/v2, /openapi/v3, /healthz, /livez, /readyz路由
func (s *GenericAPIServer) PrepareRun() preparedGenericAPIServer {
	s.delegationTarget.PrepareRun()

	if s.openAPIConfig != nil && !s.skipOpenAPIInstallation {
		s.OpenAPIVersionedService, s.StaticOpenAPISpec = routes.OpenAPI{
			Config: s.openAPIConfig,
		}.InstallV2(s.Handler.GoRestfulContainer, s.Handler.NonGoRestfulMux)
	}

	if s.openAPIV3Config != nil && !s.skipOpenAPIInstallation {
		if utilfeature.DefaultFeatureGate.Enabled(features.OpenAPIV3) {
			s.OpenAPIV3VersionedService = routes.OpenAPI{
				Config: s.openAPIV3Config,
			}.InstallV3(s.Handler.GoRestfulContainer, s.Handler.NonGoRestfulMux)
		}
	}

	// 注册/healthz路由
	s.installHealthz()
	// 注册/livez路由
	s.installLivez()

	// as soon as shutdown is initiated, readiness should start failing
	readinessStopCh := s.lifecycleSignals.ShutdownInitiated.Signaled()
	err := s.addReadyzShutdownCheck(readinessStopCh)
	if err != nil {
		klog.Errorf("Failed to install readyz shutdown check %s", err)
	}
	// 注册/readyz路由
	s.installReadyz()

	return preparedGenericAPIServer{s}
}

// Run spawns the secure http server. It only returns if stopCh is closed
// or the secure port cannot be listened on initially.
// This is the diagram of what channels/signals are dependent on each other:
//
// |                                  stopCh
// |                                    |
// |           ---------------------------------------------------------
// |           |                                                       |
// |    ShutdownInitiated (shutdownInitiatedCh)                        |
// |           |                                                       |
// | (ShutdownDelayDuration)                                    (PreShutdownHooks)
// |           |                                                       |
// |  AfterShutdownDelayDuration (delayedStopCh)   PreShutdownHooksStopped (preShutdownHooksHasStoppedCh)
// |           |                                                       |
// |           |-------------------------------------------------------|
// |                                    |
// |                                    |
// |               NotAcceptingNewRequest (notAcceptingNewRequestCh)
// |                                    |
// |                                    |
// |           |----------------------------------------------------------------------------------|
// |           |                        |              |                                          |
// |        [without                 [with             |                                          |
// | ShutdownSendRetryAfter]  ShutdownSendRetryAfter]  |                                          |
// |           |                        |              |                                          |
// |           |                        ---------------|                                          |
// |           |                                       |                                          |
// |           |                      |----------------|-----------------------|                  |
// |           |                      |                                        |                  |
// |           |         (NonLongRunningRequestWaitGroup::Wait)   (WatchRequestWaitGroup::Wait)   |
// |           |                      |                                        |                  |
// |           |                      |------------------|---------------------|                  |
// |           |                                         |                                        |
// |           |                         InFlightRequestsDrained (drainedCh)                      |
// |           |                                         |                                        |
// |           |-------------------|---------------------|----------------------------------------|
// |                               |                     |
// |                       stopHttpServerCh     (AuditBackend::Shutdown())
// |                               |
// |                       listenerStoppedCh
// |                               |
// |      HTTPServerStoppedListening (httpServerStoppedListeningCh)
func (s preparedGenericAPIServer) Run(stopCh <-chan struct{}) error {
	delayedStopCh := s.lifecycleSignals.AfterShutdownDelayDuration
	shutdownInitiatedCh := s.lifecycleSignals.ShutdownInitiated

	// Clean up resources on shutdown.
	// 如果GenericServer被关闭，就需要执行销毁方法，释放某些资源
	defer s.Destroy()

	// If UDS profiling is enabled, start a local http server listening on that socket
	// TODO 开启DEBUG
	if s.UnprotectedDebugSocket != nil {
		go func() {
			defer utilruntime.HandleCrash()
			klog.Error(s.UnprotectedDebugSocket.Run(stopCh))
		}()
	}

	// spawn a new goroutine for closing the MuxAndDiscoveryComplete signal
	// registration happens during construction of the generic api server
	// the last server in the chain aggregates signals from the previous instances
	go func() {
		for _, muxAndDiscoveryCompletedSignal := range s.GenericAPIServer.MuxAndDiscoveryCompleteSignals() {
			select {
			// 这里不同的muxAndDiscoveryCompletedSignal可能完成的时间点不一样，因此这里在等待的时候，其他的muxAndDiscoveryCompletedSignal
			// 已经完成，但是我们要的是最终所有信号完成，所以这里这样写并没有什么毛病。因为我们并不关心不同的muxAndDiscoveryCompletedSignal
			// 完成的快慢，我们只关心最终所有的muxAndDiscoveryCompletedSignal都已经完成
			case <-muxAndDiscoveryCompletedSignal:
				continue
			case <-stopCh:
				klog.V(1).Infof("haven't completed %s, stop requested", s.lifecycleSignals.MuxAndDiscoveryComplete.Name())
				return
			}
		}
		// 只要GenericServer所有的路由已经注册完成
		s.lifecycleSignals.MuxAndDiscoveryComplete.Signal()
		klog.V(1).Infof("%s has all endpoints registered and discovery information is complete", s.lifecycleSignals.MuxAndDiscoveryComplete.Name())
	}()

	go func() {
		defer delayedStopCh.Signal()
		defer klog.V(1).InfoS("[graceful-termination] shutdown event", "name", delayedStopCh.Name())

		<-stopCh

		// As soon as shutdown is initiated, /readyz should start returning failure.
		// This gives the load balancer a window defined by ShutdownDelayDuration to detect that /readyz is red
		// and stop sending traffic to this server.
		// 发送关机信号
		shutdownInitiatedCh.Signal()
		klog.V(1).InfoS("[graceful-termination] shutdown event", "name", shutdownInitiatedCh.Name())

		time.Sleep(s.ShutdownDelayDuration)
	}()

	// close socket after delayed stopCh
	shutdownTimeout := s.ShutdownTimeout
	if s.ShutdownSendRetryAfter {
		// when this mode is enabled, we do the following:
		// - the server will continue to listen until all existing requests in flight
		//   (not including active long running requests) have been drained.
		// - once drained, http Server Shutdown is invoked with a timeout of 2s,
		//   net/http waits for 1s for the peer to respond to a GO_AWAY frame, so
		//   we should wait for a minimum of 2s
		shutdownTimeout = 2 * time.Second
		klog.V(1).InfoS("[graceful-termination] using HTTP Server shutdown timeout", "shutdownTimeout", shutdownTimeout)
	}

	notAcceptingNewRequestCh := s.lifecycleSignals.NotAcceptingNewRequest
	drainedCh := s.lifecycleSignals.InFlightRequestsDrained
	stopHttpServerCh := make(chan struct{})
	go func() {
		defer close(stopHttpServerCh)

		timeToStopHttpServerCh := notAcceptingNewRequestCh.Signaled()
		if s.ShutdownSendRetryAfter {
			timeToStopHttpServerCh = drainedCh.Signaled()
		}

		<-timeToStopHttpServerCh
	}()

	// Start the audit backend before any request comes in. This means we must call Backend.Run
	// before http server start serving. Otherwise the Backend.ProcessEvents call might block.
	// AuditBackend.Run will stop as soon as all in-flight requests are drained.
	if s.AuditBackend != nil {
		// 启动审计
		if err := s.AuditBackend.Run(drainedCh.Signaled()); err != nil {
			return fmt.Errorf("failed to run the audit backend: %v", err)
		}
	}

	// 以非阻塞的方式启动GenericServer，其实就是使用了一个协程启动GenericServer
	stoppedCh, listenerStoppedCh, err := s.NonBlockingRun(stopHttpServerCh, shutdownTimeout)
	if err != nil {
		return err
	}

	httpServerStoppedListeningCh := s.lifecycleSignals.HTTPServerStoppedListening
	go func() {
		<-listenerStoppedCh
		httpServerStoppedListeningCh.Signal()
		klog.V(1).InfoS("[graceful-termination] shutdown event", "name", httpServerStoppedListeningCh.Name())
	}()

	// we don't accept new request as soon as both ShutdownDelayDuration has
	// elapsed and preshutdown hooks have completed.
	preShutdownHooksHasStoppedCh := s.lifecycleSignals.PreShutdownHooksStopped
	go func() {
		defer klog.V(1).InfoS("[graceful-termination] shutdown event", "name", notAcceptingNewRequestCh.Name())
		defer notAcceptingNewRequestCh.Signal()

		// wait for the delayed stopCh before closing the handler chain
		<-delayedStopCh.Signaled()

		// Additionally wait for preshutdown hooks to also be finished, as some of them need
		// to send API calls to clean up after themselves (e.g. lease reconcilers removing
		// itself from the active servers).
		<-preShutdownHooksHasStoppedCh.Signaled()
	}()

	// wait for all in-flight non-long running requests to finish
	nonLongRunningRequestDrainedCh := make(chan struct{})
	go func() {
		defer close(nonLongRunningRequestDrainedCh)
		defer klog.V(1).Info("[graceful-termination] in-flight non long-running request(s) have drained")

		// wait for the delayed stopCh before closing the handler chain (it rejects everything after Wait has been called).
		<-notAcceptingNewRequestCh.Signaled()

		// Wait for all requests to finish, which are bounded by the RequestTimeout variable.
		// once NonLongRunningRequestWaitGroup.Wait is invoked, the apiserver is
		// expected to reject any incoming request with a {503, Retry-After}
		// response via the WithWaitGroup filter. On the contrary, we observe
		// that incoming request(s) get a 'connection refused' error, this is
		// because, at this point, we have called 'Server.Shutdown' and
		// net/http server has stopped listening. This causes incoming
		// request to get a 'connection refused' error.
		// On the other hand, if 'ShutdownSendRetryAfter' is enabled incoming
		// requests will be rejected with a {429, Retry-After} since
		// 'Server.Shutdown' will be invoked only after in-flight requests
		// have been drained.
		// TODO: can we consolidate these two modes of graceful termination?
		s.NonLongRunningRequestWaitGroup.Wait()
	}()

	// wait for all in-flight watches to finish
	activeWatchesDrainedCh := make(chan struct{})
	go func() {
		defer close(activeWatchesDrainedCh)

		<-notAcceptingNewRequestCh.Signaled()
		if s.ShutdownWatchTerminationGracePeriod <= time.Duration(0) {
			klog.V(1).InfoS("[graceful-termination] not going to wait for active watch request(s) to drain")
			return
		}

		// Wait for all active watches to finish
		grace := s.ShutdownWatchTerminationGracePeriod
		activeBefore, activeAfter, err := s.WatchRequestWaitGroup.Wait(func(count int) (utilwaitgroup.RateLimiter, context.Context, context.CancelFunc) {
			qps := float64(count) / grace.Seconds()
			// TODO: we don't want the QPS (max requests drained per second) to
			//  get below a certain floor value, since we want the server to
			//  drain the active watch requests as soon as possible.
			//  For now, it's hard coded to 200, and it is subject to change
			//  based on the result from the scale testing.
			if qps < 200 {
				qps = 200
			}

			ctx, cancel := context.WithTimeout(context.Background(), grace)
			// We don't expect more than one token to be consumed
			// in a single Wait call, so setting burst to 1.
			return rate.NewLimiter(rate.Limit(qps), 1), ctx, cancel
		})
		klog.V(1).InfoS("[graceful-termination] active watch request(s) have drained",
			"duration", grace, "activeWatchesBefore", activeBefore, "activeWatchesAfter", activeAfter, "error", err)
	}()

	go func() {
		defer klog.V(1).InfoS("[graceful-termination] shutdown event", "name", drainedCh.Name())
		defer drainedCh.Signal()

		<-nonLongRunningRequestDrainedCh
		<-activeWatchesDrainedCh
	}()

	klog.V(1).Info("[graceful-termination] waiting for shutdown to be initiated")
	<-stopCh

	// run shutdown hooks directly. This includes deregistering from
	// the kubernetes endpoint in case of kube-apiserver.
	func() {
		defer func() {
			preShutdownHooksHasStoppedCh.Signal()
			klog.V(1).InfoS("[graceful-termination] pre-shutdown hooks completed", "name", preShutdownHooksHasStoppedCh.Name())
		}()
		err = s.RunPreShutdownHooks()
	}()
	if err != nil {
		return err
	}

	// Wait for all requests in flight to drain, bounded by the RequestTimeout variable.
	<-drainedCh.Signaled()

	// 关闭审计后端
	if s.AuditBackend != nil {
		s.AuditBackend.Shutdown()
		klog.V(1).InfoS("[graceful-termination] audit backend shutdown completed")
	}

	// wait for stoppedCh that is closed when the graceful termination (server.Shutdown) is finished.
	<-listenerStoppedCh
	<-stoppedCh

	klog.V(1).Info("[graceful-termination] apiserver is exiting")
	return nil
}

// NonBlockingRun spawns the secure http server. An error is
// returned if the secure port cannot be listened on.
// The returned channel is closed when the (asynchronous) termination is finished.
func (s preparedGenericAPIServer) NonBlockingRun(stopCh <-chan struct{}, shutdownTimeout time.Duration) (<-chan struct{}, <-chan struct{}, error) {
	// Use an internal stop channel to allow cleanup of the listeners on error.
	internalStopCh := make(chan struct{})
	var stoppedCh <-chan struct{}
	var listenerStoppedCh <-chan struct{}
	// GenericServer必须以HTTPS的方式提供服务
	if s.SecureServingInfo != nil && s.Handler != nil {
		var err error
		stoppedCh, listenerStoppedCh, err = s.SecureServingInfo.Serve(s.Handler, shutdownTimeout, internalStopCh)
		if err != nil {
			close(internalStopCh)
			return nil, nil, err
		}
	}

	// Now that listener have bound successfully, it is the
	// responsibility of the caller to close the provided channel to
	// ensure cleanup.
	go func() {
		<-stopCh
		close(internalStopCh)
	}()

	// 启动PostStartHook
	s.RunPostStartHooks(stopCh)

	// TODO 这玩意干嘛的？
	if _, err := systemd.SdNotify(true, "READY=1\n"); err != nil {
		klog.Errorf("Unable to send systemd daemon successful start message: %v\n", err)
	}

	return stoppedCh, listenerStoppedCh, nil
}

// installAPIResources is a private method for installing the REST storage backing each api groupversionresource
// 1、把当前传入的APIGroupInfo中的资源所对应的路由注册到GenericServer当中
func (s *GenericAPIServer) installAPIResources(
	apiPrefix string, // 添加的路由URL前缀，目前只有两种前缀，核心资源使用/api前缀，而其余资源使用/apis前缀
	apiGroupInfo *APIGroupInfo, // APIGroupInfo对象携带了当前组下所有的资源
	openAPIModels map[string]*spec.Schema, // TODO 这里看起来和OpenAPI相关的东西
) error {
	// TODO 有何作用？
	var typeConverter managedfields.TypeConverter

	if len(openAPIModels) > 0 {
		var err error
		typeConverter, err = managedfields.NewTypeConverter(openAPIModels, false)
		if err != nil {
			return err
		}
	}
	var resourceInfos []*storageversion.ResourceInfo
	// 依次遍历APIGroupInfo中的组不同版本，分别注册每个版本不同资源的路由
	for _, groupVersion := range apiGroupInfo.PrioritizedVersions {
		// 某个组的某个版本如果没有资源，那么直接跳过
		if len(apiGroupInfo.VersionedResourcesStorageMap[groupVersion.Version]) == 0 {
			klog.Warningf("Skipping API %v because it has no resources.", groupVersion)
			continue
		}

		// 1、实例化一个APIGroupVersion，并根据APIGroupInfo信息进行初始化
		// 2、这一步还没有向GenericServer当中注册路由
		apiGroupVersion, err := s.getAPIGroupVersion(apiGroupInfo, groupVersion, apiPrefix)
		if err != nil {
			return err
		}
		if apiGroupInfo.OptionsExternalVersion != nil {
			// TODO OptionsExternalVersion有啥用？
			apiGroupVersion.OptionsExternalVersion = apiGroupInfo.OptionsExternalVersion
		}
		apiGroupVersion.TypeConverter = typeConverter
		apiGroupVersion.MaxRequestBodyBytes = s.maxRequestBodyBytes

		// 注册/<group>/<version>下所有资源的路由
		discoveryAPIResources, r, err := apiGroupVersion.InstallREST(s.Handler.GoRestfulContainer)

		if err != nil {
			return fmt.Errorf("unable to setup API %v: %v", apiGroupInfo, err)
		}
		resourceInfos = append(resourceInfos, r...)

		if utilfeature.DefaultFeatureGate.Enabled(features.AggregatedDiscoveryEndpoint) {
			// Aggregated discovery only aggregates resources under /apis
			if apiPrefix == APIGroupPrefix {
				s.AggregatedDiscoveryGroupManager.AddGroupVersion(
					groupVersion.Group,
					apidiscoveryv2beta1.APIVersionDiscovery{
						Freshness: apidiscoveryv2beta1.DiscoveryFreshnessCurrent,
						Version:   groupVersion.Version,
						Resources: discoveryAPIResources,
					},
				)
			} else {
				// There is only one group version for legacy resources, priority can be defaulted to 0.
				s.AggregatedLegacyDiscoveryGroupManager.AddGroupVersion(
					groupVersion.Group,
					apidiscoveryv2beta1.APIVersionDiscovery{
						Freshness: apidiscoveryv2beta1.DiscoveryFreshnessCurrent,
						Version:   groupVersion.Version,
						Resources: discoveryAPIResources,
					},
				)
			}
		}

	}

	s.RegisterDestroyFunc(apiGroupInfo.destroyStorage)

	if utilfeature.DefaultFeatureGate.Enabled(features.StorageVersionAPI) &&
		utilfeature.DefaultFeatureGate.Enabled(features.APIServerIdentity) {
		// API installation happens before we start listening on the handlers,
		// therefore it is safe to register ResourceInfos here. The handler will block
		// write requests until the storage versions of the targeting resources are updated.
		s.StorageVersionManager.AddResourceInfo(resourceInfos...)
	}

	return nil
}

// InstallLegacyAPIGroup exposes the given legacy api group in the API.
// The <apiGroupInfo> passed into this function shouldn't be used elsewhere as the
// underlying storage will be destroyed on this servers shutdown.
func (s *GenericAPIServer) InstallLegacyAPIGroup(apiPrefix string, apiGroupInfo *APIGroupInfo) error {
	if !s.legacyAPIGroupPrefixes.Has(apiPrefix) {
		return fmt.Errorf("%q is not in the allowed legacy API prefixes: %v", apiPrefix, s.legacyAPIGroupPrefixes.List())
	}

	openAPIModels, err := s.getOpenAPIModels(apiPrefix, apiGroupInfo)
	if err != nil {
		return fmt.Errorf("unable to get openapi models: %v", err)
	}

	if err := s.installAPIResources(apiPrefix, apiGroupInfo, openAPIModels); err != nil {
		return err
	}

	// Install the version handler.
	// Add a handler at /<apiPrefix> to enumerate the supported api versions.
	legacyRootAPIHandler := discovery.NewLegacyRootAPIHandler(s.discoveryAddresses, s.Serializer, apiPrefix)
	if utilfeature.DefaultFeatureGate.Enabled(features.AggregatedDiscoveryEndpoint) {
		wrapped := discoveryendpoint.WrapAggregatedDiscoveryToHandler(legacyRootAPIHandler, s.AggregatedLegacyDiscoveryGroupManager)
		s.Handler.GoRestfulContainer.Add(wrapped.GenerateWebService("/api", metav1.APIVersions{}))
	} else {
		s.Handler.GoRestfulContainer.Add(legacyRootAPIHandler.WebService())
	}

	return nil
}

// InstallAPIGroups exposes given api groups in the API.
// The <apiGroupInfos> passed into this function shouldn't be used elsewhere as the
// underlying storage will be destroyed on this servers shutdown.
// 向GenericServer当中注册当前组下所有资源的路由
func (s *GenericAPIServer) InstallAPIGroups(apiGroupInfos ...*APIGroupInfo) error {
	// 每个组都需要保证PrioritizedVersions不为空，因为这个属性中保存这该组下所有的版本
	for _, apiGroupInfo := range apiGroupInfos {
		// Do not register empty group or empty version.  Doing so claims /apis/ for the wrong entity to be returned.
		// Catching these here places the error  much closer to its origin
		if len(apiGroupInfo.PrioritizedVersions[0].Group) == 0 {
			return fmt.Errorf("cannot register handler with an empty group for %#v", *apiGroupInfo)
		}
		if len(apiGroupInfo.PrioritizedVersions[0].Version) == 0 {
			return fmt.Errorf("cannot register handler with an empty version for %#v", *apiGroupInfo)
		}
	}

	openAPIModels, err := s.getOpenAPIModels(APIGroupPrefix, apiGroupInfos...)
	if err != nil {
		return fmt.Errorf("unable to get openapi models: %v", err)
	}

	// 遍历每个APIGroupInfo信息，依次注册每个组下不同版本资源的路由
	for _, apiGroupInfo := range apiGroupInfos {
		// 注册某个组下不同版本资源的路由
		if err := s.installAPIResources(APIGroupPrefix, apiGroupInfo, openAPIModels); err != nil {
			return fmt.Errorf("unable to install api resources: %v", err)
		}

		// setup discovery
		// Install the version handler.
		// Add a handler at /apis/<groupName> to enumerate all versions supported by this group.
		var apiVersionsForDiscovery []metav1.GroupVersionForDiscovery
		for _, groupVersion := range apiGroupInfo.PrioritizedVersions {
			// Check the config to make sure that we elide versions that don't have any resources
			if len(apiGroupInfo.VersionedResourcesStorageMap[groupVersion.Version]) == 0 {
				continue
			}
			apiVersionsForDiscovery = append(apiVersionsForDiscovery, metav1.GroupVersionForDiscovery{
				GroupVersion: groupVersion.String(),
				Version:      groupVersion.Version,
			})
		}
		preferredVersionForDiscovery := metav1.GroupVersionForDiscovery{
			GroupVersion: apiGroupInfo.PrioritizedVersions[0].String(),
			Version:      apiGroupInfo.PrioritizedVersions[0].Version,
		}
		apiGroup := metav1.APIGroup{
			Name:             apiGroupInfo.PrioritizedVersions[0].Group,
			Versions:         apiVersionsForDiscovery,
			PreferredVersion: preferredVersionForDiscovery,
		}

		s.DiscoveryGroupManager.AddGroup(apiGroup)
		s.Handler.GoRestfulContainer.Add(discovery.NewAPIGroupHandler(s.Serializer, apiGroup).WebService())
	}
	return nil
}

// InstallAPIGroup exposes the given api group in the API.
// The <apiGroupInfo> passed into this function shouldn't be used elsewhere as the
// underlying storage will be destroyed on this servers shutdown.
// 向GenericServer当中注册当前组下所有资源的路由
func (s *GenericAPIServer) InstallAPIGroup(apiGroupInfo *APIGroupInfo) error {
	return s.InstallAPIGroups(apiGroupInfo)
}

// 实例化一个APIGroupVersion，并根据APIGroupInfo信息进行初始化
func (s *GenericAPIServer) getAPIGroupVersion(apiGroupInfo *APIGroupInfo, groupVersion schema.GroupVersion, apiPrefix string) (*genericapi.APIGroupVersion, error) {
	// storage为每个资源的增删改查操作
	storage := make(map[string]rest.Storage)
	for k, v := range apiGroupInfo.VersionedResourcesStorageMap[groupVersion.Version] {
		// k就是不同的版本，譬如v1, v1alpha1, v2alpha2, v2, v2beta2
		// v就是每个资源对应的增删改查实现
		if strings.ToLower(k) != k {
			return nil, fmt.Errorf("resource names must be lowercase only, not %q", k)
		}
		storage[k] = v
	}
	version := s.newAPIGroupVersion(apiGroupInfo, groupVersion)
	version.Root = apiPrefix
	version.Storage = storage
	return version, nil
}

func (s *GenericAPIServer) newAPIGroupVersion(apiGroupInfo *APIGroupInfo, groupVersion schema.GroupVersion) *genericapi.APIGroupVersion {
	return &genericapi.APIGroupVersion{
		GroupVersion:     groupVersion,
		MetaGroupVersion: apiGroupInfo.MetaGroupVersion,

		ParameterCodec:        apiGroupInfo.ParameterCodec,
		Serializer:            apiGroupInfo.NegotiatedSerializer,
		Creater:               apiGroupInfo.Scheme,
		Convertor:             apiGroupInfo.Scheme,
		ConvertabilityChecker: apiGroupInfo.Scheme,
		UnsafeConvertor:       runtime.UnsafeObjectConvertor(apiGroupInfo.Scheme),
		Defaulter:             apiGroupInfo.Scheme,
		Typer:                 apiGroupInfo.Scheme,
		Namer:                 runtime.Namer(meta.NewAccessor()),

		EquivalentResourceRegistry: s.EquivalentResourceRegistry,

		Admit:             s.admissionControl,
		MinRequestTimeout: s.minRequestTimeout,
		Authorizer:        s.Authorizer,
	}
}

// NewDefaultAPIGroupInfo returns an APIGroupInfo stubbed with "normal" values
// exposed for easier composition from other packages
func NewDefaultAPIGroupInfo(
	group string,
	scheme *runtime.Scheme,
	parameterCodec runtime.ParameterCodec,
	codecs serializer.CodecFactory,
) APIGroupInfo {
	return APIGroupInfo{
		PrioritizedVersions:          scheme.PrioritizedVersionsForGroup(group),
		VersionedResourcesStorageMap: map[string]map[string]rest.Storage{},
		// TODO unhardcode this.  It was hardcoded before, but we need to re-evaluate
		OptionsExternalVersion: &schema.GroupVersion{Version: "v1"},
		Scheme:                 scheme,
		ParameterCodec:         parameterCodec,
		NegotiatedSerializer:   codecs,
	}
}

// getOpenAPIModels is a private method for getting the OpenAPI models
func (s *GenericAPIServer) getOpenAPIModels(apiPrefix string, apiGroupInfos ...*APIGroupInfo) (map[string]*spec.Schema, error) {
	if s.openAPIV3Config == nil {
		//!TODO: A future work should add a requirement that
		// OpenAPIV3 config is required. May require some refactoring of tests.
		return nil, nil
	}
	pathsToIgnore := openapiutil.NewTrie(s.openAPIConfig.IgnorePrefixes)
	resourceNames := make([]string, 0)
	for _, apiGroupInfo := range apiGroupInfos {
		groupResources, err := getResourceNamesForGroup(apiPrefix, apiGroupInfo, pathsToIgnore)
		if err != nil {
			return nil, err
		}
		resourceNames = append(resourceNames, groupResources...)
	}

	// Build the openapi definitions for those resources and convert it to proto models
	openAPISpec, err := openapibuilder3.BuildOpenAPIDefinitionsForResources(s.openAPIV3Config, resourceNames...)
	if err != nil {
		return nil, err
	}
	for _, apiGroupInfo := range apiGroupInfos {
		apiGroupInfo.StaticOpenAPISpec = openAPISpec
	}
	return openAPISpec, nil
}

// getResourceNamesForGroup is a private method for getting the canonical names for each resource to build in an api group
func getResourceNamesForGroup(apiPrefix string, apiGroupInfo *APIGroupInfo, pathsToIgnore openapiutil.Trie) ([]string, error) {
	// Get the canonical names of every resource we need to build in this api group
	resourceNames := make([]string, 0)
	for _, groupVersion := range apiGroupInfo.PrioritizedVersions {
		for resource, storage := range apiGroupInfo.VersionedResourcesStorageMap[groupVersion.Version] {
			path := gpath.Join(apiPrefix, groupVersion.Group, groupVersion.Version, resource)
			if !pathsToIgnore.HasPrefix(path) {
				kind, err := genericapi.GetResourceKind(groupVersion, storage, apiGroupInfo.Scheme)
				if err != nil {
					return nil, err
				}
				sampleObject, err := apiGroupInfo.Scheme.New(kind)
				if err != nil {
					return nil, err
				}
				name := openapiutil.GetCanonicalTypeName(sampleObject)
				resourceNames = append(resourceNames, name)
			}
		}
	}

	return resourceNames, nil
}
