/*
Copyright 2022 The Kubernetes Authors.

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

package util

import (
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/utils/clock"
)

// PodStartupLatencyTracker records key moments for startup latency calculation,
// e.g. image pulling or pod observed running on watch.
type PodStartupLatencyTracker interface {
	// ObservedPodOnWatch 用于记录容器启动耗时，容器的启动耗时等于容器创建总时间减去镜像拉取时间；通过metric指标记录
	ObservedPodOnWatch(pod *v1.Pod, when time.Time)
	// RecordImageStartedPulling 记录容器镜像开始拉取时间
	RecordImageStartedPulling(podUID types.UID)
	// RecordImageFinishedPulling 记录容器镜像拉取完成时间
	RecordImageFinishedPulling(podUID types.UID)
	// RecordStatusUpdated 设置观察到容器启动完成的时间
	RecordStatusUpdated(pod *v1.Pod)
	// DeletePodStartupState 删除记录，释放不必要的内容
	DeletePodStartupState(podUID types.UID)
}

type basicPodStartupLatencyTracker struct {
	// protect against concurrent read and write on pods map
	lock sync.Mutex
	pods map[types.UID]*perPodState
	// For testability
	clock clock.Clock
}

type perPodState struct {
	firstStartedPulling time.Time
	lastFinishedPulling time.Time
	// first time, when pod status changed into Running
	observedRunningTime time.Time
	// log, if pod latency was already Observed
	metricRecorded bool
}

// NewPodStartupLatencyTracker creates an instance of PodStartupLatencyTracker
func NewPodStartupLatencyTracker() PodStartupLatencyTracker {
	return &basicPodStartupLatencyTracker{
		pods:  map[types.UID]*perPodState{},
		clock: clock.RealClock{},
	}
}

// ObservedPodOnWatch
// 用于记录容器启动耗时，容器的启动耗时等于容器创建总时间减去镜像拉取时间
func (p *basicPodStartupLatencyTracker) ObservedPodOnWatch(pod *v1.Pod, when time.Time) {
	p.lock.Lock()
	defer p.lock.Unlock()

	// if the pod is terminal, we do not have to track it anymore for startup
	// 如果Pod已经结束执行，那么直接删除Pod的启动记录
	if pod.Status.Phase == v1.PodFailed || pod.Status.Phase == v1.PodSucceeded {
		delete(p.pods, pod.UID)
		return
	}

	// 获取Pod的状态
	state := p.pods[pod.UID]
	if state == nil {
		// create a new record for pod, only if it was not yet acknowledged by the Kubelet
		// this is required, as we want to log metric only for those pods, that where scheduled
		// after Kubelet started
		// 如果当前没有缓存过Pod的状态，那么实例化一个空的
		if pod.Status.StartTime.IsZero() {
			p.pods[pod.UID] = &perPodState{}
		}
		// TODO 如果pod.Status.StartTime非空，那么说明当前Pod已经启动了

		return
	}

	// 说明当前Pod还未启动
	if state.observedRunningTime.IsZero() {
		// skip, pod didn't start yet
		return
	}

	// 说明当前Pod已经启动了

	// 如果指标已经记录了，那么直接返回
	if state.metricRecorded {
		// skip, pod's latency already recorded
		return
	}

	// 检查Pod所有的容器都已经启动
	if hasPodStartedSLO(pod) {
		// 容器的启动耗时等于容器创建总时间减去镜像拉取时间
		podStartingDuration := when.Sub(pod.CreationTimestamp.Time)
		imagePullingDuration := state.lastFinishedPulling.Sub(state.firstStartedPulling)
		podStartSLOduration := (podStartingDuration - imagePullingDuration).Seconds()

		klog.InfoS("Observed pod startup duration",
			"pod", klog.KObj(pod),
			"podStartSLOduration", podStartSLOduration,
			"podCreationTimestamp", pod.CreationTimestamp.Time,
			"firstStartedPulling", state.firstStartedPulling,
			"lastFinishedPulling", state.lastFinishedPulling,
			"observedRunningTime", state.observedRunningTime,
			"watchObservedRunningTime", when)

		metrics.PodStartSLIDuration.WithLabelValues().Observe(podStartSLOduration)
		state.metricRecorded = true
	}
}

// RecordImageStartedPulling
// 记录Pod第一次拉取镜像的时间
func (p *basicPodStartupLatencyTracker) RecordImageStartedPulling(podUID types.UID) {
	p.lock.Lock()
	defer p.lock.Unlock()

	state := p.pods[podUID]
	if state == nil {
		return
	}

	if state.firstStartedPulling.IsZero() {
		state.firstStartedPulling = p.clock.Now()
	}
}

// RecordImageFinishedPulling
// 记录Pod完成拉取镜像的时间
func (p *basicPodStartupLatencyTracker) RecordImageFinishedPulling(podUID types.UID) {
	p.lock.Lock()
	defer p.lock.Unlock()

	state := p.pods[podUID]
	if state == nil {
		return
	}

	state.lastFinishedPulling = p.clock.Now() // Now is always grater than values from the past.
}

// RecordStatusUpdated
// 用于设置观察到Pod的启动时间
func (p *basicPodStartupLatencyTracker) RecordStatusUpdated(pod *v1.Pod) {
	p.lock.Lock()
	defer p.lock.Unlock()

	state := p.pods[pod.UID]
	if state == nil {
		return
	}

	// 如果当前Pod已经记录过了，那么就不再管了
	if state.metricRecorded {
		// skip, pod latency already recorded
		return
	}

	if !state.observedRunningTime.IsZero() {
		// skip, pod already started
		return
	}

	// 检查Pod所有的容器都已经启动
	if hasPodStartedSLO(pod) {
		klog.V(3).InfoS("Mark when the pod was running for the first time", "pod", klog.KObj(pod), "rv", pod.ResourceVersion)
		state.observedRunningTime = p.clock.Now()
	}
}

// hasPodStartedSLO, check if for given pod, each container has been started at least once
//
// This should reflect "Pod startup latency SLI" definition
// ref: https://github.com/kubernetes/community/blob/master/sig-scalability/slos/pod_startup_latency.md
// 检查Pod所有的容器都已经启动
func hasPodStartedSLO(pod *v1.Pod) bool {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Running == nil || cs.State.Running.StartedAt.IsZero() {
			return false
		}
	}

	return true
}

func (p *basicPodStartupLatencyTracker) DeletePodStartupState(podUID types.UID) {
	p.lock.Lock()
	defer p.lock.Unlock()

	delete(p.pods, podUID)
}
