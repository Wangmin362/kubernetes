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

package kubelet

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	"k8s.io/klog/v2"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/eviction"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/queue"
	"k8s.io/utils/clock"
)

// OnCompleteFunc is a function that is invoked when an operation completes.
// If err is non-nil, the operation did not complete successfully.
type OnCompleteFunc func(err error)

// PodStatusFunc is a function that is invoked to override the pod status when a pod is killed.
type PodStatusFunc func(podStatus *v1.PodStatus)

// KillPodOptions are options when performing a pod update whose update type is kill.
type KillPodOptions struct {
	// CompletedCh is closed when the kill request completes (syncTerminatingPod has completed
	// without error) or if the pod does not exist, or if the pod has already terminated. This
	// could take an arbitrary amount of time to be closed, but is never left open once
	// CouldHaveRunningContainers() returns false.
	CompletedCh chan<- struct{}
	// Evict is true if this is a pod triggered eviction - once a pod is evicted some resources are
	// more aggressively reaped than during normal pod operation (stopped containers).
	Evict bool
	// PodStatusFunc is invoked (if set) and overrides the status of the pod at the time the pod is killed.
	// The provided status is populated from the latest state.
	PodStatusFunc PodStatusFunc
	// PodTerminationGracePeriodSecondsOverride is optional override to use if a pod is being killed as part of kill operation.
	PodTerminationGracePeriodSecondsOverride *int64
}

// UpdatePodOptions is an options struct to pass to a UpdatePod operation.
// 1、用于表示当前Pod的变更
type UpdatePodOptions struct {
	// Pod的更新类型，有四种更新类型，分别是：create, update, sync, kill
	UpdateType kubetypes.SyncPodType
	// StartTime is an optional timestamp for when this update was created. If set,
	// when this update is fully realized by the pod worker it will be recorded in
	// the PodWorkerDuration metric.
	// 1、SyncHandler执行这个动作（UpdateType）的开始时间
	StartTime time.Time
	// Pod to update. Required.
	// 1、用于表示当前变更的Pod，来自于File, HTTP, APIServer这三个源
	// 2、当Pod被删除时，这个属性就会为空，同时RunningPod将会被赋值。
	Pod *v1.Pod
	// MirrorPod is the mirror pod if Pod is a static pod. Optional when UpdateType
	// is kill or terminated.
	// 1、如果当前Pod是一个StaticPod，那么必然会有一个MirrorPod与之对应
	// 2、如果MirrorPod非空，那么Pod属性一定是StaticPod
	MirrorPod *v1.Pod
	// RunningPod is a runtime pod that is no longer present in config. Required
	// if Pod is nil, ignored if Pod is set.
	// TODO 什么情况下RunningPod不为空，但是Pod是nil呢？  RunningPod代表着什么？ 抽象了什么？
	// 1、当Pod被删除时，RunningPod会被设置，于此同时，由于Pod被删除了，所以Pod=nil
	RunningPod *kubecontainer.Pod
	// KillPodOptions is used to override the default termination behavior of the
	// pod or to update the pod status after an operation is completed. Since a
	// pod can be killed for multiple reasons, PodStatusFunc is invoked in order
	// and later kills have an opportunity to override the status (i.e. a preemption
	// may be later turned into an eviction).
	// 1、这个参数应该是当UpdateType = SyncPodKill才会生效
	KillPodOptions *KillPodOptions
}

// PodWorkType classifies the status of pod as seen by the pod worker - setup (sync),
// teardown of containers (terminating), or cleanup (terminated).
type PodWorkerState int

const (
	// SyncPod is when the pod is expected to be started and running.
	// TODO 当前容器希望被运行，也就是说Kubelet应该是刚发现用户提交的Pod
	SyncPod PodWorkerState = iota
	// TerminatingPod is when the pod is no longer being set up, but some
	// containers may be running and are being torn down.
	// TODO 当前Pod正在被删除，不过有些容器可能还在运行，这个状态的Pod应该发生在Pod刚刚被删除的时候
	TerminatingPod
	// TerminatedPod indicates the pod is stopped, can have no more running
	// containers, and any foreground cleanup can be executed.
	// TODO 标识当前Pod已经停止，即所有的容器都已经停止运行
	TerminatedPod
)

func (state PodWorkerState) String() string {
	switch state {
	case SyncPod:
		return "sync"
	case TerminatingPod:
		return "terminating"
	case TerminatedPod:
		return "terminated"
	default:
		panic(fmt.Sprintf("the state %d is not defined", state))
	}
}

// PodWorkerSync is the summarization of a single pod worker for sync. Values
// besides state are used to provide metric counts for operators.
type PodWorkerSync struct {
	// State of the pod.
	State PodWorkerState
	// Orphan is true if the pod is no longer in the desired set passed to SyncKnownPods.
	Orphan bool
	// HasConfig is true if we have a historical pod spec for this pod.
	HasConfig bool
	// Static is true if we have config and the pod came from a static source.
	Static bool
}

// podWork is the internal changes
type podWork struct {
	// WorkType is the type of sync to perform - sync (create), terminating (stop
	// containers), terminated (clean up and write status).
	WorkType PodWorkerState

	// Options contains the data to sync.
	Options UpdatePodOptions
}

// PodWorkers is an abstract interface for testability.
// TODO 如何理解这里的PodWorker接口的抽象？ 如何理解这个接口是为了可测试性？
// 1、PodWorker是Kubelet非常核心的一个组件，APIServer中保存的所有Pod元数据就是通过PodWorker落地实现的。了解K8S的人都知道，APIServer中
// 仅仅是保存了我们希望的Pod元数据，而我们期望的最终目标就是在节点上运行我们期望的Pod。PodWorker就是干这个事情的，它为每一个Pod启动了一个
// 协程，一旦这个Pod发生变化，就会触发这个协程同步更新用户期望并落地到真实的容器当中的。
// 2、从上面的描述当中，自然不难发现；PodManager仅仅是Kubelet的一个缓存，记录了Kubelet当前管理的所有Pod。更加重要的是，PodManager记录了
// StaticPod与MirrorPod之间的映射关系。即通过PodManager我们可以快速知道一个Pod到底是否是MirrorPod。
type PodWorkers interface {
	// UpdatePod notifies the pod worker of a change to a pod, which will then
	// be processed in FIFO order by a goroutine per pod UID. The state of the
	// pod will be passed to the syncPod method until either the pod is marked
	// as deleted, it reaches a terminal phase (Succeeded/Failed), or the pod
	// is evicted by the kubelet. Once that occurs the syncTerminatingPod method
	// will be called until it exits successfully, and after that all further
	// UpdatePod() calls will be ignored for that pod until it has been forgotten
	// due to significant time passing. A pod that is terminated will never be
	// restarted.
	// TODO 核心中的核心，难的扣脚
	// 1、此函数用于通知PodWorker关于一个Pod的变更，PodWorker会
	UpdatePod(options UpdatePodOptions)
	// SyncKnownPods removes workers for pods that are not in the desiredPods set
	// and have been terminated for a significant period of time. Once this method
	// has been called once, the workers are assumed to be fully initialized and
	// subsequent calls to ShouldPodContentBeRemoved on unknown pods will return
	// true. It returns a map describing the state of each known pod worker. It
	// is the responsibility of the caller to re-add any desired pods that are not
	// returned as knownPods.
	SyncKnownPods(desiredPods []*v1.Pod) (knownPods map[types.UID]PodWorkerSync)

	// IsPodKnownTerminated returns true once SyncTerminatingPod completes
	// successfully - the provided pod UID it is known by the pod
	// worker to be terminated. If the pod has been force deleted and the pod worker
	// has completed termination this method will return false, so this method should
	// only be used to filter out pods from the desired set such as in admission.
	//
	// Intended for use by the kubelet config loops, but not subsystems, which should
	// use ShouldPod*().
	IsPodKnownTerminated(uid types.UID) bool
	// CouldHaveRunningContainers returns true before the pod workers have synced,
	// once the pod workers see the pod (syncPod could be called), and returns false
	// after the pod has been terminated (running containers guaranteed stopped).
	//
	// Intended for use by the kubelet config loops, but not subsystems, which should
	// use ShouldPod*().
	CouldHaveRunningContainers(uid types.UID) bool

	// ShouldPodBeFinished returns true once SyncTerminatedPod completes
	// successfully - the provided pod UID it is known to the pod worker to
	// be terminated and have resources reclaimed. It returns false before the
	// pod workers have synced (syncPod could be called). Once the pod workers
	// have synced it returns false if the pod has a sync status until
	// SyncTerminatedPod completes successfully. If the pod workers have synced,
	// but the pod does not have a status it returns true.
	//
	// Intended for use by subsystem sync loops to avoid performing background setup
	// after termination has been requested for a pod. Callers must ensure that the
	// syncPod method is non-blocking when their data is absent.
	ShouldPodBeFinished(uid types.UID) bool
	// IsPodTerminationRequested returns true when pod termination has been requested
	// until the termination completes and the pod is removed from config. This should
	// not be used in cleanup loops because it will return false if the pod has already
	// been cleaned up - use ShouldPodContainersBeTerminating instead. Also, this method
	// may return true while containers are still being initialized by the pod worker.
	//
	// Intended for use by the kubelet sync* methods, but not subsystems, which should
	// use ShouldPod*().
	IsPodTerminationRequested(uid types.UID) bool

	// ShouldPodContainersBeTerminating returns false before pod workers have synced,
	// or once a pod has started terminating. This check is similar to
	// ShouldPodRuntimeBeRemoved but is also true after pod termination is requested.
	//
	// Intended for use by subsystem sync loops to avoid performing background setup
	// after termination has been requested for a pod. Callers must ensure that the
	// syncPod method is non-blocking when their data is absent.
	ShouldPodContainersBeTerminating(uid types.UID) bool
	// ShouldPodRuntimeBeRemoved returns true if runtime managers within the Kubelet
	// should aggressively cleanup pod resources that are not containers or on disk
	// content, like attached volumes. This is true when a pod is not yet observed
	// by a worker after the first sync (meaning it can't be running yet) or after
	// all running containers are stopped.
	// TODO: Once pod logs are separated from running containers, this method should
	// be used to gate whether containers are kept.
	//
	// Intended for use by subsystem sync loops to know when to start tearing down
	// resources that are used by running containers. Callers should ensure that
	// runtime content they own is not required for post-termination - for instance
	// containers are required in docker to preserve pod logs until after the pod
	// is deleted.
	ShouldPodRuntimeBeRemoved(uid types.UID) bool
	// ShouldPodContentBeRemoved returns true if resource managers within the Kubelet
	// should aggressively cleanup all content related to the pod. This is true
	// during pod eviction (when we wish to remove that content to free resources)
	// as well as after the request to delete a pod has resulted in containers being
	// stopped (which is a more graceful action). Note that a deleting pod can still
	// be evicted.
	//
	// Intended for use by subsystem sync loops to know when to start tearing down
	// resources that are used by non-deleted pods. Content is generally preserved
	// until deletion+removal_from_etcd or eviction, although garbage collection
	// can free content when this method returns false.
	ShouldPodContentBeRemoved(uid types.UID) bool
	// IsPodForMirrorPodTerminatingByFullName returns true if a static pod with the
	// provided pod name is currently terminating and has yet to complete. It is
	// intended to be used only during orphan mirror pod cleanup to prevent us from
	// deleting a terminating static pod from the apiserver before the pod is shut
	// down.
	IsPodForMirrorPodTerminatingByFullName(podFullname string) bool
}

