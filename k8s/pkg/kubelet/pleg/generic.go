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

package pleg

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/features"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/utils/clock"
)

// GenericPLEG is an extremely simple generic PLEG that relies solely on
// periodic listing to discover container changes. It should be used
// as temporary replacement for container runtimes do not support a proper
// event generator yet.
//
// Note that GenericPLEG assumes that a container would not be created,
// terminated, and garbage collected within one relist period. If such an
// incident happens, GenenricPLEG would miss all events regarding this
// container. In the case of relisting failure, the window may become longer.
// Note that this assumption is not unique -- many kubelet internal components
// rely on terminated containers as tombstones for bookkeeping purposes. The
// garbage collector is implemented to work with such situations. However, to
// guarantee that kubelet can handle missing container events, it is
// recommended to set the relist period short and have an auxiliary, longer
// periodic sync in kubelet as the safety net.
// PLEG就是通过前后两个时刻的容器状态，从而生成容器的生命周期事件，并把事件写入到eventChannel当中
type GenericPLEG struct {
	// The container runtime.
	// 容器运行时的抽象，底层就是在调用CRI接口
	runtime kubecontainer.Runtime
	// The channel from which the subscriber listens events.
	// 1、eventChannel当中存放的数据是每个Pod的容器的生命周期事件，而data属性为ContainerID
	// 2、channel深度为1000
	// 3、外部组件通过调用GenericPLEG.Watch()获取到这个channel，并监听这个channel，从而获取Pod生命周期事件
	eventChannel chan *PodLifecycleEvent
	// 1、用于记录K8S Pod前后两个时间点的状态， 从而可以通过对比前后两个时间点之间的变化得出Pod的状态
	// 2、这里缓存的是当前运行Kubelet进程节点的所有容器
	podRecords podRecords
	// Time of the last relisting.
	// 1、PLEG每次relist成功之后都会更新relistTime，从而说明PLEG是否正常工作，说白了就是健康检测
	// 2、PLEG的健康阈值为10分钟，如果每隔10分钟PLEG都没有重新relist，说明PLEG处于非健康的工作状态
	relistTime atomic.Value
	// Cache for storing the runtime states required for syncing pods.
	// 缓存Pod状态，用于获取Pod的状态，或者获取比指定时间更新的Pod状态
	cache kubecontainer.Cache
	// For testability.
	clock clock.Clock
	// Pods that failed to have their status retrieved during a relist. These pods will be
	// retried during the next relisting.
	// 需要重新检视的容器都是之前出错的Pod，所以需要通过CRI接口重新获取一次最新的Pod状态
	podsToReinspect map[types.UID]*kubecontainer.Pod
	// Stop the Generic PLEG by closing the channel.
	// 用于停止运行PLEG
	stopCh chan struct{}
	// Locks the relisting of the Generic PLEG
	relistLock sync.Mutex
	// Indicates if the Generic PLEG is running or not
	// 用于标识当前PLEG是否处于运行当汇总
	isRunning bool
	// Locks the start/stop operation of Generic PLEG
	runningMu sync.Mutex
	// Indicates relisting related parameters
	// 用于设置PLEG的relist周期以及relist阈值
	relistDuration *RelistDuration
	// Mutex to serialize updateCache called by relist vs UpdateCache interface
	podCacheMutex sync.Mutex
}

// plegContainerState has a one-to-one mapping to the
// kubecontainer.State except for the non-existent state. This state
// is introduced here to complete the state transition scenarios.
type plegContainerState string

const (
	plegContainerRunning     plegContainerState = "running"
	plegContainerExited      plegContainerState = "exited"
	plegContainerUnknown     plegContainerState = "unknown"
	plegContainerNonExistent plegContainerState = "non-existent"
)

func convertState(state kubecontainer.State) plegContainerState {
	switch state {
	case kubecontainer.ContainerStateCreated:
		// kubelet doesn't use the "created" state yet, hence convert it to "unknown".
		return plegContainerUnknown
	case kubecontainer.ContainerStateRunning:
		return plegContainerRunning
	case kubecontainer.ContainerStateExited:
		return plegContainerExited
	case kubecontainer.ContainerStateUnknown:
		return plegContainerUnknown
	default:
		panic(fmt.Sprintf("unrecognized container state: %v", state))
	}
}

