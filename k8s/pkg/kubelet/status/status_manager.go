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

//go:generate mockgen -source=status_manager.go -destination=testing/mock_pod_status_provider.go -package=testing PodStatusProvider
package status

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	clientset "k8s.io/client-go/kubernetes"

	v1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/diff"
	"k8s.io/apimachinery/pkg/util/wait"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/features"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	kubepod "k8s.io/kubernetes/pkg/kubelet/pod"
	"k8s.io/kubernetes/pkg/kubelet/status/state"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	statusutil "k8s.io/kubernetes/pkg/util/pod"
)

// podStatusManagerStateFile is the file name where status manager stores its state
const podStatusManagerStateFile = "pod_status_manager_state"

// A wrapper around v1.PodStatus that includes a version to enforce that stale pod statuses are
// not sent to the API server.
type versionedPodStatus struct {
	// version is a monotonically increasing version number (per pod).
	// PodManager自己维护的版本，每次修改会自增一
	version uint64
	// Pod name & namespace, for sending updates to API server.
	podName      string
	podNamespace string
	// at is the time at which the most recent status update was detected
	at time.Time

	// True if the status is generated at the end of SyncTerminatedPod, or after it is completed.
	// Pod是否已经执行结束
	podIsFinished bool

	// Pod的最新的状态
	status v1.PodStatus
}

/* Pod状态示例如下
status:
  conditions:
  - lastProbeTime: null
    lastTransitionTime: "2023-06-13T01:54:54Z"
    status: "True"
    type: Initialized
  - lastProbeTime: null
    lastTransitionTime: "2023-06-15T02:58:13Z"
    status: "True"
    type: Ready
  - lastProbeTime: null
    lastTransitionTime: "2023-06-15T02:58:13Z"
    status: "True"
    type: ContainersReady
  - lastProbeTime: null
    lastTransitionTime: "2023-06-13T01:54:32Z"
    status: "True"
    type: PodScheduled
  containerStatuses:
  - containerID: docker://13aabc5a5cb8a5b11968d00e109dfb9d58bd057a8f5c764dc07ac321b581b8a5
    image: 172.30.3.150/k8s/kube-rbac-proxy:v0.8.0
    imageID: docker-pullable://172.30.3.150/k8s/kube-rbac-proxy@sha256:34e8724e0f47e31eb2ec3279ac398b657db5f60f167426ee73138e2e84af6486
    lastState: {}
    name: kube-rbac-proxy
    ready: true
    restartCount: 0
    started: true
    state:
      running:
        startedAt: "2023-06-13T01:54:57Z"
  - containerID: docker://55f5c9b0145d5aef9c7d6e471902f4c61d9ed275882b3e5855f0a6f9746739af
    image: 172.30.3.150/cloud/sk-gatorcloud-operator-manager:f36b66ab
    imageID: docker-pullable://172.30.3.150/cloud/sk-gatorcloud-operator-manager@sha256:0ed2e2b4b6d3377491d93c4103d13d3b9a26fa171e67c826da2330aca9f5038e
    lastState:
      terminated:
        containerID: docker://5d20106555ef399334004849452d39a489f212fe4693434739c9cb0e9ae6cb76
        exitCode: 1
        finishedAt: "2023-06-15T02:46:18Z"
        reason: Error
        startedAt: "2023-06-14T00:00:58Z"
    name: manager
    ready: true
    restartCount: 5
    started: true
    state:
      running:
        startedAt: "2023-06-15T02:49:15Z"
*/

// Updates pod statuses in apiserver. Writes only when new status has changed.
// All methods are thread-safe.
type manager struct {
	// ClientSet，将来StatusManager需要更新Pod状态时就通过kubeClient访问APIServer
	kubeClient clientset.Interface
	// 1、PodManager主要用于管理可以访问的Pod，并且维护StaticPod以及MirrorPod之间的映射关系
	// 2、所谓的StaticPod，实际上指的不是来资源APIServer的所有Pod，简单来说就是来资源File以及HTTP的Pod
	// 3、由于StaticPod是直接通过Kubelet运行的，因此APIServer无法感知StaticPod。为了能够让APIServer能够感知StaticPod，PodManager为
	// 每一个StaticPod创建了一个MirrorPod。并且StaticPod的状态会影响MirrorPod。如果StaticPod被删除了，那么也需要删除MirrorPod
	// 4、PodManager的实现非常简单，就是一个map缓存，缓存了常规Pod以及MirrorPod
	// 5、PodManager的数据来源就是syncLoop,syncLoop对于感知到的所有Pod的增删改查都会维护PodManager
	// 6、PodManager缓存了最新的容器状态，PodManager缓存的Pod来自于APIServer，所以这些Pod是用于期望的Pod状态
	podManager kubepod.Manager
	// 1、缓存Pod的状态，Key为PodUID
	// 2、podStatuses中缓存的Pod状态为底层CRI Pod的实际状态，有PodWorker进行修改
	// 3、PodStatuses中缓存了Pod的最新状态，每次状态更新时，都会在老状态的版本上加一，而apiStatusVersions则保存的是StatusManager上报给
	// APIServer的最新状态，因此只要apiStatusVersions保存的版本比podStatuses中保存的版本低，StatusManager就需要上报Pod的状态
	podStatuses     map[types.UID]versionedPodStatus
	podStatusesLock sync.RWMutex
	// 1、StatusManager.SetContainerReadiness方法会调用StatusManager.updateStatusInternal方法写入数据
	// 2、StatusManager.SetContainerStartup方法会调用StatusManager.updateStatusInternal方法写入数据
	// 3、StatusManager.SetPodStatus方法会调用StatusManager.updateStatusInternal方法写入数据
	// 4、StatusManager.TerminatePod方法会调用StatusManager.updateStatusInternal方法写入数据
	// 5、当其它组件通过调用StatusManager的这些方法时，StatusManager就会把消息发送到这个Channel当中
	podStatusChannel chan struct{}
	// Map from (mirror) pod UID to latest status version successfully sent to the API server.
	// apiStatusVersions must only be accessed from the sync thread.
	// 1、apiStatusVersions缓存了StatusManager上报给APIServer的状态，可以理解为apiStatusVersions缓存了APIServer中每个Pod状态版本
	// 2、而podStatuses则缓存了真实Pod的最新版本，只要podStatuses中的版本大于apiStatusVersions中缓存的版本，我们就需要上报APIServer这个
	// Pod最新的状态
	apiStatusVersions map[kubetypes.MirrorPodUID]uint64
	// 如果Pod还有正在运行的容器，那么就不允许删除
	podDeletionSafety PodDeletionSafetyProvider

	// 用于记录一些指标
	podStartupLatencyHelper PodStartupLatencyStateHelper
	// state allows to save/restore pod resource allocation and tolerate kubelet restarts.
	// 用于记录Pod每个容器的资源分配大小，以及Pod资源分配状态
	state state.State
	// stateFileDirectory holds the directory where the state file for checkpoints is held.
	stateFileDirectory string
}