// podSyncer describes the core lifecyle operations of the pod state machine. A pod is first
// synced until it naturally reaches termination (true is returned) or an external agent decides
// the pod should be terminated. Once a pod should be terminating, SyncTerminatingPod is invoked
// until it returns no error. Then the SyncTerminatedPod method is invoked until it exits without
// error, and the pod is considered terminal. Implementations of this interface must be threadsafe
// for simultaneous invocation of these methods for multiple pods.
// TODO 如何理解这个接口？
type podSyncer interface {
	// SyncPod configures the pod and starts and restarts all containers. If it returns true, the
	// pod has reached a terminal state and the presence of the error indicates succeeded or failed.
	// If an error is returned, the sync was not successful and should be rerun in the future. This
	// is a long running method and should exit early with context.Canceled if the context is canceled.
	// 1、pod参数为用户期望状态，podStatus为pod的实际状态, mirrorPod如果非空，说明pod是StaticPod
	SyncPod(ctx context.Context, updateType kubetypes.SyncPodType, pod *v1.Pod, mirrorPod *v1.Pod, podStatus *kubecontainer.PodStatus) (bool, error)
	// SyncTerminatingPod attempts to ensure the pod's containers are no longer running and to collect
	// any final status. This method is repeatedly invoked with diminishing grace periods until it exits
	// without error. Once this method exits with no error other components are allowed to tear down
	// supporting resources like volumes and devices. If the context is canceled, the method should
	// return context.Canceled unless it has successfully finished, which may occur when a shorter
	// grace period is detected.

	SyncTerminatingPod(ctx context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus, gracePeriod *int64, podStatusFn func(*v1.PodStatus)) error
	// SyncTerminatingRuntimePod is invoked when running containers are found that correspond to
	// a pod that is no longer known to the kubelet to terminate those containers. It should not
	// exit without error unless all containers are known to be stopped.
	// 1、podStatus为pod的实际状态
	SyncTerminatingRuntimePod(ctx context.Context, runningPod *kubecontainer.Pod) error
	// SyncTerminatedPod is invoked after all running containers are stopped and is responsible
	// for releasing resources that should be executed right away rather than in the background.
	// Once it exits without error the pod is considered finished on the node.
	// 1、pod参数为用户期望状态，podStatus为pod的实际状态
	SyncTerminatedPod(ctx context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus) error
}

type syncPodFnType func(ctx context.Context, updateType kubetypes.SyncPodType, pod *v1.Pod, mirrorPod *v1.Pod, podStatus *kubecontainer.PodStatus) (bool, error)
type syncTerminatingPodFnType func(ctx context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus, gracePeriod *int64, podStatusFn func(*v1.PodStatus)) error
type syncTerminatingRuntimePodFnType func(ctx context.Context, runningPod *kubecontainer.Pod) error
type syncTerminatedPodFnType func(ctx context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus) error

// podSyncerFuncs implements podSyncer and accepts functions for each method.
type podSyncerFuncs struct {
	syncPod                   syncPodFnType
	syncTerminatingPod        syncTerminatingPodFnType
	syncTerminatingRuntimePod syncTerminatingRuntimePodFnType
	syncTerminatedPod         syncTerminatedPodFnType
}

func newPodSyncerFuncs(s podSyncer) podSyncerFuncs {
	return podSyncerFuncs{
		syncPod:                   s.SyncPod,
		syncTerminatingPod:        s.SyncTerminatingPod,
		syncTerminatingRuntimePod: s.SyncTerminatingRuntimePod,
		syncTerminatedPod:         s.SyncTerminatedPod,
	}
}

var _ podSyncer = podSyncerFuncs{}

func (f podSyncerFuncs) SyncPod(ctx context.Context, updateType kubetypes.SyncPodType, pod *v1.Pod, mirrorPod *v1.Pod, podStatus *kubecontainer.PodStatus) (bool, error) {
	return f.syncPod(ctx, updateType, pod, mirrorPod, podStatus)
}
func (f podSyncerFuncs) SyncTerminatingPod(ctx context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus, gracePeriod *int64, podStatusFn func(*v1.PodStatus)) error {
	return f.syncTerminatingPod(ctx, pod, podStatus, gracePeriod, podStatusFn)
}
func (f podSyncerFuncs) SyncTerminatingRuntimePod(ctx context.Context, runningPod *kubecontainer.Pod) error {
	return f.syncTerminatingRuntimePod(ctx, runningPod)
}
func (f podSyncerFuncs) SyncTerminatedPod(ctx context.Context, pod *v1.Pod, podStatus *kubecontainer.PodStatus) error {
	return f.syncTerminatedPod(ctx, pod, podStatus)
}

const (
	// jitter factor for resyncInterval
	workerResyncIntervalJitterFactor = 0.5

	// jitter factor for backOffPeriod and backOffOnTransientErrorPeriod
	workerBackOffPeriodJitterFactor = 0.5

	// backoff period when transient error occurred.
	backOffOnTransientErrorPeriod = time.Second
)

// podSyncStatus tracks per-pod transitions through the three phases of pod
// worker sync (setup, terminating, terminated).
// TODO 似乎是用来记录Pod同步状态的数据结构
// 1、TODO 用于记录Pod状态转换，Pod可能的状态有：setup, terminating, terminated
type podSyncStatus struct {
	// ctx is the context that is associated with the current pod sync.
	// TODO: remove this from the struct by having the context initialized
	// in startPodSync, the cancelFn used by UpdatePod, and cancellation of
	// a parent context for tearing down workers (if needed) on shutdown
	ctx context.Context
	// cancelFn if set is expected to cancel the current podSyncer operation.
	cancelFn context.CancelFunc

	// 即<podName>_<namespace>
	fullname string

	// working is true if an update is pending or being worked by a pod worker
	// goroutine.
	// TODO 这里似乎在说，当go协程在处理这个Pod的时候就会设置为true
	working bool
	// pendingUpdate is the updated state the pod worker should observe. It is
	// cleared and moved to activeUpdate when a pod worker reads it. A new update
	// may always replace a pending update as the pod worker does not guarantee
	// that all intermediate states are synced to a worker, only the most recent.
	// This state will not be visible to downstream components until a pod worker
	// has begun processing it.
	// TODO 干嘛的？ 一个Ping更新意味着什么？
	pendingUpdate *UpdatePodOptions
	// activeUpdate is the most recent version of the pod's state that will be
	// passed to a sync*Pod function. A pod becomes visible to downstream components
	// once a worker decides to start a pod (startedAt is set). The pod and mirror
	// pod fields are accumulated if they are missing on a particular call (the last
	// known version), and the value of KillPodOptions is accumulated as pods cannot
	// have their grace period shortened. This is the source of truth for the pod spec
	// the kubelet is reconciling towards for all components that act on running pods.
	// TODO 干嘛的？
	activeUpdate *UpdatePodOptions

	// syncedAt is the time at which the pod worker first observed this pod.
	// PodWorker第一次看到这个Pod的时间
	syncedAt time.Time
	// startedAt is the time at which the pod worker allowed the pod to start.
	// Pod启动的时间
	startedAt time.Time
	// terminatingAt is set once the pod is requested to be killed - note that
	// this can be set before the pod worker starts terminating the pod, see
	// terminating.
	// Pod执行结束开始的时间
	terminatingAt time.Time
	// terminatedAt is set once the pod worker has completed a successful
	// syncTerminatingPod call and means all running containers are stopped.
	// Pod执行结束完成的时间
	terminatedAt time.Time
	// gracePeriod is the requested gracePeriod once terminatingAt is nonzero.
	// 等待Pod执行结束的时间
	gracePeriod int64
	// notifyPostTerminating will be closed once the pod transitions to
	// terminated. After the pod is in terminated state, nothing should be
	// added to this list.
	// TODO 这玩意干嘛的？ 谁会使用？
	notifyPostTerminating []chan<- struct{}
	// statusPostTerminating is a list of the status changes associated
	// with kill pod requests. After the pod is in terminated state, nothing
	// should be added to this list. The worker will execute the last function
	// in this list on each termination attempt.
	statusPostTerminating []PodStatusFunc

	// startedTerminating is true once the pod worker has observed the request to
	// stop a pod (exited syncPod and observed a podWork with WorkType
	// TerminatingPod). Once this is set, it is safe for other components
	// of the kubelet to assume that no other containers may be started.
	// 当PodWorker观测到当前Pod正在被停止，就会把这个属性设置为true。于此同时，其余的组件也会认为这个Pod中的所有容器已经停止。
	startedTerminating bool
	// deleted is true if the pod has been marked for deletion on the apiserver
	// or has no configuration represented (was deleted before).
	deleted bool
	// evicted is true if the kill indicated this was an eviction (an evicted
	// pod can be more aggressively cleaned up).
	// 1、表明当前Pod是否被驱逐，如果是被驱逐，说明当前Pod只是被当前节点的kubelet删除，其它节点需要重新创建这个Pod。否则很有可能当前Pod
	// 就是在被删除
	evicted bool
	// finished is true once the pod worker completes for a pod
	// (syncTerminatedPod exited with no errors) until SyncKnownPods is invoked
	// to remove the pod. A terminal pod (Succeeded/Failed) will have
	// termination status until the pod is deleted.
	// 1、如果为true，说明当前Pod已经在PodWorker中处理完成；
	// 2、已经处理完成的Pod不允许再次进入到PodWorker当中
	finished bool
	// restartRequested is true if the pod worker was informed the pod is
	// expected to exist (update type of create, update, or sync) after
	// it has been killed. When known pods are synced, any pod that is
	// terminated and has restartRequested will have its history cleared.
	restartRequested bool
	// observedRuntime is true if the pod has been observed to be present in the
	// runtime. A pod that has been observed at runtime must go through either
	// SyncTerminatingRuntimePod or SyncTerminatingPod. Otherwise, we can avoid
	// invoking the terminating methods if the pod is deleted or orphaned before
	// it has been started.
	// TODO 这个状态Pod似乎处于被删除的状态
	// 1、APIServer中不存在这个Pod，但是底层的CRI容器还在运行的Pod
	observedRuntime bool
}

