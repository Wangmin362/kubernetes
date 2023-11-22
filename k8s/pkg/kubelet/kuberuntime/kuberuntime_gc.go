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

package kuberuntime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

// containerGC is the manager of garbage collection.
type containerGC struct {
	client           internalapi.RuntimeService
	manager          *kubeGenericRuntimeManager
	podStateProvider podStateProvider
	tracer           trace.Tracer
}

// NewContainerGC creates a new containerGC.
func newContainerGC(client internalapi.RuntimeService, podStateProvider podStateProvider, manager *kubeGenericRuntimeManager, tracer trace.Tracer) *containerGC {
	return &containerGC{
		client:           client,
		manager:          manager,
		podStateProvider: podStateProvider,
		tracer:           tracer,
	}
}

// containerGCInfo is the internal information kept for containers being considered for GC.
type containerGCInfo struct {
	// The ID of the container.
	id string
	// The name of the container.
	name string
	// Creation time for the container.
	createTime time.Time
	// If true, the container is in unknown state. Garbage collector should try
	// to stop containers before removal.
	// 如果容器的状态是stop状态，那么需要先停止容器，然后才能删除容器
	unknown bool
}

// sandboxGCInfo is the internal information kept for sandboxes being considered for GC.
type sandboxGCInfo struct {
	// The ID of the sandbox.
	id string
	// Creation time for the sandbox.
	createTime time.Time
	// If true, the sandbox is ready or still has containers.
	active bool
}

// evictUnit is considered for eviction as units of (UID, container name) pair.
type evictUnit struct {
	// UID of the pod.
	uid types.UID
	// Name of the container in the pod.
	name string
}

// 可以为{pidUid, ContainerName}，由于一个Pod很有可能有多个容器，所以value是数组
type containersByEvictUnit map[evictUnit][]containerGCInfo
type sandboxesByPodUID map[types.UID][]sandboxGCInfo

// NumContainers returns the number of containers in this map.
func (cu containersByEvictUnit) NumContainers() int {
	num := 0
	for key := range cu {
		num += len(cu[key])
	}
	return num
}

// NumEvictUnits returns the number of pod in this map.
func (cu containersByEvictUnit) NumEvictUnits() int {
	return len(cu)
}

// Newest first.
type byCreated []containerGCInfo

func (a byCreated) Len() int           { return len(a) }
func (a byCreated) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byCreated) Less(i, j int) bool { return a[i].createTime.After(a[j].createTime) }

// Newest first.
type sandboxByCreated []sandboxGCInfo

func (a sandboxByCreated) Len() int           { return len(a) }
func (a sandboxByCreated) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a sandboxByCreated) Less(i, j int) bool { return a[i].createTime.After(a[j].createTime) }

// enforceMaxContainersPerEvictUnit enforces MaxPerPodContainer for each evictUnit.
// 通过CRI接口移除Pod中最老的N个已经死亡的容器
func (cgc *containerGC) enforceMaxContainersPerEvictUnit(ctx context.Context, evictUnits containersByEvictUnit, MaxContainers int) {
	for key := range evictUnits {
		toRemove := len(evictUnits[key]) - MaxContainers

		if toRemove > 0 {
			// 移除Pod中的最老的N个容器
			evictUnits[key] = cgc.removeOldestN(ctx, evictUnits[key], toRemove)
		}
	}
}

