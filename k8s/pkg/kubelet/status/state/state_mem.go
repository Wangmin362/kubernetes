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
	"sync"

	"k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// 记录Pod资源分配情况，以及资源重新配分的状态
type stateMemory struct {
	sync.RWMutex
	// Pod重新分配资源大小的状态，可能的状态有Proposed, InProgress, Deferred, Infeasible
	podResizeStatus PodResizeStatus
	// 用于保存Pod每个容器分配的资源大小
	podAllocation PodResourceAllocation
}

var _ State = &stateMemory{}

// NewStateMemory creates new State to track resources allocated to pods
func NewStateMemory() State {
	klog.V(2).InfoS("Initialized new in-memory state store for pod resource allocation tracking")
	return &stateMemory{
		podAllocation:   PodResourceAllocation{},
		podResizeStatus: PodResizeStatus{},
	}
}

func (s *stateMemory) GetContainerResourceAllocation(podUID string, containerName string) (v1.ResourceList, bool) {
	s.RLock()
	defer s.RUnlock()

	alloc, ok := s.podAllocation[podUID][containerName]
	return alloc.DeepCopy(), ok
}

func (s *stateMemory) GetPodResourceAllocation() PodResourceAllocation {
	s.RLock()
	defer s.RUnlock()
	return s.podAllocation.Clone()
}

func (s *stateMemory) GetPodResizeStatus(podUID string) (v1.PodResizeStatus, bool) {
	s.RLock()
	defer s.RUnlock()

	resizeStatus, ok := s.podResizeStatus[podUID]
	return resizeStatus, ok
}

func (s *stateMemory) GetResizeStatus() PodResizeStatus {
	s.RLock()
	defer s.RUnlock()
	prs := make(map[string]v1.PodResizeStatus)
	for k, v := range s.podResizeStatus {
		prs[k] = v
	}
	return prs
}

func (s *stateMemory) SetContainerResourceAllocation(podUID string, containerName string, alloc v1.ResourceList) error {
	s.Lock()
	defer s.Unlock()

	if _, ok := s.podAllocation[podUID]; !ok {
		s.podAllocation[podUID] = make(map[string]v1.ResourceList)
	}

	s.podAllocation[podUID][containerName] = alloc
	klog.V(3).InfoS("Updated container resource allocation", "podUID", podUID, "containerName", containerName, "alloc", alloc)
	return nil
}

func (s *stateMemory) SetPodResourceAllocation(a PodResourceAllocation) error {
	s.Lock()
	defer s.Unlock()

	s.podAllocation = a.Clone()
	klog.V(3).InfoS("Updated pod resource allocation", "allocation", a)
	return nil
}

func (s *stateMemory) SetPodResizeStatus(podUID string, resizeStatus v1.PodResizeStatus) error {
	s.Lock()
	defer s.Unlock()

	if resizeStatus != "" {
		s.podResizeStatus[podUID] = resizeStatus
	} else {
		delete(s.podResizeStatus, podUID)
	}
	klog.V(3).InfoS("Updated pod resize state", "podUID", podUID, "resizeStatus", resizeStatus)
	return nil
}

func (s *stateMemory) SetResizeStatus(rs PodResizeStatus) error {
	s.Lock()
	defer s.Unlock()
	prs := make(map[string]v1.PodResizeStatus)
	for k, v := range rs {
		prs[k] = v
	}
	s.podResizeStatus = prs
	klog.V(3).InfoS("Updated pod resize state", "resizes", rs)
	return nil
}

func (s *stateMemory) deleteContainer(podUID string, containerName string) {
	delete(s.podAllocation[podUID], containerName)
	if len(s.podAllocation[podUID]) == 0 {
		delete(s.podAllocation, podUID)
		delete(s.podResizeStatus, podUID)
	}
	klog.V(3).InfoS("Deleted pod resource allocation", "podUID", podUID, "containerName", containerName)
}

// Delete 删除键Pod容器资源分配情况以及分配状态，如果容器名为空，那么清除整个Pod所有容器的资源分配情况分配状态
func (s *stateMemory) Delete(podUID string, containerName string) error {
	s.Lock()
	defer s.Unlock()
	// 删除整个Pod的资源分配情况以及分配状态
	if len(containerName) == 0 {
		delete(s.podAllocation, podUID)
		delete(s.podResizeStatus, podUID)
		klog.V(3).InfoS("Deleted pod resource allocation and resize state", "podUID", podUID)
		return nil
	}
	// 删除Pod的容器资源分配情况
	s.deleteContainer(podUID, containerName)
	return nil
}

func (s *stateMemory) ClearState() error {
	s.Lock()
	defer s.Unlock()

	s.podAllocation = make(PodResourceAllocation)
	s.podResizeStatus = make(PodResizeStatus)
	klog.V(3).InfoS("Cleared state")
	return nil
}