func (s *podSyncStatus) IsWorking() bool              { return s.working }
func (s *podSyncStatus) IsTerminationRequested() bool { return !s.terminatingAt.IsZero() }
func (s *podSyncStatus) IsTerminationStarted() bool   { return s.startedTerminating }
func (s *podSyncStatus) IsTerminated() bool           { return !s.terminatedAt.IsZero() }
func (s *podSyncStatus) IsFinished() bool             { return s.finished }
func (s *podSyncStatus) IsEvicted() bool              { return s.evicted }
func (s *podSyncStatus) IsDeleted() bool              { return s.deleted }
func (s *podSyncStatus) IsStarted() bool              { return !s.startedAt.IsZero() }

// WorkType returns this pods' current state of the pod in pod lifecycle state machine.
func (s *podSyncStatus) WorkType() PodWorkerState {
	if s.IsTerminated() {
		return TerminatedPod
	}
	if s.IsTerminationRequested() {
		return TerminatingPod
	}
	return SyncPod
}

// mergeLastUpdate records the most recent state from a new update. Pod and MirrorPod are
// incremented. KillPodOptions is accumulated. If RunningPod is set, Pod is synthetic and
// will *not* be used as the last pod state unless no previous pod state exists (because
// the pod worker may be responsible for terminating a pod from a previous run of the
// kubelet where no config state is visible). The contents of activeUpdate are used as the
// source of truth for components downstream of the pod workers.
// activeUpdate的数据也是来源于pendingUpdate
func (s *podSyncStatus) mergeLastUpdate(other UpdatePodOptions) {
	opts := s.activeUpdate
	if opts == nil {
		opts = &UpdatePodOptions{}
		s.activeUpdate = opts
	}

	// UpdatePodOptions states (and UpdatePod enforces) that either Pod or RunningPod
	// is set, and we wish to preserve the most recent Pod we have observed, so only
	// overwrite our Pod when we have no Pod or when RunningPod is nil.
	if opts.Pod == nil || other.RunningPod == nil {
		opts.Pod = other.Pod
	}
	// running pods will not persist but will be remembered for replay
	opts.RunningPod = other.RunningPod
	// if mirrorPod was not provided, remember the last one for replay
	if other.MirrorPod != nil {
		opts.MirrorPod = other.MirrorPod
	}
	// accumulate kill pod options
	if other.KillPodOptions != nil {
		opts.KillPodOptions = &KillPodOptions{}
		if other.KillPodOptions.Evict {
			opts.KillPodOptions.Evict = true
		}
		if override := other.KillPodOptions.PodTerminationGracePeriodSecondsOverride; override != nil {
			value := *override
			opts.KillPodOptions.PodTerminationGracePeriodSecondsOverride = &value
		}
	}
	// StartTime is not copied - that is purely for tracking latency of config propagation
	// from kubelet to pod worker.
}

// podWorkers keeps track of operations on pods and ensures each pod is
// reconciled with the container runtime and other subsystems. The worker
// also tracks which pods are in flight for starting, which pods are
// shutting down but still have running containers, and which pods have
// terminated recently and are guaranteed to have no running containers.
//
// podWorkers is the source of truth for what pods should be active on a
// node at any time, and is kept up to date with the desired state of the
// node (tracked by the kubelet pod config loops and the state in the
// kubelet's podManager) via the UpdatePod method. Components that act
// upon running pods should look to the pod worker for state instead of the
// kubelet podManager. The pod worker is periodically reconciled with the
// state of the podManager via SyncKnownPods() and is responsible for
// ensuring the completion of all observed pods no longer present in
// the podManager (no longer part of the node's desired config).
//
// A pod passed to a pod worker is either being synced (expected to be
// running), terminating (has running containers but no new containers are
// expected to start), terminated (has no running containers but may still
// have resources being consumed), or cleaned up (no resources remaining).
// Once a pod is set to be "torn down" it cannot be started again for that
// UID (corresponding to a delete or eviction) until:
//
//  1. The pod worker is finalized (syncTerminatingPod and
//     syncTerminatedPod exit without error sequentially)
//  2. The SyncKnownPods method is invoked by kubelet housekeeping and the pod
//     is not part of the known config.
//
// Pod workers provide a consistent source of information to other kubelet
// loops about the status of the pod and whether containers can be
// running. The ShouldPodContentBeRemoved() method tracks whether a pod's
// contents should still exist, which includes non-existent pods after
// SyncKnownPods() has been called once (as per the contract, all existing
// pods should be provided via UpdatePod before SyncKnownPods is invoked).
// Generally other sync loops are expected to separate "setup" and
// "teardown" responsibilities and the information methods here assist in
// each by centralizing that state. A simple visualization of the time
// intervals involved might look like:
//
// ---|                                         = kubelet config has synced at least once
// -------|                                  |- = pod exists in apiserver config
// --------|                  |---------------- = CouldHaveRunningContainers() is true
//
//	^- pod is observed by pod worker  .
//	.                                 .
//
// ----------|       |------------------------- = syncPod is running
//
//	. ^- pod worker loop sees change and invokes syncPod
//	. .                               .
//
// --------------|                     |------- = ShouldPodContainersBeTerminating() returns true
// --------------|                     |------- = IsPodTerminationRequested() returns true (pod is known)
//
//	. .   ^- Kubelet evicts pod       .
//	. .                               .
//
// -------------------|       |---------------- = syncTerminatingPod runs then exits without error
//
//	        . .        ^ pod worker loop exits syncPod, sees pod is terminating,
//					 . .          invokes syncTerminatingPod
//	        . .                               .
//
// ---|    |------------------|              .  = ShouldPodRuntimeBeRemoved() returns true (post-sync)
//
//	.                ^ syncTerminatingPod has exited successfully
//	.                               .
//
// ----------------------------|       |------- = syncTerminatedPod runs then exits without error
//
//	.                         ^ other loops can tear down
//	.                               .
//
// ------------------------------------|  |---- = status manager is waiting for SyncTerminatedPod() finished
//
//	.                         ^     .
//
// ----------|                               |- = status manager can be writing pod status
//
//	^ status manager deletes pod because no longer exists in config
//
// Other components in the Kubelet can request a termination of the pod
// via the UpdatePod method or the killPodNow wrapper - this will ensure
// the components of the pod are stopped until the kubelet is restarted
// or permanently (if the phase of the pod is set to a terminal phase
// in the pod status change).
type podWorkers struct {
	// Protects all per worker fields.
	podLock sync.Mutex
	// podsSynced is true once the pod worker has been synced at least once,
	// which means that all working pods have been started via UpdatePod().
	podsSynced bool

	// Tracks all running per-pod goroutines - per-pod goroutine will be
	// processing updates received through its corresponding channel. Sending
	// a message on this channel will signal the corresponding goroutine to
	// consume podSyncStatuses[uid].pendingUpdate if set.
	// 1、key为PodUID，而value为一个channel，channel的容量为1
	// 2、podWorker会给每为每一个Pod启动一个协程，同时这里的Value就是协程的数据来源，每当数据Pod状态变动时，就会把数据写入到channel当中，
	// 启动的协程只需要监听这个channel即可
	podUpdates map[types.UID]chan struct{}
	// Tracks by UID the termination status of a pod - syncing, terminating,
	// terminated, and evicted.
	// TODO ?
	podSyncStatuses map[types.UID]*podSyncStatus

	// Tracks all uids for started static pods by full name
	// 记录已经启动的StaticPod, Key为：<podName>_<namespace>
	startedStaticPodsByFullname map[string]types.UID
	// Tracks all uids for static pods that are waiting to start by full name
	// 1、记录等待启动的StaticPod，Key为：<podName>_<namespace>，value为PodUID
	// 2、一个Pod可能会被重启，重启之后Pod UID可能会发生变化，所以这里的Value为一个数组
	waitingToStartStaticPodsByFullname map[string][]types.UID

	// 1、很简单的延迟队列，放入元素的时候可以指定元素的过期时间，只有当元素过期了才可以从队列当中取出来
	// TODO 2、什么时候会放入元素？
	// TODO 3、什么时候会取出元素？
	workQueue queue.WorkQueue

	// This function is run to sync the desired state of pod.
	// NOTE: This function has to be thread-safe - it can be called for
	// different pods at the same time.
	podSyncer podSyncer

	// workerChannelFn is exposed for testing to allow unit tests to impose delays
	// in channel communication. The function is invoked once each time a new worker
	// goroutine starts.
	// todo 这玩意似乎是用来做单元测试的
	workerChannelFn func(uid types.UID, in chan struct{}) (out <-chan struct{})

	// The EventRecorder to use
	recorder record.EventRecorder

	// backOffPeriod is the duration to back off when there is a sync error.
	backOffPeriod time.Duration

	// resyncInterval is the duration to wait until the next sync.
	resyncInterval time.Duration

	// podCache stores kubecontainer.PodStatus for all pods.
	// 1、缓存Pod状态，用于获取Pod的状态，或者获取比指定时间更新的Pod状态
	// 2、PodCache缓存的是底层CRI返回的Pod的状态，而PodManager缓存的是APIServer中的状态。PodCache缓存可以理解为Pod的实际状态，
	// 而PodManager缓存的则是用户的期望状态
	podCache kubecontainer.Cache

	// clock is used for testing timing
	clock clock.PassiveClock
}

func newPodWorkers(
	podSyncer podSyncer,
	recorder record.EventRecorder,
	workQueue queue.WorkQueue, // 很简单的延迟队列，放入元素的时候可以指定元素的过期时间，只有当元素过期了才可以从队列当中取出来
	resyncInterval, backOffPeriod time.Duration,
	podCache kubecontainer.Cache, // 缓存Pod状态，用于获取Pod的状态，或者获取比指定时间更新的Pod状态
) PodWorkers {
	return &podWorkers{
		podSyncStatuses:                    map[types.UID]*podSyncStatus{},
		podUpdates:                         map[types.UID]chan struct{}{},
		startedStaticPodsByFullname:        map[string]types.UID{},
		waitingToStartStaticPodsByFullname: map[string][]types.UID{},
		podSyncer:                          podSyncer,
		recorder:                           recorder,
		workQueue:                          workQueue,
		resyncInterval:                     resyncInterval,
		backOffPeriod:                      backOffPeriod,
		podCache:                           podCache,
		clock:                              clock.RealClock{},
	}
}