type podRecord struct {
	old     *kubecontainer.Pod // 上一次的状态
	current *kubecontainer.Pod // 当前状态，显然一旦PLEG执行Relist动作，那么就需要把current赋值给old，然后把当前时刻的Pod状态复制给current
}

// 用于记录一个Pod的前后两个时刻的状态，对比此有意义。因为要得出一个事物的状态，一定是前后两个时刻的对比从而产生的结果，因此我们需要记录
// 一个Pod前后两次的所有属性，才能对比出这个Pod在前后两个时间点之间发生了什么
type podRecords map[types.UID]*podRecord

// NewGenericPLEG instantiates a new GenericPLEG object and return it.
// PLEG就是通过前后两个时刻的容器状态，从而生成容器的生命周期事件，并把事件写入到eventChannel当中
func NewGenericPLEG(
	runtime kubecontainer.Runtime, // CRI运行时接口
	eventChannel chan *PodLifecycleEvent, // 事件channel
	relistDuration *RelistDuration,
	cache kubecontainer.Cache,
	clock clock.Clock,
) PodLifecycleEventGenerator {
	return &GenericPLEG{
		relistDuration: relistDuration,
		runtime:        runtime,
		eventChannel:   eventChannel,
		podRecords:     make(podRecords),
		cache:          cache,
		clock:          clock,
	}
}

// Watch returns a channel from which the subscriber can receive PodLifecycleEvent
// events.
// TODO: support multiple subscribers.
func (g *GenericPLEG) Watch() chan *PodLifecycleEvent {
	// 返回事件Channel, 这个channel的缓存深度为1000
	return g.eventChannel
}

// Start spawns a goroutine to relist periodically.
func (g *GenericPLEG) Start() {
	g.runningMu.Lock()
	defer g.runningMu.Unlock()
	// PLEG只需要启动一次即可
	if !g.isRunning {
		g.isRunning = true
		// 初始化停止Channel
		g.stopCh = make(chan struct{})
		// 每300秒执行一次
		go wait.Until(g.Relist, g.relistDuration.RelistPeriod, g.stopCh)
	}
}

func (g *GenericPLEG) Stop() {
	g.runningMu.Lock()
	defer g.runningMu.Unlock()
	if g.isRunning {
		close(g.stopCh)
		g.isRunning = false
	}
}

func (g *GenericPLEG) Update(relistDuration *RelistDuration) {
	g.relistDuration = relistDuration
}

// Healthy check if PLEG work properly.
// relistThreshold is the maximum interval between two relist.
func (g *GenericPLEG) Healthy() (bool, error) {
	relistTime := g.getRelistTime()
	if relistTime.IsZero() {
		return false, fmt.Errorf("pleg has yet to be successful")
	}
	// Expose as metric so you can alert on `time()-pleg_last_seen_seconds > nn`
	metrics.PLEGLastSeen.Set(float64(relistTime.Unix()))
	elapsed := g.clock.Since(relistTime)
	// 如果PLEG启动后的每隔10分钟都没有重新relist，说明PLEG的运行出了问题
	if elapsed > g.relistDuration.RelistThreshold {
		return false, fmt.Errorf("pleg was last seen active %v ago; threshold is %v", elapsed, g.relistDuration.RelistThreshold)
	}
	return true, nil
}

func generateEvents(podID types.UID, cid string, oldState, newState plegContainerState) []*PodLifecycleEvent {
	// 如果新旧状态相等，那么就认为当前容器没有发生变化
	if newState == oldState {
		return nil
	}

	klog.V(4).InfoS("GenericPLEG", "podUID", podID, "containerID", cid, "oldState", oldState, "newState", newState)
	switch newState {
	case plegContainerRunning:
		return []*PodLifecycleEvent{{ID: podID, Type: ContainerStarted, Data: cid}}
	case plegContainerExited:
		return []*PodLifecycleEvent{{ID: podID, Type: ContainerDied, Data: cid}}
	case plegContainerUnknown:
		return []*PodLifecycleEvent{{ID: podID, Type: ContainerChanged, Data: cid}}
	case plegContainerNonExistent:
		switch oldState {
		case plegContainerExited:
			// We already reported that the container died before.
			return []*PodLifecycleEvent{{ID: podID, Type: ContainerRemoved, Data: cid}}
		default:
			return []*PodLifecycleEvent{{ID: podID, Type: ContainerDied, Data: cid}, {ID: podID, Type: ContainerRemoved, Data: cid}}
		}
	default:
		panic(fmt.Sprintf("unrecognized container state: %v", newState))
	}
}

