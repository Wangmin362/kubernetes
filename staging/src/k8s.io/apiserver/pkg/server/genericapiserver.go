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
	"fmt"
	"net/http"
	gpath "path"
	"strings"
	"sync"
	"time"

	systemd "github.com/coreos/go-systemd/v22/daemon"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/sets"
	utilwaitgroup "k8s.io/apimachinery/pkg/util/waitgroup"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	genericapi "k8s.io/apiserver/pkg/endpoints"
	"k8s.io/apiserver/pkg/endpoints/discovery"
	"k8s.io/apiserver/pkg/endpoints/handlers/fieldmanager"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/server/healthz"
	"k8s.io/apiserver/pkg/server/routes"
	"k8s.io/apiserver/pkg/storageversion"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	utilopenapi "k8s.io/apiserver/pkg/util/openapi"
	restclient "k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	openapibuilder2 "k8s.io/kube-openapi/pkg/builder"
	openapicommon "k8s.io/kube-openapi/pkg/common"
	"k8s.io/kube-openapi/pkg/handler"
	"k8s.io/kube-openapi/pkg/handler3"
	openapiutil "k8s.io/kube-openapi/pkg/util"
	openapiproto "k8s.io/kube-openapi/pkg/util/proto"
	"k8s.io/kube-openapi/pkg/validation/spec"
	"k8s.io/utils/clock"
)

// APIGroupInfo Info about an API group.
// 1、APIGroupInfo是K8S抽象出来关于一个组的信息，该信息中包含了以下信息：
// 1.1：当前组的优先级版本信息
// 1.2：当前组不同版本资源的后端存储，这个后端存储本质上是 rest.StandardStorage 接口实现。最终传递的是各个资源通过genericregistry.Store
// 的实现。简单来说，保存的是各个资源增删改查Handler，譬如GET /api/v1/pod/namesapce/default/xxxxx路径的http.Handler
type APIGroupInfo struct {
	// 1、当前组的版本优先级，PrioritizedVersions中所有元素的Group必定是相同的，因为APIGroupInfo是关于一个组的所有信息的抽象
	// 2、譬如当当前组为apps时，顺序为：v1, v1beta2, v1beta1; 当时autoscaling组时，顺序为：v2, v1, v2beta2, v2beta1
	// 3、TODO 优先级版本有啥用？什么时候发生作用？
	PrioritizedVersions []schema.GroupVersion
	// Info about the resources in this group. It's a map from version to resource to the storage.
	// 1、用于设置每种资源的增删改查Handler,第一级key为version, 第二级key为resource， value为增删改查Handler, 实际上是 rest.StandardStorage
	// 接口，每个资源都是通过 rest.StandardStorage 接口的标注实现 registry.Store 来实现的。
	// 2、TODO 为什么是rest.Storage接口，而不是rest.StandardStorage接口
	VersionedResourcesStorageMap map[string]map[string]rest.Storage
	// OptionsExternalVersion controls the APIVersion used for common objects in the
	// schema like api.Status, api.DeleteOptions, and metav1.ListOptions. Other implementors may
	// define a version "v1beta1" but want to use the Kubernetes "v1" internal objects.
	// If nil, defaults to groupMeta.GroupVersion.
	// TODO: Remove this when https://github.com/kubernetes/kubernetes/issues/19018 is fixed.
	// TODO 这个字段又增加了什么信息??
	// TODO K8S的资源有ExternalVersion以及InternalVersion之分，这里的ExternalVersion是指的它么？
	OptionsExternalVersion *schema.GroupVersion
	// MetaGroupVersion defaults to "meta.k8s.io/v1" and is the scheme group version used to decode
	// common API implementations like ListOptions. Future changes will allow this to vary by group
	// version (for when the inevitable meta/v2 group emerges).
	// TODO K8S中的META信息是啥？ 什么叫做MetaGroupVersion
	MetaGroupVersion *schema.GroupVersion

	// Scheme includes all of the types used by this group and how to convert between them (or
	// to convert objects from outside of this group that are accepted in this API).
	// TODO: replace with interfaces
	// TODO 如何理解K8S中的策略
	Scheme *runtime.Scheme
	// NegotiatedSerializer controls how this group encodes and decodes data
	// TODO 编解码器居然是以组为单位设计的，这么做有什么好处？  组下的每个资源可以指定自己的序列化和反序列化器么？
	NegotiatedSerializer runtime.NegotiatedSerializer
	// ParameterCodec performs conversions for query parameters passed to API calls
	// TODO 参数编解码器,这玩意应该是用来把http body的信息反序列化为go type的东西吧
	// TODO 为什么也是一个组一个参数编解码器，讲道理来说不应该是每个资源一个参数编解码器么？ 毕竟每个资源的goType定义的不一样啊
	ParameterCodec runtime.ParameterCodec

	// StaticOpenAPISpec is the spec derived from the definitions of all resources installed together.
	// It is set during InstallAPIGroups, InstallAPIGroup, and InstallLegacyAPIGroup.
	// TODO 这玩意应该就和openapi标准相关了
	StaticOpenAPISpec *spec.Swagger
}