// removeOldestN removes the oldest toRemove containers and returns the resulting slice.
// 移除最老的N个容器
func (cgc *containerGC) removeOldestN(ctx context.Context, containers []containerGCInfo, toRemove int) []containerGCInfo {
	// Remove from oldest to newest (last to first).
	numToKeep := len(containers) - toRemove
	if numToKeep > 0 {
		//  如果需要保留的容器大于零个，由于函数的方法名为删除最牢的N个容器，因此这里需要给容器排序，通过容器的创建的时间排序，创建时间越早，就排在前面
		sort.Sort(byCreated(containers))
	}
	for i := len(containers) - 1; i >= numToKeep; i-- {
		// 如果容器的状态时unknown，需要先停止容器，然后才能移除容器
		if containers[i].unknown {
			// Containers in known state could be running, we should try
			// to stop it before removal.
			id := kubecontainer.ContainerID{
				Type: cgc.manager.runtimeName,
				ID:   containers[i].id,
			}
			message := "Container is in unknown state, try killing it before removal"
			// 先停止容器
			if err := cgc.manager.killContainer(ctx, nil, id, containers[i].name, message, reasonUnknown, nil); err != nil {
				klog.ErrorS(err, "Failed to stop container", "containerID", containers[i].id)
				continue
			}
		}

		// 移除容器
		if err := cgc.manager.removeContainer(ctx, containers[i].id); err != nil {
			klog.ErrorS(err, "Failed to remove container", "containerID", containers[i].id)
		}
	}

	// Assume we removed the containers so that we're not too aggressive.
	return containers[:numToKeep]
}

// removeOldestNSandboxes removes the oldest inactive toRemove sandboxes and
// returns the resulting slice.
func (cgc *containerGC) removeOldestNSandboxes(ctx context.Context, sandboxes []sandboxGCInfo, toRemove int) {
	numToKeep := len(sandboxes) - toRemove
	if numToKeep > 0 {
		sort.Sort(sandboxByCreated(sandboxes))
	}
	// Remove from oldest to newest (last to first).
	for i := len(sandboxes) - 1; i >= numToKeep; i-- {
		if !sandboxes[i].active {
			cgc.removeSandbox(ctx, sandboxes[i].id)
		}
	}
}

// removeSandbox removes the sandbox by sandboxID.
func (cgc *containerGC) removeSandbox(ctx context.Context, sandboxID string) {
	klog.V(4).InfoS("Removing sandbox", "sandboxID", sandboxID)
	// In normal cases, kubelet should've already called StopPodSandbox before
	// GC kicks in. To guard against the rare cases where this is not true, try
	// stopping the sandbox before removing it.
	if err := cgc.client.StopPodSandbox(ctx, sandboxID); err != nil {
		klog.ErrorS(err, "Failed to stop sandbox before removing", "sandboxID", sandboxID)
		return
	}
	if err := cgc.client.RemovePodSandbox(ctx, sandboxID); err != nil {
		klog.ErrorS(err, "Failed to remove sandbox", "sandboxID", sandboxID)
	}
}

// evictableContainers gets all containers that are evictable. Evictable containers are: not running
// and created more than MinAge ago.
// 1、获取当前Node节点上所有不是运行中的容器，并且此容器的年龄已经超过了minAge
// 2、返回值为一个Map,key为PodID,value为这个Pod中的所有容器
func (cgc *containerGC) evictableContainers(ctx context.Context, minAge time.Duration) (containersByEvictUnit, error) {
	// 通过CRI接口查询容器运行时获取当前Node的所有容器
	containers, err := cgc.manager.getKubeletContainers(ctx, true)
	if err != nil {
		return containersByEvictUnit{}, err
	}

	evictUnits := make(containersByEvictUnit)
	newestGCTime := time.Now().Add(-minAge)
	for _, container := range containers {
		// 如果当前容器正在运行，肯定不能驱逐
		if container.State == runtimeapi.ContainerState_CONTAINER_RUNNING {
			continue
		}

		createdAt := time.Unix(0, container.CreatedAt)
		// 如果当前容器才被创建出来一会儿，还没有过最小生存时间，则直接跳过此容器，暂时不用移除该容器
		if newestGCTime.Before(createdAt) {
			continue
		}

		// 除此之外的所有容器，由于各种各样的原因没有处于运行当中，且已经过了容器的最小生存时间。

		// 获取容器的信息，譬如PodName, Namespace, PodUID以及容器名字
		labeledInfo := getContainerInfoFromLabels(container.Labels)
		containerInfo := containerGCInfo{
			id:         container.Id,
			name:       container.Metadata.Name,
			createTime: createdAt,
			unknown:    container.State == runtimeapi.ContainerState_CONTAINER_UNKNOWN,
		}
		key := evictUnit{
			uid:  labeledInfo.PodUID,
			name: containerInfo.name,
		}
		evictUnits[key] = append(evictUnits[key], containerInfo)
	}

	return evictUnits, nil
}

