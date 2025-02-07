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
	"context"
	"fmt"
	"math/rand"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/component-base/metrics"
	"k8s.io/klog/v2"
	podutil "k8s.io/kubernetes/pkg/api/v1/pod"
	"k8s.io/kubernetes/pkg/apis/apps"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/prober/results"
)

// worker handles the periodic probing of its assigned container. Each worker has a go-routine
// associated with it which runs the probe loop until the container permanently terminates, or the
// stop channel is closed. The worker uses the probe Manager's statusManager to get up-to-date
// container IDs.
// 1、worker用于抽象Pod的一个探针的执行
type worker struct {
	// Channel for stopping the probe.
	// ProbeManager会再必要的时候向这个channel当写入数据以停止worker
	// 1、容器停止的时候，这是应该的，容器停止了肯定也没有必要执行容器探针了
	// 2、ProbeManager执行CleanupPod的时候
	// 3、ProbeManager执行StopLivenessAndStartup的时候
	stopCh chan struct{}

	// Channel for triggering the probe manually.
	// 1、所谓的手动触发，其实就是当我们从探针状态管理器当中获取不到容器的探针状态，此时就可以向这个通道中写入数据，触发探针执行一次，从而
	// 更新探针的状态
	manualTriggerCh chan struct{}

	// 当前执行探测的Pod
	pod *v1.Pod

	// 当中执行探针的容器，这个容器一定是属于上面的Pod
	container v1.Container

	// 当前指定的探针规则，readiness, liveness, startup三种探针使用相同的探针规则
	spec *v1.Probe

	// 当前的探针类型
	probeType probeType

	// 探针的初始值，在实例化worker的时候，会被初始化
	initialValue results.Result

	// Where to store this workers results.
	// 探针结果管理器，用于保存探针的结果，通过probeManager进行初始化
	resultsManager results.Manager
	// 引用外部的ProbeManager
	probeManager *manager

	// The last known container ID for this worker.
	// 容器ID
	containerID kubecontainer.ContainerID
	// The last probe result for this worker.
	lastResult results.Result
	// How many times in a row the probe has returned the same result.
	// 当前探针已经执行了多少次相同的结果了
	resultRun int

	// If set, skip probing.
	onHold bool

	// proberResultsMetricLabels holds the labels attached to this worker
	// for the ProberResults metric by result.
	proberResultsSuccessfulMetricLabels metrics.Labels
	proberResultsFailedMetricLabels     metrics.Labels
	proberResultsUnknownMetricLabels    metrics.Labels
	// proberDurationMetricLabels holds the labels attached to this worker
	// for the ProberDuration metric by result.
	proberDurationSuccessfulMetricLabels metrics.Labels
	proberDurationUnknownMetricLabels    metrics.Labels
}

