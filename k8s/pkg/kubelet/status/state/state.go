/*
Copyright 2021 The Kubernetes Authors.

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

package state

import (
	"k8s.io/api/core/v1"
)

// PodResourceAllocation type is used in tracking resources allocated to pod's containers
// 1、第一级key为Pod UID, 第二级Key为容器名（之所以不使用容器 ID，是因为容器可能会被重启，ID会发生改变）
// 2、Value为容器的各种资源分配的大小，目前有：cpu, memory, storage, ephemeral-storage
type PodResourceAllocation map[string]map[string]v1.ResourceList

// PodResizeStatus type is used in tracking the last resize decision for pod
// Key为Pod UID
type PodResizeStatus map[string]v1.PodResizeStatus

// Clone returns a copy of PodResourceAllocation
func (pr PodResourceAllocation) Clone() PodResourceAllocation {
	prCopy := make(PodResourceAllocation)
	for pod := range pr {
		prCopy[pod] = make(map[string]v1.ResourceList)
		for container, alloc := range pr[pod] {
			prCopy[pod][container] = alloc.DeepCopy()
		}
	}
	return prCopy
}

// Reader interface used to read current pod resource allocation state
type Reader interface {
	// GetContainerResourceAllocation 获取Pod中某个容器的资源分配情况
	GetContainerResourceAllocation(podUID string, containerName string) (v1.ResourceList, bool)
	// GetPodResourceAllocation 获取所有Pod的资源分配情况
	GetPodResourceAllocation() PodResourceAllocation
	// GetPodResizeStatus 获取Pod容器资源分配状态
	GetPodResizeStatus(podUID string) (v1.PodResizeStatus, bool)
	// GetResizeStatus 获取所有容器资源分配状态
	GetResizeStatus() PodResizeStatus
}

type writer interface {
	// SetContainerResourceAllocation 设置Pod中某个容器的资源分配情况
	SetContainerResourceAllocation(podUID string, containerName string, alloc v1.ResourceList) error
	// SetPodResourceAllocation 替换Pod资源分配情况
	SetPodResourceAllocation(PodResourceAllocation) error
	// SetPodResizeStatus 设置Pod资源分配状态，如果状态为空，会清空缓存中记录的Pod资源分配状态
	SetPodResizeStatus(podUID string, resizeStatus v1.PodResizeStatus) error
	// SetResizeStatus 更新所有Pod重新分配资源的状态
	SetResizeStatus(PodResizeStatus) error
	// Delete 删除键Pod容器资源分配情况以及分配状态，如果容器名为空，那么清除整个Pod所有容器的资源分配情况分配状态
	Delete(podUID string, containerName string) error
	// ClearState 清空Pod资源分配情况，以及分配状态
	ClearState() error
}

// State interface provides methods for tracking and setting pod resource allocation
type State interface {
	Reader
	writer
}
