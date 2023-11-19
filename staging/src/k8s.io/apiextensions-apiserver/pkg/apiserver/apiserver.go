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

package apiserver

import (
	"fmt"
	"net/http"
	"time"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/install"
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	// TODO externalInformer和InternalInformer有何区别？
	externalinformers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions"
	"k8s.io/apiextensions-apiserver/pkg/controller/apiapproval"
	"k8s.io/apiextensions-apiserver/pkg/controller/establish"
	"k8s.io/apiextensions-apiserver/pkg/controller/finalizer"
	"k8s.io/apiextensions-apiserver/pkg/controller/nonstructuralschema"
	openapicontroller "k8s.io/apiextensions-apiserver/pkg/controller/openapi"
	openapiv3controller "k8s.io/apiextensions-apiserver/pkg/controller/openapiv3"
	"k8s.io/apiextensions-apiserver/pkg/controller/status"
	"k8s.io/apiextensions-apiserver/pkg/registry/customresourcedefinition"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/apiserver/pkg/endpoints/discovery"
	"k8s.io/apiserver/pkg/endpoints/discovery/aggregated"
	"k8s.io/apiserver/pkg/features"
	genericregistry "k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	serverstorage "k8s.io/apiserver/pkg/server/storage"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/apiserver/pkg/util/webhook"
)

var (
	// Scheme ExtensionServer中的Scheme只会注册CRD资源
	Scheme = runtime.NewScheme()
	Codecs = serializer.NewCodecFactory(Scheme)

	// if you modify this, make sure you update the crEncoder
	unversionedVersion = schema.GroupVersion{Group: "", Version: "v1"}
	unversionedTypes   = []runtime.Object{
		&metav1.Status{},
		&metav1.WatchEvent{},
		&metav1.APIVersions{},
		&metav1.APIGroupList{},
		&metav1.APIGroup{},
		&metav1.APIResourceList{},
	}
)

func init() {
	// 这里就是ExtensionServer注册的地方
	install.Install(Scheme)

	// we need to add the options to empty v1
	// 向Scheme添加GVK到GoStruct的映射，内部会调用
	metav1.AddToGroupVersion(Scheme, schema.GroupVersion{Group: "", Version: "v1"})

	// 向Schema中添加没有版本号的资源
	Scheme.AddUnversionedTypes(unversionedVersion, unversionedTypes...)
}

// ExtraConfig ExtensionServer需要的额外配置
type ExtraConfig struct {
	// TODO 这玩意特别重要
	CRDRESTOptionsGetter genericregistry.RESTOptionsGetter

	// MasterCount is used to detect whether cluster is HA, and if it is
	// the CRD Establishing will be hold by 5 seconds.
	MasterCount int

	// ServiceResolver is used in CR webhook converters to resolve webhook's service names
	ServiceResolver webhook.ServiceResolver
	// AuthResolverWrapper is used in CR webhook converters
	AuthResolverWrapper webhook.AuthenticationInfoResolverWrapper
}

// Config ExtensionServer配置
type Config struct {
	GenericConfig *genericapiserver.RecommendedConfig // GenericServer配置
	ExtraConfig   ExtraConfig                         // ExtensionServer额外配置
}

type completedConfig struct {
	GenericConfig genericapiserver.CompletedConfig
	ExtraConfig   *ExtraConfig
}

type CompletedConfig struct {
	// Embed a private pointer that cannot be instantiated outside of this package.
	*completedConfig
}

type CustomResourceDefinitions struct {
	GenericAPIServer *genericapiserver.GenericAPIServer

	// provided for easier embedding
	Informers externalinformers.SharedInformerFactory
}

// Complete fills in any fields not set that are required to have valid data. It's mutating the receiver.
func (cfg *Config) Complete() CompletedConfig {
	c := completedConfig{
		cfg.GenericConfig.Complete(),
		&cfg.ExtraConfig,
	}

	// APIServer中已经设置了路由发现，ExtensionServer无需再进行路由发现
	c.GenericConfig.EnableDiscovery = false
	if c.GenericConfig.Version == nil {
		c.GenericConfig.Version = &version.Info{
			Major: "0",
			Minor: "1",
		}
	}

	return CompletedConfig{&c}
}

