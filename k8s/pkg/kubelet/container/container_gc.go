/*
Copyright 2014 The Kubernetes Authors.

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
	"context"
	"fmt"
	"time"

	"k8s.io/klog/v2"
)

// GCPolicy specifies a policy for garbage collecting containers.
// 容器垃圾收集策略
type GCPolicy struct {
	// Minimum age at which a container can be garbage collected, zero for no limit.
	// 一个容器创建出来之后，最小生存时间，如果为零，表示没有限制，即一个容器创建出来可以立即被回收
	MinAge time.Duration

	// Max number of dead containers any single pod (UID, container name) pair is
	// allowed to have, less than zero for no limit.
	// 每个Pod中最多死亡的容器，默认为一，0表示不限制
	MaxPerPodContainer int

	// Max number of total dead containers, less than zero for no limit.
	// 一个Node最多运行的容器死亡数量，小于零表示没有限制
	MaxContainers int
}

// GC manages garbage collection of dead containers.
//
// Implementation is thread-compatible.
type GC interface {
	// GarbageCollect Garbage collect containers.
	GarbageCollect(ctx context.Context) error
	// DeleteAllUnusedContainers Deletes all unused containers, including containers belonging to pods that are terminated but not deleted
	DeleteAllUnusedContainers(ctx context.Context) error
}

// SourcesReadyProvider knows how to determine if configuration sources are ready
type SourcesReadyProvider interface {
	// AllReady returns true if the currently configured sources have all been seen.
	AllReady() bool
}

// TODO(vmarmol): Preferentially remove pod infra containers.
type realContainerGC struct {
	// Container runtime
	runtime Runtime

	// Policy for garbage collection.
	policy GCPolicy

	// sourcesReadyProvider provides the readiness of kubelet configuration sources.
	sourcesReadyProvider SourcesReadyProvider
}

// NewContainerGC creates a new instance of GC with the specified policy.
func NewContainerGC(
	runtime Runtime, // CRI接口
	policy GCPolicy, // 容器回收策略
	sourcesReadyProvider SourcesReadyProvider, // HTTP, Static, APIServer三种方式源是否同步完成
) (GC, error) {
	if policy.MinAge < 0 {
		return nil, fmt.Errorf("invalid minimum garbage collection age: %v", policy.MinAge)
	}

	return &realContainerGC{
		runtime:              runtime,
		policy:               policy,
		sourcesReadyProvider: sourcesReadyProvider,
	}, nil
}

// GarbageCollect
// 1、根据容器回收策略删除满足条件的并且已经死亡的容器
// 2、容器删除之后，遍历所有的沙箱，如果这个沙箱下已经没有任何容器在运行了，那么删除这个沙箱
// 3、容器删除之后，容器的日志也就没有保留的必要了
func (cgc *realContainerGC) GarbageCollect(ctx context.Context) error {
	return cgc.runtime.GarbageCollect(ctx, cgc.policy, cgc.sourcesReadyProvider.AllReady(), false)
}

// DeleteAllUnusedContainers
// 1、根据容器回收策略删除满足条件的并且已经死亡的容器
// 2、容器删除之后，遍历所有的沙箱，如果这个沙箱下已经没有任何容器在运行了，那么删除这个沙箱
// 3、容器删除之后，容器的日志也就没有保留的必要了
func (cgc *realContainerGC) DeleteAllUnusedContainers(ctx context.Context) error {
	klog.InfoS("Attempting to delete unused containers")
	return cgc.runtime.GarbageCollect(ctx, cgc.policy, cgc.sourcesReadyProvider.AllReady(), true)
}