// Creates and starts a new probe worker.
func newWorker(
	m *manager, // 探针管理器
	probeType probeType, // 探针类型
	pod *v1.Pod, // 当前Pod
	container v1.Container, // 当前容器
) *worker {

	w := &worker{
		stopCh:          make(chan struct{}, 1), // Buffer so stop() can be non-blocking.
		manualTriggerCh: make(chan struct{}, 1), // Buffer so prober_manager can do non-blocking calls to doProbe.
		pod:             pod,
		container:       container,
		probeType:       probeType,
		probeManager:    m,
	}

	switch probeType {
	case readiness:
		// 如果当前的是Readiness类型的探针，那么把结果放入到ReadinessResultManager当中
		w.spec = container.ReadinessProbe
		w.resultsManager = m.readinessManager
		w.initialValue = results.Failure
	case liveness:
		// 如果当前的是Liveness类型的探针，那么把结果放入到LivenessResultManager当中
		w.spec = container.LivenessProbe
		w.resultsManager = m.livenessManager
		w.initialValue = results.Success
	case startup:
		// 如果当前的是Startup类型的探针，那么把结果放入到StartupResultManager当中
		w.spec = container.StartupProbe
		w.resultsManager = m.startupManager
		w.initialValue = results.Unknown
	}

	// 获取Pod名字
	podName := getPodLabelName(w.pod)

	// 初始化指标
	basicMetricLabels := metrics.Labels{
		"probe_type": w.probeType.String(),
		"container":  w.container.Name,
		"pod":        podName,
		"namespace":  w.pod.Namespace,
		"pod_uid":    string(w.pod.UID),
	}

	proberDurationLabels := metrics.Labels{
		"probe_type": w.probeType.String(),
		"container":  w.container.Name,
		"pod":        podName,
		"namespace":  w.pod.Namespace,
	}

	w.proberResultsSuccessfulMetricLabels = deepCopyPrometheusLabels(basicMetricLabels)
	w.proberResultsSuccessfulMetricLabels["result"] = probeResultSuccessful

	w.proberResultsFailedMetricLabels = deepCopyPrometheusLabels(basicMetricLabels)
	w.proberResultsFailedMetricLabels["result"] = probeResultFailed

	w.proberResultsUnknownMetricLabels = deepCopyPrometheusLabels(basicMetricLabels)
	w.proberResultsUnknownMetricLabels["result"] = probeResultUnknown

	w.proberDurationSuccessfulMetricLabels = deepCopyPrometheusLabels(proberDurationLabels)
	w.proberDurationUnknownMetricLabels = deepCopyPrometheusLabels(proberDurationLabels)

	return w
}

// run periodically probes the container.
func (w *worker) run() {
	ctx := context.Background()
	// 探针执行的周期
	probeTickerPeriod := time.Duration(w.spec.PeriodSeconds) * time.Second

	// If kubelet restarted the probes could be started in rapid succession.
	// Let the worker wait for a random portion of tickerPeriod before probing.
	// Do it only if the kubelet has started recently.
	// 如果当前ProbeManager才刚启动，而此时还没有到探针的执行周期，那么直接先睡眠一次探针执行周期时间，保证探针第一次执行的时候一定是过了
	// 探针执行周期
	if probeTickerPeriod > time.Since(w.probeManager.start) {
		time.Sleep(time.Duration(rand.Float64() * float64(probeTickerPeriod)))
	}

	// 通过探针执行周期，实例化一个Ticker
	probeTicker := time.NewTicker(probeTickerPeriod)

	defer func() {
		// Clean up.
		probeTicker.Stop()
		if !w.containerID.IsEmpty() {
			// 1、如果当前探针任务被停止，那么需要从探针结果管理中移除这个容器的结果；
			// 2、这种情况只有当容器被移除的时候才会停止
			w.resultsManager.Remove(w.containerID)
		}

		w.probeManager.removeWorker(w.pod.UID, w.container.Name, w.probeType)
		ProberResults.Delete(w.proberResultsSuccessfulMetricLabels)
		ProberResults.Delete(w.proberResultsFailedMetricLabels)
		ProberResults.Delete(w.proberResultsUnknownMetricLabels)
		ProberDuration.Delete(w.proberDurationSuccessfulMetricLabels)
		ProberDuration.Delete(w.proberDurationUnknownMetricLabels)
	}()

probeLoop:
	// 按照探针指定的规则执行
	for w.doProbe(ctx) {
		// Wait for next probe tick.
		select {
		case <-w.stopCh:
			break probeLoop
		case <-probeTicker.C: // 探针定期执行后期到了
		case <-w.manualTriggerCh: // 手动触发探针运行一次
			// continue
		}
	}
}

// stop stops the probe worker. The worker handles cleanup and removes itself from its manager.
// It is safe to call stop multiple times.
func (w *worker) stop() {
	select {
	case w.stopCh <- struct{}{}:
	default: // Non-blocking.
	}
}