func (g *GenericPLEG) getRelistTime() time.Time {
	val := g.relistTime.Load()
	if val == nil {
		return time.Time{}
	}
	return val.(time.Time)
}

func (g *GenericPLEG) updateRelistTime(timestamp time.Time) {
	g.relistTime.Store(timestamp)
}

// Relist queries the container runtime for list of pods/containers, compare
// with the internal pods/containers, and generates events accordingly.
// 1、默认每300秒执行一次
func (g *GenericPLEG) Relist() {
	g.relistLock.Lock()
	defer g.relistLock.Unlock()

	ctx := context.Background()
	klog.V(5).InfoS("GenericPLEG: Relisting")

	// 获取上一次重新扫描的时间
	if lastRelistTime := g.getRelistTime(); !lastRelistTime.IsZero() {
		metrics.PLEGRelistInterval.Observe(metrics.SinceInSeconds(lastRelistTime))
	}

	timestamp := g.clock.Now()
	defer func() {
		metrics.PLEGRelistDuration.Observe(metrics.SinceInSeconds(timestamp))
	}()

	// Get all the pods.
	// 通过CRI接口获取当前运行Kubelet的节点上所有运行的Pod，这里的数据是通过查询Sandbox以及容器组合出来的
	podList, err := g.runtime.GetPods(ctx, true)
	if err != nil {
		klog.ErrorS(err, "GenericPLEG: Unable to retrieve pods")
		return
	}

	// 更新relistTime属性
	g.updateRelistTime(timestamp)

	pods := kubecontainer.Pods(podList)
	// update running pod and container count
	// 更新当前当前节点正在运行的容器数量指标以及正在运行的沙箱数量指标
	updateRunningPodAndContainerMetrics(pods)
	// 更新PodRecord的current属性
	g.podRecords.setCurrent(pods)

	// Compare the old and the current pods, and generate events.
	// Key为PodID，value为容器的生命周期事件
	eventsByPodID := map[types.UID][]*PodLifecycleEvent{}
	for pid := range g.podRecords {
		oldPod := g.podRecords.getOld(pid)
		pod := g.podRecords.getCurrent(pid)
		// 合并current以及old两个时刻的所有容器，然后在current以及old状态中对比，从而得出当前容器的状态，到底是创建、删除还是修改、死亡等等
		allContainers := getContainersFromPods(oldPod, pod)
		for _, container := range allContainers {
			// 对比两个时刻的容器的状态，从而计算出当前的容器的事件
			events := computeEvents(oldPod, pod, &container.ID)
			for _, e := range events {
				// events是以容器的角度获取到的事件，而这里需要以Pod为单位合并所有的容器事件
				updateEvents(eventsByPodID, e)
			}
		}
	}

	var needsReinspection map[types.UID]*kubecontainer.Pod
	if g.cacheEnabled() {
		needsReinspection = make(map[types.UID]*kubecontainer.Pod)
	}

	// If there are events associated with a pod, we should update the
	// podCache.
	// 遍历当前所有产生了容器事件的所有Pod
	for pid, events := range eventsByPodID {
		// 获取Pod的当前状态
		pod := g.podRecords.getCurrent(pid)
		if g.cacheEnabled() {
			// updateCache() will inspect the pod and update the cache. If an
			// error occurs during the inspection, we want PLEG to retry again
			// in the next relist. To achieve this, we do not update the
			// associated podRecord of the pod, so that the change will be
			// detect again in the next relist.
			// TODO: If many pods changed during the same relist period,
			// inspecting the pod and getting the PodStatus to update the cache
			// serially may take a while. We should be aware of this and
			// parallelize if needed.
			// 更新PodCache缓存
			if err, updated := g.updateCache(ctx, pod, pid); err != nil {
				// Rely on updateCache calling GetPodStatus to log the actual error.
				klog.V(4).ErrorS(err, "PLEG: Ignoring events for pod", "pod", klog.KRef(pod.Namespace, pod.Name))

				// make sure we try to reinspect the pod during the next relisting
				// 出错了就需要重新检查
				needsReinspection[pid] = pod

				continue
			} else {
				// this pod was in the list to reinspect and we did so because it had events, so remove it
				// from the list (we don't want the reinspection code below to inspect it a second time in
				// this relist execution)
				// 没有出错就不需要重新检查
				delete(g.podsToReinspect, pid)
				if utilfeature.DefaultFeatureGate.Enabled(features.EventedPLEG) {
					if !updated {
						continue
					}
				}
			}
		}
		// 更新PodRecord, 把current赋值给old，然后把current置为空，下一次relist的时候会重新赋值current
		g.podRecords.update(pid)

		// Map from containerId to exit code; used as a temporary cache for lookup
		containerExitCode := make(map[string]int)

		for i := range events { // 遍历Pod的容器事件
			// Filter out events that are not reliable and no other components use yet.
			// ContainerChanged事件是由于当前的容器处于Unknown状态造成的，所以跳过
			if events[i].Type == ContainerChanged {
				continue
			}
			select {
			case g.eventChannel <- events[i]: // 把容器事件写入到channel当中
			default:
				metrics.PLEGDiscardEvents.Inc()
				klog.ErrorS(nil, "Event channel is full, discard this relist() cycle event")
			}
			// Log exit code of containers when they finished in a particular event
			// 如果当前的容器通过对比前后状态，被认为是死亡
			if events[i].Type == ContainerDied {
				// Fill up containerExitCode map for ContainerDied event when first time appeared
				if len(containerExitCode) == 0 && pod != nil && g.cache != nil {
					// Get updated podStatus
					status, err := g.cache.Get(pod.ID)
					if err == nil {
						for _, containerStatus := range status.ContainerStatuses {
							containerExitCode[containerStatus.ID.ID] = containerStatus.ExitCode
						}
					}
				}
				if containerID, ok := events[i].Data.(string); ok {
					if exitCode, ok := containerExitCode[containerID]; ok && pod != nil {
						klog.V(2).InfoS("Generic (PLEG): container finished", "podID", pod.ID, "containerID", containerID, "exitCode", exitCode)
					}
				}
			}
		}
	}

	if g.cacheEnabled() {
		// reinspect any pods that failed inspection during the previous relist
		// 如果Pod在更新状态的时候出错了，那么需要重新更新一次
		if len(g.podsToReinspect) > 0 {
			klog.V(5).InfoS("GenericPLEG: Reinspecting pods that previously failed inspection")
			for pid, pod := range g.podsToReinspect {
				if err, _ := g.updateCache(ctx, pod, pid); err != nil {
					// Rely on updateCache calling GetPodStatus to log the actual error.
					klog.V(5).ErrorS(err, "PLEG: pod failed reinspection", "pod", klog.KRef(pod.Namespace, pod.Name))
					// 还是出错
					needsReinspection[pid] = pod
				}
			}
		}

		// Update the cache timestamp.  This needs to happen *after*
		// all pods have been properly updated in the cache.
		g.cache.UpdateTime(timestamp)
	}

	// make sure we retain the list of pods that need reinspecting the next time relist is called
	g.podsToReinspect = needsReinspection
}