// PodStatusProvider knows how to provide status for a pod. It's intended to be used by other components
// that need to introspect status.
type PodStatusProvider interface {
	// GetPodStatus returns the cached status for the provided pod UID, as well as whether it
	// was a cache hit.
	GetPodStatus(uid types.UID) (v1.PodStatus, bool)
}

// PodDeletionSafetyProvider provides guarantees that a pod can be safely deleted.
type PodDeletionSafetyProvider interface {
	// PodCouldHaveRunningContainers returns true if the pod could have running containers.
	PodCouldHaveRunningContainers(pod *v1.Pod) bool
}

type PodStartupLatencyStateHelper interface {
	RecordStatusUpdated(pod *v1.Pod)
	DeletePodStartupState(podUID types.UID)
}

// Manager is the Source of truth for kubelet pod status, and should be kept up-to-date with
// the latest v1.PodStatus. It also syncs updates back to the API server.
// 1、StatusManager的核心目标是把Pod的状态更新到APIServer，也就是说APIServer的Pod的状态来源就是StatusManager
// 2、StatusManager并不是主动监听Pod的状态变化，因为APIServer的Pod状态就是通过StatusManager修改的
type Manager interface {
	PodStatusProvider

	// Start the API server status sync loop.
	Start()

	// SetPodStatus caches updates the cached status for the given pod, and triggers a status update.
	// 1、PodWorker会通过SyncHandler设置Pod状态，SyncHandler则会通过这个接口更新StatusManger
	// 2、TODO 这里的Status可以理解为Pod最新的状态么？
	SetPodStatus(pod *v1.Pod, status v1.PodStatus)

	// SetContainerReadiness updates the cached container status with the given readiness, and
	// triggers a status update.
	// 1、当ReadinessManager感知到Pod探针探针状态发生改变时，会通过此接口更新StatusManager
	SetContainerReadiness(podUID types.UID, containerID kubecontainer.ContainerID, ready bool)

	// SetContainerStartup updates the cached container status with the given startup, and
	// triggers a status update.
	// 2、当StartupManager感知到Pod探针状态发生改变时，会通过此结构更新StatusManger
	SetContainerStartup(podUID types.UID, containerID kubecontainer.ContainerID, started bool)

	// TerminatePod resets the container status for the provided pod to terminated and triggers
	// a status update.
	// 1、PodWorker会通过SyncHandler设置Pod状态，SyncHandler则会通过这个接口更新StatusManger
	TerminatePod(pod *v1.Pod)

	// RemoveOrphanedStatuses scans the status cache and removes any entries for pods not included in
	// the provided podUIDs.
	RemoveOrphanedStatuses(podUIDs map[types.UID]bool)

	// GetContainerResourceAllocation returns checkpointed AllocatedResources value for the container
	GetContainerResourceAllocation(podUID string, containerName string) (v1.ResourceList, bool)

	// GetPodResizeStatus returns checkpointed PodStatus.Resize value
	GetPodResizeStatus(podUID string) (v1.PodResizeStatus, bool)

	// SetPodAllocation checkpoints the resources allocated to a pod's containers.
	SetPodAllocation(pod *v1.Pod) error

	// SetPodResizeStatus checkpoints the last resizing decision for the pod.
	// 修改Pod资源分配大小，并使用checkpoint持久化
	SetPodResizeStatus(podUID types.UID, resize v1.PodResizeStatus) error
}

const syncPeriod = 10 * time.Second

// NewManager returns a functional Manager.
func NewManager(
	kubeClient clientset.Interface, // ClientSet
	podManager kubepod.Manager, // podManager，用于MirrorPod与StaticPod之间的映射关系
	podDeletionSafety PodDeletionSafetyProvider,
	podStartupLatencyHelper PodStartupLatencyStateHelper,
	stateFileDirectory string,
) Manager {
	return &manager{
		kubeClient:              kubeClient,
		podManager:              podManager,
		podStatuses:             make(map[types.UID]versionedPodStatus),
		podStatusChannel:        make(chan struct{}, 1), // 之所以容量为1，是因为StatusManager每次更新时都会对比每个Pod的状态，因此这里只需要有一个人通知有Pod状态变更了即可
		apiStatusVersions:       make(map[kubetypes.MirrorPodUID]uint64),
		podDeletionSafety:       podDeletionSafety,
		podStartupLatencyHelper: podStartupLatencyHelper,
		stateFileDirectory:      stateFileDirectory,
	}
}

// isPodStatusByKubeletEqual returns true if the given pod statuses are equal when non-kubelet-owned
// pod conditions are excluded.
// This method normalizes the status before comparing so as to make sure that meaningless
// changes will be ignored.
func isPodStatusByKubeletEqual(oldStatus, status *v1.PodStatus) bool {
	oldCopy := oldStatus.DeepCopy()
	for _, c := range status.Conditions {
		// both owned and shared conditions are used for kubelet status equality
		if kubetypes.PodConditionByKubelet(c.Type) || kubetypes.PodConditionSharedByKubelet(c.Type) {
			_, oc := podutil.GetPodCondition(oldCopy, c.Type)
			if oc == nil || oc.Status != c.Status || oc.Message != c.Message || oc.Reason != c.Reason {
				return false
			}
		}
	}
	oldCopy.Conditions = status.Conditions
	return apiequality.Semantic.DeepEqual(oldCopy, status)
}

func (m *manager) Start() {
	// Initialize m.state to no-op state checkpoint manager
	m.state = state.NewNoopStateCheckpoint()

	// Create pod allocation checkpoint manager even if client is nil so as to allow local get/set of AllocatedResources & Resize
	if utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScaling) {
		// 如果启用了InPlacePodVerticalScaling特性，就实例化StateCheckpoint用于存储Pod资源分配大小以及资源分配状态
		stateImpl, err := state.NewStateCheckpoint(m.stateFileDirectory, podStatusManagerStateFile)
		if err != nil {
			// This is a crictical, non-recoverable failure.
			klog.ErrorS(err, "Could not initialize pod allocation checkpoint manager, please drain node and remove policy state file")
			panic(err)
		}
		m.state = stateImpl
	}

	// Don't start the status manager if we don't have a client. This will happen
	// on the master, where the kubelet is responsible for bootstrapping the pods
	// of the master components.
	if m.kubeClient == nil {
		klog.InfoS("Kubernetes client is nil, not starting status manager")
		return
	}

	klog.InfoS("Starting to sync pod status with apiserver")

	//nolint:staticcheck // SA1015 Ticker can leak since this is only called once and doesn't handle termination.
	// 每10秒钟同步一次状态
	syncTicker := time.NewTicker(syncPeriod).C

	// syncPod and syncBatch share the same go routine to avoid sync races.
	// 触发StatusManager更新Pod状态的数据来源有两种方式，其一就是通过定时器触发，每10秒钟触发一次；其二就是通过其它组件调用StatusManager
	// 对外暴露的接口，从而把数据写入到podStatusChannel当中，从而触发StatusManager更新APIServer
	go wait.Forever(func() {
		for {
			select {
			case <-m.podStatusChannel:
				// 说明有pod更新了状态
				klog.V(4).InfoS("Syncing updated statuses")
				m.syncBatch(false)
			case <-syncTicker: // 每10秒钟触发一次
				klog.V(4).InfoS("Syncing all statuses")
				m.syncBatch(true)
			}
		}
	}, 0)
}

