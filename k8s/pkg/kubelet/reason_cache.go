/*
Copyright 2016 The Kubernetes Authors.

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

package kubelet

import (
	"fmt"
	"sync"

	"github.com/golang/groupcache/lru"
	"k8s.io/apimachinery/pkg/types"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

// ReasonCache stores the failure reason of the latest container start
// in a string, keyed by <pod_UID>_<container_name>. The goal is to
// propagate this reason to the container status. This endeavor is
// "best-effort" for two reasons:
//  1. The cache is not persisted.
//  2. We use an LRU cache to avoid extra garbage collection work. This
//     means that some entries may be recycled before a pod has been
//     deleted.
//
// TODO(random-liu): Use more reliable cache which could collect garbage of failed pod.
// TODO(random-liu): Move reason cache to somewhere better.
type ReasonCache struct {
	lock  sync.Mutex
	cache *lru.Cache
}

// ReasonItem is the cached item in ReasonCache
type ReasonItem struct {
	Err     error
	Message string
}

// maxReasonCacheEntries is the cache entry number in lru cache. 1000 is a proper number
// for our 100 pods per node target. If we support more pods per node in the future, we
// may want to increase the number.
const maxReasonCacheEntries = 1000

// NewReasonCache creates an instance of 'ReasonCache'.
// 1、ReasonCache最多保存1000条记录，并且这个缓存为LRU缓存，不常用的原因会被清除。
// 2、ReasonCache用于保存容器启动失败的原因（容器其它时刻的原因并不保存）。缓存的key为<pod-uid>_<name>
func NewReasonCache() *ReasonCache {
	return &ReasonCache{cache: lru.New(maxReasonCacheEntries)}
}

func (c *ReasonCache) composeKey(uid types.UID, name string) string {
	return fmt.Sprintf("%s_%s", uid, name)
}

// add adds error reason into the cache
func (c *ReasonCache) add(uid types.UID, name string, reason error, message string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.cache.Add(c.composeKey(uid, name), ReasonItem{reason, message})
}

// Update updates the reason cache with the SyncPodResult. Only SyncResult with
// StartContainer action will change the cache.
func (c *ReasonCache) Update(uid types.UID, result kubecontainer.PodSyncResult) {
	for _, r := range result.SyncResults {
		// ReasonCache只保存容器启动失败的原因，其它时候失败的原因不保存
		if r.Action != kubecontainer.StartContainer {
			continue
		}
		name := r.Target.(string)
		if r.Error != nil {
			// 如果容器启动失败，记录下来
			c.add(uid, name, r.Error, r.Message)
		} else {
			// 如果容器启动成功，移除容器启动失败
			c.Remove(uid, name)
		}
	}
}

// Remove removes error reason from the cache
// 移除容器失败原因
func (c *ReasonCache) Remove(uid types.UID, name string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.cache.Remove(c.composeKey(uid, name))
}

// Get gets error reason from the cache. The return values are error reason, error message and
// whether an error reason is found in the cache. If no error reason is found, empty string will
// be returned for error reason and error message.
// 获取容器失败的原因
func (c *ReasonCache) Get(uid types.UID, name string) (*ReasonItem, bool) {
	c.lock.Lock()
	defer c.lock.Unlock()
	value, ok := c.cache.Get(c.composeKey(uid, name))
	if !ok {
		return nil, false
	}
	info := value.(ReasonItem)
	return &info, true
}
