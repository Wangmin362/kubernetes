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

package prober

import (
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	"k8s.io/component-base/metrics"
	"k8s.io/klog/v2"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/prober/results"
	"k8s.io/kubernetes/pkg/kubelet/status"
	"k8s.io/utils/clock"
)

// ProberResults stores the cumulative number of a probe by result as prometheus metrics.
var ProberResults = metrics.NewCounterVec(
	&metrics.CounterOpts{
		Subsystem:      "prober",
		Name:           "probe_total",
		Help:           "Cumulative number of a liveness, readiness or startup probe for a container by result.",
		StabilityLevel: metrics.ALPHA,
	},
	[]string{"probe_type",
		"result",
		"container",
		"pod",
		"namespace",
		"pod_uid"},
)

// ProberDuration stores the duration of a successful probe lifecycle by result as prometheus metrics.
var ProberDuration = metrics.NewHistogramVec(
	&metrics.HistogramOpts{
		Subsystem:      "prober",
		Name:           "probe_duration_seconds",
		Help:           "Duration in seconds for a probe response.",
		StabilityLevel: metrics.ALPHA,
	},
	[]string{"probe_type",
		"container",
		"pod",
		"namespace"},
)

// Manager manages pod probing. It creates a probe "worker" for every container that specifies a
// probe (AddPod). The worker periodically probes its assigned container and caches the results. The
// manager use the cached probe results to set the appropriate Ready state in the PodStatus when
// requested (UpdatePodStatus). Updating probe parameters is not currently supported.
// 1、用于管理Pod的探针，ProbeManager会针对Pod的需求，针对每个容器的readiness, liveness, startup探针分别启动一个协程，同时此协程会定时
// 通过探针的配置检查状态，譬如执行HTTP请求，或者执行Exec命令，或者是看端口是否处于监听状态
// 2、于此同时，每个容器的每种类型的探针结果都会被存入到ResultManager，从而触发PLEG的syncLoop更新Pod状态
type Manager interface {
	// AddPod creates new probe workers for every container probe. This should be called for every
	// pod created.
	AddPod(pod *v1.Pod)

	// StopLivenessAndStartup handles stopping liveness and startup probes during termination.
	// 将容器的存活探针以及启动探针关闭
	StopLivenessAndStartup(pod *v1.Pod)

	// RemovePod handles cleaning up the removed pod state, including terminating probe workers and
	// deleting cached results.
	// 移除容器，显然容器移除时，应该停止此容器的所有探针
	RemovePod(pod *v1.Pod)

	// CleanupPods handles cleaning up pods which should no longer be running.
	// It takes a map of "desired pods" which should not be cleaned up.
	// 清除所有目标Pod的所有探针
	CleanupPods(desiredPods map[types.UID]sets.Empty)

	// UpdatePodStatus modifies the given PodStatus with the appropriate Ready state for each
	// container based on container running status, cached probe results and worker states.
	UpdatePodStatus(types.UID, *v1.PodStatus)
}

type manager struct {
	// Map of active workers for probes
	// 1、Key由三个字段构成，分别是是：PodUID, ContainerID, 探针类型（liveness, readiness, startup）
	// 2、value抽象为一个worker，实际上将来会针对每个worker启动一个协程运行
	workers map[probeKey]*worker
	// Lock for accessing & mutating workers
	workerLock sync.RWMutex

	// The statusManager cache provides pod IP and container IDs for probing.
	// Pod状态管理器，探针管理器的跟目标是为了修改Pod中容器的状态
	statusManager status.Manager

	// 用于记录Readiness探针的结果，每个容器只要指定了readiness探针，ProbeManager就会启动一个协程定期按照探针配置的规则执行，并把
	// 结果保存到ResultManager当中
	readinessManager results.Manager

	// 用于记录Readiness探针的结果，每个容器只要指定了readiness探针，ProbeManager就会启动一个协程定期按照探针配置的规则执行，并把
	// 结果保存到ResultManager当中
	livenessManager results.Manager

	// 用于记录Readiness探针的结果，每个容器只要指定了readiness探针，ProbeManager就会启动一个协程定期按照探针配置的规则执行，并把
	// 结果保存到ResultManager当中
	startupManager results.Manager

	// prober executes the probe actions.
	// TODO 用于执行tcp, exec, http, grpc方式的探针
	prober *prober

	// 实例化ProbeManager的时间
	start time.Time
}