// GetContainerResourceAllocation returns the last checkpointed AllocatedResources values
// If checkpoint manager has not been initialized, it returns nil, false
func (m *manager) GetContainerResourceAllocation(podUID string, containerName string) (v1.ResourceList, bool) {
	m.podStatusesLock.RLock()
	defer m.podStatusesLock.RUnlock()
	return m.state.GetContainerResourceAllocation(podUID, containerName)
}

// GetPodResizeStatus returns the last checkpointed ResizeStaus value
// If checkpoint manager has not been initialized, it returns nil, false
func (m *manager) GetPodResizeStatus(podUID string) (v1.PodResizeStatus, bool) {
	m.podStatusesLock.RLock()
	defer m.podStatusesLock.RUnlock()
	return m.state.GetPodResizeStatus(podUID)
}

// SetPodAllocation checkpoints the resources allocated to a pod's containers
// 通过checkpoint持久化Pod的资源分配情况
func (m *manager) SetPodAllocation(pod *v1.Pod) error {
	m.podStatusesLock.RLock()
	defer m.podStatusesLock.RUnlock()
	for _, container := range pod.Spec.Containers {
		var alloc v1.ResourceList
		if container.Resources.Requests != nil {
			alloc = container.Resources.Requests.DeepCopy()
		}
		if err := m.state.SetContainerResourceAllocation(string(pod.UID), container.Name, alloc); err != nil {
			return err
		}
	}
	return nil
}

// SetPodResizeStatus checkpoints the last resizing decision for the pod.
// 通过checkpoint持久化Pod资源分配状态
func (m *manager) SetPodResizeStatus(podUID types.UID, resizeStatus v1.PodResizeStatus) error {
	m.podStatusesLock.RLock()
	defer m.podStatusesLock.RUnlock()
	return m.state.SetPodResizeStatus(string(podUID), resizeStatus)
}

func (m *manager) GetPodStatus(uid types.UID) (v1.PodStatus, bool) {
	m.podStatusesLock.RLock()
	defer m.podStatusesLock.RUnlock()
	status, ok := m.podStatuses[types.UID(m.podManager.TranslatePodUID(uid))]
	return status.status, ok
}

// SetPodStatus 更新Pod状态
func (m *manager) SetPodStatus(pod *v1.Pod, status v1.PodStatus) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()

	// Make sure we're caching a deep copy.
	status = *status.DeepCopy()

	// Force a status update if deletion timestamp is set. This is necessary
	// because if the pod is in the non-running state, the pod worker still
	// needs to be able to trigger an update and/or deletion.
	m.updateStatusInternal(pod, status, pod.DeletionTimestamp != nil, false)
}

func (m *manager) SetContainerReadiness(podUID types.UID, containerID kubecontainer.ContainerID, ready bool) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()

	pod, ok := m.podManager.GetPodByUID(podUID)
	if !ok {
		klog.V(4).InfoS("Pod has been deleted, no need to update readiness", "podUID", string(podUID))
		return
	}

	oldStatus, found := m.podStatuses[pod.UID]
	if !found {
		klog.InfoS("Container readiness changed before pod has synced",
			"pod", klog.KObj(pod),
			"containerID", containerID.String())
		return
	}

	// Find the container to update.
	containerStatus, _, ok := findContainerStatus(&oldStatus.status, containerID.String())
	if !ok {
		klog.InfoS("Container readiness changed for unknown container",
			"pod", klog.KObj(pod),
			"containerID", containerID.String())
		return
	}

	if containerStatus.Ready == ready {
		klog.V(4).InfoS("Container readiness unchanged",
			"ready", ready,
			"pod", klog.KObj(pod),
			"containerID", containerID.String())
		return
	}

	// Make sure we're not updating the cached version.
	status := *oldStatus.status.DeepCopy()
	containerStatus, _, _ = findContainerStatus(&status, containerID.String())
	containerStatus.Ready = ready

	// updateConditionFunc updates the corresponding type of condition
	updateConditionFunc := func(conditionType v1.PodConditionType, condition v1.PodCondition) {
		conditionIndex := -1
		for i, condition := range status.Conditions {
			if condition.Type == conditionType {
				conditionIndex = i
				break
			}
		}
		if conditionIndex != -1 {
			status.Conditions[conditionIndex] = condition
		} else {
			klog.InfoS("PodStatus missing condition type", "conditionType", conditionType, "status", status)
			status.Conditions = append(status.Conditions, condition)
		}
	}
	updateConditionFunc(v1.PodReady, GeneratePodReadyCondition(&pod.Spec, status.Conditions, status.ContainerStatuses, status.Phase))
	updateConditionFunc(v1.ContainersReady, GenerateContainersReadyCondition(&pod.Spec, status.ContainerStatuses, status.Phase))
	m.updateStatusInternal(pod, status, false, false)
}

// SetContainerStartup 设置Pod的指定容器已经启动
func (m *manager) SetContainerStartup(podUID types.UID, containerID kubecontainer.ContainerID, started bool) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()

	// 通过PodManager获取当前容器
	pod, ok := m.podManager.GetPodByUID(podUID)
	if !ok {
		klog.V(4).InfoS("Pod has been deleted, no need to update startup", "podUID", string(podUID))
		return
	}

	// 获取当前容器的状态
	oldStatus, found := m.podStatuses[pod.UID]
	if !found {
		klog.InfoS("Container startup changed before pod has synced",
			"pod", klog.KObj(pod),
			"containerID", containerID.String())
		return
	}

	// Find the container to update.
	// 获取容器的状态
	containerStatus, _, ok := findContainerStatus(&oldStatus.status, containerID.String())
	if !ok { // 如果没有找到容器状态，直接打印日志
		klog.InfoS("Container startup changed for unknown container",
			"pod", klog.KObj(pod),
			"containerID", containerID.String())
		return
	}

	// 容器状态没有改变，无需做任何操作
	if containerStatus.Started != nil && *containerStatus.Started == started {
		klog.V(4).InfoS("Container startup unchanged",
			"pod", klog.KObj(pod),
			"containerID", containerID.String())
		return
	}

	// Make sure we're not updating the cached version.
	status := *oldStatus.status.DeepCopy()
	// 获取容器状态，并设置值
	containerStatus, _, _ = findContainerStatus(&status, containerID.String())
	containerStatus.Started = &started

	m.updateStatusInternal(pod, status, false, false)
}

func findContainerStatus(status *v1.PodStatus, containerID string) (containerStatus *v1.ContainerStatus, init bool, ok bool) {
	// Find the container to update.
	for i, c := range status.ContainerStatuses {
		if c.ContainerID == containerID {
			return &status.ContainerStatuses[i], false, true
		}
	}

	for i, c := range status.InitContainerStatuses {
		if c.ContainerID == containerID {
			return &status.InitContainerStatuses[i], true, true
		}
	}

	return nil, false, false

}

