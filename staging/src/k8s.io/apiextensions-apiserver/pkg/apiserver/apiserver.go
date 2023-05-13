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
	"k8s.io/apiserver/pkg/features"
	genericregistry "k8s.io/apiserver/pkg/registry/generic"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	serverstorage "k8s.io/apiserver/pkg/server/storage"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/apiserver/pkg/util/webhook"
)

var (
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
	install.Install(Scheme)

	// we need to add the options to empty v1
	metav1.AddToGroupVersion(Scheme, schema.GroupVersion{Group: "", Version: "v1"})

	Scheme.AddUnversionedTypes(unversionedVersion, unversionedTypes...)
}

// ExtraConfig TODO 每种apiserver都有自己的额外配置信息
type ExtraConfig struct {
	// TODO CRD的后端存储
	CRDRESTOptionsGetter genericregistry.RESTOptionsGetter

	// MasterCount is used to detect whether cluster is HA, and if it is
	// the CRD Establishing will be hold by 5 seconds.
	MasterCount int

	// ServiceResolver is used in CR webhook converters to resolve webhook's service names
	// 根据服务的name, namespace, port解析出正确的服务访问地址
	ServiceResolver webhook.ServiceResolver
	// AuthResolverWrapper is used in CR webhook converters
	// TODO wrapper的具体作用是啥？
	AuthResolverWrapper webhook.AuthenticationInfoResolverWrapper
}

type Config struct {
	// 本质上就是generic apiserver的配置信息，只不过增加了informer
	GenericConfig *genericapiserver.RecommendedConfig
	ExtraConfig   ExtraConfig
}

type completedConfig struct {
	GenericConfig genericapiserver.CompletedConfig
	ExtraConfig   *ExtraConfig
}

type CompletedConfig struct {
	// Embed a private pointer that cannot be instantiated outside of this package.
	*completedConfig
}

// CustomResourceDefinitions 实际上就是Extension APIServer
type CustomResourceDefinitions struct {
	GenericAPIServer *genericapiserver.GenericAPIServer

	// provided for easier embedding
	Informers externalinformers.SharedInformerFactory
}

