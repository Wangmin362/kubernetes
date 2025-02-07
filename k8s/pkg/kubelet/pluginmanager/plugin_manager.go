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

package pluginmanager

import (
	"time"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/config"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/metrics"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/operationexecutor"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/pluginwatcher"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/reconciler"
)

/*
root@k8s-master1:/var/lib/kubelet# tree plugins_registry plugins device-plugins/
plugins_registry
└── nfs.csi.k8s.io-reg.sock
plugins
└── csi-nfsplugin
    └── csi.sock
device-plugins/
└── kubelet.sock
*/

// PluginManager runs a set of asynchronous loops that figure out which plugins
// need to be registered/deregistered and makes it so.
// 1、监听/var/lib/kubelet/plugins_registry目录，维护插件期望状态缓存。如果有socket文件新增，就把这个插件添加到期望缓存中。
// 同时，如果有socket文件被删除，PlugingManager也会把此插件从期望状态缓存中删除
// 2、对比期望状态缓存和实际状态缓存的插件，如果实际状态缓存不存在此插件，就说明此插件还没有初始化，那么调用这个插件的Register函数进行注册。
// 另外，如果这个插件在期望状态缓存和实际状态缓存中存在，但是两个缓存的时间不一致，就会把这个插件从实际状态缓存中注销。
type PluginManager interface {
	// Run Starts the plugin manager and all the asynchronous loops that it controls
	// 1、PluginManager需要关系哪些资源就绪？ 实际上并不需要等待任何资源
	Run(sourcesReady config.SourcesReady, stopCh <-chan struct{})

	// AddHandler adds the given plugin handler for a specific plugin type, which
	// will be added to the actual state of world cache so that it can be passed to
	// the desired state of world cache in order to be used during plugin
	// registration/deregistration
	// 1、pluginType有CSIPlugin, DevicePlugin, DRAPlugin
	// TODO 1、pluginHandler是插件回调，每个插件的回调需要完成什么事情？
	AddHandler(pluginType string, pluginHandler cache.PluginHandler)
}

const (
	// loopSleepDuration is the amount of time the reconciler loop waits
	// between successive executions
	loopSleepDuration = 1 * time.Second
)

// NewPluginManager returns a new concrete instance implementing the
// PluginManager interface.
func NewPluginManager(
	sockDir string, // 默认为：/var/lib/kubelet/plugins_registry
	recorder record.EventRecorder, // 事件记录器
) PluginManager {
	// 插件的实际状态
	asw := cache.NewActualStateOfWorld()
	// 插件的真实状态
	dsw := cache.NewDesiredStateOfWorld()
	// 1、Reconciler用于完成插件真正的注册、注销动作。主要就是通过对比DesiredStateOfWorld以及ActualStateOfWorld缓存，从而判断插件需要
	// 注册还是注销
	reconciler := reconciler.NewReconciler(
		operationexecutor.NewOperationExecutor(
			operationexecutor.NewOperationGenerator(
				recorder,
			),
		),
		loopSleepDuration, // 每隔一秒钟循环一次
		dsw,
		asw,
	)

	pm := &pluginManager{
		// 1、遍历/var/lib/kubelet/plugins_registry目录,把/var/lib/kubelet/plugins_registry目录下的所有socket文件遍历出来，保存到缓存当中
		// 2、监听/var/lib/kubelet/plugins_registry目录，如果有socket文件被删除了，那么从desiredStateOfWorld缓存中移除；如果有新的socket文件
		// 被创建，这个socket文件路径会被保存到DesiredStateOfWorld
		// 3、pluginwatcher的核心目标就是通过监听文件发现查询注册与插件注销；只要有新的socket文件被创建，就说明这个插件需要被注册；只要
		// 有socket文件被删除，那么这个插件需要被注销；当然，插件的注册与注销并不是由pluginwatcher来完成的，pluginwatcher仅仅是做一个记录。
		// 指针执行插件的注册与注销是由reconciler完成的
		desiredStateOfWorldPopulator: pluginwatcher.NewWatcher(
			sockDir,
			dsw,
		),
		reconciler:          reconciler,
		desiredStateOfWorld: dsw,
		actualStateOfWorld:  asw,
	}
	return pm
}

// pluginManager implements the PluginManager interface
type pluginManager struct {
	// desiredStateOfWorldPopulator (the plugin watcher) runs an asynchronous
	// periodic loop to populate the desiredStateOfWorld.
	// 插件的期望状态
	desiredStateOfWorldPopulator *pluginwatcher.Watcher

	// reconciler runs an asynchronous periodic loop to reconcile the
	// desiredStateOfWorld with the actualStateOfWorld by triggering register
	// and unregister operations using the operationExecutor.
	reconciler reconciler.Reconciler

	// actualStateOfWorld is a data structure containing the actual state of
	// the world according to the manager: i.e. which plugins are registered.
	// The data structure is populated upon successful completion of register
	// and unregister actions triggered by the reconciler.
	// 插件的实际状态
	actualStateOfWorld cache.ActualStateOfWorld

	// desiredStateOfWorld is a data structure containing the desired state of
	// the world according to the plugin manager: i.e. what plugins are registered.
	// The data structure is populated by the desired state of the world
	// populator (plugin watcher).
	// 插件的期望状态
	desiredStateOfWorld cache.DesiredStateOfWorld
}

var _ PluginManager = &pluginManager{}

func (pm *pluginManager) Run(sourcesReady config.SourcesReady, stopCh <-chan struct{}) {
	defer runtime.HandleCrash()

	// 1、遍历/var/lib/kubelet/plugins_registry目录,把/var/lib/kubelet/plugins_registry目录下的所有socket文件遍历出来，保存到缓存当中
	// 2、监听/var/lib/kubelet/plugins_registry目录，如果有socket文件被删除了，那么从desiredStateOfWorld缓存中移除
	// 3、说白了desiredStateOfWorldPopulator实际上就是要给监听器，用于监听/var/lib/kubelet/plugins_registry，于此同时
	// 维护desiredStateOfWorld
	// 4、这里实际上可以理解为插件的发现原理。实际上就是通过linux文件来发现插件
	if err := pm.desiredStateOfWorldPopulator.Start(stopCh); err != nil {
		klog.ErrorS(err, "The desired_state_of_world populator (plugin watcher) starts failed!")
		return
	}

	klog.V(2).InfoS("The desired_state_of_world populator (plugin watcher) starts")

	klog.InfoS("Starting Kubelet Plugin Manager")
	// 对比插件期望状态缓存以及插件实际状态缓存，执行插件的注册、注销动作
	go pm.reconciler.Run(stopCh)

	metrics.Register(pm.actualStateOfWorld, pm.desiredStateOfWorld)
	<-stopCh
	klog.InfoS("Shutting down Kubelet Plugin Manager")
}

func (pm *pluginManager) AddHandler(pluginType string, handler cache.PluginHandler) {
	pm.reconciler.AddHandler(pluginType, handler)
}