func (p *podWorkers) IsPodKnownTerminated(uid types.UID) bool {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	if status, ok := p.podSyncStatuses[uid]; ok {
		return status.IsTerminated()
	}
	// if the pod is not known, we return false (pod worker is not aware of it)
	return false
}

func (p *podWorkers) CouldHaveRunningContainers(uid types.UID) bool {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	if status, ok := p.podSyncStatuses[uid]; ok {
		return !status.IsTerminated()
	}
	// once all pods are synced, any pod without sync status is known to not be running.
	return !p.podsSynced
}

func (p *podWorkers) ShouldPodBeFinished(uid types.UID) bool {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	if status, ok := p.podSyncStatuses[uid]; ok {
		return status.IsFinished()
	}
	// once all pods are synced, any pod without sync status is assumed to
	// have SyncTerminatedPod finished.
	return p.podsSynced
}

func (p *podWorkers) IsPodTerminationRequested(uid types.UID) bool {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	if status, ok := p.podSyncStatuses[uid]; ok {
		// the pod may still be setting up at this point.
		return status.IsTerminationRequested()
	}
	// an unknown pod is considered not to be terminating (use ShouldPodContainersBeTerminating in
	// cleanup loops to avoid failing to cleanup pods that have already been removed from config)
	return false
}

func (p *podWorkers) ShouldPodContainersBeTerminating(uid types.UID) bool {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	if status, ok := p.podSyncStatuses[uid]; ok {
		// we wait until the pod worker goroutine observes the termination, which means syncPod will not
		// be executed again, which means no new containers can be started
		return status.IsTerminationStarted()
	}
	// once we've synced, if the pod isn't known to the workers we should be tearing them
	// down
	return p.podsSynced
}

func (p *podWorkers) ShouldPodRuntimeBeRemoved(uid types.UID) bool {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	if status, ok := p.podSyncStatuses[uid]; ok {
		return status.IsTerminated()
	}
	// a pod that hasn't been sent to the pod worker yet should have no runtime components once we have
	// synced all content.
	return p.podsSynced
}

func (p *podWorkers) ShouldPodContentBeRemoved(uid types.UID) bool {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	if status, ok := p.podSyncStatuses[uid]; ok {
		return status.IsEvicted() || (status.IsDeleted() && status.IsTerminated())
	}
	// a pod that hasn't been sent to the pod worker yet should have no content on disk once we have
	// synced all content.
	return p.podsSynced
}

func (p *podWorkers) IsPodForMirrorPodTerminatingByFullName(podFullName string) bool {
	p.podLock.Lock()
	defer p.podLock.Unlock()
	uid, started := p.startedStaticPodsByFullname[podFullName]
	if !started {
		return false
	}
	status, exists := p.podSyncStatuses[uid]
	if !exists {
		return false
	}
	if !status.IsTerminationRequested() || status.IsTerminated() {
		return false
	}

	return true
}

// 如果当前Pod没有任何一个容器再运行，同时也没有任何一个沙箱正在运行，我们就认为Pod已经停止
func isPodStatusCacheTerminal(status *kubecontainer.PodStatus) bool {
	runningContainers := 0
	runningSandboxes := 0
	for _, container := range status.ContainerStatuses {
		if container.State == kubecontainer.ContainerStateRunning {
			runningContainers++
		}
	}
	for _, sb := range status.SandboxStatuses {
		if sb.State == runtimeapi.PodSandboxState_SANDBOX_READY {
			runningSandboxes++
		}
	}
	return runningContainers == 0 && runningSandboxes == 0
}

