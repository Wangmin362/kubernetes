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

// Package app does all of the work necessary to create a Kubernetes
// APIServer by binding together the API, master and APIServer infrastructure.
// It can be configured and called directly or via the hyperkube framework.
package app

import (
	v1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	apiextensionsapiserver "k8s.io/apiextensions-apiserver/pkg/apiserver"
	apiextensionsoptions "k8s.io/apiextensions-apiserver/pkg/cmd/server/options"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/features"
	genericapiserver "k8s.io/apiserver/pkg/server"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/apiserver/pkg/util/webhook"
	kubeexternalinformers "k8s.io/client-go/informers"
	"k8s.io/kubernetes/cmd/kube-apiserver/app/options"
)

func createAPIExtensionsConfig(
	kubeAPIServerConfig genericapiserver.Config, // TODO 注意，这里传递的是一个实体，因此extension server初始化的修改并不会影响外面
	externalInformers kubeexternalinformers.SharedInformerFactory,
	pluginInitializers []admission.PluginInitializer,
	commandOptions *options.ServerRunOptions,
	masterCount int,
	serviceResolver webhook.ServiceResolver,
	authResolverWrapper webhook.AuthenticationInfoResolverWrapper,
) (*apiextensionsapiserver.Config, error) {
	// make a shallow copy to let us twiddle a few things
	// most of the config actually remains the same.  We only need to mess with a couple items related to the particulars of the apiextensions
	// 由于golang对于结构体是值传递的，因此这里实际上是对于kubeAPIServerConfig的一份拷贝
	// generic server config大部分配置都是可以直接使用的，只有少量配置是需要重新设置的
	genericConfig := kubeAPIServerConfig
	// 清空generic server的后置处理器
	genericConfig.PostStartHooks = map[string]genericapiserver.PostStartHookConfigEntry{}
	// TODO 为什么这里后端存储也重置为空
	genericConfig.RESTOptionsGetter = nil

	// override genericConfig.AdmissionControl with apiextensions' scheme,
	// because apiextensions apiserver should use its own scheme to convert resources.
	// 实例化准入控制插件，然后使用准入控制插件初始化器初始化所有的准入控制插件，并把所有的准入控制插件放到一个数组当中，这个数组也实现了
	// Admit、Validation以及Handles接口，换言之，这个准入控制插件数组就是一个一个准入控制插件，只不过在这个数组准入控制插件在执行准入
	// 控制的时候，是挨个遍历内部的准入控制插件实现的
	err := commandOptions.Admission.ApplyTo(
		&genericConfig,
		externalInformers,
		genericConfig.LoopbackClientConfig,
		utilfeature.DefaultFeatureGate,
		pluginInitializers...)
	if err != nil {
		return nil, err
	}

	// copy the etcd options so we don't mutate originals.
	etcdOptions := *commandOptions.Etcd
	// TODO APILISTChunking是啥？
	etcdOptions.StorageConfig.Paging = utilfeature.DefaultFeatureGate.Enabled(features.APIListChunking)
	// this is where the true decodable levels come from.
	// 实例化extension server的编解码器
	etcdOptions.StorageConfig.Codec = apiextensionsapiserver.Codecs.LegacyCodec(v1beta1.SchemeGroupVersion, v1.SchemeGroupVersion)
	// prefer the more compact serialization (v1beta1) for storage until http://issue.k8s.io/82292 is resolved for objects whose v1 serialization is too big but whose v1beta1 serialization can be stored
	// TODO MultiGroupVersioner是干嘛的？
	etcdOptions.StorageConfig.EncodeVersioner = runtime.NewMultiGroupVersioner(v1beta1.SchemeGroupVersion, schema.GroupKind{Group: v1beta1.GroupName})
	// TODO 重新设置K8S后端存储
	genericConfig.RESTOptionsGetter = &genericoptions.SimpleRestOptionsFactory{Options: etcdOptions}

	// override MergedResourceConfig with apiextensions defaults and registry
	// 设置启用或者禁用的K8S资源
	if err := commandOptions.APIEnablement.ApplyTo(
		&genericConfig,
		apiextensionsapiserver.DefaultAPIResourceConfigSource(),
		apiextensionsapiserver.Scheme); err != nil {
		return nil, err
	}

	apiextensionsConfig := &apiextensionsapiserver.Config{
		GenericConfig: &genericapiserver.RecommendedConfig{
			Config:                genericConfig,
			SharedInformerFactory: externalInformers,
		},
		ExtraConfig: apiextensionsapiserver.ExtraConfig{
			CRDRESTOptionsGetter: apiextensionsoptions.NewCRDRESTOptionsGetter(etcdOptions),
			MasterCount:          masterCount, // master节点数量
			// AuthenticationInfoResolverWrapper主要是给AuthenticationInfoResolver增加了两种功能：
			// 1、为AuthenticationInfoResolver增加了APIServerTracing功能
			// 2、为AuthenticationInfoResolver增加了EgressSelector
			AuthResolverWrapper: authResolverWrapper,
			ServiceResolver:     serviceResolver, // 根据服务的name,namespace,port解析为正确的服务访问地址
		},
	}

	// we need to clear the poststarthooks so we don't add them multiple times to all the servers (that fails)
	// TODO 为什么这里需要清空后置处理器？
	apiextensionsConfig.GenericConfig.PostStartHooks = map[string]genericapiserver.PostStartHookConfigEntry{}

	return apiextensionsConfig, nil
}

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
func createAPIExtensionsServer(apiextensionsConfig *apiextensionsapiserver.Config, delegateAPIServer genericapiserver.DelegationTarget) (*apiextensionsapiserver.CustomResourceDefinitions, error) {
	return apiextensionsConfig.Complete().New(delegateAPIServer)
}