// TerminatePod ensures that the status of containers is properly defaulted at the end of the pod
// lifecycle. As the Kubelet must reconcile with the container runtime to observe container status
// there is always the possibility we are unable to retrieve one or more container statuses due to
// garbage collection, admin action, or loss of temporary data on a restart. This method ensures
// that any absent container status is treated as a failure so that we do not incorrectly describe
// the pod as successful. If we have not yet initialized the pod in the presence of init containers,
// the init container failure status is sufficient to describe the pod as failing, and we do not need
// to override waiting containers (unless there is evidence the pod previously started those containers).
// It also makes sure that pods are transitioned to a terminal phase (Failed or Succeeded) before
// their deletion.
func (m *manager) TerminatePod(pod *v1.Pod) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()

	// ensure that all containers have a terminated state - because we do not know whether the container
	// was successful, always report an error
	oldStatus := &pod.Status
	cachedStatus, isCached := m.podStatuses[pod.UID]
	if isCached {
		oldStatus = &cachedStatus.status
	}
	status := *oldStatus.DeepCopy()

	// once a pod has initialized, any missing status is treated as a failure
	if hasPodInitialized(pod) {
		for i := range status.ContainerStatuses {
			if status.ContainerStatuses[i].State.Terminated != nil {
				continue
			}
			status.ContainerStatuses[i].State = v1.ContainerState{
				Terminated: &v1.ContainerStateTerminated{
					Reason:   "ContainerStatusUnknown",
					Message:  "The container could not be located when the pod was terminated",
					ExitCode: 137,
				},
			}
		}
	}

	// all but the final suffix of init containers which have no evidence of a container start are
	// marked as failed containers
	for i := range initializedContainers(status.InitContainerStatuses) {
		if status.InitContainerStatuses[i].State.Terminated != nil {
			continue
		}
		status.InitContainerStatuses[i].State = v1.ContainerState{
			Terminated: &v1.ContainerStateTerminated{
				Reason:   "ContainerStatusUnknown",
				Message:  "The container could not be located when the pod was terminated",
				ExitCode: 137,
			},
		}
	}

	// Make sure all pods are transitioned to a terminal phase.
	// TODO(#116484): Also assign terminal phase to static an pods.
	if !kubetypes.IsStaticPod(pod) {
		switch status.Phase {
		case v1.PodSucceeded, v1.PodFailed:
			// do nothing, already terminal
		case v1.PodPending, v1.PodRunning:
			if status.Phase == v1.PodRunning && isCached {
				klog.InfoS("Terminal running pod should have already been marked as failed, programmer error", "pod", klog.KObj(pod), "podUID", pod.UID)
			}
			klog.V(3).InfoS("Marking terminal pod as failed", "oldPhase", status.Phase, "pod", klog.KObj(pod), "podUID", pod.UID)
			status.Phase = v1.PodFailed
		default:
			klog.ErrorS(fmt.Errorf("unknown phase: %v", status.Phase), "Unknown phase, programmer error", "pod", klog.KObj(pod), "podUID", pod.UID)
			status.Phase = v1.PodFailed
		}
	}

	klog.V(5).InfoS("TerminatePod calling updateStatusInternal", "pod", klog.KObj(pod), "podUID", pod.UID)
	m.updateStatusInternal(pod, status, true, true)
}

// hasPodInitialized returns true if the pod has no evidence of ever starting a regular container, which
// implies those containers should not be transitioned to terminated status.
func hasPodInitialized(pod *v1.Pod) bool {
	// a pod without init containers is always initialized
	if len(pod.Spec.InitContainers) == 0 {
		return true
	}
	// if any container has ever moved out of waiting state, the pod has initialized
	for _, status := range pod.Status.ContainerStatuses {
		if status.LastTerminationState.Terminated != nil || status.State.Waiting == nil {
			return true
		}
	}
	// if the last init container has ever completed with a zero exit code, the pod is initialized
	if l := len(pod.Status.InitContainerStatuses); l > 0 {
		container := pod.Status.InitContainerStatuses[l-1]
		if state := container.LastTerminationState; state.Terminated != nil && state.Terminated.ExitCode == 0 {
			return true
		}
		if state := container.State; state.Terminated != nil && state.Terminated.ExitCode == 0 {
			return true
		}
	}
	// otherwise the pod has no record of being initialized
	return false
}

// initializedContainers returns all status except for suffix of containers that are in Waiting
// state, which is the set of containers that have attempted to start at least once. If all containers
// are Watiing, the first container is always returned.
func initializedContainers(containers []v1.ContainerStatus) []v1.ContainerStatus {
	for i := len(containers) - 1; i >= 0; i-- {
		if containers[i].State.Waiting == nil || containers[i].LastTerminationState.Terminated != nil {
			return containers[0 : i+1]
		}
	}
	// always return at least one container
	if len(containers) > 0 {
		return containers[0:1]
	}
	return nil
}

// checkContainerStateTransition ensures that no container is trying to transition
// from a terminated to non-terminated state, which is illegal and indicates a
// logical error in the kubelet.
// 如果一个容器已经处于Terminated状态，但是在下一个时刻却被修改为非Terminated状态，说明这个状态一定是有问题的，因为已经被杀掉的容器不可能
// 死而复生，只能被杀掉。
func checkContainerStateTransition(oldStatuses, newStatuses []v1.ContainerStatus, restartPolicy v1.RestartPolicy) error {
	// If we should always restart, containers are allowed to leave the terminated state
	if restartPolicy == v1.RestartPolicyAlways {
		return nil
	}
	for _, oldStatus := range oldStatuses {
		// Skip any container that wasn't terminated
		if oldStatus.State.Terminated == nil {
			continue
		}
		// Skip any container that failed but is allowed to restart
		if oldStatus.State.Terminated.ExitCode != 0 && restartPolicy == v1.RestartPolicyOnFailure {
			continue
		}
		for _, newStatus := range newStatuses {
			// 一个容器钱一个时刻处于Terminated状态，下一个时刻没有处于这个状态说明一定是有问题的
			if oldStatus.Name == newStatus.Name && newStatus.State.Terminated == nil {
				return fmt.Errorf("terminated container %v attempted illegal transition to non-terminated state", newStatus.Name)
			}
		}
	}
	return nil
}

