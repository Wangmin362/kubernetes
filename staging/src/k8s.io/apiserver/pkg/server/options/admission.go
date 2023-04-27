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

package options

import (
	"fmt"
	"strings"

	"github.com/spf13/pflag"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/initializer"
	admissionmetrics "k8s.io/apiserver/pkg/admission/metrics"
	"k8s.io/apiserver/pkg/admission/plugin/namespace/lifecycle"
	mutatingwebhook "k8s.io/apiserver/pkg/admission/plugin/webhook/mutating"
	validatingwebhook "k8s.io/apiserver/pkg/admission/plugin/webhook/validating"
	apiserverapi "k8s.io/apiserver/pkg/apis/apiserver"
	apiserverapiv1 "k8s.io/apiserver/pkg/apis/apiserver/v1"
	apiserverapiv1alpha1 "k8s.io/apiserver/pkg/apis/apiserver/v1alpha1"
	"k8s.io/apiserver/pkg/server"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/component-base/featuregate"
)

// 注册不同版本的AdmissionConfiguration，EgressSelectorConfiguration资源
var configScheme = runtime.NewScheme()

func init() {
	utilruntime.Must(apiserverapi.AddToScheme(configScheme))
	utilruntime.Must(apiserverapiv1alpha1.AddToScheme(configScheme))
	utilruntime.Must(apiserverapiv1.AddToScheme(configScheme))
}

// AdmissionOptions holds the admission options
type AdmissionOptions struct {
	// RecommendedPluginOrder holds an ordered list of plugin names we recommend to use by default
	// TODO 这玩意有啥用？ 再New
	RecommendedPluginOrder []string
	// DefaultOffPlugins is a set of plugin names that is disabled by default
	// 设置默认禁用的插件
	DefaultOffPlugins sets.String

	// EnablePlugins indicates plugins to be enabled passed through `--enable-admission-plugins`.
	// 启用的准入控制插件
	EnablePlugins []string
	// DisablePlugins indicates plugins to be disabled passed through `--disable-admission-plugins`.
	// 禁用的注入控制插件
	DisablePlugins []string
	// ConfigFile is the file path with admission control configuration.
	// 准入控制的配置文件是啥？ 答：每个准入控制插件是可以增加配置文件的
	// 配置文件其实就是AdmissionPluginConfiguration资源，这个配置文件中可以指定不同的准入控制插件所需要的配置
	ConfigFile string
	// Plugins contains all registered plugins.
	// 所有注册的准入控制插件
	Plugins *admission.Plugins
	// Decorators is a list of admission decorator to wrap around the admission plugins
	// TODO 从代码上来看似乎目前是为了Metrics指标
	Decorators admission.Decorators
}

// NewAdmissionOptions creates a new instance of AdmissionOptions
// Note:
//
//	In addition it calls RegisterAllAdmissionPlugins to register
//	all generic admission plugins.
//
//	Provides the list of RecommendedPluginOrder that holds sane values
//	that can be used by servers that don't care about admission chain.
//	Servers that do care can overwrite/append that field after creation.
func NewAdmissionOptions() *AdmissionOptions {
	options := &AdmissionOptions{
		Plugins:    admission.NewPlugins(),
		Decorators: admission.Decorators{admission.DecoratorFunc(admissionmetrics.WithControllerMetrics)},
		// This list is mix of mutating admission plugins and validating
		// admission plugins. The apiserver always runs the validating ones
		// after all the mutating ones, so their relative order in this list
		// doesn't matter.
		RecommendedPluginOrder: []string{lifecycle.PluginName, mutatingwebhook.PluginName, validatingwebhook.PluginName},
		DefaultOffPlugins:      sets.NewString(), // 目前没有默认禁用的插件
	}
	// 注册NamespaceLifecycle, ValidatingAdmissionWebhook,MutatingAdmissionWebhook webhook
	server.RegisterAllAdmissionPlugins(options.Plugins)
	return options
}

// AddFlags adds flags related to admission for a specific APIServer to the specified FlagSet
func (a *AdmissionOptions) AddFlags(fs *pflag.FlagSet) {
	if a == nil {
		return
	}

	fs.StringSliceVar(&a.EnablePlugins, "enable-admission-plugins", a.EnablePlugins, ""+
		"admission plugins that should be enabled in addition to default enabled ones ("+
		strings.Join(a.defaultEnabledPluginNames(), ", ")+"). "+
		"Comma-delimited list of admission plugins: "+strings.Join(a.Plugins.Registered(), ", ")+". "+
		"The order of plugins in this flag does not matter.")
	fs.StringSliceVar(&a.DisablePlugins, "disable-admission-plugins", a.DisablePlugins, ""+
		"admission plugins that should be disabled although they are in the default enabled plugins list ("+
		strings.Join(a.defaultEnabledPluginNames(), ", ")+"). "+
		"Comma-delimited list of admission plugins: "+strings.Join(a.Plugins.Registered(), ", ")+". "+
		"The order of plugins in this flag does not matter.")
	fs.StringVar(&a.ConfigFile, "admission-control-config-file", a.ConfigFile,
		"File with admission control configuration.")
}