// UpdatePod carries a configuration change or termination state to a pod. A pod is either runnable,
// terminating, or terminated, and will transition to terminating if: deleted on the apiserver,
// discovered to have a terminal phase (Succeeded or Failed), or evicted by the kubelet.
// 1、一个Pod只可能处于runnable, terminating, terminated三个状态。并且只有当Pod被APIServer删除之后，或者Pod运行完成，处于Succeeded/Failed
// 状态，这个时候Pod就会从runnable状态转变为terminating状态
func (p *podWorkers) UpdatePod(options UpdatePodOptions) {
	// Handle when the pod is an orphan (no config) and we only have runtime status by running only
	// the terminating part of the lifecycle. A running pod contains only a minimal set of information
	// about the pod
	var isRuntimePod bool // 所谓RuntimePod，其实指的是APIServer中已经删除，但是底层CRI还没有删除的Pod
	var uid types.UID     // PodUID
	var name, ns string   // PodName以及Pod所在的名称空间
	// 1、如果RunningPod非空，但是Pod为空，说明当前Pod一定是处于被删除的状态，因此更新类型一定是PodKill，否则一定是状态更新有问题。
	// 2、如果RunningPod非空，并且Pod非空，那么RunningPod将会被忽略，只使用Pod属性。Pod非空意味着这个Pod肯定没有被删除，那么可能是更新了
	// 当前的Pod
	if runningPod := options.RunningPod; runningPod != nil {
		if options.Pod == nil {
			// the sythetic pod created here is used only as a placeholder and not tracked
			// 如果从APIServer中没有找到Pod,但是runningPod非空，那么说明Pod一定是被删除了，因此这种状态下更新事件必须是Kill事件，否则忽略此次更新
			if options.UpdateType != kubetypes.SyncPodKill {
				klog.InfoS("Pod update is ignored, runtime pods can only be killed", "pod",
					klog.KRef(runningPod.Namespace, runningPod.Name), "podUID", runningPod.ID, "updateType", options.UpdateType)
				return
			}
			uid, ns, name = runningPod.ID, runningPod.Namespace, runningPod.Name
			isRuntimePod = true
		} else {
			// 如果Pod不为空，说明当前Pod一定没有被删除，那么忽略RunningPod属性
			options.RunningPod = nil
			uid, ns, name = options.Pod.UID, options.Pod.Namespace, options.Pod.Name
			klog.InfoS("Pod update included RunningPod which is only valid when Pod is not specified", "pod",
				klog.KRef(ns, name), "podUID", uid, "updateType", options.UpdateType)
		}
	} else {
		uid, ns, name = options.Pod.UID, options.Pod.Namespace, options.Pod.Name
	}

	p.podLock.Lock()
	defer p.podLock.Unlock()

	// decide what to do with this pod - we are either setting it up, tearing it down, or ignoring it
	var firstTime bool
	now := p.clock.Now() // 获取当前时间
	// 获取Pod是Sync状态，如果没有找到，说明当前Pod是PodWorker第一次处理，直接实例化一个Pod状态同步数据结构用于记录当前Pod的状态转换
	status, ok := p.podSyncStatuses[uid]
	if !ok {
		klog.V(4).InfoS("Pod is being synced for the first time", "pod", klog.KRef(ns, name), "podUID", uid, "updateType", options.UpdateType)
		firstTime = true // 如果没有找到当前Pod的同步状态，那么这个Pod肯定是PodWorker第一次看到
		status = &podSyncStatus{
			syncedAt: now,
			fullname: kubecontainer.BuildPodFullName(name, ns),
		}
		// if this pod is being synced for the first time, we need to make sure it is an active pod
		// 1、Pod属性非空，说明当前Pod一定没有被删除，如果当前Pod的状态被设置为PodFailed或者PodSucceeded状态，说明当前Pod已经执行完了。
		// 2、如果Pod非空，我们第一时间需要检测Pod是否运行结束，如果Pod已经运行结束，那么我们需要记录Pod的结束状态
		if options.Pod != nil && (options.Pod.Status.Phase == v1.PodFailed || options.Pod.Status.Phase == v1.PodSucceeded) {
			// Check to see if the pod is not running and the pod is terminal; if this succeeds then record in the podWorker that it is terminated.
			// This is needed because after a kubelet restart, we need to ensure terminal pods will NOT be considered active in Pod Admission. See http://issues.k8s.io/105523
			// However, `filterOutInactivePods`, considers pods that are actively terminating as active. As a result, `IsPodKnownTerminated()` needs to return true and thus `terminatedAt` needs to be set.
			// 1、获取Pod的实际状态，err为空，说明这个Pod之前通过CRI运行时获取状态没有错误
			if statusCache, err := p.podCache.Get(uid); err == nil {
				// 1、如果当前Pod没有任何一个容器再运行，同时也没有任何一个沙箱正在运行，我们就认为Pod已经停止
				// 2、如果PodWorker观测到Pod已经停止，这也意味着这个容器之前一定被调用过SyncTerminatingPod方法，这种情况下一般是由于
				// kubelet重启造成的。因为kubelet第一次看到这个Pod都已经运行结束，说明之前一定被运行过，也就是说这个Pod之前一定被
				// kubelet处理过。所以这种情况下，只能是kubelet被重启。
				if isPodStatusCacheTerminal(statusCache) {
					// At this point we know:
					// (1) The pod is terminal based on the config source. TODO 什么叫做config source? 我猜测ConfigSource指的是PodConfig，也就是用户期望状态
					// (2) The pod is terminal based on the runtime cache. TODO 而RuntimeCache则是Pod的实际状态
					// This implies that this pod had already completed `SyncTerminatingPod` sometime in the past. The pod is likely being synced for the first time due to a kubelet restart.
					// These pods need to complete SyncTerminatedPod to ensure that all resources are cleaned and that the status manager makes the final status updates for the pod.
					// As a result, set finished: false, to ensure a Terminated event will be sent and `SyncTerminatedPod` will run.
					status = &podSyncStatus{
						terminatedAt:       now,
						terminatingAt:      now,
						syncedAt:           now,
						startedTerminating: true,
						// 设置Kubelet还没有处理完这个Pod，不能直接干掉这个Pod，还需确保所有的源都已经删除了这个Pod，同时状态管理器
						// 也更新了这个Pod最后的状态
						finished: false,
						fullname: kubecontainer.BuildPodFullName(name, ns),
					}
				}
			}
		}
		// 如果这个Pod是第一次出现，显然还没有记录过这个pod的状态，因此这里需要把初始化的状态记录下来
		p.podSyncStatuses[uid] = status
	}

	// RunningPods represent an unknown pod execution and don't contain pod spec information
	// sufficient to perform any action other than termination. If we received a RunningPod
	// after a real pod has already been provided, use the most recent spec instead. Also,
	// once we observe a runtime pod we must drive it to completion, even if we weren't the
	// ones who started it.
	pod := options.Pod
	// 说明RunningPod非空，并且Pod为空
	if isRuntimePod {
		status.observedRuntime = true
		switch {
		case status.pendingUpdate != nil && status.pendingUpdate.Pod != nil:
			pod = status.pendingUpdate.Pod
			options.Pod = pod
			options.RunningPod = nil
		case status.activeUpdate != nil && status.activeUpdate.Pod != nil:
			pod = status.activeUpdate.Pod
			options.Pod = pod
			options.RunningPod = nil
		default:
			// we will continue to use RunningPod.ToAPIPod() as pod here, but
			// options.Pod will be nil and other methods must handle that appropriately.
			pod = options.RunningPod.ToAPIPod()
		}
	}

	// When we see a create update on an already terminating pod, that implies two pods with the same UID were created in
	// close temporal proximity (usually static pod but it's possible for an apiserver to extremely rarely do something
	// similar) - flag the sync status to indicate that after the pod terminates it should be reset to "not running" to
	// allow a subsequent add/update to start the pod worker again. This does not apply to the first time we see a pod,
	// such as when the kubelet restarts and we see already terminated pods for the first time.
	// 如果当前Pod不是第一次看见，并且当前Pod处于Terminating状态，并且当前Pod的更新类型为Create，那么需要重启这个Pod
	if !firstTime && status.IsTerminationRequested() {
		if options.UpdateType == kubetypes.SyncPodCreate {
			status.restartRequested = true
			klog.V(4).InfoS("Pod is terminating but has been requested to restart with same UID, will be reconciled later", "pod", klog.KRef(ns, name), "podUID", uid, "updateType", options.UpdateType)
			return
		}
	}

	// once a pod is terminated by UID, it cannot reenter the pod worker (until the UID is purged by housekeeping)
	// 说明当前Pod已经在PodWorker中处理完成，那么PodWorker将不再处理这个Pod
	if status.IsFinished() {
		klog.V(4).InfoS("Pod is finished processing, no further updates", "pod", klog.KRef(ns, name), "podUID", uid, "updateType", options.UpdateType)
		return
	}

	// check for a transition to terminating
	var becameTerminating bool
	// 如果当前Pod没有处于Terminating状态，说明当前Pod正在运行
	if !status.IsTerminationRequested() {
		switch {
		case isRuntimePod: // 所谓RuntimePod其实就是在APIServer中不存在，但是在底层的CRI容器运行时中存在的Pod，因此这类的Pod应当要删除
			klog.V(4).InfoS("Pod is orphaned and must be torn down", "pod", klog.KRef(ns, name), "podUID", uid, "updateType", options.UpdateType)
			status.deleted = true
			status.terminatingAt = now
			becameTerminating = true
		case pod.DeletionTimestamp != nil: // 说明Pod需要优雅删除
			klog.V(4).InfoS("Pod is marked for graceful deletion, begin teardown", "pod", klog.KRef(ns, name), "podUID", uid, "updateType", options.UpdateType)
			status.deleted = true
			status.terminatingAt = now
			becameTerminating = true
		case pod.Status.Phase == v1.PodFailed, pod.Status.Phase == v1.PodSucceeded: // 说明当前Pod已经运行结束
			klog.V(4).InfoS("Pod is in a terminal phase (success/failed), begin teardown", "pod", klog.KRef(ns, name), "podUID", uid, "updateType", options.UpdateType)
			// TODO  这里并没有设置terminatedAt时间，我猜测是因为Pod虽然已经运行完成，但是Pod在运行之前申请的某些在原还需要释放，等这些动作
			// 都执行完成之后，我猜测最后应该会设置terminatedAt时间
			status.terminatingAt = now
			becameTerminating = true
		case options.UpdateType == kubetypes.SyncPodKill:
			if options.KillPodOptions != nil && options.KillPodOptions.Evict {
				// 说明当前Pod正在被驱逐，一般是因为当前节点的资源不充足，并且这个Pod的优先级比较低造成的。
				klog.V(4).InfoS("Pod is being evicted by the kubelet, begin teardown", "pod", klog.KRef(ns, name), "podUID", uid, "updateType", options.UpdateType)
				status.evicted = true
			} else {
				klog.V(4).InfoS("Pod is being removed by the kubelet, begin teardown", "pod", klog.KRef(ns, name), "podUID", uid, "updateType", options.UpdateType)
			}
			status.terminatingAt = now
			becameTerminating = true
		}
	}

	// once a pod is terminating, all updates are kills and the grace period can only decrease
	var wasGracePeriodShortened bool
	// TODO 这里在干嘛？
	switch {
	case status.IsTerminated(): // 判断当前Pod是否已经处于Terminated状态
		// A terminated pod may still be waiting for cleanup - if we receive a runtime pod kill request
		// due to housekeeping seeing an older cached version of the runtime pod simply ignore it until
		// after the pod worker completes.
		// 如果当前Pod已经处于Terminated状态，并且当前Pod是RunningPod，此时无需在做任何事情，因为我们本来就需要删除这个RunningPod
		if isRuntimePod {
			klog.V(3).InfoS("Pod is waiting for termination, ignoring runtime-only kill until after pod worker is fully terminated", "pod", klog.KRef(ns, name), "podUID", uid, "updateType", options.UpdateType)
			return
		}

		if options.KillPodOptions != nil {
			// 表示当前Pod已经处于Terminated状态，关闭channel
			if ch := options.KillPodOptions.CompletedCh; ch != nil {
				close(ch)
			}
		}
		options.KillPodOptions = nil

	case status.IsTerminationRequested(): // 说明当前Pod正处于Terminating状态， 需要被干掉
		if options.KillPodOptions == nil {
			options.KillPodOptions = &KillPodOptions{}
		}

		// TODO 这里在干嘛?
		if ch := options.KillPodOptions.CompletedCh; ch != nil {
			status.notifyPostTerminating = append(status.notifyPostTerminating, ch)
		}
		if fn := options.KillPodOptions.PodStatusFunc; fn != nil {
			status.statusPostTerminating = append(status.statusPostTerminating, fn)
		}

		// 计算Pod优雅停止时间
		gracePeriod, gracePeriodShortened := calculateEffectiveGracePeriod(status, pod, options.KillPodOptions)

		wasGracePeriodShortened = gracePeriodShortened
		status.gracePeriod = gracePeriod
		// always set the grace period for syncTerminatingPod so we don't have to recalculate,
		// will never be zero.
		options.KillPodOptions.PodTerminationGracePeriodSecondsOverride = &gracePeriod

	default:
		// KillPodOptions is not valid for sync actions outside of the terminating phase
		if options.KillPodOptions != nil {
			if ch := options.KillPodOptions.CompletedCh; ch != nil {
				close(ch)
			}
			options.KillPodOptions = nil
		}
	}

	// start the pod worker goroutine if it doesn't exist
	podUpdates, exists := p.podUpdates[uid]
	if !exists {
		// 之前没有处理过这个Pod
		// buffer the channel to avoid blocking this method
		podUpdates = make(chan struct{}, 1)
		p.podUpdates[uid] = podUpdates

		// ensure that static pods start in the order they are received by UpdatePod
		if kubetypes.IsStaticPod(pod) {
			p.waitingToStartStaticPodsByFullname[status.fullname] =
				append(p.waitingToStartStaticPodsByFullname[status.fullname], uid)
		}

		// allow testing of delays in the pod update channel
		var outCh <-chan struct{}
		if p.workerChannelFn != nil {
			outCh = p.workerChannelFn(uid, podUpdates)
		} else {
			outCh = podUpdates
		}

		// spawn a pod worker
		go func() {
			// TODO: this should be a wait.Until with backoff to handle panics, and
			// accept a context for shutdown
			defer runtime.HandleCrash()
			defer klog.V(3).InfoS("Pod worker has stopped", "podUID", uid)
			// 处理Pod的更新
			p.podWorkerLoop(uid, outCh)
		}()
	}

	// measure the maximum latency between a call to UpdatePod and when the pod worker reacts to it
	// by preserving the oldest StartTime
	// 更新开始时间
	if status.pendingUpdate != nil && !status.pendingUpdate.StartTime.IsZero() && status.pendingUpdate.StartTime.Before(options.StartTime) {
		options.StartTime = status.pendingUpdate.StartTime
	}

	// notify the pod worker there is a pending update
	status.pendingUpdate = &options
	status.working = true
	klog.V(4).InfoS("Notifying pod of pending update", "pod", klog.KRef(ns, name), "podUID", uid, "workType", status.WorkType())
	// 通知协程当前Pod已经发生了变更
	select {
	case podUpdates <- struct{}{}:
	default:
	}

	if (becameTerminating || wasGracePeriodShortened) && status.cancelFn != nil {
		klog.V(3).InfoS("Cancelling current pod sync", "pod", klog.KRef(ns, name), "podUID", uid, "workType", status.WorkType())
		status.cancelFn()
		return
	}
}

// calculateEffectiveGracePeriod sets the initial grace period for a newly terminating pod or allows a
// shorter grace period to be provided, returning the desired value.
// 计算Pod优雅停止时间
func calculateEffectiveGracePeriod(status *podSyncStatus, pod *v1.Pod, options *KillPodOptions) (int64, bool) {
	// enforce the restriction that a grace period can only decrease and track whatever our value is,
	// then ensure a calculated value is passed down to lower levels
	gracePeriod := status.gracePeriod
	// this value is bedrock truth - the apiserver owns telling us this value calculated by apiserver
	if override := pod.DeletionGracePeriodSeconds; override != nil {
		if gracePeriod == 0 || *override < gracePeriod {
			gracePeriod = *override
		}
	}
	// we allow other parts of the kubelet (namely eviction) to request this pod be terminated faster
	if options != nil {
		if override := options.PodTerminationGracePeriodSecondsOverride; override != nil {
			if gracePeriod == 0 || *override < gracePeriod {
				gracePeriod = *override
			}
		}
	}
	// make a best effort to default this value to the pod's desired intent, in the event
	// the kubelet provided no requested value (graceful termination?)
	if gracePeriod == 0 && pod.Spec.TerminationGracePeriodSeconds != nil {
		gracePeriod = *pod.Spec.TerminationGracePeriodSeconds
	}
	// no matter what, we always supply a grace period of 1
	if gracePeriod < 1 {
		gracePeriod = 1
	}
	return gracePeriod, status.gracePeriod != 0 && status.gracePeriod != gracePeriod
}