// updateStatusInternal updates the internal status cache, and queues an update to the api server if
// necessary.
// This method IS NOT THREAD SAFE and must be called from a locked function.
func (m *manager) updateStatusInternal(
	pod *v1.Pod, // 当前状态变更的Pod
	status v1.PodStatus, // 当前Pod的最新状态
	forceUpdate, // 是否强制更新，只要Pod的DeletionTimestamp非空，就需要强制更新
	podIsFinished bool, // 用于表示当前Pod是否处于Terminated状态
) {
	var oldStatus v1.PodStatus
	// 获取当前缓存的Pod状态
	cachedStatus, isCached := m.podStatuses[pod.UID]
	if isCached {
		// 记录Pod上一次的状态
		oldStatus = cachedStatus.status
		// TODO(#116484): Also assign terminal phase to static pods.
		if !kubetypes.IsStaticPod(pod) {
			// 如果当前Pod处于Finished阶段，但是调用此方法的调用方说改Pod没有处于Finished，那一定是程序有问题
			if cachedStatus.podIsFinished && !podIsFinished {
				klog.InfoS("Got unexpected podIsFinished=false, while podIsFinished=true in status cache, programmer error.", "pod", klog.KObj(pod))
				podIsFinished = true
			}
		}
	} else if mirrorPod, ok := m.podManager.GetMirrorPodByPod(pod); ok {
		// 如果当前Pod是一个MirrorPod，那么当前Pod的状态是由MirrorPod状态决定的
		oldStatus = mirrorPod.Status
	} else {
		oldStatus = pod.Status
	}

	// Check for illegal state transition in containers
	// 如果一个容器已经处于Terminated状态，但是在下一个时刻却被修改为非Terminated状态，说明这个状态一定是有问题的，因为已经被杀掉的容器不可能
	// 死而复生，只能被杀掉。
	if err := checkContainerStateTransition(oldStatus.ContainerStatuses, status.ContainerStatuses, pod.Spec.RestartPolicy); err != nil {
		klog.ErrorS(err, "Status update on pod aborted", "pod", klog.KObj(pod))
		return
	}
	// 如果一个容器已经处于Terminated状态，但是在下一个时刻却被修改为非Terminated状态，说明这个状态一定是有问题的，因为已经被杀掉的容器不可能
	// 死而复生，只能被杀掉。
	if err := checkContainerStateTransition(oldStatus.InitContainerStatuses, status.InitContainerStatuses, pod.Spec.RestartPolicy); err != nil {
		klog.ErrorS(err, "Status update on pod aborted", "pod", klog.KObj(pod))
		return
	}

	// Set ContainersReadyCondition.LastTransitionTime.
	// 更新Condition ContainersReady状态变更时间
	updateLastTransitionTime(&status, &oldStatus, v1.ContainersReady)

	// Set ReadyCondition.LastTransitionTime.
	// 更新Condition Ready状态变更时间
	updateLastTransitionTime(&status, &oldStatus, v1.PodReady)

	// Set InitializedCondition.LastTransitionTime.
	// 更新Condition PodInitialized状态变更时间
	updateLastTransitionTime(&status, &oldStatus, v1.PodInitialized)

	// Set PodHasNetwork.LastTransitionTime.
	// 更新Condition PodHasNetwork状态变更时间
	updateLastTransitionTime(&status, &oldStatus, kubetypes.PodHasNetwork)

	// Set PodScheduledCondition.LastTransitionTime.
	// 更新Condition PodScheduled状态变更时间
	updateLastTransitionTime(&status, &oldStatus, v1.PodScheduled)

	if utilfeature.DefaultFeatureGate.Enabled(features.PodDisruptionConditions) {
		// Set DisruptionTarget.LastTransitionTime.
		// 更新Condition DisruptionTarget状态变更时间
		updateLastTransitionTime(&status, &oldStatus, v1.DisruptionTarget)
	}

	// ensure that the start time does not change across updates.
	// Pod的启动时间不应该更新
	if oldStatus.StartTime != nil && !oldStatus.StartTime.IsZero() {
		status.StartTime = oldStatus.StartTime
	} else if status.StartTime.IsZero() {
		// if the status has no start time, we need to set an initial time
		now := metav1.Now()
		status.StartTime = &now
	}

	// 时间转换
	normalizeStatus(pod, &status)

	// Perform some more extensive logging of container termination state to assist in
	// debugging production races (generally not needed).
	if klogV := klog.V(5); klogV.Enabled() {
		var containers []string
		for _, s := range append(append([]v1.ContainerStatus(nil), status.InitContainerStatuses...), status.ContainerStatuses...) {
			var current, previous string
			switch {
			case s.State.Running != nil:
				current = "running"
			case s.State.Waiting != nil:
				current = "waiting"
			case s.State.Terminated != nil:
				current = fmt.Sprintf("terminated=%d", s.State.Terminated.ExitCode)
			default:
				current = "unknown"
			}
			switch {
			case s.LastTerminationState.Running != nil:
				previous = "running"
			case s.LastTerminationState.Waiting != nil:
				previous = "waiting"
			case s.LastTerminationState.Terminated != nil:
				previous = fmt.Sprintf("terminated=%d", s.LastTerminationState.Terminated.ExitCode)
			default:
				previous = "<none>"
			}
			containers = append(containers, fmt.Sprintf("(%s state=%s previous=%s)", s.Name, current, previous))
		}
		sort.Strings(containers)
		klogV.InfoS("updateStatusInternal", "version", cachedStatus.version+1, "podIsFinished",
			podIsFinished, "pod", klog.KObj(pod), "podUID", pod.UID, "containers", strings.Join(containers, " "))
	}

	// The intent here is to prevent concurrent updates to a pod's status from
	// clobbering each other so the phase of a pod progresses monotonically.
	// 如果新旧状态相等了，就没有必要更新了
	if isCached && isPodStatusByKubeletEqual(&cachedStatus.status, &status) && !forceUpdate {
		klog.V(3).InfoS("Ignoring same status for pod", "pod", klog.KObj(pod), "status", status)
		return
	}

	newStatus := versionedPodStatus{
		status:        status,
		version:       cachedStatus.version + 1,
		podName:       pod.Name,
		podNamespace:  pod.Namespace,
		podIsFinished: podIsFinished,
	}

	// Multiple status updates can be generated before we update the API server,
	// so we track the time from the first status update until we retire it to
	// the API.
	if cachedStatus.at.IsZero() {
		newStatus.at = time.Now()
	} else {
		newStatus.at = cachedStatus.at
	}

	// 缓存Pod状态
	m.podStatuses[pod.UID] = newStatus

	select {
	// 触发pod更新状态
	case m.podStatusChannel <- struct{}{}:
	default:
		// there's already a status update pending
	}
}

// updateLastTransitionTime updates the LastTransitionTime of a pod condition.
// 更新Condition状态变更时间
func updateLastTransitionTime(status, oldStatus *v1.PodStatus, conditionType v1.PodConditionType) {
	_, condition := podutil.GetPodCondition(status, conditionType)
	if condition == nil {
		return
	}
	// Need to set LastTransitionTime.
	lastTransitionTime := metav1.Now()
	_, oldCondition := podutil.GetPodCondition(oldStatus, conditionType)
	// 说明Pod当前的Condition转台没有发生改变
	if oldCondition != nil && condition.Status == oldCondition.Status {
		lastTransitionTime = oldCondition.LastTransitionTime
	}
	condition.LastTransitionTime = lastTransitionTime
}

// deletePodStatus simply removes the given pod from the status cache.
func (m *manager) deletePodStatus(uid types.UID) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()
	delete(m.podStatuses, uid)
	m.podStartupLatencyHelper.DeletePodStartupState(uid)
	if utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScaling) {
		// 移除当前Pod资源分配的持久化checkpoint
		m.state.Delete(string(uid), "")
	}
}