// evict all containers that are evictable
func (cgc *containerGC) evictContainers(ctx context.Context, gcPolicy kubecontainer.GCPolicy, allSourcesReady bool, evictNonDeletedPods bool) error {
	// 1、获取当前Node节点上所有不是运行中的容器，并且此容器的年龄已经超过了minAge
	// 2、返回值为一个Map,key为PodID,value为这个Pod中的所有容器
	evictUnits, err := cgc.evictableContainers(ctx, gcPolicy.MinAge)
	if err != nil {
		return err
	}

	// Remove deleted pod containers if all sources are ready.
	// 如果HTTP, FILE, APIServer源都已经就绪
	if allSourcesReady {
		for key, unit := range evictUnits {
			// 如果当前的Pod需要被移除，那么直接移除这个Pod的所有容器
			if cgc.podStateProvider.ShouldPodContentBeRemoved(key.uid) || (evictNonDeletedPods && cgc.podStateProvider.ShouldPodRuntimeBeRemoved(key.uid)) {
				cgc.removeOldestN(ctx, unit, len(unit)) // Remove all.
				delete(evictUnits, key)
			}
		}
	}

	// Enforce max containers per evict unit.
	// 如果设置了每个Pod允许死亡的容器数量，那么过CRI接口移除Pod中最老的N个已经死亡的容器
	if gcPolicy.MaxPerPodContainer >= 0 {
		cgc.enforceMaxContainersPerEvictUnit(ctx, evictUnits, gcPolicy.MaxPerPodContainer)
	}

	// Enforce max total number of containers.
	// 如果设置了Node节点最多允许死亡的容器数量，并且当前已经死亡的容器数量超过了这个限制，那么就需要移除一些比较老的容器
	if gcPolicy.MaxContainers >= 0 && evictUnits.NumContainers() > gcPolicy.MaxContainers {
		// Leave an equal number of containers per evict unit (min: 1).
		// 平摊下来，每个Pod删除一定的死亡容器就行了
		numContainersPerEvictUnit := gcPolicy.MaxContainers / evictUnits.NumEvictUnits()
		if numContainersPerEvictUnit < 1 {
			numContainersPerEvictUnit = 1
		}
		// 每个Pod都移除最老的N个容器
		cgc.enforceMaxContainersPerEvictUnit(ctx, evictUnits, numContainersPerEvictUnit)

		// If we still need to evict, evict oldest first.
		// 如果每个Pod都删除了一些死亡的容器，此时还是超出数量，那么就需要直接移除最先死亡的N个容器
		numContainers := evictUnits.NumContainers()
		if numContainers > gcPolicy.MaxContainers {
			flattened := make([]containerGCInfo, 0, numContainers)
			for key := range evictUnits {
				flattened = append(flattened, evictUnits[key]...)
			}
			sort.Sort(byCreated(flattened))

			cgc.removeOldestN(ctx, flattened, numContainers-gcPolicy.MaxContainers)
		}
	}
	return nil
}