// allowPodStart tries to start the pod and returns true if allowed, otherwise
// it requeues the pod and returns false. If the pod will never be able to start
// because data is missing, or the pod was terminated before start, canEverStart
// is false. This method can only be called while holding the pod lock.
func (p *podWorkers) allowPodStart(pod *v1.Pod) (canStart bool, canEverStart bool) {
	// 如果当前Pod不是StaticPod，那么允许启动
	if !kubetypes.IsStaticPod(pod) {
		// TODO: Do we want to allow non-static pods with the same full name?
		// Note that it may disable the force deletion of pods.
		return true, true
	}

	// 说明当前Pod是StaticPod

	status, ok := p.podSyncStatuses[pod.UID]
	if !ok {
		klog.ErrorS(nil, "Pod sync status does not exist, the worker should not be running", "pod", klog.KObj(pod), "podUID", pod.UID)
		return false, false
	}
	// 如果当前Pod正处于Terminating状态，说明当前StaticPod已经被干掉，因此这个Pod肯定再次启动
	if status.IsTerminationRequested() {
		return false, false
	}
	if !p.allowStaticPodStart(status.fullname, pod.UID) {
		p.workQueue.Enqueue(pod.UID, wait.Jitter(p.backOffPeriod, workerBackOffPeriodJitterFactor))
		return false, true
	}
	return true, true
}

// allowStaticPodStart tries to start the static pod and returns true if
// 1. there are no other started static pods with the same fullname
// 2. the uid matches that of the first valid static pod waiting to start
func (p *podWorkers) allowStaticPodStart(fullname string, uid types.UID) bool {
	// 如果存在这个StaticPod，说明当前Static
	startedUID, started := p.startedStaticPodsByFullname[fullname]
	if started {
		return startedUID == uid
	}

	waitingPods := p.waitingToStartStaticPodsByFullname[fullname]
	// TODO: This is O(N) with respect to the number of updates to static pods
	// with overlapping full names, and ideally would be O(1).
	for i, waitingUID := range waitingPods {
		// has pod already terminated or been deleted?
		status, ok := p.podSyncStatuses[waitingUID]
		// 如果当前没有找到这个Pod或者是这个Pod处于Terminating，或者是已经Terminated状态，那么直接忽略这个Pod
		if !ok || status.IsTerminationRequested() || status.IsTerminated() {
			continue
		}
		// another pod is next in line
		// TODO 这里在干嘛？ 完全看不懂
		if waitingUID != uid {
			p.waitingToStartStaticPodsByFullname[fullname] = waitingPods[i:]
			return false
		}
		// we are up next, remove ourselves
		waitingPods = waitingPods[i+1:]
		break
	}
	if len(waitingPods) != 0 {
		p.waitingToStartStaticPodsByFullname[fullname] = waitingPods
	} else {
		delete(p.waitingToStartStaticPodsByFullname, fullname)
	}
	p.startedStaticPodsByFullname[fullname] = uid
	return true
}

// cleanupUnstartedPod is invoked if a pod that has never been started receives a termination
// signal before it can be started. This method must be called holding the pod lock.
func (p *podWorkers) cleanupUnstartedPod(pod *v1.Pod, status *podSyncStatus) {
	p.cleanupPodUpdates(pod.UID)

	if status.terminatingAt.IsZero() {
		klog.V(4).InfoS("Pod worker is complete but did not have terminatingAt set, likely programmer error", "pod", klog.KObj(pod), "podUID", pod.UID)
	}
	if !status.terminatedAt.IsZero() {
		klog.V(4).InfoS("Pod worker is complete and had terminatedAt set, likely programmer error", "pod", klog.KObj(pod), "podUID", pod.UID)
	}
	status.finished = true
	status.working = false
	status.terminatedAt = p.clock.Now()

	if p.startedStaticPodsByFullname[status.fullname] == pod.UID {
		delete(p.startedStaticPodsByFullname, status.fullname)
	}
}

// startPodSync is invoked by each pod worker goroutine when a message arrives on the pod update channel.
// This method consumes a pending update, initializes a context, decides whether the pod is already started
// or can be started, and updates the cached pod state so that downstream components can observe what the
// pod worker goroutine is currently attempting to do. If ok is false, there is no available event. If any
// of the boolean values is false, ensure the appropriate cleanup happens before returning.
//
// This method should ensure that either status.pendingUpdate is cleared and merged into status.activeUpdate,
// or when a pod cannot be started status.pendingUpdate remains the same. Pods that have not been started
// should never have an activeUpdate because that is exposed to downstream components on started pods.
func (p *podWorkers) startPodSync(podUID types.UID) (ctx context.Context, update podWork, canStart, canEverStart, ok bool) {
	p.podLock.Lock()
	defer p.podLock.Unlock()

	// verify we are known to the pod worker still
	// 获取Pod同步状态
	status, ok := p.podSyncStatuses[podUID]

	if !ok {
		// 既然go协程开始处理这个pod了，那么肯定这个pod的状态一定经过初始化，如果没有拿到，那一定是哪里出问题了
		// pod status has disappeared, the worker should exit
		klog.V(4).InfoS("Pod worker no longer has status, worker should exit", "podUID", podUID)
		return nil, update, false, false, false
	}
	if !status.working {
		// 当Pod已经有一个go协程在处理时，podSyncStatuses.working必须是true，否则就是程序哪里出错了
		// working is used by unit tests to observe whether a worker is currently acting on this pod
		klog.V(4).InfoS("Pod should be marked as working by the pod worker, programmer error", "podUID", podUID)
	}
	// 这里肯定不会为空
	if status.pendingUpdate == nil {
		// no update available, this means we were queued without work being added or there is a
		// race condition, both of which are unexpected
		status.working = false
		klog.V(4).InfoS("Pod worker received no pending work, programmer error?", "podUID", podUID)
		return nil, update, false, false, false
	}

	// consume the pending update
	update.WorkType = status.WorkType()
	update.Options = *status.pendingUpdate
	status.pendingUpdate = nil
	select {
	case <-p.podUpdates[podUID]:
		// ensure the pod update channel is empty (it is only ever written to under lock)
	default:
	}

	// initialize a context for the worker if one does not exist
	if status.ctx == nil || status.ctx.Err() == context.Canceled {
		status.ctx, status.cancelFn = context.WithCancel(context.Background())
	}
	ctx = status.ctx

	// if we are already started, make our state visible to downstream components
	// 如果PodWorker已经开始处理Pod，更新一下activeUpdate
	if status.IsStarted() {
		status.mergeLastUpdate(update.Options)
		return ctx, update, true, true, true
	}

	// if we are already terminating and we only have a running pod, allow the worker
	// to "start" since we are immediately moving to terminating
	// 如果PodWorker已经开始处理Pod，更新一下activeUpdate
	if update.Options.RunningPod != nil && update.WorkType == TerminatingPod {
		status.mergeLastUpdate(update.Options)
		return ctx, update, true, true, true
	}

	// If we receive an update where Pod is nil (running pod is set) but haven't
	// started yet, we can only terminate the pod, not start it. We should not be
	// asked to start such a pod, but guard here just in case an accident occurs.
	if update.Options.Pod == nil {
		status.mergeLastUpdate(update.Options)
		klog.V(4).InfoS("Running pod cannot start ever, programmer error", "pod", klog.KObj(update.Options.Pod), "podUID", podUID, "updateType", update.WorkType)
		return ctx, update, false, false, true
	}

	// verify we can start
	// TODO 如何校验是否允许一个Pod启动？
	canStart, canEverStart = p.allowPodStart(update.Options.Pod)
	switch {
	case !canEverStart:
		p.cleanupUnstartedPod(update.Options.Pod, status)
		status.working = false
		if start := update.Options.StartTime; !start.IsZero() {
			metrics.PodWorkerDuration.WithLabelValues("terminated").Observe(metrics.SinceInSeconds(start))
		}
		klog.V(4).InfoS("Pod cannot start ever", "pod", klog.KObj(update.Options.Pod), "podUID", podUID, "updateType", update.WorkType)
		return ctx, update, canStart, canEverStart, true
	case !canStart:
		// this is the only path we don't start the pod, so we need to put the change back in pendingUpdate
		status.pendingUpdate = &update.Options
		status.working = false
		klog.V(4).InfoS("Pod cannot start yet", "pod", klog.KObj(update.Options.Pod), "podUID", podUID)
		return ctx, update, canStart, canEverStart, true
	}

	// mark the pod as started
	status.startedAt = p.clock.Now()
	status.mergeLastUpdate(update.Options)

	return ctx, update, true, true, true
}

func podUIDAndRefForUpdate(update UpdatePodOptions) (types.UID, klog.ObjectRef) {
	if update.RunningPod != nil {
		return update.RunningPod.ID, klog.KObj(update.RunningPod.ToAPIPod())
	}
	return update.Pod.UID, klog.KObj(update.Pod)
}

