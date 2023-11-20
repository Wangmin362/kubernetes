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

package manager

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	"k8s.io/api/core/v1"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/kubernetes/pkg/kubelet/util"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
)

// GetObjectTTLFunc defines a function to get value of TTL.
type GetObjectTTLFunc func() (time.Duration, bool)

// GetObjectFunc defines a function to get object with a given namespace and name.
// 用于根据资源所在的名称空间、名字获取资源，一般是通过直接查询APIServer
type GetObjectFunc func(namespace string, name string, opts metav1.GetOptions) (runtime.Object, error)

type objectKey struct {
	namespace string
	name      string
	uid       types.UID
}

// objectStoreItems is a single item stored in objectStore.
type objectStoreItem struct {
	// 当前资源被引用的次数，譬如多个Pod引用同一个ConfigMap,或者多个Pod引用同一个Secret
	refCount int
	data     *objectData
}

type objectData struct {
	sync.Mutex

	object         runtime.Object
	err            error
	lastUpdateTime time.Time
}

// objectStore is a local cache of objects.
// 用于缓存某个资源对象
type objectStore struct {
	getObject GetObjectFunc
	clock     clock.Clock

	lock  sync.Mutex
	items map[objectKey]*objectStoreItem

	defaultTTL time.Duration
	getTTL     GetObjectTTLFunc
}

// NewObjectStore returns a new ttl-based instance of Store interface.
func NewObjectStore(
	getObject GetObjectFunc, // 用于根据资源所在的名称空间、名字获取资源，一般是通过直接查询APIServer
	clock clock.Clock, // 时间封装
	getTTL GetObjectTTLFunc, // 获取资源存活时间，或者说获取资源的过期时间
	ttl time.Duration, // 资源默认的过期时间，默认为一分钟
) Store {
	return &objectStore{
		getObject:  getObject,
		clock:      clock,
		items:      make(map[objectKey]*objectStoreItem),
		defaultTTL: ttl,
		getTTL:     getTTL,
	}
}

func isObjectOlder(newObject, oldObject runtime.Object) bool {
	if newObject == nil || oldObject == nil {
		return false
	}
	newVersion, _ := storage.APIObjectVersioner{}.ObjectResourceVersion(newObject)
	oldVersion, _ := storage.APIObjectVersioner{}.ObjectResourceVersion(oldObject)
	return newVersion < oldVersion
}

func (s *objectStore) AddReference(namespace, name string) {
	key := objectKey{namespace: namespace, name: name}

	// AddReference is called from RegisterPod, thus it needs to be efficient.
	// Thus Add() is only increasing refCount and generation of a given object.
	// Then Get() is responsible for fetching if needed.
	s.lock.Lock()
	defer s.lock.Unlock()
	item, exists := s.items[key]
	if !exists {
		item = &objectStoreItem{
			refCount: 0,
			data:     &objectData{},
		}
		s.items[key] = item
	}

	item.refCount++
	// 触发执行Get()操作时，重新获取Pod依赖的资源对象
	item.data = nil
}

func (s *objectStore) DeleteReference(namespace, name string) {
	key := objectKey{namespace: namespace, name: name}

	s.lock.Lock()
	defer s.lock.Unlock()
	if item, ok := s.items[key]; ok {
		// 减少资源被引用的次数
		item.refCount--
		if item.refCount == 0 {
			delete(s.items, key)
		}
	}
}

// GetObjectTTLFromNodeFunc returns a function that returns TTL value
// from a given Node object.
func GetObjectTTLFromNodeFunc(getNode func() (*v1.Node, error)) GetObjectTTLFunc {
	return func() (time.Duration, bool) {
		node, err := getNode()
		if err != nil {
			return time.Duration(0), false
		}
		if node != nil && node.Annotations != nil {
			if value, ok := node.Annotations[v1.ObjectTTLAnnotationKey]; ok {
				if intValue, err := strconv.Atoi(value); err == nil {
					return time.Duration(intValue) * time.Second, true
				}
			}
		}
		return time.Duration(0), false
	}
}

func (s *objectStore) isObjectFresh(data *objectData) bool {
	objectTTL := s.defaultTTL
	// 获取缓存失效时间
	if ttl, ok := s.getTTL(); ok {
		objectTTL = ttl
	}
	// 判断当前数据是否过了缓存失效时间
	return s.clock.Now().Before(data.lastUpdateTime.Add(objectTTL))
}

