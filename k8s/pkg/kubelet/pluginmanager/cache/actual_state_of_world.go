/*
Copyright 2019 The Kubernetes Authors.

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

/*
Package cache implements data structures used by the kubelet plugin manager to
keep track of registered plugins.
*/
package cache

import (
	"fmt"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// ActualStateOfWorld defines a set of thread-safe operations for the kubelet
// plugin manager's actual state of the world cache.
// This cache contains a map of socket file path to plugin information of
// all plugins attached to this node.
type ActualStateOfWorld interface {

	// GetRegisteredPlugins generates and returns a list of plugins
	// that are successfully registered plugins in the current actual state of world.
	// 获取当前插件管理器所有注册插件的信息
	GetRegisteredPlugins() []PluginInfo

	// AddPlugin add the given plugin in the cache.
	// An error will be returned if socketPath of the PluginInfo object is empty.
	// Note that this is different from desired world cache's AddOrUpdatePlugin
	// because for the actual state of world cache, there won't be a scenario where
	// we need to update an existing plugin if the timestamps don't match. This is
	// because the plugin should have been unregistered in the reconciler and therefore
	// removed from the actual state of world cache first before adding it back into
	// the actual state of world cache again with the new timestamp
	AddPlugin(pluginInfo PluginInfo) error

	// RemovePlugin deletes the plugin with the given socket path from the actual
	// state of world.
	// If a plugin does not exist with the given socket path, this is a no-op.
	RemovePlugin(socketPath string)

	// PluginExistsWithCorrectTimestamp PluginExists checks if the given plugin exists in the current actual
	// state of world cache with the correct timestamp
	// 只有当查询存在且插件的时间相等时才认为是正确的
	PluginExistsWithCorrectTimestamp(pluginInfo PluginInfo) bool
}

// NewActualStateOfWorld returns a new instance of ActualStateOfWorld
func NewActualStateOfWorld() ActualStateOfWorld {
	return &actualStateOfWorld{
		socketFileToInfo: make(map[string]PluginInfo),
	}
}

type actualStateOfWorld struct {

	// socketFileToInfo is a map containing the set of successfully registered plugins
	// The keys are plugin socket file paths. The values are PluginInfo objects
	// key为插件的注册信息，主要包含了插件的socket监听路径以及回调
	socketFileToInfo map[string]PluginInfo
	sync.RWMutex
}

var _ ActualStateOfWorld = &actualStateOfWorld{}

// PluginInfo holds information of a plugin
// 1、kubelet插件机制，主要是通过Unix Domain Socket进行通信，因此kubelet需要直到注册插件的socket路径
// 2、在kubelet插件机制这种模型下，kubelet是插件的客户端，而kubelet插件则是服务端
type PluginInfo struct {
	// 1、kubelet插件注册socket监听文件路径，所谓注册socket，其实就是实现了插件注册接口的socket路径。之所以要这么说，是因为插件的核心能力
	// 是通过不同的插件接口实现的，不同类型的插件需要实现不同的插件服务接口。譬如DevicePlugin类型的插件需要实现设备插件相关的接口，而对于
	// CSI类型的插件，需要实现CSI Spec的NodeService接口。对于DRA类型的插件是需要实现动态资源分配接口。
	// 2、想要实现kubelet插件就需要实现这两类接口，一类是通用的注册接口，一类是不同类型的服务接口。一般来说，插件实现方会分开实现这两类接口，因此
	// 会监听两个Unix Domain Socket，而这里的SocketPath就是注册Socket。之所以没有服务Socket，是因为服务Socket可以通过注册Socket获取到。
	// 另外，插件实现方实际上是可以通过一个服务同时实现注册接口以及服务接口，这样就可以只暴露一个socket文件。
	SocketPath string
	Timestamp  time.Time     // socket文件被PluginManager发现的时间
	Handler    PluginHandler // 不同类型的插件回调函数，PluginManager在发现插件的注册socket之后会获取插件类型的插件回调函数
	Name       string        // 插件名字
}

func (asw *actualStateOfWorld) AddPlugin(pluginInfo PluginInfo) error {
	asw.Lock()
	defer asw.Unlock()

	if pluginInfo.SocketPath == "" {
		return fmt.Errorf("socket path is empty")
	}
	if _, ok := asw.socketFileToInfo[pluginInfo.SocketPath]; ok {
		// 说明这个插件之前已经注册过，此时将会覆盖此插件
		klog.V(2).InfoS("Plugin exists in actual state cache", "path", pluginInfo.SocketPath)
	}
	asw.socketFileToInfo[pluginInfo.SocketPath] = pluginInfo
	return nil
}

func (asw *actualStateOfWorld) RemovePlugin(socketPath string) {
	asw.Lock()
	defer asw.Unlock()

	delete(asw.socketFileToInfo, socketPath)
}

func (asw *actualStateOfWorld) GetRegisteredPlugins() []PluginInfo {
	asw.RLock()
	defer asw.RUnlock()

	var currentPlugins []PluginInfo
	for _, pluginInfo := range asw.socketFileToInfo {
		currentPlugins = append(currentPlugins, pluginInfo)
	}
	return currentPlugins
}

// PluginExistsWithCorrectTimestamp
// 只有当查询存在且插件的时间相等时才认为是正确的
func (asw *actualStateOfWorld) PluginExistsWithCorrectTimestamp(pluginInfo PluginInfo) bool {
	asw.RLock()
	defer asw.RUnlock()

	// We need to check both if the socket file path exists, and the timestamp
	// matches the given plugin (from the desired state cache) timestamp
	actualStatePlugin, exists := asw.socketFileToInfo[pluginInfo.SocketPath]
	return exists && (actualStatePlugin.Timestamp == pluginInfo.Timestamp)
}