// RemoveOrphanedStatuses
// TODO(filipg): It'd be cleaner if we can do this without signal from user.
// 移除无用的Pod状态
func (m *manager) RemoveOrphanedStatuses(podUIDs map[types.UID]bool) {
	m.podStatusesLock.Lock()
	defer m.podStatusesLock.Unlock()
	for key := range m.podStatuses {
		if _, ok := podUIDs[key]; !ok {
			klog.V(5).InfoS("Removing pod from status map.", "podUID", key)
			delete(m.podStatuses, key)
			if utilfeature.DefaultFeatureGate.Enabled(features.InPlacePodVerticalScaling) {
				// 移除checkpoint持久化记录
				m.state.Delete(string(key), "")
			}
		}
	}
}

// syncBatch syncs pods statuses with the apiserver. Returns the number of syncs
// attempted for testing.
func (m *manager) syncBatch(all bool) int {
	type podSync struct {
		podUID    types.UID
		statusUID kubetypes.MirrorPodUID
		status    versionedPodStatus
	}

	// 需要更新状态的Pod
	var updatedStatuses []podSync

	// 获取StaticPod和MirrorPod之间的映射关系
	podToMirror, mirrorToPod := m.podManager.GetUIDTranslations()
	func() { // Critical section
		m.podStatusesLock.RLock()
		defer m.podStatusesLock.RUnlock()

		// Clean up orphaned versions.
		if all {
			// 遍历所有MirrorPod
			for uid := range m.apiStatusVersions {
				// 是否缓存了当前MirrorPod的状态
				_, hasPod := m.podStatuses[types.UID(uid)]
				// 是否有MirrorPod
				_, hasMirror := mirrorToPod[uid]
				// 如果既没有缓存当前MirrorPod的状态，同事也没有找到当前MirrorPod与StaticPod的映射，那么直接删除这个MirrorPod的状态，因为
				// 当前MirrorPod的记录可能是错乱的
				if !hasPod && !hasMirror {
					delete(m.apiStatusVersions, uid)
				}
			}
		}

		// Decide which pods need status updates.
		// 遍历所有Pod的状态
		for uid, status := range m.podStatuses {
			// translate the pod UID (source) to the status UID (API pod) -
			// static pods are identified in source by pod UID but tracked in the
			// API via the uid of the mirror pod
			// 其实大部分Pod都是普通Pod，并不是StaticPod
			uidOfStatus := kubetypes.MirrorPodUID(uid)
			// 获取当前Pod的MirrorPod
			if mirrorUID, ok := podToMirror[kubetypes.ResolvedPodUID(uid)]; ok {
				if mirrorUID == "" {
					klog.V(5).InfoS("Static pod does not have a corresponding mirror pod; skipping",
						"podUID", uid,
						"pod", klog.KRef(status.podNamespace, status.podName))
					continue
				}
				// 获取当前Pod的MirrorPod UID
				uidOfStatus = mirrorUID
			}

			// if a new status update has been delivered, trigger an update, otherwise the
			// pod can wait for the next bulk check (which performs reconciliation as well)
			if !all {
				// 若StatusManager
				if m.apiStatusVersions[uidOfStatus] >= status.version {
					continue
				}
				// 如果当前缓存的Pod版本小于当前Pod的实际状态版本，那么直接更新
				updatedStatuses = append(updatedStatuses, podSync{uid, uidOfStatus, status})
				continue
			}

			// Ensure that any new status, or mismatched status, or pod that is ready for
			// deletion gets updated. If a status update fails we retry the next time any
			// other pod is updated.
			// 判断当前Pod是否需要更新状态
			if m.needsUpdate(types.UID(uidOfStatus), status) {
				updatedStatuses = append(updatedStatuses, podSync{uid, uidOfStatus, status})
			} else if m.needsReconcile(uid, status.status) {
				// 主要是判断StatusManager缓存的Pod状态是否和PodManager中缓存的状态相等，如果不等，那么需要reconciler

				// Delete the apiStatusVersions here to force an update on the pod status
				// In most cases the deleted apiStatusVersions here should be filled
				// soon after the following syncPod() [If the syncPod() sync an update
				// successfully].
				delete(m.apiStatusVersions, uidOfStatus)
				updatedStatuses = append(updatedStatuses, podSync{uid, uidOfStatus, status})
			}
		}
	}()

	for _, update := range updatedStatuses {
		klog.V(5).InfoS("Sync pod status", "podUID", update.podUID, "statusUID", update.statusUID, "version", update.status.version)
		// 上报Pod最新的状态到APIServer
		m.syncPod(update.podUID, update.status)
	}

	return len(updatedStatuses)
}

// syncPod syncs the given status with the API server. The caller must not hold the status lock.
// 1、syncPod方法的核心目标是为了更新Pod的状态到APIServer
func (m *manager) syncPod(uid types.UID, status versionedPodStatus) {
	// TODO: make me easier to express from client code
	// 直接通过APIServer查询Pod的状态，如果没找到这个Pod或者查询异常，那肯定更新状态必然会失败，因此直接退出
	pod, err := m.kubeClient.CoreV1().Pods(status.podNamespace).Get(context.TODO(), status.podName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		klog.V(3).InfoS("Pod does not exist on the server",
			"podUID", uid,
			"pod", klog.KRef(status.podNamespace, status.podName))
		// If the Pod is deleted the status will be cleared in
		// RemoveOrphanedStatuses, so we just ignore the update here.
		return
	}
	if err != nil {
		klog.InfoS("Failed to get status for pod",
			"podUID", uid,
			"pod", klog.KRef(status.podNamespace, status.podName),
			"err", err)
		return
	}

	// 通过MirrorPod UID找到与之对应的StaticPod UID
	translatedUID := m.podManager.TranslatePodUID(pod.UID)
	// Type convert original uid just for the purpose of comparison.
	// 如果当前的Pod是MirrorPod，并且其PodUID发生了改变，那么一定是这个Pod被重建了
	if len(translatedUID) > 0 && translatedUID != kubetypes.ResolvedPodUID(uid) {
		klog.V(2).InfoS("Pod was deleted and then recreated, skipping status update",
			"pod", klog.KObj(pod),
			"oldPodUID", uid,
			"podUID", translatedUID)
		m.deletePodStatus(uid)
		return
	}

	// TODO 合并Pod状态
	mergedStatus := mergePodStatus(pod.Status, status.status, m.podDeletionSafety.PodCouldHaveRunningContainers(pod))

	// 更新APServer 当前Pod的最新状态
	newPod, patchBytes, unchanged, err := statusutil.PatchPodStatus(context.TODO(), m.kubeClient, pod.Namespace, pod.Name, pod.UID, pod.Status, mergedStatus)
	klog.V(3).InfoS("Patch status for pod", "pod", klog.KObj(pod), "podUID", uid, "patch", string(patchBytes))

	if err != nil {
		klog.InfoS("Failed to update status for pod", "pod", klog.KObj(pod), "err", err)
		return
	}
	if unchanged {
		klog.V(3).InfoS("Status for pod is up-to-date", "pod", klog.KObj(pod), "statusVersion", status.version)
	} else {
		klog.V(3).InfoS("Status for pod updated successfully", "pod", klog.KObj(pod), "statusVersion", status.version, "status", mergedStatus)
		pod = newPod
		// We pass a new object (result of API call which contains updated ResourceVersion)
		m.podStartupLatencyHelper.RecordStatusUpdated(pod)
	}

	// measure how long the status update took to propagate from generation to update on the server
	if status.at.IsZero() {
		klog.V(3).InfoS("Pod had no status time set", "pod", klog.KObj(pod), "podUID", uid, "version", status.version)
	} else {
		duration := time.Since(status.at).Truncate(time.Millisecond)
		metrics.PodStatusSyncDuration.Observe(duration.Seconds())
	}

	// 更新StatusManager上报给APIServer最新的Pod状态版本
	m.apiStatusVersions[kubetypes.MirrorPodUID(pod.UID)] = status.version

	// We don't handle graceful deletion of mirror pods.
	if m.canBeDeleted(pod, status.status, status.podIsFinished) {
		deleteOptions := metav1.DeleteOptions{
			GracePeriodSeconds: new(int64),
			// Use the pod UID as the precondition for deletion to prevent deleting a
			// newly created pod with the same name and namespace.
			Preconditions: metav1.NewUIDPreconditions(string(pod.UID)),
		}
		err = m.kubeClient.CoreV1().Pods(pod.Namespace).Delete(context.TODO(), pod.Name, deleteOptions)
		if err != nil {
			klog.InfoS("Failed to delete status for pod", "pod", klog.KObj(pod), "err", err)
			return
		}
		klog.V(3).InfoS("Pod fully terminated and removed from etcd", "pod", klog.KObj(pod))
		m.deletePodStatus(uid)
	}
}