func getContainersFromPods(pods ...*kubecontainer.Pod) []*kubecontainer.Container {
	cidSet := sets.NewString()
	var containers []*kubecontainer.Container
	fillCidSet := func(cs []*kubecontainer.Container) {
		for _, c := range cs {
			cid := c.ID.ID
			if cidSet.Has(cid) {
				continue
			}
			cidSet.Insert(cid)
			containers = append(containers, c)
		}
	}

	for _, p := range pods {
		if p == nil {
			continue
		}
		fillCidSet(p.Containers)
		// Update sandboxes as containers
		// TODO: keep track of sandboxes explicitly.
		fillCidSet(p.Sandboxes)
	}
	return containers
}

func computeEvents(oldPod, newPod *kubecontainer.Pod, cid *kubecontainer.ContainerID) []*PodLifecycleEvent {
	var pid types.UID
	// PodID一定可以从current和old总的一个状态当中获取到
	if oldPod != nil {
		pid = oldPod.ID
	} else if newPod != nil {
		pid = newPod.ID
	}
	oldState := getContainerState(oldPod, cid)
	newState := getContainerState(newPod, cid)
	return generateEvents(pid, cid.ID, oldState, newState)
}

func (g *GenericPLEG) cacheEnabled() bool {
	return g.cache != nil
}