// evictSandboxes remove all evictable sandboxes. An evictable sandbox must
// meet the following requirements:
//  1. not in ready state
//  2. contains no containers.
//  3. belong to a non-existent (i.e., already removed) pod, or is not the
//     most recently created sandbox for the pod.
func (cgc *containerGC) evictSandboxes(ctx context.Context, evictNonDeletedPods bool) error {
	// 获取所有的容器
	containers, err := cgc.manager.getKubeletContainers(ctx, true)
	if err != nil {
		return err
	}

	// 获取所有的沙箱
	sandboxes, err := cgc.manager.getKubeletSandboxes(ctx, true)
	if err != nil {
		return err
	}

	// collect all the PodSandboxId of container
	sandboxIDs := sets.NewString()
	// 保存所有运行中的容器的沙箱的ID，显然这些沙箱是万万不能删除的
	for _, container := range containers {
		sandboxIDs.Insert(container.PodSandboxId)
	}

	sandboxesByPod := make(sandboxesByPodUID)
	// 遍历所有的沙箱，如果当前沙箱处于Ready状态或者是有容器正在沙箱中运行，那么就把当前沙箱设置为激活状态，这种状态的沙箱时不能删除的，
	// 而所有并非处于激活状态的沙箱就需要删除
	for _, sandbox := range sandboxes {
		podUID := types.UID(sandbox.Metadata.Uid)
		sandboxInfo := sandboxGCInfo{
			id:         sandbox.Id,
			createTime: time.Unix(0, sandbox.CreatedAt),
		}

		// Set ready sandboxes to be active.
		if sandbox.State == runtimeapi.PodSandboxState_SANDBOX_READY {
			sandboxInfo.active = true
		}

		// Set sandboxes that still have containers to be active.
		if sandboxIDs.Has(sandbox.Id) {
			sandboxInfo.active = true
		}

		sandboxesByPod[podUID] = append(sandboxesByPod[podUID], sandboxInfo)
	}

	// 遍历所有的沙箱,把所有不是处于运行中的沙箱移除
	for podUID, sandboxes := range sandboxesByPod {
		if cgc.podStateProvider.ShouldPodContentBeRemoved(podUID) || (evictNonDeletedPods && cgc.podStateProvider.ShouldPodRuntimeBeRemoved(podUID)) {
			// Remove all evictable sandboxes if the pod has been removed.
			// Note that the latest dead sandbox is also removed if there is
			// already an active one.
			cgc.removeOldestNSandboxes(ctx, sandboxes, len(sandboxes))
		} else {
			// Keep latest one if the pod still exists.
			cgc.removeOldestNSandboxes(ctx, sandboxes, len(sandboxes)-1)
		}
	}
	return nil
}