func (a *APIGroupInfo) destroyStorage() {
	for _, stores := range a.VersionedResourcesStorageMap {
		for _, store := range stores {
			store.Destroy()
		}
	}
}

// GenericAPIServer contains state for a Kubernetes cluster api server.
// TODO k8s是如何抽象GenericAPIServer的？ 通用Server应该具有什么样的功能？ 以及在哪些地方需要扩展？
// TODO GenericAPIServer到底有多generic? 这东西似乎是用来给开发人员开发聚合接口使用的
// TODO 本质上GenericAPIServer就是一个WebServer，用于处理HTTP请求
type GenericAPIServer struct {
	// discoveryAddresses is used to build cluster IPs for discovery.
	// TODO 什么叫做集群的IP发现？
	discoveryAddresses discovery.Addresses

	// LoopbackClientConfig is a config for a privileged loopback connection to the API server
	// 什么叫做loopback连接？ 可以理解为本地环回地址，也就是lo网卡；实际上就是127.0.0.1
	// TODO 为什么说LoopbackClientConfig可以拥有一个特权级别的环回地址连接？ 这样配置有啥好处
	LoopbackClientConfig *restclient.Config

	// minRequestTimeout is how short the request timeout can be.  This is used to build the RESTHandler
	minRequestTimeout time.Duration

	// ShutdownTimeout is the timeout used for server shutdown. This specifies the timeout before server
	// gracefully shutdown returns.
	ShutdownTimeout time.Duration

	// legacyAPIGroupPrefixes is used to set up URL parsing for authorization and for validating requests
	// to InstallLegacyAPIGroup
	// TODO 为什么是集合？ 这应该是可扩展性的考虑，虽然目前只有/api前缀，但是保不齐还会有其它前缀
	legacyAPIGroupPrefixes sets.String

	// admissionControl is used to build the RESTStorage that backs an API Group.
	// 实际上，这里的admissionControl是一个准入控制插件数组，只不过这个数组实现了Admit, Validate, Handles接口，对于GenericServer来说，
	// 就是一个准入控制插件，只不过这个插件本质上是准入控制插件数组，在真正执行内部的注入控制函数时，准入控制数组依次遍历内部的准入控制插件完成
	// 准入控制逻辑
	admissionControl admission.Interface

	// SecureServingInfo holds configuration of the TLS server.
	// 启动HTTPS服务所需要的信息
	SecureServingInfo *SecureServingInfo

	// ExternalAddress is the address (hostname or IP and port) that should be used in
	// external (public internet) URLs for this GenericAPIServer.
	// 这个应该就是apiserver的地址了
	ExternalAddress string

	// Serializer controls how common API objects not in a group/version prefix are serialized for this server.
	// Individual APIGroups may define their own serializers.
	// 序列化、反序列化器
	Serializer runtime.NegotiatedSerializer

	// "Outputs"
	// Handler holds the handlers being used by this API server
	// TODO URL + Method => Handler的映射就在这里面了
	// TODO APIServer AggregatorServer  ExtensionServer是如何初始化这个属性的?
	// TODO 如何理解K8S APIServerHandler的设计？
	Handler *APIServerHandler

	// listedPathProvider is a lister which provides the set of paths to show at /
	// TODO 这玩意是用来干嘛的？ 如何理解？
	// 答：用于返回当前的generic server可以处理哪些URL开头的路径
	listedPathProvider routes.ListedPathProvider

	// DiscoveryGroupManager serves /apis
	DiscoveryGroupManager discovery.GroupManager

	// Enable swagger and/or OpenAPI if these configs are non-nil.
	// Swagger文档
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
	// server开始监听端口后，postStartHookLock就会被调用
	postStartHookLock sync.Mutex
	// TODO postStartHooks是如何涉及的？为什么要这么设计？
	// TODO postStartHook是什么时候被调用的？
	postStartHooks       map[string]postStartHookEntry
	postStartHooksCalled bool
	// TODO 猜测这个属性中的值未postStartHooks中的key
	disabledPostStartHooks sets.String

	// 在停止server之前，需要做的收尾工作
	// TODO APIServer, ExtendServer, AggregatorServer都是如何利用这些Hook点的呢？
	preShutdownHookLock sync.Mutex
	// GenericServer停止的时候会执行这些HOOK点
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
	// TODO 如何理解审计后端？
	AuditBackend audit.Backend

	// Authorizer determines whether a user is allowed to make a certain request. The Handler does a preliminary
	// authorization check using the request URI but it may be necessary to make additional checks, such as in
	// the create-on-update case
	// 1、K8S有AlwaysAllow,AlwaysDeny,ABAC,Webhook,RBAC,Node这几种模式，用户可以选择配置其中的一个或者几个，甚至可以全部配置；
	// 2、AuthorizationInfo本质上就是Authorizer，也就是是我们所谓的鉴权器。
	// 3、GenericServer的鉴权器本质上是一个[]Authorizer，也就是一个鉴权器数组。这个鉴权器数组实现了Authorize方法，也就是说鉴权其数组实际上
	// 就是一个鉴权器，只不过在判断一个HTTP请求是否有权限访问K8S资源的时候鉴权器数组会依次委托给内部的鉴权器
	// 4、从第三点可以看出，鉴权器数组的顺序非常重要，前面的鉴权器只要做除了鉴权决定吗，后面的鉴权器将永远不会得到执行
	Authorizer authorizer.Authorizer

	// EquivalentResourceRegistry provides information about resources equivalent to a given resource,
	// and the kind associated with a given resource. As resources are installed, they are registered here.
	// TODO 什么叫做等效资源注册器？ 意思是多个 URL+Method可以映射到一个Handler之上么？
	EquivalentResourceRegistry runtime.EquivalentResourceRegistry

	// delegationTarget is the next delegate in the chain. This is never nil.
	// 当前server处理不了，就把请求委派给delegationTarget
	delegationTarget DelegationTarget

	// HandlerChainWaitGroup allows you to wait for all chain handlers finish after the server shutdown.
	// TODO 啥时候需要
	HandlerChainWaitGroup *utilwaitgroup.SafeWaitGroup

	// ShutdownDelayDuration allows to block shutdown for some time, e.g. until endpoints pointing to this API server
	// have converged on all node. During this time, the API server keeps serving, /healthz will return 200,
	// but /readyz will return failure.
	// server关闭的时候可能需要处理什么东西，因此可以通过这个时间阻塞一会儿再关闭
	ShutdownDelayDuration time.Duration

	// The limit on the request body size that would be accepted and decoded in a write request.
	// 0 means no limit.
	// 定义了HTTP请求的Body最大能传输多少数据
	maxRequestBodyBytes int64

	// APIServerID is the ID of this API server
	// TODO 如果有多个apiserver, 那么这个ID是怎么产生的？
	APIServerID string

	// StorageVersionManager holds the storage versions of the API resources installed by this server.
	// TODO 什么叫做StorageVersion
	StorageVersionManager storageversion.Manager

	// Version will enable the /version endpoint if non-nil
	// 这个适用于/version接口响应数据的定义
	Version *version.Info

	// lifecycleSignals provides access to the various signals that happen during the life cycle of the apiserver.
	// TODO 生命周期信号 从K8S的定义中我们能够学习到什么？
	lifecycleSignals lifecycleSignals

	// destroyFns contains a list of functions that should be called on shutdown to clean up resources.
	// 这玩意和preShutdownHooks有何区别？ 调用的时间点不一样？ 两者都是GenericServer停止之后执行，preShutdownHooks先执行，
	// 而destroyFns后执行
	destroyFns []func()

	// muxAndDiscoveryCompleteSignals holds signals that indicate all known HTTP paths have been registered.
	// it exists primarily to avoid returning a 404 response when a resource actually exists but we haven't installed the path to a handler.
	// it is exposed for easier composition of the individual servers.
	// the primary users of this field are the WithMuxCompleteProtection filter and the NotFoundHandler
	// TODO 如何理解这个属性？
	// 答：从上面的注释可以发现，这个属性是一个标志，用于标识Server的API是否都已经注册,如果在初始化的过程当中，
	// Server提供服务的API还未来得及注册此时就会返回404
	muxAndDiscoveryCompleteSignals map[string]<-chan struct{}

	// ShutdownSendRetryAfter dictates when to initiate shutdown of the HTTP
	// Server during the graceful termination of the apiserver. If true, we wait
	// for non longrunning requests in flight to be drained and then initiate a
	// shutdown of the HTTP Server. If false, we initiate a shutdown of the HTTP
	// Server as soon as ShutdownDelayDuration has elapsed.
	// If enabled, after ShutdownDelayDuration elapses, any incoming request is
	// rejected with a 429 status code and a 'Retry-After' response.
	// TODO 如何理解这个属性？
	ShutdownSendRetryAfter bool
}