// getPodIP preserves an older cached status' pod IP if the new status has no pod IPs
// and its sandboxes have exited
func (g *GenericPLEG) getPodIPs(pid types.UID, status *kubecontainer.PodStatus) []string {
	if len(status.IPs) != 0 {
		return status.IPs
	}

	oldStatus, err := g.cache.Get(pid)
	if err != nil || len(oldStatus.IPs) == 0 {
		return nil
	}

	for _, sandboxStatus := range status.SandboxStatuses {
		// If at least one sandbox is ready, then use this status update's pod IP
		if sandboxStatus.State == runtimeapi.PodSandboxState_SANDBOX_READY {
			return status.IPs
		}
	}

	// For pods with no ready containers or sandboxes (like exited pods)
	// use the old status' pod IP
	return oldStatus.IPs
}

// updateCache tries to update the pod status in the kubelet cache and returns true if the
// pod status was actually updated in the cache. It will return false if the pod status
// was ignored by the cache.
func (g *GenericPLEG) updateCache(ctx context.Context, pod *kubecontainer.Pod, pid types.UID) (error, bool) {
	// Pod为空，说明当前Pod已经被删除
	if pod == nil {
		// The pod is missing in the current relist. This means that
		// the pod has no visible (active or inactive) containers.
		klog.V(4).InfoS("PLEG: Delete status for pod", "podUID", string(pid))
		g.cache.Delete(pid)
		return nil, true
	}

	g.podCacheMutex.Lock()
	defer g.podCacheMutex.Unlock()
	timestamp := g.clock.Now()

	// 查询CRI，获取当前Pod的状态
	status, err := g.runtime.GetPodStatus(ctx, pod.ID, pod.Name, pod.Namespace)
	if err != nil {
		// nolint:logcheck // Not using the result of klog.V inside the
		// if branch is okay, we just use it to determine whether the
		// additional "podStatus" key and its value should be added.
		if klog.V(6).Enabled() {
			klog.ErrorS(err, "PLEG: Write status", "pod", klog.KRef(pod.Namespace, pod.Name), "podStatus", status)
		} else {
			klog.ErrorS(err, "PLEG: Write status", "pod", klog.KRef(pod.Namespace, pod.Name))
		}
	} else {
		if klogV := klog.V(6); klogV.Enabled() {
			klogV.InfoS("PLEG: Write status", "pod", klog.KRef(pod.Namespace, pod.Name), "podStatus", status)
		} else {
			klog.V(4).InfoS("PLEG: Write status", "pod", klog.KRef(pod.Namespace, pod.Name))
		}
		// Preserve the pod IP across cache updates if the new IP is empty.
		// When a pod is torn down, kubelet may race with PLEG and retrieve
		// a pod status after network teardown, but the kubernetes API expects
		// the completed pod's IP to be available after the pod is dead.
		// 获取Pod所有的IP
		status.IPs = g.getPodIPs(pid, status)
	}

	// When we use Generic PLEG only, the PodStatus is saved in the cache without
	// any validation of the existing status against the current timestamp.
	// This works well when there is only Generic PLEG setting the PodStatus in the cache however,
	// if we have multiple entities, such as Evented PLEG, while trying to set the PodStatus in the
	// cache we may run into the racy timestamps given each of them were to calculate the timestamps
	// in their respective execution flow. While Generic PLEG calculates this timestamp and gets
	// the PodStatus, we can only calculate the corresponding timestamp in
	// Evented PLEG after the event has been received by the Kubelet.
	// For more details refer to:
	// https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/3386-kubelet-evented-pleg#timestamp-of-the-pod-status
	if utilfeature.DefaultFeatureGate.Enabled(features.EventedPLEG) && isEventedPLEGInUse() {
		timestamp = status.TimeStamp
	}

	// 更新PodCache缓存
	return err, g.cache.Set(pod.ID, status, err, timestamp)
}