// evictPodLogsDirectories evicts all evictable pod logs directories. Pod logs directories
// are evictable if there are no corresponding pods.
func (cgc *containerGC) evictPodLogsDirectories(ctx context.Context, allSourcesReady bool) error {
	osInterface := cgc.manager.osInterface
	if allSourcesReady {
		// Only remove pod logs directories when all sources are ready.
		dirs, err := osInterface.ReadDir(podLogsRootDirectory)
		if err != nil {
			return fmt.Errorf("failed to read podLogsRootDirectory %q: %v", podLogsRootDirectory, err)
		}
		for _, dir := range dirs {
			name := dir.Name()
			// 目录名为：/var/log/pods/NAMESPACE_NAME_UID，所以可以解析出容器的名字、名称空间、UID
			podUID := parsePodUIDFromLogsDirectory(name)
			if !cgc.podStateProvider.ShouldPodContentBeRemoved(podUID) {
				continue
			}
			klog.V(4).InfoS("Removing pod logs", "podUID", podUID)
			err := osInterface.RemoveAll(filepath.Join(podLogsRootDirectory, name))
			if err != nil {
				klog.ErrorS(err, "Failed to remove pod logs directory", "path", name)
			}
		}
	}

	// Remove dead container log symlinks.
	// TODO(random-liu): Remove this after cluster logging supports CRI container log path.
	logSymlinks, _ := osInterface.Glob(filepath.Join(legacyContainerLogsDir, fmt.Sprintf("*.%s", legacyLogSuffix)))
	for _, logSymlink := range logSymlinks {
		if _, err := osInterface.Stat(logSymlink); os.IsNotExist(err) {
			if containerID, err := getContainerIDFromLegacyLogSymlink(logSymlink); err == nil {
				resp, err := cgc.manager.runtimeService.ContainerStatus(ctx, containerID, false)
				if err != nil {
					// TODO: we should handle container not found (i.e. container was deleted) case differently
					// once https://github.com/kubernetes/kubernetes/issues/63336 is resolved
					klog.InfoS("Error getting ContainerStatus for containerID", "containerID", containerID, "err", err)
				} else {
					status := resp.GetStatus()
					if status == nil {
						klog.V(4).InfoS("Container status is nil")
						continue
					}
					if status.State != runtimeapi.ContainerState_CONTAINER_EXITED {
						// Here is how container log rotation works (see containerLogManager#rotateLatestLog):
						//
						// 1. rename current log to rotated log file whose filename contains current timestamp (fmt.Sprintf("%s.%s", log, timestamp))
						// 2. reopen the container log
						// 3. if #2 fails, rename rotated log file back to container log
						//
						// There is small but indeterministic amount of time during which log file doesn't exist (between steps #1 and #2, between #1 and #3).
						// Hence the symlink may be deemed unhealthy during that period.
						// See https://github.com/kubernetes/kubernetes/issues/52172
						//
						// We only remove unhealthy symlink for dead containers
						klog.V(5).InfoS("Container is still running, not removing symlink", "containerID", containerID, "path", logSymlink)
						continue
					}
				}
			} else {
				klog.V(4).InfoS("Unable to obtain container ID", "err", err)
			}
			err := osInterface.Remove(logSymlink)
			if err != nil {
				klog.ErrorS(err, "Failed to remove container log dead symlink", "path", logSymlink)
			} else {
				klog.V(4).InfoS("Removed symlink", "path", logSymlink)
			}
		}
	}
	return nil
}

// GarbageCollect removes dead containers using the specified container gc policy.
// Note that gc policy is not applied to sandboxes. Sandboxes are only removed when they are
// not ready and containing no containers.
//
// GarbageCollect consists of the following steps:
// * gets evictable containers which are not active and created more than gcPolicy.MinAge ago.
// * removes oldest dead containers for each pod by enforcing gcPolicy.MaxPerPodContainer.
// * removes oldest dead containers by enforcing gcPolicy.MaxContainers.
// * gets evictable sandboxes which are not ready and contains no containers.
// * removes evictable sandboxes.
// 1、根据容器回收策略删除满足条件的并且已经死亡的容器
// 2、容器删除之后，遍历所有的沙箱，如果这个沙箱下已经没有任何容器在运行了，那么删除这个沙箱
// 3、容器删除之后，容器的日志也就没有保留的必要了
func (cgc *containerGC) GarbageCollect(
	ctx context.Context,
	gcPolicy kubecontainer.GCPolicy, // 容器回收策略
	allSourcesReady bool, // 是否HTTP, Static, APIServer这三个源都已经同步完成
	evictNonDeletedPods bool, // 是否允许驱逐没有被删除的Pod
) error {
	ctx, otelSpan := cgc.tracer.Start(ctx, "Containers/GarbageCollect")
	defer otelSpan.End()
	var errors []error
	// Remove evictable containers
	// 按照容器回收策略移除所有处于非运行状态的容器
	if err := cgc.evictContainers(ctx, gcPolicy, allSourcesReady, evictNonDeletedPods); err != nil {
		errors = append(errors, err)
	}

	// Remove sandboxes with zero containers
	// 移除无用的沙箱，这里的Sandbox就是Infra/Pause容器
	if err := cgc.evictSandboxes(ctx, evictNonDeletedPods); err != nil {
		errors = append(errors, err)
	}

	// Remove pod sandbox log directory
	// 容器被删除了，那么记录这个容器日志也就没啥用了，直接删除
	if err := cgc.evictPodLogsDirectories(ctx, allSourcesReady); err != nil {
		errors = append(errors, err)
	}
	return utilerrors.NewAggregate(errors)
}
