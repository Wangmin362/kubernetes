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

package results

import (
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

// Manager provides a probe results cache and channel of updates.
// 1、用于管理探针的结果，并且把结果通过Update channel传出
// 2、ResultManager的核心目的其实是为了在Pod的容器探针状态发生改变的时候触发PLEG的更新
type Manager interface {
	// Get returns the cached result for the container with the given ID.
	// 获取容器的探针状态
	Get(kubecontainer.ContainerID) (Result, bool)
	// Set sets the cached result for the container with the given ID.
	// The pod is only included to be sent with the update.
	// 设置容器的探针状态
	Set(kubecontainer.ContainerID, Result, *v1.Pod)
	// Remove clears the cached result for the container with the given ID.
	// 一处容器的谭政结果
	Remove(kubecontainer.ContainerID)
	// Updates creates a channel that receives an Update whenever its result changes (but not
	// removed).
	// NOTE: The current implementation only supports a single updates channel.
	// 1、通过channel监听容器的探针状态
	// 2、通过缓存容器的状态，放入这个channel一定是容器的探针状态发生了改变
	// 3、用于外部组件通过此方法监听容器探针状态的改变。 容器探针结果的改变一般会影响容器状态
	Updates() <-chan Update
}

// Result is the type for probe results.
type Result int

const (
	// Unknown is encoded as -1 (type Result)
	Unknown Result = iota - 1

	// Success is encoded as 0 (type Result)
	Success

	// Failure is encoded as 1 (type Result)
	Failure
)

func (r Result) String() string {
	switch r {
	case Success:
		return "Success"
	case Failure:
		return "Failure"
	default:
		return "UNKNOWN"
	}
}

// ToPrometheusType translates a Result to a form which is better understood by prometheus.
func (r Result) ToPrometheusType() float64 {
	switch r {
	case Success:
		return 0
	case Failure:
		return 1
	default:
		return -1
	}
}

// Update is an enum of the types of updates sent over the Updates channel.
type Update struct {
	ContainerID kubecontainer.ContainerID
	Result      Result
	PodUID      types.UID
}

// Manager implementation.
type manager struct {
	// guards the cache
	sync.RWMutex
	// map of container ID -> probe Result
	cache map[kubecontainer.ContainerID]Result
	// channel of updates
	updates chan Update
}

var _ Manager = &manager{}

// NewManager creates and returns an empty results manager.
func NewManager() Manager {
	return &manager{
		cache: make(map[kubecontainer.ContainerID]Result),
		// 默认只能放20个容器探针结果
		updates: make(chan Update, 20),
	}
}

func (m *manager) Get(id kubecontainer.ContainerID) (Result, bool) {
	m.RLock()
	defer m.RUnlock()
	// 直接从缓存当中获取
	result, found := m.cache[id]
	return result, found
}

func (m *manager) Set(id kubecontainer.ContainerID, result Result, pod *v1.Pod) {
	if m.setInternal(id, result) {
		m.updates <- Update{id, result, pod.UID}
	}
}

// Internal helper for locked portion of set. Returns whether an update should be sent.
// 设置容器的探针状态，返回值为true,表示容器的探针状态发生了改变，返回值为false，则表示容器的探针状态没有发生改变
func (m *manager) setInternal(id kubecontainer.ContainerID, result Result) bool {
	m.Lock()
	defer m.Unlock()
	prev, exists := m.cache[id]
	// 如果当前容器还没有探针状态或者当前容器的探针状态发生了改变，就把探针结果缓存起来
	if !exists || prev != result {
		// 说明容器探针状态发生了改变
		m.cache[id] = result
		return true
	}
	// 说明容器探针状态没有发生改变
	return false
}

func (m *manager) Remove(id kubecontainer.ContainerID) {
	m.Lock()
	defer m.Unlock()
	delete(m.cache, id)
}

func (m *manager) Updates() <-chan Update {
	return m.updates
}
