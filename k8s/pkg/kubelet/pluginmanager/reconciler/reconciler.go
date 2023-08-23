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

// Package reconciler implements interfaces that attempt to reconcile the
// desired state of the world with the actual state of the world by triggering
// relevant actions (register/deregister plugins).
package reconciler

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/operationexecutor"
	"k8s.io/kubernetes/pkg/util/goroutinemap"
	"k8s.io/kubernetes/pkg/util/goroutinemap/exponentialbackoff"
)

// Reconciler runs a periodic loop to reconcile the desired state of the world
// with the actual state of the world by triggering register and unregister
// operations. Also provides a means to add a handler for a plugin type.
type Reconciler interface {
	// Run starts running the reconciliation loop which executes periodically,
	// checks if plugins are correctly registered or unregistered.
	// If not, it will trigger register/unregister operations to rectify.
	// 对比期望状态缓存和实际状态缓存，如果实际状态缓存中不存在某个插件，或者个插件在实际缓存和期望缓存的时间不等，就需要重新注册插件
	Run(stopCh <-chan struct{})

	// AddHandler adds the given plugin handler for a specific plugin type,
	// which will be added to the actual state of world cache.
	// 1、这里的pluginType实际上只有DevicePlugin, DRAPlugin, CSIPlugin
	// 2、而插件的Handler，实际上就是不同类型插件的注册、注销回调方法
	AddHandler(pluginType string, pluginHandler cache.PluginHandler)
}

// NewReconciler returns a new instance of Reconciler.
//
// operationExecutor - used to trigger register/unregister operations safely
// (prevents more than one operation from being triggered on the same
// socket path)
//
// loopSleepDuration - the amount of time the reconciler loop sleeps between
// successive executions
//
// desiredStateOfWorld - cache containing the desired state of the world
//
// actualStateOfWorld - cache containing the actual state of the world
func NewReconciler(
	operationExecutor operationexecutor.OperationExecutor,
	loopSleepDuration time.Duration,
	desiredStateOfWorld cache.DesiredStateOfWorld,
	actualStateOfWorld cache.ActualStateOfWorld) Reconciler {
	return &reconciler{
		operationExecutor:   operationExecutor,
		loopSleepDuration:   loopSleepDuration,
		desiredStateOfWorld: desiredStateOfWorld,
		actualStateOfWorld:  actualStateOfWorld,
		handlers:            make(map[string]cache.PluginHandler),
	}
}

type reconciler struct {
	operationExecutor   operationexecutor.OperationExecutor
	loopSleepDuration   time.Duration
	desiredStateOfWorld cache.DesiredStateOfWorld
	actualStateOfWorld  cache.ActualStateOfWorld
	handlers            map[string]cache.PluginHandler
	sync.RWMutex
}

var _ Reconciler = &reconciler{}

func (rc *reconciler) Run(stopCh <-chan struct{}) {
	wait.Until(func() {
		rc.reconcile()
	},
		rc.loopSleepDuration, // 一秒钟执行一次reconcile
		stopCh)
}

func (rc *reconciler) AddHandler(pluginType string, pluginHandler cache.PluginHandler) {
	rc.Lock()
	defer rc.Unlock()

	rc.handlers[pluginType] = pluginHandler
}

func (rc *reconciler) getHandlers() map[string]cache.PluginHandler {
	rc.RLock()
	defer rc.RUnlock()

	var copyHandlers = make(map[string]cache.PluginHandler)
	for pluginType, handler := range rc.handlers {
		copyHandlers[pluginType] = handler
	}
	return copyHandlers
}

func (rc *reconciler) reconcile() {
	// Unregisterations are triggered before registrations

	// Ensure plugins that should be unregistered are unregistered.
	// 从实际状态缓存中获取value
	for _, registeredPlugin := range rc.actualStateOfWorld.GetRegisteredPlugins() {
		// 默认不注销kubelet插件
		unregisterPlugin := false
		// 判断期望缓存中是否还存在
		if !rc.desiredStateOfWorld.PluginExists(registeredPlugin.SocketPath) {
			// 如果不存在了，说明当前缓存需要注销
			unregisterPlugin = true
		} else {
			// We also need to unregister the plugins that exist in both actual state of world
			// and desired state of world cache, but the timestamps don't match.
			// Iterate through desired state of world plugins and see if there's any plugin
			// with the same socket path but different timestamp.
			// 如果实际状态缓存和期望状态缓存的时间不一样，那么需要注销此插件，因为很有可能是被删除了重新创建的，所以需要注销插件，让插件自己
			// 重新注册
			for _, dswPlugin := range rc.desiredStateOfWorld.GetPluginsToRegister() {
				if dswPlugin.SocketPath == registeredPlugin.SocketPath && dswPlugin.Timestamp != registeredPlugin.Timestamp {
					klog.V(5).InfoS("An updated version of plugin has been found, unregistering the plugin first before reregistering", "plugin", registeredPlugin)
					unregisterPlugin = true
					break
				}
			}
		}

		// 如果当前插件需要注销，所谓的注销插件，其实就是执行插件的DeRegisterPlugin方法，同时从实际状态缓存当中删除
		if unregisterPlugin {
			klog.V(5).InfoS("Starting operationExecutor.UnregisterPlugin", "plugin", registeredPlugin)
			err := rc.operationExecutor.UnregisterPlugin(registeredPlugin, rc.actualStateOfWorld)
			if err != nil &&
				!goroutinemap.IsAlreadyExists(err) &&
				!exponentialbackoff.IsExponentialBackoff(err) {
				// Ignore goroutinemap.IsAlreadyExists and exponentialbackoff.IsExponentialBackoff errors, they are expected.
				// Log all other errors.
				klog.ErrorS(err, "OperationExecutor.UnregisterPlugin failed", "plugin", registeredPlugin)
			}
			if err == nil {
				klog.V(1).InfoS("OperationExecutor.UnregisterPlugin started", "plugin", registeredPlugin)
			}
		}
	}

	// Ensure plugins that should be registered are registered
	// 遍历期望状态缓存
	for _, pluginToRegister := range rc.desiredStateOfWorld.GetPluginsToRegister() {
		// 如果实际缓存中不存在此插件，或者时间和期望状态的缓存时间不等，就需要重新进行注册
		if !rc.actualStateOfWorld.PluginExistsWithCorrectTimestamp(pluginToRegister) {
			klog.V(5).InfoS("Starting operationExecutor.RegisterPlugin", "plugin", pluginToRegister)
			// 注册插件
			err := rc.operationExecutor.RegisterPlugin(pluginToRegister.SocketPath, pluginToRegister.Timestamp, rc.getHandlers(), rc.actualStateOfWorld)
			if err != nil &&
				!goroutinemap.IsAlreadyExists(err) &&
				!exponentialbackoff.IsExponentialBackoff(err) {
				// Ignore goroutinemap.IsAlreadyExists and exponentialbackoff.IsExponentialBackoff errors, they are expected.
				klog.ErrorS(err, "OperationExecutor.RegisterPlugin failed", "plugin", pluginToRegister)
			}
			if err == nil {
				klog.V(1).InfoS("OperationExecutor.RegisterPlugin started", "plugin", pluginToRegister)
			}
		}
	}
}
