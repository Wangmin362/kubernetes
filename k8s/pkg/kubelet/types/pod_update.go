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

package types

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/apis/scheduling"
)

// Annotation keys for annotations used in this package.
const (
	ConfigSourceAnnotationKey = "kubernetes.io/config.source"
	ConfigMirrorAnnotationKey = v1.MirrorPodAnnotationKey
	// ConfigFirstSeenAnnotationKey 保存Pod第一次被Kubelet观测到的事件
	ConfigFirstSeenAnnotationKey = "kubernetes.io/config.seen"
	ConfigHashAnnotationKey      = "kubernetes.io/config.hash"
)

// PodOperation defines what changes will be made on a pod configuration.
type PodOperation int

// These constants identify the PodOperations that can be made on a pod configuration.
const (
	// SET is the current pod configuration.
	// SET用于清空某个源在PodStorage中缓存的Pod，然后缓存当前变更的所有Pod；SET的目标主要对比前后两个时刻的Pod变化从而发现已经被删除的Pod
	SET PodOperation = iota
	// ADD signifies pods that are new to this source.
	ADD
	// DELETE signifies pods that are gracefully deleted from this source.
	// TODO Delete和Remove有何区别？
	DELETE
	// REMOVE signifies pods that have been removed from this source.
	REMOVE
	// UPDATE signifies pods have been updated in this source.
	UPDATE
	// RECONCILE signifies pods that have unexpected status in this source,
	// kubelet should reconcile status with this source.
	// Pod状态和期望不符，Kubelet应该进行调谐
	RECONCILE
)

// These constants identify the sources of pods.
const (
	// Filesource idenitified updates from a file.
	FileSource = "file"
	// HTTPSource identifies updates from querying a web page.
	HTTPSource = "http"
	// ApiserverSource identifies updates from Kubernetes API Server.
	ApiserverSource = "api"
	// AllSource identifies updates from all sources.
	AllSource = "*"
)

// NamespaceDefault is a string representing the default namespace.
const NamespaceDefault = metav1.NamespaceDefault

// PodUpdate defines an operation sent on the channel. You can add or remove single services by
// sending an array of size one and Op == ADD|REMOVE (with REMOVE, only the ID is required).
// For setting the state of the system to a given state for this source configuration, set
// Pods as desired and Op to SET, which will reset the system state to that specified in this
// operation for this source channel. To remove all pods, set Pods to empty object and Op to SET.
//
// Additionally, Pods should never be nil - it should always point to an empty slice. While
// functionally similar, this helps our unit tests properly check that the correct PodUpdates
// are generated.
type PodUpdate struct {
	// 1、一次可以针对多个Pod做相同的操作
	// 2、在这里我们不需要知道Pod究竟发生了什么样的变更。我们只需要记录用户希望这个Pod最终是什么样子。kubelet自己会记录变更之前的状态，
	// 因此kubelet必须知道如何把一个pod从之前的状态调整为用户期望的状态。
	Pods []*v1.Pod
	Op   PodOperation
	// Pod的产生源头有HTTP、File、APIServer
	Source string
}

// GetValidatedSources gets all validated sources from the specified sources.
func GetValidatedSources(sources []string) ([]string, error) {
	validated := make([]string, 0, len(sources))
	for _, source := range sources {
		switch source {
		case AllSource:
			return []string{FileSource, HTTPSource, ApiserverSource}, nil
		case FileSource, HTTPSource, ApiserverSource:
			validated = append(validated, source)
		case "":
			// Skip
		default:
			return []string{}, fmt.Errorf("unknown pod source %q", source)
		}
	}
	return validated, nil
}

// GetPodSource returns the source of the pod based on the annotation.
func GetPodSource(pod *v1.Pod) (string, error) {
	if pod.Annotations != nil {
		if source, ok := pod.Annotations[ConfigSourceAnnotationKey]; ok {
			return source, nil
		}
	}
	return "", fmt.Errorf("cannot get source of pod %q", pod.UID)
}

// SyncPodType classifies pod updates, eg: create, update.
type SyncPodType int

const (
	// SyncPodSync is when the pod is synced to ensure desired state
	SyncPodSync SyncPodType = iota
	// SyncPodUpdate is when the pod is updated from source
	SyncPodUpdate
	// SyncPodCreate is when the pod is created from source
	SyncPodCreate
	// SyncPodKill is when the pod should have no running containers. A pod stopped in this way could be
	// restarted in the future due config changes.
	SyncPodKill
)

func (sp SyncPodType) String() string {
	switch sp {
	case SyncPodCreate:
		return "create"
	case SyncPodUpdate:
		return "update"
	case SyncPodSync:
		return "sync"
	case SyncPodKill:
		return "kill"
	default:
		return "unknown"
	}
}

// IsMirrorPod returns true if the passed Pod is a Mirror Pod.
func IsMirrorPod(pod *v1.Pod) bool {
	if pod.Annotations == nil {
		return false
	}
	_, ok := pod.Annotations[ConfigMirrorAnnotationKey]
	return ok
}

// IsStaticPod returns true if the pod is a static pod.
func IsStaticPod(pod *v1.Pod) bool {
	source, err := GetPodSource(pod)
	return err == nil && source != ApiserverSource
}

// IsCriticalPod returns true if pod's priority is greater than or equal to SystemCriticalPriority.
func IsCriticalPod(pod *v1.Pod) bool {
	if IsStaticPod(pod) {
		return true
	}
	if IsMirrorPod(pod) {
		return true
	}
	if pod.Spec.Priority != nil && IsCriticalPodBasedOnPriority(*pod.Spec.Priority) {
		return true
	}
	return false
}

// Preemptable returns true if preemptor pod can preempt preemptee pod
// if preemptee is not critical or if preemptor's priority is greater than preemptee's priority
func Preemptable(preemptor, preemptee *v1.Pod) bool {
	if IsCriticalPod(preemptor) && !IsCriticalPod(preemptee) {
		return true
	}
	if (preemptor != nil && preemptor.Spec.Priority != nil) &&
		(preemptee != nil && preemptee.Spec.Priority != nil) {
		return *(preemptor.Spec.Priority) > *(preemptee.Spec.Priority)
	}

	return false
}

// IsCriticalPodBasedOnPriority checks if the given pod is a critical pod based on priority resolved from pod Spec.
func IsCriticalPodBasedOnPriority(priority int32) bool {
	return priority >= scheduling.SystemCriticalPriority
}

// IsNodeCriticalPod checks if the given pod is a system-node-critical
func IsNodeCriticalPod(pod *v1.Pod) bool {
	return IsCriticalPod(pod) && (pod.Spec.PriorityClassName == scheduling.SystemNodeCritical)
}
