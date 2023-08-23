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

package pluginwatcher

import (
	"fmt"
	"os"
	"strings"

	"github.com/fsnotify/fsnotify"
	"k8s.io/klog/v2"

	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
	"k8s.io/kubernetes/pkg/kubelet/util"
	utilfs "k8s.io/kubernetes/pkg/util/filesystem"
)

// Watcher is the plugin watcher
type Watcher struct {
	// /var/lib/kubelet/plugins_registry目录
	path string
	fs   utilfs.Filesystem
	// 文件监听器，当然也可以监听目录，这里监听的是/var/lib/kubelet/plugins_registry目录
	fsWatcher *fsnotify.Watcher
	// 1、本质上就是一个map，key为插件名，value为PluginInfo
	desiredStateOfWorld cache.DesiredStateOfWorld
}

// NewWatcher provides a new watcher for socket registration
func NewWatcher(sockDir string, desiredStateOfWorld cache.DesiredStateOfWorld) *Watcher {
	return &Watcher{
		path:                sockDir,             // /var/lib/kubelet/plugins_registry
		fs:                  &utilfs.DefaultFs{}, // 初始化时，文件是空的，init回初始化为/var/lib/kubelet/plugins_registry
		desiredStateOfWorld: desiredStateOfWorld,
	}
}

// Start watches for the creation and deletion of plugin sockets at the path
// 1、遍历/var/lib/kubelet/plugins_registry目录,把/var/lib/kubelet/plugins_registry目录下的所有socket文件遍历出来，保存到缓存当中
// 2、监听/var/lib/kubelet/plugins_registry目录，如果有socket文件被删除了，那么从desiredStateOfWorld缓存中移除
func (w *Watcher) Start(stopCh <-chan struct{}) error {
	klog.V(2).InfoS("Plugin Watcher Start", "path", w.path)

	// Creating the directory to be watched if it doesn't exist yet,
	// and walks through the directory to discover the existing plugins.
	// 创建/var/lib/kubelet/plugins_registry目录，如果此目录不存在的话
	if err := w.init(); err != nil {
		return err
	}

	// 初始化linux文件监听器
	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("failed to start plugin fsWatcher, err: %v", err)
	}
	w.fsWatcher = fsWatcher

	// Traverse plugin dir and add filesystem watchers before starting the plugin processing goroutine.
	// 遍历/var/lib/kubelet/plugins_registry目录目录，// 把/var/lib/kubelet/plugins_registry目录中的所有socket文件保存到期望状态当中
	if err := w.traversePluginDir(w.path); err != nil {
		klog.ErrorS(err, "Failed to traverse plugin socket path", "path", w.path)
	}

	// 监听/var/lib/kubelet/plugins_registry目录，以及其中的子目录的变化情况
	go func(fsWatcher *fsnotify.Watcher) {
		for {
			select {
			case event := <-fsWatcher.Events:
				//TODO: Handle errors by taking corrective measures
				if event.Has(fsnotify.Create) {
					err := w.handleCreateEvent(event)
					if err != nil {
						klog.ErrorS(err, "Error when handling create event", "event", event)
					}
				} else if event.Has(fsnotify.Remove) {
					// 显然，如果socket文件被删除了，那么肯定需要从缓存中删除
					w.handleDeleteEvent(event)
				}
				continue
			case err := <-fsWatcher.Errors:
				if err != nil {
					klog.ErrorS(err, "FsWatcher received error")
				}
				continue
			case <-stopCh:
				w.fsWatcher.Close()
				return
			}
		}
	}(fsWatcher)

	return nil
}

func (w *Watcher) init() error {
	klog.V(4).InfoS("Ensuring Plugin directory", "path", w.path)

	if err := w.fs.MkdirAll(w.path, 0755); err != nil {
		return fmt.Errorf("error (re-)creating root %s: %v", w.path, err)
	}

	return nil
}