// podWorkerLoop manages sequential state updates to a pod in a goroutine, exiting once the final
// state is reached. The loop is responsible for driving the pod through four main phases:
//
// 1. Wait to start, guaranteeing no two pods with the same UID or same fullname are running at the same time
// 2. Sync, orchestrating pod setup by reconciling the desired pod spec with the runtime state of the pod
// 3. Terminating, ensuring all running containers in the pod are stopped
// 4. Terminated, cleaning up any resources that must be released before the pod can be deleted
//
// The podWorkerLoop is driven by updates delivered to UpdatePod and by SyncKnownPods. If a particular
// sync method fails, p.workerQueue is updated with backoff but it is the responsibility of the kubelet
// to trigger new UpdatePod calls. SyncKnownPods will only retry pods that are no longer known to the
// caller. When a pod transitions working->terminating or terminating->terminated, the next update is
// queued immediately and no kubelet action is required.
func (p *podWorkers) podWorkerLoop(podUID types.UID, podUpdates <-chan struct{}) {
	var lastSyncTime time.Time
	// 遍历Pod的更新事件
	for range podUpdates {
		// TODO startPodSync干了啥？
		ctx, update, canStart, canEverStart, ok := p.startPodSync(podUID)
		// If we had no update waiting, it means someone initialized the channel without filling out pendingUpdate.
		if !ok {
			continue
		}
		// If the pod was terminated prior to the pod being allowed to start, we exit the loop.
		if !canEverStart {
			return
		}
		// If the pod is not yet ready to start, continue and wait for more updates.
		if !canStart {
			continue
		}

		podUID, podRef := podUIDAndRefForUpdate(update.Options)

		klog.V(4).InfoS("Processing pod event", "pod", podRef, "podUID", podUID, "updateType", update.WorkType)
		var isTerminal bool
		err := func() error {
			// The worker is responsible for ensuring the sync method sees the appropriate
			// status updates on resyncs (the result of the last sync), transitions to
			// terminating (no wait), or on terminated (whatever the most recent state is).
			// Only syncing and terminating can generate pod status changes, while terminated
			// pods ensure the most recent status makes it to the api server.
			var status *kubecontainer.PodStatus
			var err error
			switch {
			case update.Options.RunningPod != nil:
				// when we receive a running pod, we don't need status at all because we are
				// guaranteed to be terminating and we skip updates to the pod
			default:
				// wait until we see the next refresh from the PLEG via the cache (max 2s)
				// TODO: this adds ~1s of latency on all transitions from sync to terminating
				//  to terminated, and on all termination retries (including evictions). We should
				//  improve latency by making the pleg continuous and by allowing pod status
				//  changes to be refreshed when key events happen (killPod, sync->terminating).
				//  Improving this latency also reduces the possibility that a terminated
				//  container's status is garbage collected before we have a chance to update the
				//  API server (thus losing the exit code).
				// 获取比当前Pod最后一次同步时间更新的Pod状态
				status, err = p.podCache.GetNewerThan(update.Options.Pod.UID, lastSyncTime)

				if err != nil {
					// This is the legacy event thrown by manage pod loop all other events are now dispatched
					// from syncPodFn
					p.recorder.Eventf(update.Options.Pod, v1.EventTypeWarning, events.FailedSync, "error determining status: %v", err)
					return err
				}
			}

			// Take the appropriate action (illegal phases are prevented by UpdatePod)
			switch {
			case update.WorkType == TerminatedPod:
				// 1、同步Pod的Terminated状态到APIServer。由于此操作执行后，Pod资源就已经处于Terminated状态了，此时就需要把Pod占用的资源回收。主要有
				// 卷、ConfigMap, Secret, Cgroup, userNamespace
				// TODO 具体分析
				err = p.podSyncer.SyncTerminatedPod(ctx, update.Options.Pod, status)

			case update.WorkType == TerminatingPod:
				var gracePeriod *int64
				if opt := update.Options.KillPodOptions; opt != nil {
					gracePeriod = opt.PodTerminationGracePeriodSecondsOverride
				}
				podStatusFn := p.acknowledgeTerminating(podUID)

				// if we only have a running pod, terminate it directly
				if update.Options.RunningPod != nil {
					// 直接通过CRI接口干掉Pod
					err = p.podSyncer.SyncTerminatingRuntimePod(ctx, update.Options.RunningPod)
				} else {
					// TODO 通过底层的容器运行时删除Pod，同时更新cGroup以及StatusManager
					err = p.podSyncer.SyncTerminatingPod(ctx, update.Options.Pod, status, gracePeriod, podStatusFn)
				}

			default:
				// TODO 最难的其实是这里
				isTerminal, err = p.podSyncer.SyncPod(ctx, update.Options.UpdateType, update.Options.Pod, update.Options.MirrorPod, status)
			}

			lastSyncTime = p.clock.Now()
			return err
		}()

		var phaseTransition bool
		switch {
		case err == context.Canceled:
			// when the context is cancelled we expect an update to already be queued
			klog.V(2).InfoS("Sync exited with context cancellation error", "pod", podRef, "podUID", podUID, "updateType", update.WorkType)

		case err != nil:
			// we will queue a retry
			klog.ErrorS(err, "Error syncing pod, skipping", "pod", podRef, "podUID", podUID)

		case update.WorkType == TerminatedPod:
			// we can shut down the worker
			// TODO pod已经处于Terminated状态，不可能再运行
			p.completeTerminated(podUID)
			if start := update.Options.StartTime; !start.IsZero() {
				metrics.PodWorkerDuration.WithLabelValues("terminated").Observe(metrics.SinceInSeconds(start))
			}
			klog.V(4).InfoS("Processing pod event done", "pod", podRef, "podUID", podUID, "updateType", update.WorkType)
			return

		case update.WorkType == TerminatingPod:
			// pods that don't exist in config don't need to be terminated, other loops will clean them up
			if update.Options.RunningPod != nil {
				p.completeTerminatingRuntimePod(podUID)
				if start := update.Options.StartTime; !start.IsZero() {
					metrics.PodWorkerDuration.WithLabelValues(update.Options.UpdateType.String()).Observe(metrics.SinceInSeconds(start))
				}
				klog.V(4).InfoS("Processing pod event done", "pod", podRef, "podUID", podUID, "updateType", update.WorkType)
				return
			}
			// otherwise we move to the terminating phase
			p.completeTerminating(podUID)
			phaseTransition = true

		case isTerminal:
			// if syncPod indicated we are now terminal, set the appropriate pod status to move to terminating
			klog.V(4).InfoS("Pod is terminal", "pod", podRef, "podUID", podUID, "updateType", update.WorkType)
			p.completeSync(podUID)
			phaseTransition = true
		}

		// queue a retry if necessary, then put the next event in the channel if any
		p.completeWork(podUID, phaseTransition, err)
		if start := update.Options.StartTime; !start.IsZero() {
			metrics.PodWorkerDuration.WithLabelValues(update.Options.UpdateType.String()).Observe(metrics.SinceInSeconds(start))
		}
		klog.V(4).InfoS("Processing pod event done", "pod", podRef, "podUID", podUID, "updateType", update.WorkType)
	}
}

// acknowledgeTerminating sets the terminating flag on the pod status once the pod worker sees
// the termination state so that other components know no new containers will be started in this
// pod. It then returns the status function, if any, that applies to this pod.
func (p *podWorkers) acknowledgeTerminating(podUID types.UID) PodStatusFunc {
	p.podLock.Lock()
	defer p.podLock.Unlock()

	status, ok := p.podSyncStatuses[podUID]
	if !ok {
		return nil
	}

	if !status.terminatingAt.IsZero() && !status.startedTerminating {
		klog.V(4).InfoS("Pod worker has observed request to terminate", "podUID", podUID)
		status.startedTerminating = true
	}

	if l := len(status.statusPostTerminating); l > 0 {
		return status.statusPostTerminating[l-1]
	}
	return nil
}

// completeSync is invoked when syncPod completes successfully and indicates the pod is now terminal and should
// be terminated. This happens when the natural pod lifecycle completes - any pod which is not RestartAlways
// exits. Unnatural completions, such as evictions, API driven deletion or phase transition, are handled by
// UpdatePod.
func (p *podWorkers) completeSync(podUID types.UID) {
	p.podLock.Lock()
	defer p.podLock.Unlock()

	klog.V(4).InfoS("Pod indicated lifecycle completed naturally and should now terminate", "podUID", podUID)

	status, ok := p.podSyncStatuses[podUID]
	if !ok {
		klog.V(4).InfoS("Pod had no status in completeSync, programmer error?", "podUID", podUID)
		return
	}

	// update the status of the pod
	if status.terminatingAt.IsZero() {
		status.terminatingAt = p.clock.Now()
	} else {
		klog.V(4).InfoS("Pod worker attempted to set terminatingAt twice, likely programmer error", "podUID", podUID)
	}
	status.startedTerminating = true

	// the pod has now transitioned to terminating and we want to run syncTerminatingPod
	// as soon as possible, so if no update is already waiting queue a synthetic update
	p.requeueLastPodUpdate(podUID, status)
}

// completeTerminating is invoked when syncTerminatingPod completes successfully, which means
// no container is running, no container will be started in the future, and we are ready for
// cleanup.  This updates the termination state which prevents future syncs and will ensure
// other kubelet loops know this pod is not running any containers.
func (p *podWorkers) completeTerminating(podUID types.UID) {
	p.podLock.Lock()
	defer p.podLock.Unlock()

	klog.V(4).InfoS("Pod terminated all containers successfully", "podUID", podUID)

	status, ok := p.podSyncStatuses[podUID]
	if !ok {
		return
	}

	// update the status of the pod
	if status.terminatingAt.IsZero() {
		klog.V(4).InfoS("Pod worker was terminated but did not have terminatingAt set, likely programmer error", "podUID", podUID)
	}
	status.terminatedAt = p.clock.Now()
	for _, ch := range status.notifyPostTerminating {
		close(ch)
	}
	status.notifyPostTerminating = nil
	status.statusPostTerminating = nil

	// the pod has now transitioned to terminated and we want to run syncTerminatedPod
	// as soon as possible, so if no update is already waiting queue a synthetic update
	p.requeueLastPodUpdate(podUID, status)
}

// completeTerminatingRuntimePod is invoked when syncTerminatingPod completes successfully,
// which means an orphaned pod (no config) is terminated and we can exit. Since orphaned
// pods have no API representation, we want to exit the loop at this point and ensure no
// status is present afterwards - the running pod is truly terminated when this is invoked.
func (p *podWorkers) completeTerminatingRuntimePod(podUID types.UID) {
	p.podLock.Lock()
	defer p.podLock.Unlock()

	klog.V(4).InfoS("Pod terminated all orphaned containers successfully and worker can now stop", "podUID", podUID)

	p.cleanupPodUpdates(podUID)

	status, ok := p.podSyncStatuses[podUID]
	if !ok {
		return
	}
	if status.terminatingAt.IsZero() {
		klog.V(4).InfoS("Pod worker was terminated but did not have terminatingAt set, likely programmer error", "podUID", podUID)
	}
	status.terminatedAt = p.clock.Now()
	status.finished = true
	status.working = false

	if p.startedStaticPodsByFullname[status.fullname] == podUID {
		delete(p.startedStaticPodsByFullname, status.fullname)
	}

	// A runtime pod is transient and not part of the desired state - once it has reached
	// terminated we can abandon tracking it.
	delete(p.podSyncStatuses, podUID)
}