// doProbe probes the container once and records the result.
// Returns whether the worker should continue.
func (w *worker) doProbe(ctx context.Context) (keepGoing bool) {
	defer func() { recover() }() // Actually eat panics (HandleCrash takes care of logging)
	defer runtime.HandleCrash(func(_ interface{}) { keepGoing = true })

	startTime := time.Now()
	// 通过StatusManager获取当前容器的状态，如果还没有当前Pod的状态，那么这个Pod中的所有容器也就还没有资格执行探针
	status, ok := w.probeManager.statusManager.GetPodStatus(w.pod.UID)
	if !ok {
		// Either the pod has not been created yet, or it was already deleted.
		klog.V(3).InfoS("No status for pod", "pod", klog.KObj(w.pod))
		return true
	}

	// Worker should terminate if pod is terminated.
	// 如果当前Pod已经执行完了，那么也不需要再执行探针功能。这种状态一般是Cronjob或者Job类型的Pod才有的状态
	if status.Phase == v1.PodFailed || status.Phase == v1.PodSucceeded {
		klog.V(3).InfoS("Pod is terminated, exiting probe worker",
			"pod", klog.KObj(w.pod), "phase", status.Phase)
		return false
	}

	// 获取当前容器的状态，如果再Pod状态当中没有找到当前的容器或者容器ID为空，说明当前的容器状态可能还没有上报，只能等待下一次执行
	c, ok := podutil.GetContainerStatus(status.ContainerStatuses, w.container.Name)
	if !ok || len(c.ContainerID) == 0 {
		// Either the container has not been created yet, or it was deleted.
		klog.V(3).InfoS("Probe target container not found",
			"pod", klog.KObj(w.pod), "containerName", w.container.Name)
		return true // Wait for more information.
	}

	// 刚开始的时候Worker中记录的容器ID确实是空的
	if w.containerID.String() != c.ContainerID {
		if !w.containerID.IsEmpty() {
			// 当容器通过存活探针或者是启动探针探测失败时，这个容器就会被重启，此时容器会被分配新的ID
			w.resultsManager.Remove(w.containerID)
		}
		w.containerID = kubecontainer.ParseContainerID(c.ContainerID)
		// 往ResultManager中设置探针的初始化状态
		w.resultsManager.Set(w.containerID, w.initialValue, w.pod)
		// We've got a new container; resume probing.
		w.onHold = false
	}

	// 1、如果容器的存活探针认为当前容器处于非就绪或者启动探针认为容器处于非启动状态，有个组件会重启容器。
	// 2、在此容器重启完成之前，无需在对这个容器做检测，直达容器启动完成，onHold将会被修改为true
	if w.onHold {
		// Worker is on hold until there is a new container.
		return true
	}

	// 如果当前容器没有处于运行状态，那么就认为当前探针的结果是失败的
	if c.State.Running == nil {
		klog.V(3).InfoS("Non-running container probed",
			"pod", klog.KObj(w.pod), "containerName", w.container.Name)
		if !w.containerID.IsEmpty() {
			w.resultsManager.Set(w.containerID, results.Failure, w.pod)
		}
		// Abort if the container will not be restarted.
		// 如果当前的容器处于Terminated状态并且设置的重启策略为Never,那么这个Pod也就不需要至此执行，也就没有执行探针的必要了
		return c.State.Terminated == nil ||
			w.pod.Spec.RestartPolicy != v1.RestartPolicyNever
	}

	// Graceful shutdown of the pod.
	// 1、如果Pod已经删除并且探针类型是liveness探针、startup探针其中的一种，那么设置探针结果为成功
	if w.pod.ObjectMeta.DeletionTimestamp != nil && (w.probeType == liveness || w.probeType == startup) {
		klog.V(3).InfoS("Pod deletion requested, setting probe result to success",
			"probeType", w.probeType, "pod", klog.KObj(w.pod), "containerName", w.container.Name)
		if w.probeType == startup {
			klog.InfoS("Pod deletion requested before container has fully started",
				"pod", klog.KObj(w.pod), "containerName", w.container.Name)
		}
		// Set a last result to ensure quiet shutdown.
		w.resultsManager.Set(w.containerID, results.Success, w.pod)
		// Stop probing at this point.
		return false
	}

	// Probe disabled for InitialDelaySeconds.
	// 如果容器刚刚启动，还没有过InitialDelaySeconds时间，那么直接退出，等待下一次执行探针
	if int32(time.Since(c.State.Running.StartedAt.Time).Seconds()) < w.spec.InitialDelaySeconds {
		return true
	}

	if c.Started != nil && *c.Started {
		// Stop probing for startup once container has started.
		// we keep it running to make sure it will work for restarted container.
		// 如果容器已经启动了，那么启动探针也就无需再运行了，因为启动探针只需要保证容器已经启动，启动过后自然就无需启动探针再运行
		if w.probeType == startup {
			return true
		}
	} else {
		// Disable other probes until container has started.
		// 如果当前容器还没有启动，那么存活探针、就绪探针也就没有运行的意义，因为肯定只有当容器启动了，才能执行就绪探针、存货探针
		if w.probeType != startup {
			return true
		}
	}

	// Note, exec probe does NOT have access to pod environment variables or downward API
	// TODO 执行探针
	result, err := w.probeManager.prober.probe(ctx, w.probeType, w.pod, status, w.container, w.containerID)
	if err != nil {
		// Prober error, throw away the result.
		return true
	}

	switch result {
	case results.Success:
		// 记录探针的执行结果以及执行时长
		ProberResults.With(w.proberResultsSuccessfulMetricLabels).Inc()
		ProberDuration.With(w.proberDurationSuccessfulMetricLabels).Observe(time.Since(startTime).Seconds())
	case results.Failure:
		ProberResults.With(w.proberResultsFailedMetricLabels).Inc()
	default:
		ProberResults.With(w.proberResultsUnknownMetricLabels).Inc()
		ProberDuration.With(w.proberDurationUnknownMetricLabels).Observe(time.Since(startTime).Seconds())
	}

	if w.lastResult == result {
		w.resultRun++
	} else {
		w.lastResult = result
		w.resultRun = 1
	}

	// 如果当前探针执行结果失败，但是还没有到达失败阈值，那么还需要继续运行探针；
	// 如果当前探针执行结果成功，但是还没有到达成功阈值，那么还需要继续运行探针；
	if (result == results.Failure && w.resultRun < int(w.spec.FailureThreshold)) ||
		(result == results.Success && w.resultRun < int(w.spec.SuccessThreshold)) {
		// Success or failure is below threshold - leave the probe state unchanged.
		return true
	}

	// 设置容器探针的执行结果
	w.resultsManager.Set(w.containerID, result, w.pod)

	// 也就是说容器的存活探针认为当前容器处于非就绪或者启动探针认为容器处于非启动状态，有个组件会重启容器。
	// TODO 我猜测应该是通过状态管理器来实现的
	if (w.probeType == liveness || w.probeType == startup) && result == results.Failure {
		// The container fails a liveness/startup check, it will need to be restarted.
		// Stop probing until we see a new container ID. This is to reduce the
		// chance of hitting #21751, where running `docker exec` when a
		// container is being stopped may lead to corrupted container state.
		w.onHold = true
		w.resultRun = 0
	}

	return true
}

func deepCopyPrometheusLabels(m metrics.Labels) metrics.Labels {
	ret := make(metrics.Labels, len(m))
	for k, v := range m {
		ret[k] = v
	}
	return ret
}

func getPodLabelName(pod *v1.Pod) string {
	podName := pod.Name
	if pod.GenerateName != "" {
		podNameSlice := strings.Split(pod.Name, "-")
		podName = strings.Join(podNameSlice[:len(podNameSlice)-1], "-")
		if label, ok := pod.GetLabels()[apps.DefaultDeploymentUniqueLabelKey]; ok {
			podName = strings.ReplaceAll(podName, fmt.Sprintf("-%s", label), "")
		}
	}
	return podName
}