func (g *GenericPLEG) UpdateCache(pod *kubecontainer.Pod, pid types.UID) (error, bool) {
	ctx := context.Background()
	if !g.cacheEnabled() {
		return fmt.Errorf("pod cache disabled"), false
	}
	if pod == nil {
		return fmt.Errorf("pod cannot be nil"), false
	}
	return g.updateCache(ctx, pod, pid)
}

func updateEvents(eventsByPodID map[types.UID][]*PodLifecycleEvent, e *PodLifecycleEvent) {
	if e == nil {
		return
	}
	eventsByPodID[e.ID] = append(eventsByPodID[e.ID], e)
}

func getContainerState(pod *kubecontainer.Pod, cid *kubecontainer.ContainerID) plegContainerState {
	// Default to the non-existent state.
	// 默认认为容器不存在
	state := plegContainerNonExistent
	if pod == nil {
		return state
	}
	// 先从Pod的Containers中获取容器
	c := pod.FindContainerByID(*cid)
	if c != nil {
		return convertState(c.State)
	}
	// Search through sandboxes too.
	// 如果没有获取到，那么就从沙箱中获取其状态
	c = pod.FindSandboxByID(*cid)
	if c != nil {
		return convertState(c.State)
	}

	return state
}

func updateRunningPodAndContainerMetrics(pods []*kubecontainer.Pod) {
	runningSandboxNum := 0
	// intermediate map to store the count of each "container_state"
	containerStateCount := make(map[string]int)

	for _, pod := range pods {
		containers := pod.Containers
		for _, container := range containers {
			// update the corresponding "container_state" in map to set value for the gaugeVec metrics
			containerStateCount[string(container.State)]++
		}

		sandboxes := pod.Sandboxes

		for _, sandbox := range sandboxes {
			if sandbox.State == kubecontainer.ContainerStateRunning {
				// 一个Pod虽然可能有多个Sandbox，但是只可能有一个正在运行的Sandbox
				runningSandboxNum++
				// every pod should only have one running sandbox
				break
			}
		}
	}
	for key, value := range containerStateCount {
		metrics.RunningContainerCount.WithLabelValues(key).Set(float64(value))
	}

	// Set the number of running pods in the parameter
	metrics.RunningPodCount.Set(float64(runningSandboxNum))
}

func (pr podRecords) getOld(id types.UID) *kubecontainer.Pod {
	r, ok := pr[id]
	if !ok {
		return nil
	}
	return r.old
}

func (pr podRecords) getCurrent(id types.UID) *kubecontainer.Pod {
	r, ok := pr[id]
	if !ok {
		return nil
	}
	return r.current
}

func (pr podRecords) setCurrent(pods []*kubecontainer.Pod) {
	// 把PodCache的current置空，因为参数pods是现在的current
	for i := range pr {
		pr[i].current = nil
	}
	for _, pod := range pods {
		if r, ok := pr[pod.ID]; ok {
			r.current = pod
		} else {
			// 第一次出现的话就实例化一个PodRecord,说明当前的Pod是刚刚创建
			pr[pod.ID] = &podRecord{current: pod}
		}
	}
	// 如果运行下来，有一个Pod的current=nil, old!=nil，说明这个Pod已经被删除
}

func (pr podRecords) update(id types.UID) {
	r, ok := pr[id]
	if !ok {
		return
	}
	pr.updateInternal(id, r)
}

func (pr podRecords) updateInternal(id types.UID, r *podRecord) {
	// 如果current还是为空，说明上一次这个Pod还存在，而当前这个pod已经不存在了，直接删除podRecord
	if r.current == nil {
		// Pod no longer exists; delete the entry.
		delete(pr, id)
		return
	}
	// current赋值给old,然后把current设置为空
	r.old = r.current
	r.current = nil
}
