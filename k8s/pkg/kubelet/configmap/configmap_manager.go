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

package configmap

import (
	"context"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	corev1 "k8s.io/kubernetes/pkg/apis/core/v1"
	"k8s.io/kubernetes/pkg/kubelet/util/manager"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/clock"
)

// Manager interface provides methods for Kubelet to manage ConfigMap.
type Manager interface {
	// GetConfigMap Get configmap by configmap namespace and name.
	// 根据名称空间以及名字查询Configmap
	GetConfigMap(namespace, name string) (*v1.ConfigMap, error)

	// WARNING: Register/UnregisterPod functions should be efficient,
	// i.e. should not block on network operations.

	// RegisterPod registers all configmaps from a given pod.
	RegisterPod(pod *v1.Pod)

	// UnregisterPod unregisters configmaps from a given pod that are not
	// used by any other registered pod.
	UnregisterPod(pod *v1.Pod)
}

// simpleConfigMapManager implements ConfigMap Manager interface with
// simple operations to apiserver.
type simpleConfigMapManager struct {
	kubeClient clientset.Interface
}

// NewSimpleConfigMapManager creates a new ConfigMapManager instance.
func NewSimpleConfigMapManager(kubeClient clientset.Interface) Manager {
	return &simpleConfigMapManager{kubeClient: kubeClient}
}

func (s *simpleConfigMapManager) GetConfigMap(namespace, name string) (*v1.ConfigMap, error) {
	return s.kubeClient.CoreV1().ConfigMaps(namespace).Get(context.TODO(), name, metav1.GetOptions{})
}

func (s *simpleConfigMapManager) RegisterPod(pod *v1.Pod) {
}

func (s *simpleConfigMapManager) UnregisterPod(pod *v1.Pod) {
}

// configMapManager keeps a cache of all configmaps necessary
// for registered pods. Different implementation of the store
// may result in different semantics for freshness of configmaps
// (e.g. ttl-based implementation vs watch-based implementation).
type configMapManager struct {
	manager manager.Manager
}

func (c *configMapManager) GetConfigMap(namespace, name string) (*v1.ConfigMap, error) {
	object, err := c.manager.GetObject(namespace, name)
	if err != nil {
		return nil, err
	}
	if configmap, ok := object.(*v1.ConfigMap); ok {
		return configmap, nil
	}
	return nil, fmt.Errorf("unexpected object type: %v", object)
}

func (c *configMapManager) RegisterPod(pod *v1.Pod) {
	c.manager.RegisterPod(pod)
}

func (c *configMapManager) UnregisterPod(pod *v1.Pod) {
	c.manager.UnregisterPod(pod)
}

func getConfigMapNames(pod *v1.Pod) sets.String {
	result := sets.NewString()
	podutil.VisitPodConfigmapNames(pod, func(name string) bool {
		result.Insert(name)
		return true
	})
	return result
}

const (
	defaultTTL = time.Minute
)

// NewCachingConfigMapManager creates a manager that keeps a cache of all configmaps
// necessary for registered pods.
// It implement the following logic:
//   - whenever a pod is create or updated, the cached versions of all configmaps
//     are invalidated
//   - every GetObject() call tries to fetch the value from local cache; if it is
//     not there, invalidated or too old, we fetch it from apiserver and refresh the
//     value in cache; otherwise it is just fetched from cache
//
// 1、用于缓存Pod依赖的ConfigMap, 主要实现了以下功能
// 1.1、当有Pod更新或者创建的时候，这个Pod所引用的ConfigMap缓存将会失效，等下次获取这个ConfigMap时才会重新请求APIServer，并重新缓存
// 1.2、每次调用GetObject()方法时，都会尝试从本地缓存中获取ConfigMap。当然，如果本地没有缓存或者是缓存失效，此时将会请求APIServer，
// 重新获取ConfigMap依赖，然后缓存起来，下一次就可以直接获取（当然前提是缓存没有失效）
func NewCachingConfigMapManager(kubeClient clientset.Interface, getTTL manager.GetObjectTTLFunc) Manager {
	// 根据名称空间、名字查询ConfigMap
	getConfigMap := func(namespace, name string, opts metav1.GetOptions) (runtime.Object, error) {
		return kubeClient.CoreV1().ConfigMaps(namespace).Get(context.TODO(), name, opts)
	}
	// 用于缓存资源对象，并且该资源对象有TTL，即资源对象有过期时间
	configMapStore := manager.NewObjectStore(getConfigMap, clock.RealClock{}, getTTL, defaultTTL)
	return &configMapManager{manager.NewCacheBasedManager(configMapStore, getConfigMapNames)}
}

// NewWatchingConfigMapManager creates a manager that keeps a cache of all configmaps
// necessary for registered pods.
// It implements the following logic:
//   - whenever a pod is created or updated, we start individual watches for all
//     referenced objects that aren't referenced from other registered pods
//   - every GetObject() returns a value from local cache propagated via watches
//
// TODO 分析原理
func NewWatchingConfigMapManager(kubeClient clientset.Interface, resyncInterval time.Duration) Manager {
	listConfigMap := func(namespace string, opts metav1.ListOptions) (runtime.Object, error) {
		return kubeClient.CoreV1().ConfigMaps(namespace).List(context.TODO(), opts)
	}
	watchConfigMap := func(namespace string, opts metav1.ListOptions) (watch.Interface, error) {
		return kubeClient.CoreV1().ConfigMaps(namespace).Watch(context.TODO(), opts)
	}
	newConfigMap := func() runtime.Object {
		return &v1.ConfigMap{}
	}
	isImmutable := func(object runtime.Object) bool {
		if configMap, ok := object.(*v1.ConfigMap); ok {
			return configMap.Immutable != nil && *configMap.Immutable
		}
		return false
	}
	gr := corev1.Resource("configmap")
	return &configMapManager{
		manager: manager.NewWatchBasedManager(listConfigMap, watchConfigMap, newConfigMap, isImmutable, gr, resyncInterval, getConfigMapNames),
	}
}
