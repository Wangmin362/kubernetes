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
	"time"

	"k8s.io/apimachinery/pkg/types"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

// PodLifeCycleEventType define the event type of pod life cycle events.
type PodLifeCycleEventType string

type RelistDuration struct {
	// The period for relisting.  默认为300秒
	RelistPeriod time.Duration
	// The relisting threshold needs to be greater than the relisting period +
	// the relisting time, which can vary significantly. Set a conservative
	// threshold to avoid flipping between healthy and unhealthy.
	// 用于判断PLEG是否正常工作，如果正常工作，那么PLEG不可能两次relist执行相差大于这个阈值，默认为10分钟
	RelistThreshold time.Duration
}

const (
	// ContainerStarted - event type when the new state of container is running.
	ContainerStarted PodLifeCycleEventType = "ContainerStarted"
	// ContainerDied - event type when the new state of container is exited.
	ContainerDied PodLifeCycleEventType = "ContainerDied"
	// ContainerRemoved - event type when the old state of container is exited.
	ContainerRemoved PodLifeCycleEventType = "ContainerRemoved"
	// PodSync is used to trigger syncing of a pod when the observed change of
	// the state of the pod cannot be captured by any single event above.
	PodSync PodLifeCycleEventType = "PodSync"
	// ContainerChanged - event type when the new state of container is unknown.
	ContainerChanged PodLifeCycleEventType = "ContainerChanged"
)

// PodLifecycleEvent is an event that reflects the change of the pod state.
type PodLifecycleEvent struct {
	// The pod ID.
	ID types.UID
	// The type of the event.
	Type PodLifeCycleEventType
	// The accompanied data which varies based on the event type.
	//   - ContainerStarted/ContainerStopped: the container name (string).
	//   - All other event types: unused.
	Data interface{}
}

// PodLifecycleEventGenerator contains functions for generating pod life cycle events.
// TODO 如何理解这个接口？
type PodLifecycleEventGenerator interface {
	Start()
	Stop()
	Update(relistDuration *RelistDuration)
	Watch() chan *PodLifecycleEvent
	Healthy() (bool, error)
	Relist()
	UpdateCache(*kubecontainer.Pod, types.UID) (error, bool)
}