// needsUpdate returns whether the status is stale for the given pod UID.
// This method is not thread safe, and must only be accessed by the sync thread.
func (m *manager) needsUpdate(uid types.UID, status versionedPodStatus) bool {
	latest, ok := m.apiStatusVersions[kubetypes.MirrorPodUID(uid)]
	// 如果没有记录当前Pod状态版本，或者缓存Pod的版本小于指定的版本，说明需要更新
	if !ok || latest < status.version {
		return true
	}

	// 否则，说明当前缓存的Pod状态就是最新的。此时我们需要看看这个Pod是否已经删除了

	// 从PodManager中获取当前Pod，如果不存在，说明当前Pod已经删除了，无需更新
	pod, ok := m.podManager.GetPodByUID(uid)
	if !ok {
		return false
	}

	// 1、Pod的DeletionTimeStamp为空，说明Pod不能被删除
	// 2、即便Pod的DeletionTimestamp非空，但是当前Pod是MirrorPod，也不能被删除
	// 3、若当前Pod的DeletionTimestamp非空,并且不是MirrorPod，如果Pod还没有执行结束，也不能删除
	// 4、如果Pod已经执行结束了，那么Pod可以删除
	return m.canBeDeleted(pod, status.status, status.podIsFinished)
}

// 1、Pod的DeletionTimeStamp为空，说明Pod不能被删除
// 2、即便Pod的DeletionTimestamp非空，但是当前Pod是MirrorPod，也不能被删除
// 3、若当前Pod的DeletionTimestamp非空,并且不是MirrorPod，如果Pod还没有执行结束，也不能删除
// 4、如果Pod已经执行结束了，那么Pod可以删除
func (m *manager) canBeDeleted(pod *v1.Pod, status v1.PodStatus, podIsFinished bool) bool {
	// 如果当前Pod的没有被删除，或者当前Pod是MirrorPod，那么不能被删除
	if pod.DeletionTimestamp == nil || kubetypes.IsMirrorPod(pod) {
		return false
	}
	// Delay deletion of pods until the phase is terminal.
	// 如果当前Pod还没有执行结束（不管是正常退出还是异常退出），那么此时还不能删除
	if !podutil.IsPodPhaseTerminal(pod.Status.Phase) {
		klog.V(3).InfoS("Delaying pod deletion as the phase is non-terminal", "phase", status.Phase, "pod", klog.KObj(pod), "podUID", pod.UID)
		return false
	}
	// If this is an update completing pod termination then we know the pod termination is finished.
	// 如果当前Pod已经结束了，那么我们认为这个Pod已经结束
	if podIsFinished {
		klog.V(3).InfoS("The pod termination is finished as SyncTerminatedPod completes its execution", "phase", status.Phase, "pod", klog.KObj(pod), "podUID", pod.UID)
		return true
	}
	return false
}

// needsReconcile compares the given status with the status in the pod manager (which
// in fact comes from apiserver), returns whether the status needs to be reconciled with
// the apiserver. Now when pod status is inconsistent between apiserver and kubelet,
// kubelet should forcibly send an update to reconcile the inconsistence, because kubelet
// should be the source of truth of pod status.
// NOTE(random-liu): It's simpler to pass in mirror pod uid and get mirror pod by uid, but
// now the pod manager only supports getting mirror pod by static pod, so we have to pass
// static pod uid here.
// TODO(random-liu): Simplify the logic when mirror pod manager is added.
// 主要是判断StatusManager缓存的Pod状态是否和PodManager中缓存的状态相等，如果不等，那么需要reconciler
func (m *manager) needsReconcile(uid types.UID, status v1.PodStatus) bool {
	// The pod could be a static pod, so we should translate first.
	// 根据ID获取当前Pod
	pod, ok := m.podManager.GetPodByUID(uid)
	if !ok {
		// 如果Pod已经被删除了，那么无需再次调谐这个Pod
		klog.V(4).InfoS("Pod has been deleted, no need to reconcile", "podUID", string(uid))
		return false
	}
	// If the pod is a static pod, we should check its mirror pod, because only status in mirror pod is meaningful to us.
	// 如果当前Pod是一个StaticPod，那么获取当前Pod的MirrorPod
	if kubetypes.IsStaticPod(pod) {
		mirrorPod, ok := m.podManager.GetMirrorPodByPod(pod)
		if !ok {
			// 如果当前Pod的MirrorPod不存在，那么无需更新
			klog.V(4).InfoS("Static pod has no corresponding mirror pod, no need to reconcile", "pod", klog.KObj(pod))
			return false
		}
		pod = mirrorPod
	}

	podStatus := pod.Status.DeepCopy()
	normalizeStatus(pod, podStatus)

	// 如果从PodManager获取到的状态和StatusManager中缓存的状态相等，那么无需更新状态
	if isPodStatusByKubeletEqual(podStatus, &status) {
		// If the status from the source is the same with the cached status,
		// reconcile is not needed. Just return.
		return false
	}
	klog.V(3).InfoS("Pod status is inconsistent with cached status for pod, a reconciliation should be triggered",
		"pod", klog.KObj(pod),
		"statusDiff", diff.ObjectDiff(podStatus, &status))

	// 如果不等，那么需要更新状态
	return true
}

