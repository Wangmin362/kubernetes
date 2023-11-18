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

package admission

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"reflect"
	"sort"
	"strings"
	"sync"

	"k8s.io/klog/v2"
)

// Factory is a function that returns an Interface for admission decisions.
// The config parameter provides an io.Reader handler to the factory in
// order to load specific configurations. If no configuration is provided
// the parameter is nil.
// 通过配置文件实例化准入控制插件
type Factory func(config io.Reader) (Interface, error)

type Plugins struct {
	lock     sync.Mutex
	registry map[string]Factory
}

func NewPlugins() *Plugins {
	return &Plugins{}
}

// All registered admission options.
var (
	// PluginEnabledFn checks whether a plugin is enabled.  By default, if you ask about it, it's enabled.
	PluginEnabledFn = func(name string, config io.Reader) bool {
		return true
	}
)

// PluginEnabledFunc is a function type that can provide an external check on whether an admission plugin may be enabled
type PluginEnabledFunc func(name string, config io.Reader) bool

// Registered enumerates the names of all registered plugins.
func (ps *Plugins) Registered() []string {
	ps.lock.Lock()
	defer ps.lock.Unlock()
	var keys []string
	for k := range ps.registry {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Register registers a plugin Factory by name. This
// is expected to happen during app startup.
func (ps *Plugins) Register(name string, plugin Factory) {
	ps.lock.Lock()
	defer ps.lock.Unlock()
	if ps.registry != nil {
		_, found := ps.registry[name]
		if found {
			klog.Fatalf("Admission plugin %q was registered twice", name)
		}
	} else {
		ps.registry = map[string]Factory{}
	}

	klog.V(1).InfoS("Registered admission plugin", "plugin", name)
	ps.registry[name] = plugin
}

// getPlugin creates an instance of the named plugin.  It returns `false` if
// the name is not known. The error is returned only when the named provider was
// known but failed to initialize.  The config parameter specifies the io.Reader
// handler of the configuration file for the cloud provider, or nil for no configuration.
func (ps *Plugins) getPlugin(name string, config io.Reader) (Interface, bool, error) {
	ps.lock.Lock()
	defer ps.lock.Unlock()
	// 直接冲插件注册中心中获取插件
	f, found := ps.registry[name]
	if !found {
		return nil, false, nil
	}

	config1, config2, err := splitStream(config)
	if err != nil {
		return nil, true, err
	}
	if !PluginEnabledFn(name, config1) {
		return nil, true, nil
	}

	// 通过配置文件实例化准入控制插件
	ret, err := f(config2)
	return ret, true, err
}

// splitStream reads the stream bytes and constructs two copies of it.
func splitStream(config io.Reader) (io.Reader, io.Reader, error) {
	if config == nil || reflect.ValueOf(config).IsNil() {
		return nil, nil, nil
	}

	configBytes, err := ioutil.ReadAll(config)
	if err != nil {
		return nil, nil, err
	}

	return bytes.NewBuffer(configBytes), bytes.NewBuffer(configBytes), nil
}

// NewFromPlugins returns an admission.Interface that will enforce admission control decisions of all
// the given plugins.
func (ps *Plugins) NewFromPlugins(pluginNames []string, configProvider ConfigProvider, pluginInitializer PluginInitializer, decorator Decorator) (Interface, error) {
	var handlers []Interface
	var mutationPlugins []string
	var validationPlugins []string
	// 遍历所有启用的插件
	for _, pluginName := range pluginNames {
		// 获取插件配置
		pluginConfig, err := configProvider.ConfigFor(pluginName)
		if err != nil {
			return nil, err
		}

		// 通过配置文件实例化准入控制插件，然后通过插件初始化器向准入控制插件当中注入依赖信息，最后校验插件是否初始化完成
		plugin, err := ps.InitPlugin(pluginName, pluginConfig, pluginInitializer)
		if err != nil {
			return nil, err
		}
		if plugin != nil {
			if decorator != nil {
				handlers = append(handlers, decorator.Decorate(plugin, pluginName))
			} else {
				handlers = append(handlers, plugin)
			}

			if _, ok := plugin.(MutationInterface); ok {
				mutationPlugins = append(mutationPlugins, pluginName)
			}
			if _, ok := plugin.(ValidationInterface); ok {
				validationPlugins = append(validationPlugins, pluginName)
			}
		}
	}
	if len(mutationPlugins) != 0 {
		klog.Infof("Loaded %d mutating admission controller(s) successfully in the following order: %s.",
			len(mutationPlugins), strings.Join(mutationPlugins, ","))
	}
	if len(validationPlugins) != 0 {
		klog.Infof("Loaded %d validating admission controller(s) successfully in the following order: %s.",
			len(validationPlugins), strings.Join(validationPlugins, ","))
	}
	return newReinvocationHandler(chainAdmissionHandler(handlers)), nil
}

// InitPlugin creates an instance of the named interface.
func (ps *Plugins) InitPlugin(
	name string, // 插件名
	config io.Reader, // 插件配置
	pluginInitializer PluginInitializer, // 插件初始化器
) (Interface, error) {
	if name == "" {
		klog.Info("No admission plugin specified.")
		return nil, nil
	}

	// 根据配置实例化插件
	plugin, found, err := ps.getPlugin(name, config)
	if err != nil {
		return nil, fmt.Errorf("couldn't init admission plugin %q: %v", name, err)
	}
	if !found {
		return nil, fmt.Errorf("unknown admission plugin: %s", name)
	}

	// 初始化插件，向插件注入需要的依赖
	pluginInitializer.Initialize(plugin)
	// ensure that plugins have been properly initialized
	// 校验插件，确保插件已经初始化完成，否则完全没有必要把初始化了一半的准入控制插件返回，因为没有意义，并不能真实使用。
	if err := ValidateInitialization(plugin); err != nil {
		return nil, fmt.Errorf("failed to initialize admission plugin %q: %v", name, err)
	}

	return plugin, nil
}

// ValidateInitialization will call the InitializationValidate function in each plugin if they implement
// the InitializationValidator interface.
func ValidateInitialization(plugin Interface) error {
	if validater, ok := plugin.(InitializationValidator); ok {
		err := validater.ValidateInitialization()
		if err != nil {
			return err
		}
	}
	return nil
}

type PluginInitializers []PluginInitializer

func (pp PluginInitializers) Initialize(plugin Interface) {
	for _, p := range pp {
		p.Initialize(plugin)
	}
}