// Complete fills in any fields not set that are required to have valid data. It's mutating the receiver.
// extension-apiserver参数的补全
func (cfg *Config) Complete() CompletedConfig {
	c := completedConfig{
		cfg.GenericConfig.Complete(),
		&cfg.ExtraConfig,
	}

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
// 1、实例化一个名为apiextensions-apiserver的GenericServer
// 2、注册CRD以及CRD/status资源，也就是通过URL可以对CRD资源进行增删改查
// 3、实例化CRD的ShardInformerFactory，每隔五分钟同步一次CRD
// 4、实例化EstablishingController，用于为用户新提交的CRD新增Established=True的Condition，当然前提是这个CRD的所有命名（
// 包括：（Singular）、复数名称（Plural）、缩写名称（ShortNames）、Kind、KindList）都已经被接受，也就是已经存在NamesAccepted=True的Condition
// 5、实例化CRDHandler，CRDHandler用于动态支撑用户对于CR的增删改查
// 6、实例化DiscoveryController，用于动态发现用户提交的CR，支持CRDHandler的功能
// 7、实例化NamingConditionController，用于检测用户提交的CRD的所有名字是否都已经被接受，如果用户提交的CRD的所有名字被接受，就会被打上
// NamesAccepted=True的Condition
// 8、实例化NonStructuralSchemaController，用于检测用户提交的CRD是否是结构化的，如果用户提交的CRD不是结构化CRD，那么这个CRD会被打上
// NonStructuralSchema=True的Condition。值得一提的是，非结构化的CRD是K8S早期的产物，那个时候K8S对于用户提交的CRD并没有复杂的校验规则，
// 后来K8S发现非结构化的CRD是有风险的，因此增加了对于CRD的校验。详情可以参考：https://kubernetes.io/blog/2019/06/20/crd-structural-schema/
// 9、实例化KubernetesAPIApprovalPolicyConformantConditionController，用于检查用户提交的CRD的Group是否以：k8s.io, kubernetes.io
// 结尾，如果是，那么这些CRD需要审批流程。详情可以参考：https://github.com/kubernetes/enhancements/pull/1111
// 10、实例化CRD Finalizer用于处理CRD的删除逻辑
// 11、添加PostStartHook，分别添加了以下几个PostStartHook：
// 11.1、start-apiextensions-informers：用于启动之前实例化的CRD SharedInformerFactory
// 11.2、start-apiextensions-controllers：用于启动前面实例化的所有Controller
// 11.3、crd-informer-synced：用于判断CRD SharedInformerFactory是否已经同步完成
func (c completedConfig) New(delegationTarget genericapiserver.DelegationTarget) (*CustomResourceDefinitions, error) {
	// 实例化一个GenericServer，实际上所有的Server都是GenericServer
	genericServer, err := c.GenericConfig.New("apiextensions-apiserver", delegationTarget)
	if err != nil {
		return nil, err
	}

	// hasCRDInformerSyncedSignal is closed when the CRD informer this server uses has been fully synchronized.
	// It ensures that requests to potential custom resource endpoints while the server hasn't installed all known HTTP paths get a 503 error instead of a 404
	hasCRDInformerSyncedSignal := make(chan struct{})
	if err := genericServer.RegisterMuxAndDiscoveryCompleteSignal("CRDInformerHasNotSynced", hasCRDInformerSyncedSignal); err != nil {
		return nil, err
	}

	// 实例化ExtensionServer
	s := &CustomResourceDefinitions{
		GenericAPIServer: genericServer,
	}

	// MergedResourceConfig用于标识禁用哪些资源，启用哪些资源
	apiResourceConfig := c.GenericConfig.MergedResourceConfig

	// 实例化APIGroupInfo，此时的APIGroupInfo对象当中什么资源都没有注册
	apiGroupInfo := genericapiserver.NewDefaultAPIGroupInfo(apiextensions.GroupName, Scheme, metav1.ParameterCodec, Codecs)
	// 构建资源的后端存储
	storage := map[string]rest.Storage{}
	// customresourcedefinitions  注册CRD资源
	if resource := "customresourcedefinitions"; apiResourceConfig.ResourceEnabled(v1.SchemeGroupVersion.WithResource(resource)) {
		customResourceDefinitionStorage, err := customresourcedefinition.NewREST(Scheme, c.GenericConfig.RESTOptionsGetter)
		if err != nil {
			return nil, err
		}
		storage[resource] = customResourceDefinitionStorage
		storage[resource+"/status"] = customresourcedefinition.NewStatusREST(Scheme, customResourceDefinitionStorage)
	}
	if len(storage) > 0 {
		// 初始化后端资源存储
		apiGroupInfo.VersionedResourcesStorageMap[v1.SchemeGroupVersion.Version] = storage
	}

	// 向go-restful的container中注册CRD资源路由，即注册CRD相关的 restful api
	if err := s.GenericAPIServer.InstallAPIGroup(&apiGroupInfo); err != nil {
		return nil, err
	}

	// 实例化ClientSet客户端
	crdClient, err := clientset.NewForConfig(s.GenericAPIServer.LoopbackClientConfig)
	if err != nil {
		// it's really bad that this is leaking here, but until we can fix the test (which I'm pretty sure isn't even testing what it wants to test),
		// we need to be able to move forward
		return nil, fmt.Errorf("failed to create clientset: %v", err)
	}
	// CRDInformer, 每五分钟重新同步一次所有的CRD资源
	s.Informers = externalinformers.NewSharedInformerFactory(crdClient, 5*time.Minute)

	// extension-server的delegator是NofFound, NotFoundHandler实现了UnprotectedHandler
	delegateHandler := delegationTarget.UnprotectedHandler()
	if delegateHandler == nil {
		delegateHandler = http.NotFoundHandler()
	}

	// 1、CRD的服务发现，是/apis/<group>/<version>路由的数据来源
	versionDiscoveryHandler := &versionDiscoveryHandler{
		discovery: map[schema.GroupVersion]*discovery.APIVersionHandler{},
		delegate:  delegateHandler,
	}
	// 1、CRD的服务发现，是/apis/<group>路由的数据来源
	groupDiscoveryHandler := &groupDiscoveryHandler{
		discovery: map[string]*discovery.APIGroupHandler{},
		delegate:  delegateHandler,
	}
	// 1、EstablishingController更新入队CRD状态的Condition, 主要是新增Established状态
	// 2、原理非常简单，更新那些处于NamesAccepted状态但是还没有Established的CRD达到Established状态
	// 3、有一点值得注意，如果用户提交的CRD的所有名字没有被接受，EstablishedController会直接跳过此CRD,并不会为这种类型的CRD生成Established=True的Condition
	establishingController := establish.NewEstablishingController(s.Informers.Apiextensions().V1().CustomResourceDefinitions(),
		crdClient.ApiextensionsV1())
	// TODO crdHandler是如何处理的？
	crdHandler, err := NewCustomResourceDefinitionHandler(
		versionDiscoveryHandler, // /apis/<group>/<version>的服务发现
		groupDiscoveryHandler,   // /apis/<group>的服务发现
		s.Informers.Apiextensions().V1().CustomResourceDefinitions(), // CRD Informer
		delegateHandler,                    // 如果CRD Handler无法处理，委派给Delegator处理
		c.ExtraConfig.CRDRESTOptionsGetter, // 后端存储相关
		c.GenericConfig.AdmissionControl,   // 准入控制，本质上是一个准入控制插件链
		establishingController,             // TODO
		c.ExtraConfig.ServiceResolver,      // 根据服务的name, namespace, port拼接出服务的访问地址
		c.ExtraConfig.AuthResolverWrapper,  // 认证
		c.ExtraConfig.MasterCount,          // master节点数量
		s.GenericAPIServer.Authorizer,      // 授权器，实际上是一个授权器链
		c.GenericConfig.RequestTimeout,     // 请求超时时间
		time.Duration(c.GenericConfig.MinRequestTimeout)*time.Second,
		apiGroupInfo.StaticOpenAPISpec,
		c.GenericConfig.MaxRequestBodyBytes,
	)
	if err != nil {
		return nil, err
	}
	// TODO 为啥需要两个同时注册？ 因为第一个是精确匹配，而第一个是前缀匹配
	// 第一个只能处理 /apis endpoint，而第二个可以处理所有以/apis/开头的所有endpoint
	s.GenericAPIServer.Handler.NonGoRestfulMux.Handle("/apis", crdHandler)
	s.GenericAPIServer.Handler.NonGoRestfulMux.HandlePrefix("/apis/", crdHandler)
	// 当GenericServer停止运行之后会执行销毁方法，实际上就是一些清理动作
	s.GenericAPIServer.RegisterDestroyFunc(crdHandler.destroy)

	// 1、监听CRD，每一个CRD都会定义group, version, resource，甚至一个CRD会有多个version，为了支持查询， DiscoveryController会把
	// 监听到的CRD以/apis/<group>/<version> API放入到 versionDiscoveryHandler，并且把监听到的CRD以/apis/<group> API放入到
	// groupDiscoveryHandler当中
	discoveryController := NewDiscoveryController(s.Informers.Apiextensions().V1().CustomResourceDefinitions(), versionDiscoveryHandler, groupDiscoveryHandler)
	// 1、判断用户新提交的CRD是否和系统中已经存在的CRD命名冲突，如果有命名冲突，该CRD就会被打上NamesAccepted=False并且Established=False的Condition
	// 如果用户提交的CRD和系统中已经存在的CRD命名没有冲突，那么此CRD会被打上NamesAccepted=True并且Established=False的Condition
	// 2、用户提交CRD时，NamingConditionController会检测CRD的单数名称（Singular）、复数名称（Plural）、缩写名称（ShortNames）、Kind、KindList
	namingController := status.NewNamingConditionController(s.Informers.Apiextensions().V1().CustomResourceDefinitions(), crdClient.ApiextensionsV1())
	// 1、NonStructuralSchema Condition含义可以参考：https://kubernetes.io/zh-cn/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/#specifying-a-structural-schema
	// 2、NonStructuralSchema 似乎是CRD刚开始使用的策略，后来由于人们意识到NonStructuralSchema定义无法验证，因此并不安全。而且NonStructuralSchema
	// 定义的数据，APIServer会全部存储到etcd当中，因此K8S后来要求CRD必须都是StructuralSchema，也就是CRD的定义必须都是结构化定义
	// 3、如果用户提交的CRD定义是非结构化的（NonStructuralSchema），那么就会被K8S打上NonStructuralSchema这个Condition
	nonStructuralSchemaController := nonstructuralschema.NewConditionController(s.Informers.Apiextensions().V1().CustomResourceDefinitions(), crdClient.ApiextensionsV1())
	// 1、用户提交的CRD的名称如果符合*.k8s.io或者*.kubernetes.io，那这个CRD就需要审批
	apiApprovalController := apiapproval.NewKubernetesAPIApprovalPolicyConformantConditionController(s.Informers.Apiextensions().V1().CustomResourceDefinitions(), crdClient.ApiextensionsV1())
	// TODO 处理CRD的删除逻辑
	finalizingController := finalizer.NewCRDFinalizer(
		s.Informers.Apiextensions().V1().CustomResourceDefinitions(),
		crdClient.ApiextensionsV1(),
		crdHandler,
	)

	// 启动Informer
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
				// TODO 这里应该是提供 /openapi/v2 接口的swagger文档
				openapiController := openapicontroller.NewController(s.Informers.Apiextensions().V1().CustomResourceDefinitions())
				go openapiController.Run(s.GenericAPIServer.StaticOpenAPISpec, s.GenericAPIServer.OpenAPIVersionedService, context.StopCh)
			}

			if s.GenericAPIServer.OpenAPIV3VersionedService != nil && utilfeature.DefaultFeatureGate.Enabled(features.OpenAPIV3) {
				// TODO 这里应该是提供 /openapi/v3 接口的swagger文档
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
	// 等待CRD资源同步完成
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