// completeTerminated is invoked after syncTerminatedPod completes successfully and means we
// can stop the pod worker. The pod is finalized at this point.
func (p *podWorkers) completeTerminated(podUID types.UID) {
	p.podLock.Lock()
	defer p.podLock.Unlock()

	klog.V(4).InfoS("Pod is complete and the worker can now stop", "podUID", podUID)

	p.cleanupPodUpdates(podUID)

	status, ok := p.podSyncStatuses[podUID]
	if !ok {
		return
	}
	if status.terminatingAt.IsZero() {
		klog.V(4).InfoS("Pod worker is complete but did not have terminatingAt set, likely programmer error", "podUID", podUID)
	}
	if status.terminatedAt.IsZero() {
		klog.V(4).InfoS("Pod worker is complete but did not have terminatedAt set, likely programmer error", "podUID", podUID)
	}
	status.finished = true
	status.working = false

	if p.startedStaticPodsByFullname[status.fullname] == podUID {
		delete(p.startedStaticPodsByFullname, status.fullname)
	}
}

// completeWork requeues on error or the next sync interval and then immediately executes any pending
// work.
func (p *podWorkers) completeWork(podUID types.UID, phaseTransition bool, syncErr error) {
	// Requeue the last update if the last sync returned error.
	switch {
	case phaseTransition:
		p.workQueue.Enqueue(podUID, 0)
	case syncErr == nil:
		// No error; requeue at the regular resync interval.
		p.workQueue.Enqueue(podUID, wait.Jitter(p.resyncInterval, workerResyncIntervalJitterFactor))
	case strings.Contains(syncErr.Error(), NetworkNotReadyErrorMsg):
		// Network is not ready; back off for short period of time and retry as network might be ready soon.
		p.workQueue.Enqueue(podUID, wait.Jitter(backOffOnTransientErrorPeriod, workerBackOffPeriodJitterFactor))
	default:
		// Error occurred during the sync; back off and then retry.
		p.workQueue.Enqueue(podUID, wait.Jitter(p.backOffPeriod, workerBackOffPeriodJitterFactor))
	}

	// if there is a pending update for this worker, requeue immediately, otherwise
	// clear working status
	p.podLock.Lock()
	defer p.podLock.Unlock()
	if status, ok := p.podSyncStatuses[podUID]; ok {
		if status.pendingUpdate != nil {
			select {
			case p.podUpdates[podUID] <- struct{}{}:
				klog.V(4).InfoS("Requeueing pod due to pending update", "podUID", podUID)
			default:
				klog.V(4).InfoS("Pending update already queued", "podUID", podUID)
			}
		} else {
			status.working = false
		}
	}
}

// SyncKnownPods will purge any fully terminated pods that are not in the desiredPods
// list, which means SyncKnownPods must be called in a threadsafe manner from calls
// to UpdatePods for new pods. Because the podworker is dependent on UpdatePod being
// invoked to drive a pod's state machine, if a pod is missing in the desired list the
// pod worker must be responsible for delivering that update. The method returns a map
// of known workers that are not finished with a value of SyncPodTerminated,
// SyncPodKill, or SyncPodSync depending on whether the pod is terminated, terminating,
// or syncing.
func (p *podWorkers) SyncKnownPods(desiredPods []*v1.Pod) map[types.UID]PodWorkerSync {
	workers := make(map[types.UID]PodWorkerSync)
	known := make(map[types.UID]struct{})
	for _, pod := range desiredPods {
		known[pod.UID] = struct{}{}
	}

	p.podLock.Lock()
	defer p.podLock.Unlock()

	p.podsSynced = true
	for uid, status := range p.podSyncStatuses {
		// We retain the worker history of any pod that is still desired according to
		// its UID. However, there are ]two scenarios during a sync that result in us
		// needing to purge the history:
		//
		// 1. The pod is no longer desired (the local version is orphaned)
		// 2. The pod received a kill update and then a subsequent create, which means
		//    the UID was reused in the source config (vanishingly rare for API servers,
		//    common for static pods that have specified a fixed UID)
		//
		// In the former case we wish to bound the amount of information we store for
		// deleted pods. In the latter case we wish to minimize the amount of time before
		// we restart the static pod. If we succeed at removing the worker, then we
		// omit it from the returned map of known workers, and the caller of SyncKnownPods
		// is expected to send a new UpdatePod({UpdateType: Create}).
		_, knownPod := known[uid]
		orphan := !knownPod
		if status.restartRequested || orphan {
			if p.removeTerminatedWorker(uid, status, orphan) {
				// no worker running, we won't return it
				continue
			}
		}

		sync := PodWorkerSync{
			State:  status.WorkType(),
			Orphan: orphan,
		}
		switch {
		case status.activeUpdate != nil:
			if status.activeUpdate.Pod != nil {
				sync.HasConfig = true
				sync.Static = kubetypes.IsStaticPod(status.activeUpdate.Pod)
			}
		case status.pendingUpdate != nil:
			if status.pendingUpdate.Pod != nil {
				sync.HasConfig = true
				sync.Static = kubetypes.IsStaticPod(status.pendingUpdate.Pod)
			}
		}
		workers[uid] = sync
	}
	return workers
}

// removeTerminatedWorker cleans up and removes the worker status for a worker
// that has reached a terminal state of "finished" - has successfully exited
// syncTerminatedPod. This "forgets" a pod by UID and allows another pod to be
// recreated with the same UID. The kubelet preserves state about recently
// terminated pods to prevent accidentally restarting a terminal pod, which is
// proportional to the number of pods described in the pod config. The method
// returns true if the worker was completely removed.
func (p *podWorkers) removeTerminatedWorker(uid types.UID, status *podSyncStatus, orphaned bool) bool {
	if !status.finished {
		// If the pod worker has not reached terminal state and the pod is still known, we wait.
		if !orphaned {
			klog.V(4).InfoS("Pod worker has been requested for removal but is still not fully terminated", "podUID", uid)
			return false
		}

		// all orphaned pods are considered deleted
		status.deleted = true

		// When a pod is no longer in the desired set, the pod is considered orphaned and the
		// the pod worker becomes responsible for driving the pod to completion (there is no
		// guarantee another component will notify us of updates).
		switch {
		case !status.IsStarted() && !status.observedRuntime:
			// The pod has not been started, which means we can safely clean up the pod - the
			// pod worker will shutdown as a result of this change without executing a sync.
			klog.V(4).InfoS("Pod is orphaned and has not been started", "podUID", uid)
		case !status.IsTerminationRequested():
			// The pod has been started but termination has not been requested - set the appropriate
			// timestamp and notify the pod worker. Because the pod has been synced at least once,
			// the value of status.activeUpdate will be the fallback for the next sync.
			status.terminatingAt = p.clock.Now()
			if status.activeUpdate != nil && status.activeUpdate.Pod != nil {
				status.gracePeriod, _ = calculateEffectiveGracePeriod(status, status.activeUpdate.Pod, nil)
			} else {
				status.gracePeriod = 1
			}
			p.requeueLastPodUpdate(uid, status)
			klog.V(4).InfoS("Pod is orphaned and still running, began terminating", "podUID", uid)
			return false
		default:
			// The pod is already moving towards termination, notify the pod worker. Because the pod
			// has been synced at least once, the value of status.activeUpdate will be the fallback for
			// the next sync.
			p.requeueLastPodUpdate(uid, status)
			klog.V(4).InfoS("Pod is orphaned and still terminating, notified the pod worker", "podUID", uid)
			return false
		}
	}

	if status.restartRequested {
		klog.V(4).InfoS("Pod has been terminated but another pod with the same UID was created, remove history to allow restart", "podUID", uid)
	} else {
		klog.V(4).InfoS("Pod has been terminated and is no longer known to the kubelet, remove all history", "podUID", uid)
	}
	delete(p.podSyncStatuses, uid)
	p.cleanupPodUpdates(uid)

	if p.startedStaticPodsByFullname[status.fullname] == uid {
		delete(p.startedStaticPodsByFullname, status.fullname)
	}
	return true
}

// killPodNow returns a KillPodFunc that can be used to kill a pod.
// It is intended to be injected into other modules that need to kill a pod.
func killPodNow(podWorkers PodWorkers, recorder record.EventRecorder) eviction.KillPodFunc {
	return func(pod *v1.Pod, isEvicted bool, gracePeriodOverride *int64, statusFn func(*v1.PodStatus)) error {
		// determine the grace period to use when killing the pod
		gracePeriod := int64(0)
		if gracePeriodOverride != nil {
			gracePeriod = *gracePeriodOverride
		} else if pod.Spec.TerminationGracePeriodSeconds != nil {
			gracePeriod = *pod.Spec.TerminationGracePeriodSeconds
		}

		// we timeout and return an error if we don't get a callback within a reasonable time.
		// the default timeout is relative to the grace period (we settle on 10s to wait for kubelet->runtime traffic to complete in sigkill)
		timeout := int64(gracePeriod + (gracePeriod / 2))
		minTimeout := int64(10)
		if timeout < minTimeout {
			timeout = minTimeout
		}
		timeoutDuration := time.Duration(timeout) * time.Second

		// open a channel we block against until we get a result
		ch := make(chan struct{}, 1)
		podWorkers.UpdatePod(UpdatePodOptions{
			Pod:        pod,
			UpdateType: kubetypes.SyncPodKill,
			KillPodOptions: &KillPodOptions{
				CompletedCh:                              ch,
				Evict:                                    isEvicted,
				PodStatusFunc:                            statusFn,
				PodTerminationGracePeriodSecondsOverride: gracePeriodOverride,
			},
		})

		// wait for either a response, or a timeout
		select {
		case <-ch:
			return nil
		case <-time.After(timeoutDuration):
			recorder.Eventf(pod, v1.EventTypeWarning, events.ExceededGracePeriod, "Container runtime did not kill the pod within specified grace period.")
			return fmt.Errorf("timeout waiting to kill pod")
		}
	}
}

// cleanupPodUpdates closes the podUpdates channel and removes it from
// podUpdates map so that the corresponding pod worker can stop. It also
// removes any undelivered work. This method must be called holding the
// pod lock.
func (p *podWorkers) cleanupPodUpdates(uid types.UID) {
	if ch, ok := p.podUpdates[uid]; ok {
		close(ch)
	}
	delete(p.podUpdates, uid)
}

// requeueLastPodUpdate creates a new pending pod update from the most recently
// executed update if no update is already queued, and then notifies the pod
// worker goroutine of the update. This method must be called while holding
// the pod lock.
func (p *podWorkers) requeueLastPodUpdate(podUID types.UID, status *podSyncStatus) {
	// if there is already an update queued, we can use that instead, or if
	// we have no previously executed update, we cannot replay it.
	if status.pendingUpdate != nil || status.activeUpdate == nil {
		return
	}
	copied := *status.activeUpdate
	status.pendingUpdate = &copied

	// notify the pod worker
	status.working = true
	select {
	case p.podUpdates[podUID] <- struct{}{}:
	default:
	}
}