// Walks through the plugin directory discover any existing plugin sockets.
// Ignore all errors except root dir not being walkable
// 把/var/lib/kubelet/plugins_registry目录中的所有socket文件保存到期望状态当中
func (w *Watcher) traversePluginDir(dir string) error {
	// watch the new dir 监听/var/lib/kubelet/plugins_registry目录
	err := w.fsWatcher.Add(dir)
	if err != nil {
		return fmt.Errorf("failed to watch %s, err: %v", w.path, err)
	}
	// traverse existing children in the dir
	// 遍历/var/lib/kubelet/plugins_registry目录
	return w.fs.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if path == dir {
				return fmt.Errorf("error accessing path: %s error: %v", path, err)
			}

			klog.ErrorS(err, "Error accessing path", "path", path)
			return nil
		}

		// do not call fsWatcher.Add twice on the root dir to avoid potential problems.
		// 如果是/var/lib/kubelet/plugins_registry目录，直接跳过，没啥卵用，我们需要关心的其中的子目录或者文件，这些文件或者子目录
		// 中的文件其实就是插件监听的socket文件，kubelet之后需要通过这个socket调用插件实现的API
		if path == dir {
			return nil
		}

		mode := info.Mode()
		if mode.IsDir() {
			// 如果是子目录，那就监听此目录
			if err := w.fsWatcher.Add(path); err != nil {
				return fmt.Errorf("failed to watch %s, err: %v", path, err)
			}
			// 否则，判断当前文件是否是socket文件，如果是普通文件，直接忽略。我们只关心socket文件
		} else if isSocket, _ := util.IsUnixDomainSocket(path); isSocket {
			event := fsnotify.Event{
				Name: path, // /var/lib/kubelet/plugins_registry/<socket-file>
				Op:   fsnotify.Create,
			}
			// TODO: Handle errors by taking corrective measures
			// 把/var/lib/kubelet/plugins_registry/<socket-file>注册
			if err := w.handleCreateEvent(event); err != nil {
				klog.ErrorS(err, "Error when handling create", "event", event)
			}
		} else {
			klog.V(5).InfoS("Ignoring file", "path", path, "mode", mode)
		}

		return nil
	})
}

// Handle filesystem notify event.
// Files names:
// - MUST NOT start with a '.'
func (w *Watcher) handleCreateEvent(event fsnotify.Event) error {
	klog.V(6).InfoS("Handling create event", "event", event)

	// 判断/var/lib/kubelet/plugins_registry/<socket-file>文件是否存在
	fi, err := getStat(event)
	if err != nil {
		return fmt.Errorf("stat file %s failed: %v", event.Name, err)
	}

	// 判断当前文件是否是以.开头的文件，如果是直接忽略
	if strings.HasPrefix(fi.Name(), ".") {
		klog.V(5).InfoS("Ignoring file (starts with '.')", "path", fi.Name())
		return nil
	}

	if !fi.IsDir() {
		// 判断当前文件/var/lib/kubelet/plugins_registry/<socket-file>文件是否是socket文件
		isSocket, err := util.IsUnixDomainSocket(util.NormalizePath(event.Name))
		if err != nil {
			return fmt.Errorf("failed to determine if file: %s is a unix domain socket: %v", event.Name, err)
		}
		// 如果不是socket文件，直接跳过
		if !isSocket {
			klog.V(5).InfoS("Ignoring non socket file", "path", fi.Name())
			return nil
		}

		// 把当前插件的socket路径包装为PluginInfo保存起来，TODO 这里是否可以认为就是注册流程
		return w.handlePluginRegistration(event.Name)
	}

	return w.traversePluginDir(event.Name)
}

func (w *Watcher) handlePluginRegistration(socketPath string) error {
	socketPath = getSocketPath(socketPath)
	// Update desired state of world list of plugins
	// If the socket path does exist in the desired world cache, there's still
	// a possibility that it has been deleted and recreated again before it is
	// removed from the desired world cache, so we still need to call AddOrUpdatePlugin
	// in this case to update the timestamp
	klog.V(2).InfoS("Adding socket path or updating timestamp to desired state cache", "path", socketPath)
	// 把当前/var/lib/kubelet/plugins_registry/<socket-file>socket添加到期望状态
	err := w.desiredStateOfWorld.AddOrUpdatePlugin(socketPath)
	if err != nil {
		return fmt.Errorf("error adding socket path %s or updating timestamp to desired state cache: %v", socketPath, err)
	}
	return nil
}

func (w *Watcher) handleDeleteEvent(event fsnotify.Event) {
	klog.V(6).InfoS("Handling delete event", "event", event)

	socketPath := event.Name
	klog.V(2).InfoS("Removing socket path from desired state cache", "path", socketPath)
	w.desiredStateOfWorld.RemovePlugin(socketPath)
}
