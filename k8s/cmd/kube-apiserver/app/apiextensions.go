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
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/apiserver/pkg/util/webhook"
	kubeexternalinformers "k8s.io/client-go/informers"
	"k8s.io/kubernetes/cmd/kube-apiserver/app/options"
)

func createAPIExtensionsConfig(
	kubeAPIServerConfig genericapiserver.Config, // GenericServer配置，这里传递的时APIServer通用配置的一个拷贝
	externalInformers kubeexternalinformers.SharedInformerFactory, // SharedInformerFactory，用于缓存各种资源
	pluginInitializers []admission.PluginInitializer,
	commandOptions *options.ServerRunOptions, // 启动KubeAPIServer时传入的启动参数
	masterCount int, // Master节点数量
	serviceResolver webhook.ServiceResolver, // 服务解析器，用于把Service解析为一个合法的URL
	authResolverWrapper webhook.AuthenticationInfoResolverWrapper, // TODO Webhook认证包装器
) (*apiextensionsapiserver.Config, error) {
	// make a shallow copy to let us twiddle a few things
	// most of the config actually remains the same.  We only need to mess with a couple items related to the particulars of the apiextensions
	genericConfig := kubeAPIServerConfig
	// 1、先清空GenericServer配置的PostStartHooks，因为APIServer已经初始化了自己的PostStartHooks, ExtensionServer并不需要APIServer
	// 的PostStartHook，因此需要先清空，然后再初始化自己的PostStartHook
	genericConfig.PostStartHooks = map[string]genericapiserver.PostStartHookConfigEntry{}
	// GenericServer的RESTOptionsGetter是给APIServer用的，所以ExtensionServer需要清空初始化自己的
	genericConfig.RESTOptionsGetter = nil

	// copy the etcd options so we don't mutate originals.
	// we assume that the etcd options have been completed already.  avoid messing with anything outside
	// of changes to StorageConfig as that may lead to unexpected behavior when the options are applied.
	etcdOptions := *commandOptions.Etcd
	etcdOptions.StorageConfig.Paging = utilfeature.DefaultFeatureGate.Enabled(features.APIListChunking)
	// this is where the true decodable levels come from.
	etcdOptions.StorageConfig.Codec = apiextensionsapiserver.Codecs.LegacyCodec(v1beta1.SchemeGroupVersion, v1.SchemeGroupVersion)
	// TODO 为什没有V1版本
	// prefer the more compact serialization (v1beta1) for storage until https://issue.k8s.io/82292 is resolved for objects whose v1 serialization is too big but whose v1beta1 serialization can be stored
	etcdOptions.StorageConfig.EncodeVersioner = runtime.NewMultiGroupVersioner(v1beta1.SchemeGroupVersion, schema.GroupKind{Group: v1beta1.GroupName})
	etcdOptions.SkipHealthEndpoints = true // avoid double wiring of health checks
	// 初始化RESTOptionsGetter
	if err := etcdOptions.ApplyTo(&genericConfig); err != nil {
		return nil, err
	}

	// override MergedResourceConfig with apiextensions defaults and registry
	// 初始化GenericConfig配置的MergedResourceConfig属性，通过MergedResourceConfig属性我们可以知道这个Server启用/禁用了哪些资源
	if err := commandOptions.APIEnablement.ApplyTo(
		&genericConfig, // GenericServer配置
		// 启用apiextensions.k8s.io/v1以及apiextensions.k8s.io/v1beta1资源，这个组下的资源只有CRD资源，没有其它资源
		apiextensionsapiserver.DefaultAPIResourceConfigSource(),
		apiextensionsapiserver.Scheme,
	); err != nil {
		return nil, err
	}
	// TODO 分析这里到底是如何存储的
	crdRESTOptionsGetter, err := apiextensionsoptions.NewCRDRESTOptionsGetter(etcdOptions)
	if err != nil {
		return nil, err
	}
	// 实例化ExtensionServer的配置
	apiextensionsConfig := &apiextensionsapiserver.Config{
		GenericConfig: &genericapiserver.RecommendedConfig{
			Config:                genericConfig,
			SharedInformerFactory: externalInformers,
		},
		ExtraConfig: apiextensionsapiserver.ExtraConfig{
			CRDRESTOptionsGetter: crdRESTOptionsGetter,
			MasterCount:          masterCount,
			AuthResolverWrapper:  authResolverWrapper,
			ServiceResolver:      serviceResolver,
		},
	}

	// we need to clear the poststarthooks so we don't add them multiple times to all the servers (that fails)
	// 清空GenericServer携带的后置处理器，否则会多次添加
	apiextensionsConfig.GenericConfig.PostStartHooks = map[string]genericapiserver.PostStartHookConfigEntry{}

	return apiextensionsConfig, nil
}

// ExtensionServer本质上就是一个GenericServer
func createAPIExtensionsServer(
	apiextensionsConfig *apiextensionsapiserver.Config,
	delegateAPIServer genericapiserver.DelegationTarget,
) (*apiextensionsapiserver.CustomResourceDefinitions, error) {
	// Complete用于补全ExtensionServer的配置
	// New用于实例化一个ExtensionServer
	return apiextensionsConfig.Complete().New(delegateAPIServer)
}