func (s *objectStore) Get(namespace, name string) (runtime.Object, error) {
	key := objectKey{namespace: namespace, name: name}

	data := func() *objectData {
		s.lock.Lock()
		defer s.lock.Unlock()
		item, exists := s.items[key]
		if !exists {
			// 如果不存在，说明Pod没有引用过这个资源，直接返回空
			return nil
		}
		if item.data == nil {
			// 如果存在，但是没有缓存数据，就实例化一个
			item.data = &objectData{}
		}
		return item.data
	}()
	if data == nil {
		return nil, fmt.Errorf("object %q/%q not registered", namespace, name)
	}

	// After updating data in objectStore, lock the data, fetch object if
	// needed and return data.
	data.Lock()
	defer data.Unlock()
	// 如果数据是空的 那么将需要重新拉取数据
	if data.err != nil || !s.isObjectFresh(data) {
		opts := metav1.GetOptions{}
		if data.object != nil && data.err == nil {
			// This is just a periodic refresh of an object we successfully fetched previously.
			// In this case, server data from apiserver cache to reduce the load on both
			// etcd and apiserver (the cache is eventually consistent).
			util.FromApiserverCache(&opts)
		}

		// 获取Pod依赖的资源对象（譬如ConfigMap或者是Secret）
		object, err := s.getObject(namespace, name, opts)
		if err != nil && !apierrors.IsNotFound(err) && data.object == nil && data.err == nil {
			// Couldn't fetch the latest object, but there is no cached data to return.
			// Return the fetch result instead.
			return object, err
		}
		if (err == nil && !isObjectOlder(object, data.object)) || apierrors.IsNotFound(err) {
			// If the fetch succeeded with a newer version of the object, or if the
			// object could not be found in the apiserver, update the cached data to
			// reflect the current status.
			data.object = object
			data.err = err
			data.lastUpdateTime = s.clock.Now()
		}
	}
	return data.object, data.err
}

// cacheBasedManager keeps a store with objects necessary
// for registered pods. Different implementations of the store
// may result in different semantics for freshness of objects
// (e.g. ttl-based implementation vs watch-based implementation).
type cacheBasedManager struct {
	objectStore Store
	// 获取Pod引用的资源，譬如ConfigMap或者Secret
	getReferencedObjects func(*v1.Pod) sets.String

	lock sync.Mutex
	// 所有缓存的Pod
	registeredPods map[objectKey]*v1.Pod
}

func (c *cacheBasedManager) GetObject(namespace, name string) (runtime.Object, error) {
	return c.objectStore.Get(namespace, name)
}

func (c *cacheBasedManager) RegisterPod(pod *v1.Pod) {
	// 获取当前Pod引用的所有资源（譬如ConfigMap或者是Secret）
	names := c.getReferencedObjects(pod)
	c.lock.Lock()
	defer c.lock.Unlock()
	for name := range names {
		c.objectStore.AddReference(pod.Namespace, name)
	}
	var prev *v1.Pod
	key := objectKey{namespace: pod.Namespace, name: pod.Name, uid: pod.UID}
	prev = c.registeredPods[key]
	c.registeredPods[key] = pod
	// 以前存在，说明现在很有可能是更新
	if prev != nil {
		// 获取上一个版本所引用的资源，然后遍历每个引用的资源并删除
		for name := range c.getReferencedObjects(prev) {
			// On an update, the .Add() call above will have re-incremented the
			// ref count of any existing object, so any objects that are in both
			// names and prev need to have their ref counts decremented. Any that
			// are only in prev need to be completely removed. This unconditional
			// call takes care of both cases.
			c.objectStore.DeleteReference(prev.Namespace, name)
		}
	}
}

func (c *cacheBasedManager) UnregisterPod(pod *v1.Pod) {
	var prev *v1.Pod
	key := objectKey{namespace: pod.Namespace, name: pod.Name, uid: pod.UID}
	c.lock.Lock()
	defer c.lock.Unlock()
	prev = c.registeredPods[key]
	delete(c.registeredPods, key)
	if prev != nil {
		for name := range c.getReferencedObjects(prev) {
			c.objectStore.DeleteReference(prev.Namespace, name)
		}
	}
}

// NewCacheBasedManager creates a manager that keeps a cache of all objects
// necessary for registered pods.
// It implements the following logic:
//   - whenever a pod is created or updated, the cached versions of all objects
//     is referencing are invalidated
//   - every GetObject() call tries to fetch the value from local cache; if it is
//     not there, invalidated or too old, we fetch it from apiserver and refresh the
//     value in cache; otherwise it is just fetched from cache
func NewCacheBasedManager(
	// 存储资源对象的地方
	objectStore Store,
	// 1、获取Pod引用的资源对象，譬如Secret, ConfigMap
	// 2、由于Pod引用的ConfigMap以及Secret只能是本名称空间中的资源，所以这里只需要资源的名字就可以唯一定位。同一种资源在相同的名称空间
	// 中肯定不可能有重复的名字
	getReferencedObjects func(*v1.Pod) sets.String,
) Manager {
	return &cacheBasedManager{
		objectStore:          objectStore,
		getReferencedObjects: getReferencedObjects,
		registeredPods:       make(map[objectKey]*v1.Pod),
	}
}