// NewManager creates a Manager for pod probing.
func NewManager(
	statusManager status.Manager, // 状态管理器，显然，Pod的探针状态会影响Pod状态，不然要Pod探针干嘛，Pod探针状态肯定会影响Pod的状态
	livenessManager results.Manager, // 存活探针结果管理器
	readinessManager results.Manager, // 就绪探针结果管理器
	startupManager results.Manager, // 启动探针结果管理器
	runner kubecontainer.CommandRunner, // 命令执行器，用于在容器内部执行命令，主要用于解决Exec的方式
	recorder record.EventRecorder, // 事件记录
) Manager {

	prober := newProber(runner, recorder)
	return &manager{
		statusManager:    statusManager,
		prober:           prober,
		readinessManager: readinessManager,
		livenessManager:  livenessManager,
		startupManager:   startupManager,
		workers:          make(map[probeKey]*worker),
		start:            clock.RealClock{}.Now(),
	}
}

// Key uniquely identifying container probes
type probeKey struct {
	podUID        types.UID
	containerName string
	probeType     probeType
}

// Type of probe (liveness, readiness or startup)
type probeType int

const (
	liveness probeType = iota
	readiness
	startup

	probeResultSuccessful string = "successful"
	probeResultFailed     string = "failed"
	probeResultUnknown    string = "unknown"
)

// For debugging.
func (t probeType) String() string {
	switch t {
	case readiness:
		return "Readiness"
	case liveness:
		return "Liveness"
	case startup:
		return "Startup"
	default:
		return "UNKNOWN"
	}
}

func (m *manager) AddPod(pod *v1.Pod) {
	m.workerLock.Lock()
	defer m.workerLock.Unlock()

	key := probeKey{podUID: pod.UID}
	// 遍历当前Pod中所办含的容器，由于InitContainer不会一直存在，因此没有探针的说法
	for _, c := range pod.Spec.Containers {
		key.containerName = c.Name

		// 如果当前容器指定了Startup类型的探针，那么就启动一个协程定期按照探针指定的规则定期执行
		if c.StartupProbe != nil {
			key.probeType = startup
			if _, ok := m.workers[key]; ok {
				klog.V(8).ErrorS(nil, "Startup probe already exists for container",
					"pod", klog.KObj(pod), "containerName", c.Name)
				return
			}
			w := newWorker(m, startup, pod, c)
			m.workers[key] = w
			go w.run()
		}

		// 如果当前容器指定了Readiness类型的探针，那么就启动一个协程定期按照探针指定的规则定期执行
		if c.ReadinessProbe != nil {
			key.probeType = readiness
			if _, ok := m.workers[key]; ok {
				klog.V(8).ErrorS(nil, "Readiness probe already exists for container",
					"pod", klog.KObj(pod), "containerName", c.Name)
				return
			}
			w := newWorker(m, readiness, pod, c)
			m.workers[key] = w
			go w.run()
		}

		// 如果当前容器指定了Liveness类型的探针，那么就启动一个协程定期按照探针指定的规则定期执行
		if c.LivenessProbe != nil {
			key.probeType = liveness
			if _, ok := m.workers[key]; ok {
				klog.V(8).ErrorS(nil, "Liveness probe already exists for container",
					"pod", klog.KObj(pod), "containerName", c.Name)
				return
			}
			w := newWorker(m, liveness, pod, c)
			m.workers[key] = w
			go w.run()
		}
	}
}

func (m *manager) StopLivenessAndStartup(pod *v1.Pod) {
	m.workerLock.RLock()
	defer m.workerLock.RUnlock()

	key := probeKey{podUID: pod.UID}
	for _, c := range pod.Spec.Containers {
		key.containerName = c.Name
		for _, probeType := range [...]probeType{liveness, startup} {
			key.probeType = probeType
			if worker, ok := m.workers[key]; ok {
				worker.stop()
			}
		}
	}
}

func (m *manager) RemovePod(pod *v1.Pod) {
	m.workerLock.RLock()
	defer m.workerLock.RUnlock()

	key := probeKey{podUID: pod.UID}
	for _, c := range pod.Spec.Containers {
		key.containerName = c.Name
		for _, probeType := range [...]probeType{readiness, liveness, startup} {
			key.probeType = probeType
			if worker, ok := m.workers[key]; ok {
				worker.stop()
			}
		}
	}
}