// normalizeStatus normalizes nanosecond precision timestamps in podStatus
// down to second precision (*RFC339NANO* -> *RFC3339*). This must be done
// before comparing podStatus to the status returned by apiserver because
// apiserver does not support RFC339NANO.
// Related issue #15262/PR #15263 to move apiserver to RFC339NANO is closed.
func normalizeStatus(pod *v1.Pod, status *v1.PodStatus) *v1.PodStatus {
	bytesPerStatus := kubecontainer.MaxPodTerminationMessageLogLength
	if containers := len(pod.Spec.Containers) + len(pod.Spec.InitContainers); containers > 0 {
		bytesPerStatus = bytesPerStatus / containers
	}
	normalizeTimeStamp := func(t *metav1.Time) {
		*t = t.Rfc3339Copy()
	}
	normalizeContainerState := func(c *v1.ContainerState) {
		if c.Running != nil {
			normalizeTimeStamp(&c.Running.StartedAt)
		}
		if c.Terminated != nil {
			normalizeTimeStamp(&c.Terminated.StartedAt)
			normalizeTimeStamp(&c.Terminated.FinishedAt)
			if len(c.Terminated.Message) > bytesPerStatus {
				c.Terminated.Message = c.Terminated.Message[:bytesPerStatus]
			}
		}
	}

	if status.StartTime != nil {
		normalizeTimeStamp(status.StartTime)
	}
	for i := range status.Conditions {
		condition := &status.Conditions[i]
		normalizeTimeStamp(&condition.LastProbeTime)
		normalizeTimeStamp(&condition.LastTransitionTime)
	}

	// update container statuses
	for i := range status.ContainerStatuses {
		cstatus := &status.ContainerStatuses[i]
		normalizeContainerState(&cstatus.State)
		normalizeContainerState(&cstatus.LastTerminationState)
	}
	// Sort the container statuses, so that the order won't affect the result of comparison
	sort.Sort(kubetypes.SortedContainerStatuses(status.ContainerStatuses))

	// update init container statuses
	for i := range status.InitContainerStatuses {
		cstatus := &status.InitContainerStatuses[i]
		normalizeContainerState(&cstatus.State)
		normalizeContainerState(&cstatus.LastTerminationState)
	}
	// Sort the container statuses, so that the order won't affect the result of comparison
	kubetypes.SortInitContainerStatuses(pod, status.InitContainerStatuses)
	return status
}

// mergePodStatus merges oldPodStatus and newPodStatus to preserve where pod conditions
// not owned by kubelet and to ensure terminal phase transition only happens after all
// running containers have terminated. This method does not modify the old status.
func mergePodStatus(oldPodStatus, newPodStatus v1.PodStatus, couldHaveRunningContainers bool) v1.PodStatus {
	podConditions := make([]v1.PodCondition, 0, len(oldPodStatus.Conditions)+len(newPodStatus.Conditions))

	for _, c := range oldPodStatus.Conditions {
		if !kubetypes.PodConditionByKubelet(c.Type) {
			podConditions = append(podConditions, c)
		}
	}

	transitioningToTerminalPhase := !podutil.IsPodPhaseTerminal(oldPodStatus.Phase) && podutil.IsPodPhaseTerminal(newPodStatus.Phase)

	for _, c := range newPodStatus.Conditions {
		if kubetypes.PodConditionByKubelet(c.Type) {
			podConditions = append(podConditions, c)
		} else if kubetypes.PodConditionSharedByKubelet(c.Type) {
			// we replace or append all the "shared by kubelet" conditions
			if c.Type == v1.DisruptionTarget {
				// guard the update of the DisruptionTarget condition with a check to ensure
				// it will only be sent once all containers have terminated and the phase
				// is terminal. This avoids sending an unnecessary patch request to add
				// the condition if the actual status phase transition is delayed.
				if transitioningToTerminalPhase && !couldHaveRunningContainers {
					// update the LastTransitionTime again here because the older transition
					// time set in updateStatusInternal is likely stale as sending of
					// the condition was delayed until all pod's containers have terminated.
					updateLastTransitionTime(&newPodStatus, &oldPodStatus, c.Type)
					if _, c := podutil.GetPodConditionFromList(newPodStatus.Conditions, c.Type); c != nil {
						// for shared conditions we update or append in podConditions
						podConditions = statusutil.ReplaceOrAppendPodCondition(podConditions, c)
					}
				}
			}
		}
	}
	newPodStatus.Conditions = podConditions

	// Delay transitioning a pod to a terminal status unless the pod is actually terminal.
	// The Kubelet should never transition a pod to terminal status that could have running
	// containers and thus actively be leveraging exclusive resources. Note that resources
	// like volumes are reconciled by a subsystem in the Kubelet and will converge if a new
	// pod reuses an exclusive resource (unmount -> free -> mount), which means we do not
	// need wait for those resources to be detached by the Kubelet. In general, resources
	// the Kubelet exclusively owns must be released prior to a pod being reported terminal,
	// while resources that have participanting components above the API use the pod's
	// transition to a terminal phase (or full deletion) to release those resources.
	if transitioningToTerminalPhase {
		if couldHaveRunningContainers {
			newPodStatus.Phase = oldPodStatus.Phase
			newPodStatus.Reason = oldPodStatus.Reason
			newPodStatus.Message = oldPodStatus.Message
		}
	}

	// If the new phase is terminal, explicitly set the ready condition to false for v1.PodReady and v1.ContainersReady.
	// It may take some time for kubelet to reconcile the ready condition, so explicitly set ready conditions to false if the phase is terminal.
	// This is done to ensure kubelet does not report a status update with terminal pod phase and ready=true.
	// See https://issues.k8s.io/108594 for more details.
	if podutil.IsPodPhaseTerminal(newPodStatus.Phase) {
		if podutil.IsPodReadyConditionTrue(newPodStatus) || podutil.IsContainersReadyConditionTrue(newPodStatus) {
			containersReadyCondition := generateContainersReadyConditionForTerminalPhase(newPodStatus.Phase)
			podutil.UpdatePodCondition(&newPodStatus, &containersReadyCondition)

			podReadyCondition := generatePodReadyConditionForTerminalPhase(newPodStatus.Phase)
			podutil.UpdatePodCondition(&newPodStatus, &podReadyCondition)
		}
	}

	return newPodStatus
}

// NeedToReconcilePodReadiness returns if the pod "Ready" condition need to be reconcile
func NeedToReconcilePodReadiness(pod *v1.Pod) bool {
	if len(pod.Spec.ReadinessGates) == 0 {
		return false
	}
	podReadyCondition := GeneratePodReadyCondition(&pod.Spec, pod.Status.Conditions, pod.Status.ContainerStatuses, pod.Status.Phase)
	i, curCondition := podutil.GetPodConditionFromList(pod.Status.Conditions, v1.PodReady)
	// Only reconcile if "Ready" condition is present and Status or Message is not expected
	if i >= 0 && (curCondition.Status != podReadyCondition.Status || curCondition.Message != podReadyCondition.Message) {
		return true
	}
	return false
}