// ApplyTo adds the admission chain to the server configuration.
// In case admission plugin names were not provided by a cluster-admin they will be prepared from the recommended/default values.
// In addition the method lazily initializes a generic plugin that is appended to the list of pluginInitializers
// note this method uses:
//
//	genericconfig.Authorizer
func (a *AdmissionOptions) ApplyTo(
	c *server.Config,
	informers informers.SharedInformerFactory,
	kubeAPIServerClientConfig *rest.Config,
	features featuregate.FeatureGate,
	pluginInitializers ...admission.PluginInitializer,
) error {
	if a == nil {
		return nil
	}

	// Admission depends on CoreAPI to set SharedInformerFactory and ClientConfig.
	if informers == nil {
		return fmt.Errorf("admission depends on a Kubernetes core API shared informer, it cannot be nil")
	}

	// 拿到RecommendedPluginOrder中没有禁用的插件，也就是拿到启用插件
	pluginNames := a.enabledPluginNames()

	// configScheme是为了能够从GVK找到goType，或者是从goType找到GVK
	// ConfigFile是AdmissionConfiguration资源，其中定义了不同准入控制插件的配置
	// pluginsConfigProvider本质上就是AdmissionConfiguration资源，其作用就是根据插件的名字返回插件配置
	pluginsConfigProvider, err := admission.ReadAdmissionConfiguration(pluginNames, a.ConfigFile, configScheme)
	if err != nil {
		return fmt.Errorf("failed to read plugin config: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(kubeAPIServerClientConfig)
	if err != nil {
		return err
	}
	// 通用的准入控制插件初始化器 实际上就是为了给准入控制插件设置client, informer, authorizer, feature这几个参数
	genericInitializer := initializer.New(clientset, informers, c.Authorization.Authorizer, features, c.DrainedNotify())
	initializersChain := admission.PluginInitializers{genericInitializer}
	initializersChain = append(initializersChain, pluginInitializers...)

	// 准入控制插件链，实际上就是按照顺序一个一个执行，一旦有一个准入控制插件决绝，那么就退出
	// TODO 最终请求到来的时候就是通过这个准入控制链执行的
	admissionChain, err := a.Plugins.NewFromPlugins(pluginNames, pluginsConfigProvider, initializersChain, a.Decorators)
	if err != nil {
		return err
	}

	// 加上指标
	c.AdmissionControl = admissionmetrics.WithStepMetrics(admissionChain)
	return nil
}

// Validate verifies flags passed to AdmissionOptions.
func (a *AdmissionOptions) Validate() []error {
	if a == nil {
		return nil
	}

	errs := []error{}

	registeredPlugins := sets.NewString(a.Plugins.Registered()...)
	for _, name := range a.EnablePlugins {
		if !registeredPlugins.Has(name) {
			errs = append(errs, fmt.Errorf("enable-admission-plugins plugin %q is unknown", name))
		}
	}

	for _, name := range a.DisablePlugins {
		if !registeredPlugins.Has(name) {
			errs = append(errs, fmt.Errorf("disable-admission-plugins plugin %q is unknown", name))
		}
	}

	enablePlugins := sets.NewString(a.EnablePlugins...)
	disablePlugins := sets.NewString(a.DisablePlugins...)
	if len(enablePlugins.Intersection(disablePlugins).List()) > 0 {
		errs = append(errs, fmt.Errorf("%v in enable-admission-plugins and disable-admission-plugins "+
			"overlapped", enablePlugins.Intersection(disablePlugins).List()))
	}

	// Verify RecommendedPluginOrder.
	recommendPlugins := sets.NewString(a.RecommendedPluginOrder...)
	intersections := registeredPlugins.Intersection(recommendPlugins)
	if !intersections.Equal(recommendPlugins) {
		// Developer error, this should never run in.
		errs = append(errs, fmt.Errorf("plugins %v in RecommendedPluginOrder are not registered",
			recommendPlugins.Difference(intersections).List()))
	}
	if !intersections.Equal(registeredPlugins) {
		// Developer error, this should never run in.
		errs = append(errs, fmt.Errorf("plugins %v registered are not in RecommendedPluginOrder",
			registeredPlugins.Difference(intersections).List()))
	}

	return errs
}

// enabledPluginNames makes use of RecommendedPluginOrder, DefaultOffPlugins,
// EnablePlugins, DisablePlugins fields
// to prepare a list of ordered plugin names that are enabled.
// 用于返回RecommendedPluginOrder插件中没有被禁用的插件
func (a *AdmissionOptions) enabledPluginNames() []string {
	// 获取到所有禁用的插件，一个来自于默认禁用的插件，一个来自于命令行禁用的插件
	allOffPlugins := append(a.DefaultOffPlugins.List(), a.DisablePlugins...)
	disabledPlugins := sets.NewString(allOffPlugins...) // 去重
	enabledPlugins := sets.NewString(a.EnablePlugins...)
	// 禁用插件和启用插件的差集，即返回那些在禁用插件中存在，但是不再启用插件中存在的插件；说白了就是计算出需要禁用的插件
	disabledPlugins = disabledPlugins.Difference(enabledPlugins)

	var orderedPlugins []string
	for _, plugin := range a.RecommendedPluginOrder {
		if !disabledPlugins.Has(plugin) { // 从这里可以看出，RecommendedPluginOrder插件也是可以禁用的
			orderedPlugins = append(orderedPlugins, plugin)
		}
	}

	// 返回RecommendedPluginOrder插件中没有被禁用的插件
	return orderedPlugins
}

// Return names of plugins which are enabled by default
func (a *AdmissionOptions) defaultEnabledPluginNames() []string {
	defaultOnPluginNames := []string{}
	for _, pluginName := range a.RecommendedPluginOrder {
		if !a.DefaultOffPlugins.Has(pluginName) {
			defaultOnPluginNames = append(defaultOnPluginNames, pluginName)
		}
	}

	return defaultOnPluginNames
}
