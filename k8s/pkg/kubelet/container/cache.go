/*
Copyright 2015 The Kubernetes Authors.

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

package container

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/kubernetes/pkg/features"
)

// Cache stores the PodStatus for the pods. It represents *all* the visible
// pods/containers in the container runtime. All cache entries are at least as
// new or newer than the global timestamp (set by UpdateTime()), while
// individual entries may be slightly newer than the global timestamp. If a pod
// has no states known by the runtime, Cache returns an empty PodStatus object
// with ID populated.
//
// Cache provides two methods to retrieve the PodStatus: the non-blocking Get()
// and the blocking GetNewerThan() method. The component responsible for
// populating the cache is expected to call Delete() to explicitly free the
// cache entries.
// 缓存Pod状态，用于获取Pod的状态，或者获取比指定时间更新的Pod状态
type Cache interface {
	// Get 通过Pod UID获取Pod状态，这里获取到的Pod状态是当前cache缓存的状态，可能不是最新的
	Get(types.UID) (*PodStatus, error)
	// Set updates the cache by setting the PodStatus for the pod only
	// if the data is newer than the cache based on the provided
	// time stamp. Returns if the cache was updated.
	// 更新Pod的状态修改时间
	Set(types.UID, *PodStatus, error, time.Time) (updated bool)
	// GetNewerThan is a blocking call that only returns the status
	// when it is newer than the given time.
	// 1、总结下来，调用方能否获取到指定Pod更新的状态屈居于两点：
	// 1.1、cache的全局时间，全局时间代表着cache缓存的Pod状态的新鲜程度，如果全局时间满足指定的时间，那么这个Pod的状态，我们认为就是比较新的。
	// 需要注意的是，全局时间满足指定时间的情况下，Pod状态的真正更新时间可能并非满足条件，但是这个条件的重要性显然比全局时间优先级低。
	// 1.2、pod状态的真实修改时间，如果这个时间也比指定的时间新，那么pod状态也是认为是新鲜的，也符合调用者的需求。
	// 2、总之，先看cache的全局时间，全局时间满足就直接返回；全局时间不满足，就看pod真正的更新时间
	GetNewerThan(types.UID, time.Time) (*PodStatus, error)
	Delete(types.UID)
	// UpdateTime 更新缓存的全局时间。全局时间说明了缓存中Pod状态的有效性，如果全局时间都比调用方指定的时间更新，那么当前Pod的状态就是满足
	// 条件的，即便当前Pod的更新时间很老。
	UpdateTime(time.Time)
}

type data struct {
	// Status of the pod.
	status *PodStatus
	// Error got when trying to inspect the pod.
	err error
	// Time when the data was last modified.
	// Pod上一次修改的时间
	modified time.Time
}

type subRecord struct {
	time time.Time
	ch   chan *data
}

// cache implements Cache.
type cache struct {
	// Lock which guards all internal data structures.
	lock sync.RWMutex
	// Map that stores the pod statuses.
	pods map[types.UID]*data
	// A global timestamp represents how fresh the cached data is. All
	// cache content is at the least newer than this timestamp. Note that the
	// timestamp is nil after initialization, and will only become non-nil when
	// it is ready to serve the cached statuses.
	// 当前缓存中的所有Pod的更新时间至少要在这个时间之后
	timestamp *time.Time
	// Map that stores the subscriber records.
	// 之所以是数组，是因为很有可能同时有多个调用方关注同一个Pod的更新状态，所以是个数组。每个调用方都可以指定自己需要的Pod状态的新鲜度（通过时间指定）
	subscribers map[types.UID][]*subRecord
}

// NewCache creates a pod cache.
func NewCache() Cache {
	return &cache{pods: map[types.UID]*data{}, subscribers: map[types.UID][]*subRecord{}}
}

// Get returns the PodStatus for the pod; callers are expected not to
// modify the objects returned.
// 获取Pod状态，如果当前没有缓存Pod状态，那么实例化一个空的Pod状态
func (c *cache) Get(id types.UID) (*PodStatus, error) {
	c.lock.RLock()
	defer c.lock.RUnlock()
	d := c.get(id)
	return d.status, d.err
}

// GetNewerThan 获取比指定Pod时间点更新的状态
func (c *cache) GetNewerThan(id types.UID, minTime time.Time) (*PodStatus, error) {
	// 1、订阅指定Pod的状态，同时这个Pod的状态必须比指定时间新
	// 2、如果cache中已经存在比指定时间更新的Pod状态，那么直接返回，数据就放在channel当中
	// 3、如果cache中不经存在比指定时间更新的Pod状态，那么只能返回一个空的channel，调用方将会被阻塞，直到有了新的数据
	ch := c.subscribe(id, minTime)
	// 调用方将被阻塞，直到channel中被放入数据
	d := <-ch
	return d.status, d.err
}

// Set sets the PodStatus for the pod only if the data is newer than the cache
func (c *cache) Set(id types.UID, status *PodStatus, err error, timestamp time.Time) (updated bool) {
	c.lock.Lock()
	defer c.lock.Unlock()

	if utilfeature.DefaultFeatureGate.Enabled(features.EventedPLEG) {
		// Set the value in the cache only if it's not present already
		// or the timestamp in the cache is older than the current update timestamp
		// 说明当前缓存中保存的数据比当前Pod设置的时间还新，那么没有必要更新Pod的状态
		if cachedVal, ok := c.pods[id]; ok && cachedVal.modified.After(timestamp) {
			return false
		}
	}

	// 缓存Pod状态，同时记录Pod修改的时间
	c.pods[id] = &data{status: status, err: err, modified: timestamp}
	// 更新了Pod的状态，那么订阅这个Pod状态的调用方很有可能会得到Pod最新的状态，因此需要通知所有的调用方
	c.notify(id, timestamp)
	return true
}

// Delete removes the entry of the pod.
func (c *cache) Delete(id types.UID) {
	c.lock.Lock()
	defer c.lock.Unlock()
	// TODO 这里为啥不需要删除subscribers
	delete(c.pods, id)
}

// UpdateTime modifies the global timestamp of the cache and notify
// subscribers if needed.
func (c *cache) UpdateTime(timestamp time.Time) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.timestamp = &timestamp
	// Notify all the subscribers if the condition is met.
	for id := range c.subscribers {
		// 如果刷新了cache缓存的全局时间，那么很有可能满足某些订阅者指定的时间，因此需要通知所有Pod的所有订阅者
		c.notify(id, *c.timestamp)
	}
}

func makeDefaultData(id types.UID) *data {
	return &data{status: &PodStatus{ID: id}, err: nil}
}

func (c *cache) get(id types.UID) *data {
	d, ok := c.pods[id]
	if !ok {
		// Cache should store *all* pod/container information known by the
		// container runtime. A cache miss indicates that there are no states
		// regarding the pod last time we queried the container runtime.
		// What this *really* means is that there are no visible pod/containers
		// associated with this pod. Simply return an default (mostly empty)
		// PodStatus to reflect this.
		// 如果没有缓存当前Pod，那么直接实例化一个
		return makeDefaultData(id)
	}
	return d
}

// getIfNewerThan returns the data it is newer than the given time.
// Otherwise, it returns nil. The caller should acquire the lock.
// 获取指定Pod的状态，同时这个状态的时间必须比指定的时间新
func (c *cache) getIfNewerThan(id types.UID, minTime time.Time) *data {
	d, ok := c.pods[id]
	if utilfeature.DefaultFeatureGate.Enabled(features.EventedPLEG) {
		// Evented PLEG has CREATED, STARTED, STOPPED and DELETED events
		// However if the container creation fails for some reason there is no
		// CRI event received by the kubelet and that pod will get stuck a
		// GetNewerThan call in the pod workers. This is reproducible with
		// the node e2e test,
		// https://github.com/kubernetes/kubernetes/blob/83415e5c9e6e59a3d60a148160490560af2178a1/test/e2e_node/pod_hostnamefqdn_test.go#L161
		// which forces failure during pod creation. This issue also exists in
		// Generic PLEG but since it updates global timestamp periodically
		// the GetNewerThan call gets unstuck.

		// During node e2e tests, it was observed this change does not have any
		// adverse impact on the behaviour of the Generic PLEG as well.
		switch {
		case !ok:
			// 如果当前cache还没有缓存过这个Pod的状态，那么直接实例化一个空的状态
			return makeDefaultData(id)
		case ok && (d.modified.After(minTime) || (c.timestamp != nil && c.timestamp.After(minTime))):
			// 如果当前cache缓存了Pod的状态并且当前Pod状态的修改之间在指定时间之后，或者是全局更新时间也在指定时间之后，说明cache
			// 缓存的当前pod的状态就是新的，满足要求，因此直接返回。
			return d
		default:
			return nil
		}
	}

	// globalTimestampIsNewer如果为true，说明cache缓存的Pod状态比较新，满足指定的时间
	globalTimestampIsNewer := c.timestamp != nil && c.timestamp.After(minTime)
	// 如果cache没有缓存pod状态，并且cache缓存的Pod状态比较新,那么实例化一个空状态
	if !ok && globalTimestampIsNewer {
		// Status is not cached, but the global timestamp is newer than
		// minTime, return the default status.
		return makeDefaultData(id)
	}
	// 如果当前cache缓存了Pod的状态并且当前Pod状态的修改之间在指定时间之后，或者是全局更新时间也在指定时间之后，说明cache
	// 缓存的当前pod的状态就是新的，满足要求，因此直接返回。
	if ok && (d.modified.After(minTime) || globalTimestampIsNewer) {
		// Status is cached, return status if either of the following is true.
		//   * status was modified after minTime
		//   * the global timestamp of the cache is newer than minTime.
		return d
	}
	// The pod status is not ready.
	return nil
}

// notify sends notifications for pod with the given id, if the requirements
// are met. Note that the caller should acquire the lock.
func (c *cache) notify(
	id types.UID, // 当前Pod
	timestamp time.Time, // 当前Pod的最新状态的修改时间
) {
	list, ok := c.subscribers[id]
	if !ok {
		// No one to notify.
		return
	}
	var newList []*subRecord
	for i, r := range list {
		// 说明当前Pod的状态还不够新，不满足订阅者的时间，只能蛰伏起来，等待Pod的更新
		if timestamp.Before(r.time) {
			// Doesn't meet the time requirement; keep the record.
			newList = append(newList, list[i])
			continue
		}
		// 否则，说明当前Pod的状态就是比较新的，满足订阅者指定的时间，可以直接返回
		r.ch <- c.get(id)
		close(r.ch)
	}
	if len(newList) == 0 {
		// 如果当前Pod没有任何订阅者了，那么直接删除这个条目
		delete(c.subscribers, id)
	} else {
		// 否则更新当前Pod的订阅者
		c.subscribers[id] = newList
	}
}

// 1、订阅指定Pod的状态，同时这个Pod的状态必须比指定时间新
// 2、如果cache中已经存在比指定时间更新的Pod状态，那么直接返回，数据就放在channel当中
// 3、如果cache中不经存在比指定时间更新的Pod状态，那么只能返回一个空的channel，调用方将会被阻塞，直到有了新的数据
func (c *cache) subscribe(id types.UID, timestamp time.Time) chan *data {
	ch := make(chan *data, 1)
	c.lock.Lock()
	defer c.lock.Unlock()
	// 获取指定Pod的状态，同时这个状态的时间必须比指定的时间新
	d := c.getIfNewerThan(id, timestamp)
	if d != nil {
		// If the cache entry is ready, send the data and return immediately.
		// 如果cache中已经存在比指定时间更新的Pod状态，那么直接返回
		ch <- d
		return ch
	}
	// Add the subscription record.
	// 如果cache中不经存在比指定时间更新的Pod状态，那么只能返回一个空的channel，调用方将会被阻塞，直到有了新的数据
	c.subscribers[id] = append(c.subscribers[id], &subRecord{time: timestamp, ch: ch})
	return ch
}
