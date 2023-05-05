/*
Copyright 2018 The Kubernetes Authors.

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

package admission

import (
	"io/ioutil"
	"net/http"
	"time"

	"k8s.io/klog/v2"

	"go.opentelemetry.io/otel/trace"

	utilwait "k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/admission"
	webhookinit "k8s.io/apiserver/pkg/admission/plugin/webhook/initializer"
	genericapiserver "k8s.io/apiserver/pkg/server"
	egressselector "k8s.io/apiserver/pkg/server/egressselector"
	"k8s.io/apiserver/pkg/util/webhook"
	cacheddiscovery "k8s.io/client-go/discovery/cached/memory"
	externalinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	quotainstall "k8s.io/kubernetes/pkg/quota/v1/install"
)

// Config holds the configuration needed to for initialize the admission plugins
type Config struct {
	// TODO The path to the cloud provider configuration file
	CloudConfigFile      string
	LoopbackClientConfig *rest.Config
	ExternalInformers    externalinformers.SharedInformerFactory
}

// New sets up the plugins and admission start hooks needed for admission
func (c *Config) New(proxyTransport *http.Transport, egressSelector *egressselector.EgressSelector, serviceResolver webhook.ServiceResolver,
	tp trace.TracerProvider) ([]admission.PluginInitializer, genericapiserver.PostStartHookFunc, error) {
	// DefaultAuthenticationInfoResolverWrapper主要是为AuthenticationInfoResolver增加了两个重要的功能，分别是：
	// 一、为AuthenticationInfoResolver增加了APIServerTracing功能
	// 二、为AuthenticationInfoResolver增加了EgressSelector功能
	webhookAuthResolverWrapper := webhook.NewDefaultAuthenticationInfoResolverWrapper(proxyTransport, egressSelector, c.LoopbackClientConfig, tp)
	// webhookPluginInitializer的作用非常简单，就是为符合条件的准入控制插件注入webhookAuthResolverWrapper以及serviceResolver属性
	webhookPluginInitializer := webhookinit.NewPluginInitializer(webhookAuthResolverWrapper, serviceResolver)

	var cloudConfig []byte
	if c.CloudConfigFile != "" {
		var err error
		// cloudConfig The path to the cloud provider configuration file
		cloudConfig, err = ioutil.ReadFile(c.CloudConfigFile)
		if err != nil {
			klog.Fatalf("Error reading from cloud configuration file %s: %#v", c.CloudConfigFile, err)
		}
	}
	clientset, err := kubernetes.NewForConfig(c.LoopbackClientConfig)
	if err != nil {
		return nil, nil, err
	}

	// TODO 分析discoveryClient
	discoveryClient := cacheddiscovery.NewMemCacheClient(clientset.Discovery())
	// RESTMapper 根据GVR找到对应的GVK
	// TODO 分析discoveryRESTMapper
	discoveryRESTMapper := restmapper.NewDeferredDiscoveryRESTMapper(discoveryClient)
	// kubePluginInitializer用于给符合条件的注入控制插件注入CloudConfig, RestMapper, QuotaConfiguration属性
	kubePluginInitializer := NewPluginInitializer(
		cloudConfig,
		discoveryRESTMapper,
		quotainstall.NewQuotaConfigurationForAdmission(),
	)

	// TODO 准入控制后置处理器
	admissionPostStartHook := func(context genericapiserver.PostStartHookContext) error {
		// TODO 详细分析
		discoveryRESTMapper.Reset()
		// TODO 详细分析
		go utilwait.Until(discoveryRESTMapper.Reset, 30*time.Second, context.StopCh)
		return nil
	}

	// 可以看到，准入控制插件初始化器有两个，分别是：webhookPluginInitializer、kubePluginInitializer
	// webhookPluginInitializer主要是为符合条件的准入控制插件注入webhookAuthResolverWrapper以及serviceResolver属性
	// kubePluginInitializer用于给符合条件的注入控制插件注入CloudConfig, RestMapper, QuotaConfiguration属性
	return []admission.PluginInitializer{webhookPluginInitializer, kubePluginInitializer}, admissionPostStartHook, nil
}