// DelegationTarget is an interface which allows for composition of API servers with top level handling that works
// as expected.
// TODO 如何理解委派目标这个接口的设计？ k8s为啥设计了这么多的接口方法？
// 实际上GenericAPIServer就是DelegationTarget的一个实现
type DelegationTarget interface {
	// UnprotectedHandler returns a handler that is NOT protected by a normal chain
	// TODO 什么叫做未保护的Handler? 未保护的Handler意味着不做认证、鉴权、审计相关动作,即原生的请求，没有做任何处理
	UnprotectedHandler() http.Handler

	// PostStartHooks returns the post-start hooks that need to be combined
	PostStartHooks() map[string]postStartHookEntry

	// PreShutdownHooks returns the pre-stop hooks that need to be combined
	PreShutdownHooks() map[string]preShutdownHookEntry

	// HealthzChecks returns the healthz checks that need to be combined
	HealthzChecks() []healthz.HealthChecker

	// ListedPaths returns the paths for supporting an index
	// TODO APIServer以及ExtensionServer是如何实现的？
	// 列出所有的资源路径
	ListedPaths() []string

	// NextDelegate returns the next delegationTarget in the chain of delegations
	NextDelegate() DelegationTarget

	// PrepareRun does post API installation setup steps. It calls recursively the same function of the delegates.
	// TODO 这里的PrepareRun接口返回值定义未一个私有对象，如何理解为什么要这么设计
	PrepareRun() preparedGenericAPIServer

	// MuxAndDiscoveryCompleteSignals exposes registered signals that indicate if all known HTTP paths have been installed.
	// TODO 如何理解这里的设计？
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
// TODO 如何理解这个函数的主要作用？
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

// TODO NotFoundHandler应该就是一个空委派吧
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
	// 看到这里因该就可以理解什么叫做UnprotectedHandler，实际上就是不经过认证、授权
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
// TODO 为什么需要PrepareRun? K8S是如何设计的？
func (s *GenericAPIServer) PrepareRun() preparedGenericAPIServer {
	// 递归调用PrepareRun方法
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

	s.installHealthz()
	s.installLivez()

	// as soon as shutdown is initiated, readiness should start failing
	readinessStopCh := s.lifecycleSignals.ShutdownInitiated.Signaled()
	err := s.addReadyzShutdownCheck(readinessStopCh)
	if err != nil {
		klog.Errorf("Failed to install readyz shutdown check %s", err)
	}
	s.installReadyz()

	return preparedGenericAPIServer{s}
}

// Run spawns the secure http server. It only returns if stopCh is closed
// or the secure port cannot be listened on initially.
// This is the diagram of what channels/signals are dependent on each other:
//
//	                              stopCh
//	                                |
//	       ---------------------------------------------------------
//	       |                                                       |
//	ShutdownInitiated (shutdownInitiatedCh)                        |
//	       |                                                       |
//
// (ShutdownDelayDuration)                                    (PreShutdownHooks)
//
//	         |                                                       |
//	AfterShutdownDelayDuration (delayedStopCh)   PreShutdownHooksStopped (preShutdownHooksHasStoppedCh)
//	         |                                                       |
//	         |-------------------------------------------------------|
//	                                  |
//	                                  |
//	             NotAcceptingNewRequest (notAcceptingNewRequestCh)
//	                                  |
//	                                  |
//	         |---------------------------------------------------------|
//	         |                        |              |                 |
//	      [without                 [with             |                 |
//
// ShutdownSendRetryAfter]  ShutdownSendRetryAfter]  |                 |
//
//	     |                        |              |                 |
//	     |                        ---------------|                 |
//	     |                                       |                 |
//	     |                         (HandlerChainWaitGroup::Wait)   |
//	     |                                       |                 |
//	     |                    InFlightRequestsDrained (drainedCh)  |
//	     |                                       |                 |
//	     ----------------------------------------|-----------------|
//	                           |                 |
//	                 stopHttpServerCh     (AuditBackend::Shutdown())
//	                           |
//	                 listenerStoppedCh
//	                           |
//	HTTPServerStoppedListening (httpServerStoppedListeningCh)
//
// todo GenericAPIServer是怎么被运行起来的？
func (s preparedGenericAPIServer) Run(stopCh <-chan struct{}) error {
	delayedStopCh := s.lifecycleSignals.AfterShutdownDelayDuration
	shutdownInitiatedCh := s.lifecycleSignals.ShutdownInitiated

	// Clean up resources on shutdown.
	// 执行清理动作
	defer s.Destroy()

	// spawn a new goroutine for closing the MuxAndDiscoveryComplete signal
	// registration happens during construction of the generic api server
	// the last server in the chain aggregates signals from the previous instances
	// TODO 分析原理
	go func() {
		for _, muxAndDiscoveryCompletedSignal := range s.GenericAPIServer.MuxAndDiscoveryCompleteSignals() {
			select {
			case <-muxAndDiscoveryCompletedSignal:
				continue
			case <-stopCh:
				klog.V(1).Infof("haven't completed %s, stop requested", s.lifecycleSignals.MuxAndDiscoveryComplete.Name())
				return
			}
		}
		s.lifecycleSignals.MuxAndDiscoveryComplete.Signal()
		klog.V(1).Infof("%s has all endpoints registered and discovery information is complete", s.lifecycleSignals.MuxAndDiscoveryComplete.Name())
	}()

	// 分析原理
	go func() {
		defer delayedStopCh.Signal()
		defer klog.V(1).InfoS("[graceful-termination] shutdown event", "name", delayedStopCh.Name())

		<-stopCh

		// As soon as shutdown is initiated, /readyz should start returning failure.
		// This gives the load balancer a window defined by ShutdownDelayDuration to detect that /readyz is red
		// and stop sending traffic to this server.
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
		klog.V(1).InfoS("[graceful-termination] using HTTP Server shutdown timeout", "ShutdownTimeout", shutdownTimeout)
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
	// TODO 分析原理
	if s.AuditBackend != nil {
		if err := s.AuditBackend.Run(drainedCh.Signaled()); err != nil {
			return fmt.Errorf("failed to run the audit backend: %v", err)
		}
	}

	// TODO NonBlockingRun在内部通过SecureServingInfo配置信息实例化了一个http.Server,这个http.Server的Handler属性被
	// 设置为APIServerHandler，因此请求进来时会首先执行FullHandlerChain,也就是认证、鉴权、限速、审计相关的东西，最后才会被
	// 代理给APIServerHandler.Director处理
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
	// TODO 分析原理
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

	// TODO 分析原理
	go func() {
		defer klog.V(1).InfoS("[graceful-termination] shutdown event", "name", drainedCh.Name())
		defer drainedCh.Signal()

		// wait for the delayed stopCh before closing the handler chain (it rejects everything after Wait has been called).
		<-notAcceptingNewRequestCh.Signaled()

		// Wait for all requests to finish, which are bounded by the RequestTimeout variable.
		// once HandlerChainWaitGroup.Wait is invoked, the apiserver is
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
		s.HandlerChainWaitGroup.Wait()
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
	if s.SecureServingInfo != nil && s.Handler != nil {
		var err error
		// TODO 分析原理
		// SecureServingInfo在初始化generic server config的时候被设置
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

	// TODO 分析原理
	s.RunPostStartHooks(stopCh)

	if _, err := systemd.SdNotify(true, "READY=1\n"); err != nil {
		klog.Errorf("Unable to send systemd daemon successful start message: %v\n", err)
	}

	return stoppedCh, listenerStoppedCh, nil
}

// installAPIResources is a private method for installing the REST storage backing each api groupversionresource
// 什么叫做安装API资源？  实际上就是注册URL + Method与Handler的映射到restful的Container当中，而所谓的Storage其实就是Handler
func (s *GenericAPIServer) installAPIResources(apiPrefix string, apiGroupInfo *APIGroupInfo, openAPIModels openapiproto.Models) error {
	var resourceInfos []*storageversion.ResourceInfo
	// 按顺序注册APIGroup信息 注册顺序应该和k8s/pkg/controlplane/import_known_versions.go这个文件的顺序有关
	for _, groupVersion := range apiGroupInfo.PrioritizedVersions {
		// 由于PrioritizedVersions中记录的版本优先级是以组为单位，即每个组下的所有资源都是按照同一的版本优先级顺序。这种机制下回存储一个
		// 问题，那就是可能某个资源并不存在当前版本，所以需要跳过。譬如apps组的优先级版本为：v1, v1beta2, v1beta1，就可能存在job资源只有
		// v1, v1beta1版本，并不存在v1beta2版本资源。因此这里需要跳过。
		if len(apiGroupInfo.VersionedResourcesStorageMap[groupVersion.Version]) == 0 {
			klog.Warningf("Skipping API %v because it has no resources.", groupVersion)
			continue
		}

		// 1、根据APIGroupInfo信息以及当前需要的组版本还有路径前缀实例化APIGroupVersion信息
		// 2、实际上APIGroupVersion就是APIGroupInfo的子集，ApiGroupInfo中保存的是关于一个组下的所有版本的所有资源的增删改查Handler,而
		// APIGroupVersion则保存的是关于一个组的某个具体的所有资源的增删改查Handler
		apiGroupVersion, err := s.getAPIGroupVersion(apiGroupInfo, groupVersion, apiPrefix)
		if err != nil {
			return err
		}
		if apiGroupInfo.OptionsExternalVersion != nil {
			apiGroupVersion.OptionsExternalVersion = apiGroupInfo.OptionsExternalVersion
		}
		apiGroupVersion.OpenAPIModels = openAPIModels

		// TODO ServerSideApply特新有啥用？ TypeConverter干嘛用的？
		if openAPIModels != nil && utilfeature.DefaultFeatureGate.Enabled(features.ServerSideApply) {
			typeConverter, err := fieldmanager.NewTypeConverter(openAPIModels, false)
			if err != nil {
				return err
			}
			apiGroupVersion.TypeConverter = typeConverter
		}

		apiGroupVersion.MaxRequestBodyBytes = s.maxRequestBodyBytes

		// 每一种资源都是在这里注册到Container当中的
		r, err := apiGroupVersion.InstallREST(s.Handler.GoRestfulContainer)
		if err != nil {
			return fmt.Errorf("unable to setup API %v: %v", apiGroupInfo, err)
		}
		resourceInfos = append(resourceInfos, r...)
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
// 把所有的apiGroupInfo都注册到apiPrefix的底下
// APIGroupInfo中包含了所有要注册的资源
func (s *GenericAPIServer) InstallLegacyAPIGroup(apiPrefix string, apiGroupInfo *APIGroupInfo) error {
	// 实际上，目前的K8S的Legacy前缀必须是/api，否则无法注册
	if !s.legacyAPIGroupPrefixes.Has(apiPrefix) {
		return fmt.Errorf("%q is not in the allowed legacy API prefixes: %v", apiPrefix, s.legacyAPIGroupPrefixes.List())
	}

	// TODO 应该是为了装在OpenAPI相关的资源，也就是我们俗称的Swagger文档
	openAPIModels, err := s.getOpenAPIModels(apiPrefix, apiGroupInfo)
	if err != nil {
		return fmt.Errorf("unable to get openapi models: %v", err)
	}

	// 注册Legacy资源路由
	if err := s.installAPIResources(apiPrefix, apiGroupInfo, openAPIModels); err != nil {
		return err
	}

	// Install the version handler.
	// Add a handler at /<apiPrefix> to enumerate the supported api versions.
	// 注册/api路由，暴露/api路径下所有的Legacy资源
	s.Handler.GoRestfulContainer.Add(discovery.NewLegacyRootAPIHandler(s.discoveryAddresses, s.Serializer, apiPrefix).WebService())

	return nil
}

// InstallAPIGroups exposes given api groups in the API.
// The <apiGroupInfos> passed into this function shouldn't be used elsewhere as the
// underlying storage will be destroyed on this servers shutdown.
func (s *GenericAPIServer) InstallAPIGroups(apiGroupInfos ...*APIGroupInfo) error {
	// 参数检查
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

	for _, apiGroupInfo := range apiGroupInfos {
		// 1、由于这里安装的是除了Legacy以外的其它所有资源，因此这里的路由前缀为/apis
		if err := s.installAPIResources(APIGroupPrefix, apiGroupInfo, openAPIModels); err != nil {
			return fmt.Errorf("unable to install api resources: %v", err)
		}

		// setup discovery
		// Install the version handler.
		// Add a handler at /apis/<groupName> to enumerate all versions supported by this group.
		var apiVersionsForDiscovery []metav1.GroupVersionForDiscovery
		// 遍历所有组的信息
		for _, groupVersion := range apiGroupInfo.PrioritizedVersions {
			// Check the config to make sure that we elide versions that don't have any resources
			if len(apiGroupInfo.VersionedResourcesStorageMap[groupVersion.Version]) == 0 {
				// 说明当前版本，譬如v2alpha2下没有任何资源
				continue
			}
			apiVersionsForDiscovery = append(apiVersionsForDiscovery, metav1.GroupVersionForDiscovery{
				GroupVersion: groupVersion.String(),
				Version:      groupVersion.Version,
			})
		}

		// TODO 优先选择第一个作为优选版本
		preferredVersionForDiscovery := metav1.GroupVersionForDiscovery{
			GroupVersion: apiGroupInfo.PrioritizedVersions[0].String(),
			Version:      apiGroupInfo.PrioritizedVersions[0].Version,
		}
		apiGroup := metav1.APIGroup{
			Name:             apiGroupInfo.PrioritizedVersions[0].Group,
			Versions:         apiVersionsForDiscovery,
			PreferredVersion: preferredVersionForDiscovery,
		}

		// TODO DiscoveryGroupManager 用于暴露/apis路由，并把注册到DiscoveryGroupManager的所有的API返回给用户
		s.DiscoveryGroupManager.AddGroup(apiGroup)
		// TODO NewAPIGroupHandler用于暴露/apis/<group>路由，并返回注册的group的信息
		s.Handler.GoRestfulContainer.Add(discovery.NewAPIGroupHandler(s.Serializer, apiGroup).WebService())
	}
	return nil
}

// InstallAPIGroup exposes the given api group in the API.
// The <apiGroupInfo> passed into this function shouldn't be used elsewhere as the
// underlying storage will be destroyed on this servers shutdown.
func (s *GenericAPIServer) InstallAPIGroup(apiGroupInfo *APIGroupInfo) error {
	return s.InstallAPIGroups(apiGroupInfo)
}

func (s *GenericAPIServer) getAPIGroupVersion(apiGroupInfo *APIGroupInfo, groupVersion schema.GroupVersion, apiPrefix string) (*genericapi.APIGroupVersion, error) {
	// storage中保存的是同一个组下，同一个版本下的所有资源的增删改查的Handler，譬如apps组下v1版本下的deployment, daemonset, job, cronjob等等
	storage := make(map[string]rest.Storage)
	// k为resource, v为rest.Storage
	for k, v := range apiGroupInfo.VersionedResourcesStorageMap[groupVersion.Version] {
		if strings.ToLower(k) != k {
			return nil, fmt.Errorf("resource names must be lowercase only, not %q", k)
		}
		// map的深拷贝 TODO 思考，这里为什么需要重新创建一个Map保存信息，为什么不能直接使用APIGroupInfo的Map?
		storage[k] = v
	}
	// 实例化一个新的APIGroupVersion信息，实际上就是ApiGroupInfo的一个子集
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
func NewDefaultAPIGroupInfo(group string, scheme *runtime.Scheme, parameterCodec runtime.ParameterCodec, codecs serializer.CodecFactory) APIGroupInfo {
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
func (s *GenericAPIServer) getOpenAPIModels(apiPrefix string, apiGroupInfos ...*APIGroupInfo) (openapiproto.Models, error) {
	if s.openAPIConfig == nil {
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
	openAPISpec, err := openapibuilder2.BuildOpenAPIDefinitionsForResources(s.openAPIConfig, resourceNames...)
	if err != nil {
		return nil, err
	}
	for _, apiGroupInfo := range apiGroupInfos {
		apiGroupInfo.StaticOpenAPISpec = openAPISpec
	}
	return utilopenapi.ToProtoModels(openAPISpec)
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