func (m *manager) CleanupPods(desiredPods map[types.UID]sets.Empty) {
	m.workerLock.RLock()
	defer m.workerLock.RUnlock()

	for key, worker := range m.workers {
		if _, ok := desiredPods[key.podUID]; !ok {
			worker.stop()
		}
	}
}

// UpdatePodStatus 通过Pod中的容器状态
func (m *manager) UpdatePodStatus(podUID types.UID, podStatus *v1.PodStatus) {
	// TODO 为什么不需要动容器的存活状态？
	for i, c := range podStatus.ContainerStatuses {
		var started bool // 容器是否启动
		if c.State.Running == nil {
			// 状态为空，认为容器还没有启动
			started = false
		} else if result, ok := m.startupManager.Get(kubecontainer.ParseContainerID(c.ContainerID)); ok {
			// 否则从启动状态管理器当中获取容器的启动状态
			started = result == results.Success
		} else {
			// The check whether there is a probe which hasn't run yet.
			// 说明容器启动状态非空，但是容器启动状态管理器当中没有获取到容器状态，所以尝试获取容器的启动探针任务，看看这个任务是否存在
			_, exists := m.getWorker(podUID, c.Name, startup)
			// 1、任务存在，说明容器确实还没有启动。
			// 2、任务不存在，说明容器根本就没有指定启动探针，那么我们永远认为容器已经启动；因为K8S永远不知道如何判断容器是否已经启动，所以
			// 只能认为容器已经启动
			started = !exists
		}
		podStatus.ContainerStatuses[i].Started = &started

		// 如果容器已经启动了，那么再看看这个容器是否处于就绪状态；如果容器还没有启动，那么容器不可能处于就绪状态
		if started {
			var ready bool // 容器是否就绪
			if c.State.Running == nil {
				// 容器的运行状态还未初始化，认为容器还未就绪
				ready = false
			} else if result, ok := m.readinessManager.Get(kubecontainer.ParseContainerID(c.ContainerID)); ok && result == results.Success {
				// 否则从就绪探针结果管理器中获取容器的就绪状态，如果就绪。那么认为容器处于就绪状态
				ready = true
			} else {
				// The check whether there is a probe which hasn't run yet.
				w, exists := m.getWorker(podUID, c.Name, readiness)
				// 1、获取当前容器就绪探针任务，如果没有获取到，说明容器没有指定就绪探针,那么我们永远认为容器已经启动；因为K8S永远不知道如何判断容器是否已经启动，所以
				// 只能认为容器已经启动
				// 2、如果获取到了就绪探针任务，那么就认为容器还未就绪，同时手动就绪任务运行一次，更新就绪状态
				ready = !exists // no readinessProbe -> always ready
				if exists {
					// Trigger an immediate run of the readinessProbe to update ready state
					select {
					case w.manualTriggerCh <- struct{}{}:
					default: // Non-blocking.
						klog.InfoS("Failed to trigger a manual run", "probe", w.probeType.String())
					}
				}
			}
			podStatus.ContainerStatuses[i].Ready = ready
		}
	}
	// init containers are ready if they have exited with success or if a readiness probe has
	// succeeded.
	for i, c := range podStatus.InitContainerStatuses {
		var ready bool
		// 如果容器的Init容器已经退出，并且退出码为0，那么认为init容器正常结束，此时认为init容器就绪，可以开始正常启动业务容器。
		if c.State.Terminated != nil && c.State.Terminated.ExitCode == 0 {
			ready = true
		}
		podStatus.InitContainerStatuses[i].Ready = ready
	}
}

func (m *manager) getWorker(podUID types.UID, containerName string, probeType probeType) (*worker, bool) {
	m.workerLock.RLock()
	defer m.workerLock.RUnlock()
	worker, ok := m.workers[probeKey{podUID, containerName, probeType}]
	return worker, ok
}

// Called by the worker after exiting.
func (m *manager) removeWorker(podUID types.UID, containerName string, probeType probeType) {
	m.workerLock.Lock()
	defer m.workerLock.Unlock()
	delete(m.workers, probeKey{podUID, containerName, probeType})
}

// workerCount returns the total number of probe workers. For testing.
func (m *manager) workerCount() int {
	m.workerLock.RLock()
	defer m.workerLock.RUnlock()
	return len(m.workers)
}