// New returns a new instance of CustomResourceDefinitions from the given config.
// 1、delegationTarget就是后续处理器，只要ExtensionServer处理不了就需要把请求转交给delegationTarget处理
func (c completedConfig) New(delegationTarget genericapiserver.DelegationTarget) (*CustomResourceDefinitions, error) {
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
	// 11、总结一下，实例化GenericServer主要干了这么几个事情，一个是实例化APIServerHandler，可以理解为一个Mux，用于把不同的URL路由到不同的
	// Handler当中。然后拷贝Delegator的PostStartHook以及PreShutdownHook到自己的当中。添加一些必要的PostStartHook，注册销毁函数，添加
	// 一些必要的API
	genericServer, err := c.GenericConfig.New("apiextensions-apiserver", delegationTarget)
	if err != nil {
		return nil, err
	}

	// hasCRDInformerSyncedSignal is closed when the CRD informer this server uses has been fully synchronized.
	// It ensures that requests to potential custom resource endpoints while the server hasn't installed all known HTTP paths get a 503 error instead of a 404
	hasCRDInformerSyncedSignal := make(chan struct{})
	// 添加一个信号，用于表示CRDInformer已经同步完成
	if err := genericServer.RegisterMuxAndDiscoveryCompleteSignal("CRDInformerHasNotSynced", hasCRDInformerSyncedSignal); err != nil {
		return nil, err
	}

	s := &CustomResourceDefinitions{
		GenericAPIServer: genericServer,
	}

	// 用于表示哪些资源被启用/禁用
	apiResourceConfig := c.GenericConfig.MergedResourceConfig

	// APIGroupInfo用于表征当前组的信息，其中最重要的就是存储信息
	apiGroupInfo := genericapiserver.NewDefaultAPIGroupInfo(apiextensions.GroupName, Scheme, metav1.ParameterCodec, Codecs)
	storage := map[string]rest.Storage{}
	// customresourcedefinitions
	// ExtensionServer只需要注册CRD资源以及CRD/status资源
	if resource := "customresourcedefinitions"; apiResourceConfig.ResourceEnabled(v1.SchemeGroupVersion.WithResource(resource)) {
		customResourceDefinitionStorage, err := customresourcedefinition.NewREST(Scheme, c.GenericConfig.RESTOptionsGetter)
		if err != nil {
			return nil, err
		}
		// 添加对于CRD资源的增删改查
		storage[resource] = customResourceDefinitionStorage
		// 添加对于CRD/status子资源的增删改查
		storage[resource+"/status"] = customresourcedefinition.NewStatusREST(Scheme, customResourceDefinitionStorage)
	}
	if len(storage) > 0 {
		apiGroupInfo.VersionedResourcesStorageMap[v1.SchemeGroupVersion.Version] = storage
	}

	// 这里注册路由的路径还是比较复杂的,对于ExtensionServer只需要注册apiextensions.k8s.io组下的资源
	if err := s.GenericAPIServer.InstallAPIGroup(&apiGroupInfo); err != nil {
		return nil, err
	}

	// 实例化APIServer客户端
	crdClient, err := clientset.NewForConfig(s.GenericAPIServer.LoopbackClientConfig)
	if err != nil {
		// it's really bad that this is leaking here, but until we can fix the test (which I'm pretty sure isn't even testing what it wants to test),
		// we need to be able to move forward
		return nil, fmt.Errorf("failed to create clientset: %v", err)
	}
	// 实例化SharedInformerFactory， TODO 这个Informer缓存了哪些资源？ 只有CRD资源还是所有资源
	s.Informers = externalinformers.NewSharedInformerFactory(crdClient, 5*time.Minute)

	// ExtensionServer的Delegator其实就是NotFoundHandler
	delegateHandler := delegationTarget.UnprotectedHandler()
	if delegateHandler == nil {
		delegateHandler = http.NotFoundHandler()
	}

	// 缓存用户自定义CRD的GroupVersion
	versionDiscoveryHandler := &versionDiscoveryHandler{
		discovery: map[schema.GroupVersion]*discovery.APIVersionHandler{},
		delegate:  delegateHandler,
	}
	// 缓存用户自定义CRD的Group
	groupDiscoveryHandler := &groupDiscoveryHandler{
		discovery: map[string]*discovery.APIGroupHandler{},
		delegate:  delegateHandler,
	}
	// 监听CRD资源，并给那些刚创建的CRD并且名字已经被接受的CRD添加Established=true的Condition
	establishingController := establish.NewEstablishingController(s.Informers.Apiextensions().V1().CustomResourceDefinitions(), crdClient.ApiextensionsV1())
	// 1、实现了CRD资源的增删改查
	crdHandler, err := NewCustomResourceDefinitionHandler(
		versionDiscoveryHandler,
		groupDiscoveryHandler,
		s.Informers.Apiextensions().V1().CustomResourceDefinitions(),
		delegateHandler,
		c.ExtraConfig.CRDRESTOptionsGetter,
		c.GenericConfig.AdmissionControl,
		establishingController,
		c.ExtraConfig.ServiceResolver,
		c.ExtraConfig.AuthResolverWrapper,
		c.ExtraConfig.MasterCount,
		s.GenericAPIServer.Authorizer,
		c.GenericConfig.RequestTimeout,
		time.Duration(c.GenericConfig.MinRequestTimeout)*time.Second,
		apiGroupInfo.StaticOpenAPISpec,
		c.GenericConfig.MaxRequestBodyBytes,
	)
	if err != nil {
		return nil, err
	}
	s.GenericAPIServer.Handler.NonGoRestfulMux.Handle("/apis", crdHandler)
	s.GenericAPIServer.Handler.NonGoRestfulMux.HandlePrefix("/apis/", crdHandler)
	s.GenericAPIServer.RegisterDestroyFunc(crdHandler.destroy)

	aggregatedDiscoveryManager := genericServer.AggregatedDiscoveryGroupManager
	if aggregatedDiscoveryManager != nil { // GenericServerConfig已经初始化了，肯定非空
		// 当前资源管理器管理的是CRD资源
		aggregatedDiscoveryManager = aggregatedDiscoveryManager.WithSource(aggregated.CRDSource)
	}
	// 1、主要用于监听CRD，并动态填充versionDiscoveryHandler, groupDiscoveryHandler
	// 2、versionDiscoveryHandler本质上就是http.Handler，用于获取某个GV的所有资源, 譬如 kubectl get --raw=/apis/crd.skyguard.com.cn/v1
	// 3、groupDiscoveryHandler本质上就是http.Handler，用于获取某个Group下的所有资源,譬如 kubectl get --raw=/apis/crd.skyguard.com.cn
	discoveryController := NewDiscoveryController(s.Informers.Apiextensions().V1().CustomResourceDefinitions(),
		versionDiscoveryHandler, groupDiscoveryHandler, aggregatedDiscoveryManager)
	// 1、判断当前CRD可以被接受的名字，其实就是看看当前CRD的单数名称、附属名称、缩写名称、Kind、ListKind是否和其它CRD有冲突
	// 2、如果有冲突，那么会给这个CRD打上NamesAccepted=False的Condition。否则，如果没有冲突，那么打上NamesAccepted=True
	// 3、更新CRD的状态，主要是更新当前CRD被接受的名称
	// 4、根据CRD的名称冲突问题初始化Established=False的Condition
	namingController := status.NewNamingConditionController(s.Informers.Apiextensions().V1().CustomResourceDefinitions(),
		crdClient.ApiextensionsV1())
	// 1、判断当前定义的CRD是否是非结构化定义的，如果是当前CRD是非结构化定义，那么给这个CRD打上NonStructuralSchema=True的Condition
	// 2、什么叫做非结构化Schema，其实说的是定义的CRD Spec字段的类型不明确，譬如是x-kubernetes-int-or-string
	// 或者x-kubernetes-preserve-unknown-fields类型，或者定义的字段没有类型，那么这个CRD就是非结构化Schema。
	// 3、目前K8S很多特性已经不再支持非结构化CRD定义，因此不在建议把CRD定义为结构化的。
	nonStructuralSchemaController := nonstructuralschema.NewConditionController(s.Informers.Apiextensions().V1().CustomResourceDefinitions(),
		crdClient.ApiextensionsV1())
	// 1、用于判断当前CRD所在的组是否是受保护的组，如果不是，那么这个Controller没有任何租用。如果当前CRD所在的组是受保护组，那么需要更具CRD的
	// api-approved.kubernetes.io注解的值打上KubernetesAPIApprovalPolicyConformant Condition
	// 2、在K8S中所谓受保护的组，其实就是k8s.io, *.k8s.io, kubernetes.io, *.kubernetes.io这几个组，只要定义的CRD满足这四个组，就是
	// 受保护组。在这个组中添加CRD是收到限制的
	// 3、如果用户自定的CRD的组是受保护的，那么这个CRD的注解必须含有api-approved.kubernetes.io，并且它的值是一个合法的URL
	apiApprovalController := apiapproval.NewKubernetesAPIApprovalPolicyConformantConditionController(
		s.Informers.Apiextensions().V1().CustomResourceDefinitions(), crdClient.ApiextensionsV1())
	// 1、如果定义的CRD定义了customresourcecleanup.apiextensions.k8s.io Finalizer，那么这个CRD在被删除的时候会删除这个CRD的所有CR
	// 2、说白了就是一个清理控制器，只要CRD使用了customresourcecleanup.apiextensions.k8s.io Finalizer，那么用在在删除这个CRD的时候，
	// 当前Controller就会清理这个CRD的所有CR资源
	finalizingController := finalizer.NewCRDFinalizer(
		s.Informers.Apiextensions().V1().CustomResourceDefinitions(),
		crdClient.ApiextensionsV1(),
		crdHandler,
	)

	// 启动Informer，开始同步所有资源
	s.GenericAPIServer.AddPostStartHookOrDie("start-apiextensions-informers", func(context genericapiserver.PostStartHookContext) error {
		s.Informers.Start(context.StopCh)
		return nil
	})
	s.GenericAPIServer.AddPostStartHookOrDie("start-apiextensions-controllers", func(context genericapiserver.PostStartHookContext) error {
		// OpenAPIVersionedService and StaticOpenAPISpec are populated in generic apiserver PrepareRun().
		// Together they serve the /openapi/v2 endpoint on a generic apiserver. A generic apiserver may
		// choose to not enable OpenAPI by having null openAPIConfig, and thus OpenAPIVersionedService
		// and StaticOpenAPISpec are both null. In that case we don't run the CRD OpenAPI controller.
		if s.GenericAPIServer.StaticOpenAPISpec != nil {
			if s.GenericAPIServer.OpenAPIVersionedService != nil {
				// TODO 这里应该是为了在Swagger文档上动态展示用户自定义的CRD，格式为openapi v2
				openapiController := openapicontroller.NewController(s.Informers.Apiextensions().V1().CustomResourceDefinitions())
				go openapiController.Run(s.GenericAPIServer.StaticOpenAPISpec, s.GenericAPIServer.OpenAPIVersionedService, context.StopCh)
			}

			if s.GenericAPIServer.OpenAPIV3VersionedService != nil && utilfeature.DefaultFeatureGate.Enabled(features.OpenAPIV3) {
				// TODO 这里应该是为了在Swagger文档上动态展示用户自定义的CRD，格式为openapi v3
				openapiv3Controller := openapiv3controller.NewController(s.Informers.Apiextensions().V1().CustomResourceDefinitions())
				go openapiv3Controller.Run(s.GenericAPIServer.OpenAPIV3VersionedService, context.StopCh)
			}
		}

		go namingController.Run(context.StopCh)
		go establishingController.Run(context.StopCh)
		go nonStructuralSchemaController.Run(5, context.StopCh)
		go apiApprovalController.Run(5, context.StopCh)
		go finalizingController.Run(5, context.StopCh)

		discoverySyncedCh := make(chan struct{})
		go discoveryController.Run(context.StopCh, discoverySyncedCh)
		select {
		case <-context.StopCh:
		case <-discoverySyncedCh:
		}

		return nil
	})
	// we don't want to report healthy until we can handle all CRDs that have already been registered.  Waiting for the informer
	// to sync makes sure that the lister will be valid before we begin.  There may still be races for CRDs added after startup,
	// but we won't go healthy until we can handle the ones already present.
	// 用于等待CRD Informer已经同步完成
	s.GenericAPIServer.AddPostStartHookOrDie("crd-informer-synced", func(context genericapiserver.PostStartHookContext) error {
		return wait.PollImmediateUntil(100*time.Millisecond, func() (bool, error) {
			if s.Informers.Apiextensions().V1().CustomResourceDefinitions().Informer().HasSynced() {
				close(hasCRDInformerSyncedSignal)
				return true, nil
			}
			return false, nil
		}, context.StopCh)
	})

	return s, nil
}

func DefaultAPIResourceConfigSource() *serverstorage.ResourceConfig {
	ret := serverstorage.NewResourceConfig()
	// NOTE: GroupVersions listed here will be enabled by default. Don't put alpha versions in the list.
	ret.EnableVersions(
		v1beta1.SchemeGroupVersion,
		v1.SchemeGroupVersion,
	)

	return ret
}
