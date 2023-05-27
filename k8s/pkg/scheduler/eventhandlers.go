/*
Copyright 2019 The Kubernetes Authors.

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

package scheduler

import (
	"fmt"
	"reflect"
	"strings"

	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	corev1nodeaffinity "k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodeaffinity"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodename"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodeports"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	"k8s.io/kubernetes/pkg/scheduler/internal/queue"
	"k8s.io/kubernetes/pkg/scheduler/profile"
)

func (sched *Scheduler) onStorageClassAdd(obj interface{}) {
	sc, ok := obj.(*storagev1.StorageClass)
	if !ok {
		klog.ErrorS(nil, "Cannot convert to *storagev1.StorageClass", "obj", obj)
		return
	}

	// CheckVolumeBindingPred fails if pod has unbound immediate PVCs. If these
	// PVCs have specified StorageClass name, creating StorageClass objects
	// with late binding will cause predicates to pass, so we need to move pods
	// to active queue.
	// We don't need to invalidate cached results because results will not be
	// cached for pod that has unbound immediate PVCs.
	if sc.VolumeBindingMode != nil && *sc.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer {
		sched.SchedulingQueue.MoveAllToActiveOrBackoffQueue(queue.StorageClassAdd, nil)
	}
}

func (sched *Scheduler) addNodeToCache(obj interface{}) {
	node, ok := obj.(*v1.Node)
	if !ok {
		klog.ErrorS(nil, "Cannot convert to *v1.Node", "obj", obj)
		return
	}

	nodeInfo := sched.Cache.AddNode(node)
	klog.V(3).InfoS("Add event for node", "node", klog.KObj(node))
	// 因为有新的Node加入K8S集群，那写以前没有调度成功的Pod现在可能调度成功了，因需要把UnschedulableQ以及BackoffQ中的所有Pod都
	// 放到ActiveQ当中，给这些Pod一次调度的机会
	sched.SchedulingQueue.MoveAllToActiveOrBackoffQueue(queue.NodeAdd, preCheckForNode(nodeInfo))
}

func (sched *Scheduler) updateNodeInCache(oldObj, newObj interface{}) {
	oldNode, ok := oldObj.(*v1.Node)
	if !ok {
		klog.ErrorS(nil, "Cannot convert oldObj to *v1.Node", "oldObj", oldObj)
		return
	}
	newNode, ok := newObj.(*v1.Node)
	if !ok {
		klog.ErrorS(nil, "Cannot convert newObj to *v1.Node", "newObj", newObj)
		return
	}

	nodeInfo := sched.Cache.UpdateNode(oldNode, newNode)
	// Only requeue unschedulable pods if the node became more schedulable.
	// TODO 仔细分析
	if event := nodeSchedulingPropertiesChange(newNode, oldNode); event != nil {
		sched.SchedulingQueue.MoveAllToActiveOrBackoffQueue(*event, preCheckForNode(nodeInfo))
	}
}

func (sched *Scheduler) deleteNodeFromCache(obj interface{}) {
	var node *v1.Node
	switch t := obj.(type) {
	case *v1.Node:
		node = t
	case cache.DeletedFinalStateUnknown:
		var ok bool
		node, ok = t.Obj.(*v1.Node)
		if !ok {
			klog.ErrorS(nil, "Cannot convert to *v1.Node", "obj", t.Obj)
			return
		}
	default:
		klog.ErrorS(nil, "Cannot convert to *v1.Node", "obj", t)
		return
	}
	klog.V(3).InfoS("Delete event for node", "node", klog.KObj(node))
	if err := sched.Cache.RemoveNode(node); err != nil {
		klog.ErrorS(err, "Scheduler cache RemoveNode failed")
	}
}

func (sched *Scheduler) addPodToSchedulingQueue(obj interface{}) {
	pod := obj.(*v1.Pod)
	klog.V(3).InfoS("Add event for unscheduled pod", "pod", klog.KObj(pod))
	if err := sched.SchedulingQueue.Add(pod); err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to queue %T: %v", obj, err))
	}
}

func (sched *Scheduler) updatePodInSchedulingQueue(oldObj, newObj interface{}) {
	oldPod, newPod := oldObj.(*v1.Pod), newObj.(*v1.Pod)
	// Bypass update event that carries identical objects; otherwise, a duplicated
	// Pod may go through scheduling and cause unexpected behavior (see #96071).
	// 由于一些不可预期的行为，如果资源版本没有发生变化，就认为当前Pod没有发生改变，因此直接退出
	if oldPod.ResourceVersion == newPod.ResourceVersion {
		return
	}

	// 所谓的AssumedPod，实际上就是已经完成SchedulingCycle的Pod，这些Pod不需要再次经过SchedulingCycle
	isAssumed, err := sched.Cache.IsAssumedPod(newPod)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("failed to check whether pod %s/%s is assumed: %v", newPod.Namespace, newPod.Name, err))
	}
	// 1、如果一个Pod已经被调度过了，这种Pod无需再次调度
	// 2、实际上这里的AssumedPod值得是那些成功经过SchedulingCycle阶段，但是还没有经过BindingCycle阶段的Pod。因为正儿八经已经
	// 调度过的Pod是不可能到这里的，在Informer的Filter当中已经过滤了
	if isAssumed {
		return
	}

	// 更新还没有调度的Pod
	if err := sched.SchedulingQueue.Update(oldPod, newPod); err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to update %T: %v", newObj, err))
	}
}

func (sched *Scheduler) deletePodFromSchedulingQueue(obj interface{}) {
	var pod *v1.Pod
	switch t := obj.(type) {
	case *v1.Pod:
		pod = obj.(*v1.Pod)
	case cache.DeletedFinalStateUnknown:
		var ok bool
		pod, ok = t.Obj.(*v1.Pod)
		if !ok {
			utilruntime.HandleError(fmt.Errorf("unable to convert object %T to *v1.Pod in %T", obj, sched))
			return
		}
	default:
		utilruntime.HandleError(fmt.Errorf("unable to handle object in %T: %T", sched, obj))
		return
	}
	klog.V(3).InfoS("Delete event for unscheduled pod", "pod", klog.KObj(pod))
	if err := sched.SchedulingQueue.Delete(pod); err != nil {
		utilruntime.HandleError(fmt.Errorf("unable to dequeue %T: %v", obj, err))
	}
	fwk, err := sched.frameworkForPod(pod)
	if err != nil {
		// This shouldn't happen, because we only accept for scheduling the pods
		// which specify a scheduler name that matches one of the profiles.
		klog.ErrorS(err, "Unable to get profile", "pod", klog.KObj(pod))
		return
	}
	// If a waiting pod is rejected, it indicates it's previously assumed and we're
	// removing it from the scheduler cache. In this case, signal a AssignedPodDelete
	// event to immediately retry some unscheduled Pods.
	if fwk.RejectWaitingPod(pod.UID) {
		sched.SchedulingQueue.MoveAllToActiveOrBackoffQueue(queue.AssignedPodDelete, nil)
	}
}

func (sched *Scheduler) addPodToCache(obj interface{}) {
	pod, ok := obj.(*v1.Pod)
	if !ok {
		klog.ErrorS(nil, "Cannot convert to *v1.Pod", "obj", obj)
		return
	}
	klog.V(3).InfoS("Add event for scheduled pod", "pod", klog.KObj(pod))

	// 把Pod添加到Cache当中记录下来，这里添加的Pod主要有两种类型的Pod,分别如下：
	// 1、所有已经真正调度了的Pod（通过监听PodInformer来实现）
	// 2、所有已经通过SchedulingCycle阶段，但是还没有完成BindingCycle阶段的Pod（通过assume操作来实现），这种类型的Pod也称之为AssumedPod
	// 2.1、之所以需要AssumedPod,是因为一个Pod如果已经通过SchedulingCycle阶段，找到了那个合适的Node去部署当前Pod。但是实际上还没有经过
	// BindingCycle真正绑定到该Node上。此时为了能够真正的把当前Pod绑定到已经分配好的Node之上，我们必须要假定当前Node已经成功部署了这个Pod，
	// 因为我们必须要把这个还没有真正绑定的Pod所需要的资源留出来，包括内存、CPU资源、亲和性、反亲和性等等。这样才能保证调度其它Pod的时候考虑到了
	// 当前还没有真正绑定的Pod。同时，这样的假设才能让KubeScheduler的性能发挥到最高，否则，如果不进行假设，KubeScheduler在一个Pod完成调度之
	// 时必须要等待这个Pod完成BindingCycle阶段才能继续调度下一个Pod
	if err := sched.Cache.AddPod(pod); err != nil {
		klog.ErrorS(err, "Scheduler cache AddPod failed", "pod", klog.KObj(pod))
	}

	// 1、AssignedPodAdded用于当KubeScheduler通过PodInformer监听到了一个Pod已经完成了调度。此时，需要把一些和当前Pod具有相同亲和性
	// 并且还没有成功调度的Pod加入到ActiveQ或者BackoffQ当中。
	// 2、之所以需要这么设计的原因是因为：有些Pod因为亲和性的原因没有找到合适的Node从而导致调度失败，如果此时KubeScheduler发现了一个已经成功
	// 调度的Pod，那肯定需要给那些因为相同的亲和性导致调度失败的Pod。说不定由于这个Pod的加入，之前因为亲和性调度失败的的Pod就能完成调度。
	// 3、这样设计的好处就是增加了因为亲和性导致调度失败的Pod的调度成功率
	sched.SchedulingQueue.AssignedPodAdded(pod)
}

func (sched *Scheduler) updatePodInCache(oldObj, newObj interface{}) {
	oldPod, ok := oldObj.(*v1.Pod)
	if !ok {
		klog.ErrorS(nil, "Cannot convert oldObj to *v1.Pod", "oldObj", oldObj)
		return
	}
	newPod, ok := newObj.(*v1.Pod)
	if !ok {
		klog.ErrorS(nil, "Cannot convert newObj to *v1.Pod", "newObj", newObj)
		return
	}
	klog.V(4).InfoS("Update event for scheduled pod", "pod", klog.KObj(oldPod))

	// 更新Cache中的Pod信息
	if err := sched.Cache.UpdatePod(oldPod, newPod); err != nil {
		klog.ErrorS(err, "Scheduler cache UpdatePod failed", "pod", klog.KObj(oldPod))
	}

	// 1、AssignedPodUpdated是用于当KubeScheduler通过PodInformer监听到一个已经完成调度的Pod发生了变更。此时，需要把一些和当前Pod具有
	// 相同亲和性并且还没有成功调度的Pod加入到ActiveQ或者BackoffQ当中。
	// 2、之所以需要这么设计的原因是因为：有些Pod因为亲和性的原因没有找到合适的Node从而导致调度失败，如果此时KubeScheduler发现了一个已经成功
	// 调度的Pod发生了变更，那肯定需要给那些因为相同的亲和性导致调度失败的Pod。说不定由于这个Pod的变更，之前因为亲和性调度失败的的Pod就能完成调度。
	sched.SchedulingQueue.AssignedPodUpdated(newPod)
}

func (sched *Scheduler) deletePodFromCache(obj interface{}) {
	var pod *v1.Pod
	switch t := obj.(type) {
	case *v1.Pod:
		pod = t
	case cache.DeletedFinalStateUnknown:
		var ok bool
		pod, ok = t.Obj.(*v1.Pod)
		if !ok {
			klog.ErrorS(nil, "Cannot convert to *v1.Pod", "obj", t.Obj)
			return
		}
	default:
		klog.ErrorS(nil, "Cannot convert to *v1.Pod", "obj", t)
		return
	}
	klog.V(3).InfoS("Delete event for scheduled pod", "pod", klog.KObj(pod))
	// 移除Cache中的Pod
	if err := sched.Cache.RemovePod(pod); err != nil {
		klog.ErrorS(err, "Scheduler cache RemovePod failed", "pod", klog.KObj(pod))
	}

	// 1、一旦删除一个Pod，就把所有未调度的Pod添加到ActiveQ或者BackoffQ当中
	// 2、之所以这么做，是因为一个已经调度的Pod的删除必然会造成某些没有调度成功的Pod可能有机会调度成功
	sched.SchedulingQueue.MoveAllToActiveOrBackoffQueue(queue.AssignedPodDelete, nil)
}

// assignedPod selects pods that are assigned (scheduled and running).
func assignedPod(pod *v1.Pod) bool {
	return len(pod.Spec.NodeName) != 0
}

// responsibleForPod returns true if the pod has asked to be scheduled by the given scheduler.
func responsibleForPod(pod *v1.Pod, profiles profile.Map) bool {
	return profiles.HandlesSchedulerName(pod.Spec.SchedulerName)
}

// addAllEventHandlers is a helper function used in tests and in Scheduler
// to add event handlers for various informers.
func addAllEventHandlers(
	sched *Scheduler,
	informerFactory informers.SharedInformerFactory,
	dynInformerFactory dynamicinformer.DynamicSharedInformerFactory,
	gvkMap map[framework.GVK]framework.ActionType,
) {
	// 1、这里主要是在维护Cache的Pod信息，这些Pod是已经被调度好了的Pod。这些信息包括：每个Node当前都运行了哪些已经调度的Pod,每个Pod的亲和性、
	// 反亲和性、申请的资源、每个Node已经分配的资源。之所以需要维护这些信息，是因为后续调度一个Pod时需要参考这些信息用来做决策；最简单的思考过程
	// 就是当一个Pod需要调度时，有哪些Node有足够的资源运行这个Pod，接下来会思考这个Pod的亲和性、反亲和性、端口分配等等因素，因此调度器需要直到
	// 每个Node的这些信息
	informerFactory.Core().V1().Pods().Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				switch t := obj.(type) {
				case *v1.Pod:
					// 1、若一个Pod的Spec.NodeName非空，就说明这个Pod已经调度成功了。KubeScheduler需要把这种类型的Pod收集起来计算
					// Node的资源，方便后续为调度逻辑处理
					// 2、因为KubeScheduling在调度一个Pod时需要知道当前K8S的Node上都跑了那些Pod，每个Pod占用的资源情况、亲和性以及反
					// 亲和性等等，因此需要收集这些已经成功调度的Pod信息
					return assignedPod(t)
				case cache.DeletedFinalStateUnknown:
					if _, ok := t.Obj.(*v1.Pod); ok {
						// The carried object may be stale, so we don't use it to check if
						// it's assigned or not. Attempting to cleanup anyways.
						return true
					}
					utilruntime.HandleError(fmt.Errorf("unable to convert object %T to *v1.Pod in %T", obj, sched))
					return false
				default:
					utilruntime.HandleError(fmt.Errorf("unable to handle object in %T: %T", sched, obj))
					return false
				}
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc:    sched.addPodToCache,
				UpdateFunc: sched.updatePodInCache,
				DeleteFunc: sched.deletePodFromCache,
			},
		},
	)
	// unscheduled pod queue
	// 对于还没有调度的Pod,这些Pod的增删改查是需要添加到SchedulerQueue当中（实际上就是PriorityQueue）
	informerFactory.Core().V1().Pods().Informer().AddEventHandler(
		cache.FilteringResourceEventHandler{
			FilterFunc: func(obj interface{}) bool {
				switch t := obj.(type) {
				case *v1.Pod:
					// 1、如果当前Pod还没有调度成功，并且当前Pod指定使用的调度器存在；如果当前Pod需要使用的调度器不存在，那显然无法调度
					// 2、如果一个Pod的Spec.NodeName非空，就说明这个Pod已经调度成功了，KubeScheduler无需再次处理
					return !assignedPod(t) && responsibleForPod(t, sched.Profiles)
				case cache.DeletedFinalStateUnknown:
					// TODO 什么时候当前元素会是DeletedFinalStateUnknown类型
					if pod, ok := t.Obj.(*v1.Pod); ok {
						// The carried object may be stale, so we don't use it to check if it's assigned or not.
						// 如果当前元素是个Pod，并且这个Pod需要使用的调度器存在
						return responsibleForPod(pod, sched.Profiles)
					}
					utilruntime.HandleError(fmt.Errorf("unable to convert object %T to *v1.Pod in %T", obj, sched))
					return false
				default:
					utilruntime.HandleError(fmt.Errorf("unable to handle object in %T: %T", sched, obj))
					return false
				}
			},
			Handler: cache.ResourceEventHandlerFuncs{
				AddFunc:    sched.addPodToSchedulingQueue,
				UpdateFunc: sched.updatePodInSchedulingQueue,
				DeleteFunc: sched.deletePodFromSchedulingQueue,
			},
		},
	)

	// TODO 维护Cache当中的Node信息,其中的维护动作有点复杂，Node信息居然有map, linkedlist, tree三种数据结构来维护
	informerFactory.Core().V1().Nodes().Informer().AddEventHandler(
		cache.ResourceEventHandlerFuncs{
			AddFunc:    sched.addNodeToCache,      // TODO 仔细分析
			UpdateFunc: sched.updateNodeInCache,   //  TODO 仔细分析
			DeleteFunc: sched.deleteNodeFromCache, //  TODO 仔细分析
		},
	)

	// TODO 这里是在干嘛？
	buildEvtResHandler := func(at framework.ActionType, gvk framework.GVK, shortGVK string) cache.ResourceEventHandlerFuncs {
		funcs := cache.ResourceEventHandlerFuncs{}
		if at&framework.Add != 0 {
			evt := framework.ClusterEvent{Resource: gvk, ActionType: framework.Add, Label: fmt.Sprintf("%vAdd", shortGVK)}
			funcs.AddFunc = func(_ interface{}) {
				sched.SchedulingQueue.MoveAllToActiveOrBackoffQueue(evt, nil)
			}
		}
		if at&framework.Update != 0 {
			evt := framework.ClusterEvent{Resource: gvk, ActionType: framework.Update, Label: fmt.Sprintf("%vUpdate", shortGVK)}
			funcs.UpdateFunc = func(_, _ interface{}) {
				sched.SchedulingQueue.MoveAllToActiveOrBackoffQueue(evt, nil)
			}
		}
		if at&framework.Delete != 0 {
			evt := framework.ClusterEvent{Resource: gvk, ActionType: framework.Delete, Label: fmt.Sprintf("%vDelete", shortGVK)}
			funcs.DeleteFunc = func(_ interface{}) {
				sched.SchedulingQueue.MoveAllToActiveOrBackoffQueue(evt, nil)
			}
		}
		return funcs
	}

	for gvk, at := range gvkMap {
		switch gvk {
		case framework.Node, framework.Pod:
			// Do nothing.
		case framework.CSINode:
			informerFactory.Storage().V1().CSINodes().Informer().AddEventHandler(
				buildEvtResHandler(at, framework.CSINode, "CSINode"),
			)
		case framework.CSIDriver:
			informerFactory.Storage().V1().CSIDrivers().Informer().AddEventHandler(
				buildEvtResHandler(at, framework.CSIDriver, "CSIDriver"),
			)
		case framework.CSIStorageCapacity:
			informerFactory.Storage().V1().CSIStorageCapacities().Informer().AddEventHandler(
				buildEvtResHandler(at, framework.CSIStorageCapacity, "CSIStorageCapacity"),
			)
		case framework.PersistentVolume:
			// MaxPDVolumeCountPredicate: since it relies on the counts of PV.
			//
			// PvAdd: Pods created when there are no PVs available will be stuck in
			// unschedulable queue. But unbound PVs created for static provisioning and
			// delay binding storage class are skipped in PV controller dynamic
			// provisioning and binding process, will not trigger events to schedule pod
			// again. So we need to move pods to active queue on PV add for this
			// scenario.
			//
			// PvUpdate: Scheduler.bindVolumesWorker may fail to update assumed pod volume
			// bindings due to conflicts if PVs are updated by PV controller or other
			// parties, then scheduler will add pod back to unschedulable queue. We
			// need to move pods to active queue on PV update for this scenario.
			informerFactory.Core().V1().PersistentVolumes().Informer().AddEventHandler(
				buildEvtResHandler(at, framework.PersistentVolume, "Pv"),
			)
		case framework.PersistentVolumeClaim:
			// MaxPDVolumeCountPredicate: add/update PVC will affect counts of PV when it is bound.
			informerFactory.Core().V1().PersistentVolumeClaims().Informer().AddEventHandler(
				buildEvtResHandler(at, framework.PersistentVolumeClaim, "Pvc"),
			)
		case framework.StorageClass:
			if at&framework.Add != 0 {
				informerFactory.Storage().V1().StorageClasses().Informer().AddEventHandler(
					cache.ResourceEventHandlerFuncs{
						AddFunc: sched.onStorageClassAdd,
					},
				)
			}
			if at&framework.Update != 0 {
				informerFactory.Storage().V1().StorageClasses().Informer().AddEventHandler(
					cache.ResourceEventHandlerFuncs{
						UpdateFunc: func(_, _ interface{}) {
							sched.SchedulingQueue.MoveAllToActiveOrBackoffQueue(queue.StorageClassUpdate, nil)
						},
					},
				)
			}
		default:
			// Tests may not instantiate dynInformerFactory.
			if dynInformerFactory == nil {
				continue
			}
			// GVK is expected to be at least 3-folded, separated by dots.
			// <kind in plural>.<version>.<group>
			// Valid examples:
			// - foos.v1.example.com
			// - bars.v1beta1.a.b.c
			// Invalid examples:
			// - foos.v1 (2 sections)
			// - foo.v1.example.com (the first section should be plural)
			if strings.Count(string(gvk), ".") < 2 {
				klog.ErrorS(nil, "incorrect event registration", "gvk", gvk)
				continue
			}
			// Fall back to try dynamic informers.
			gvr, _ := schema.ParseResourceArg(string(gvk))
			dynInformer := dynInformerFactory.ForResource(*gvr).Informer()
			dynInformer.AddEventHandler(
				buildEvtResHandler(at, gvk, strings.Title(gvr.Resource)),
			)
		}
	}
}

// TODO 仔细分析
func nodeSchedulingPropertiesChange(newNode *v1.Node, oldNode *v1.Node) *framework.ClusterEvent {
	if nodeSpecUnschedulableChanged(newNode, oldNode) {
		return &queue.NodeSpecUnschedulableChange
	}
	if nodeAllocatableChanged(newNode, oldNode) {
		return &queue.NodeAllocatableChange
	}
	if nodeLabelsChanged(newNode, oldNode) {
		return &queue.NodeLabelChange
	}
	if nodeTaintsChanged(newNode, oldNode) {
		return &queue.NodeTaintChange
	}
	if nodeConditionsChanged(newNode, oldNode) {
		return &queue.NodeConditionChange
	}

	return nil
}

func nodeAllocatableChanged(newNode *v1.Node, oldNode *v1.Node) bool {
	return !reflect.DeepEqual(oldNode.Status.Allocatable, newNode.Status.Allocatable)
}

func nodeLabelsChanged(newNode *v1.Node, oldNode *v1.Node) bool {
	return !reflect.DeepEqual(oldNode.GetLabels(), newNode.GetLabels())
}

func nodeTaintsChanged(newNode *v1.Node, oldNode *v1.Node) bool {
	return !reflect.DeepEqual(newNode.Spec.Taints, oldNode.Spec.Taints)
}

func nodeConditionsChanged(newNode *v1.Node, oldNode *v1.Node) bool {
	strip := func(conditions []v1.NodeCondition) map[v1.NodeConditionType]v1.ConditionStatus {
		conditionStatuses := make(map[v1.NodeConditionType]v1.ConditionStatus, len(conditions))
		for i := range conditions {
			conditionStatuses[conditions[i].Type] = conditions[i].Status
		}
		return conditionStatuses
	}
	return !reflect.DeepEqual(strip(oldNode.Status.Conditions), strip(newNode.Status.Conditions))
}

func nodeSpecUnschedulableChanged(newNode *v1.Node, oldNode *v1.Node) bool {
	return newNode.Spec.Unschedulable != oldNode.Spec.Unschedulable && !newNode.Spec.Unschedulable
}

func preCheckForNode(nodeInfo *framework.NodeInfo) queue.PreEnqueueCheck {
	// Note: the following checks doesn't take preemption into considerations, in very rare
	// cases (e.g., node resizing), "pod" may still fail a check but preemption helps. We deliberately
	// chose to ignore those cases as unschedulable pods will be re-queued eventually.
	return func(pod *v1.Pod) bool {
		// TODO 检查了啥？
		admissionResults := AdmissionCheck(pod, nodeInfo, false)
		if len(admissionResults) != 0 {
			return false
		}
		// TODO 这里又是在干嘛？
		_, isUntolerated := corev1helpers.FindMatchingUntoleratedTaint(nodeInfo.Node().Spec.Taints, pod.Spec.Tolerations, func(t *v1.Taint) bool {
			return t.Effect == v1.TaintEffectNoSchedule
		})
		return !isUntolerated
	}
}

// AdmissionCheck calls the filtering logic of noderesources/nodeport/nodeAffinity/nodename
// and returns the failure reasons. It's used in kubelet(pkg/kubelet/lifecycle/predicate.go) and scheduler.
// It returns the first failure if `includeAllFailures` is set to false; otherwise
// returns all failures.
// TODO 检查了啥？
func AdmissionCheck(pod *v1.Pod, nodeInfo *framework.NodeInfo, includeAllFailures bool) []AdmissionResult {
	var admissionResults []AdmissionResult
	insufficientResources := noderesources.Fits(pod, nodeInfo)
	if len(insufficientResources) != 0 {
		for i := range insufficientResources {
			admissionResults = append(admissionResults, AdmissionResult{InsufficientResource: &insufficientResources[i]})
		}
		if !includeAllFailures {
			return admissionResults
		}
	}

	if matches, _ := corev1nodeaffinity.GetRequiredNodeAffinity(pod).Match(nodeInfo.Node()); !matches {
		admissionResults = append(admissionResults, AdmissionResult{Name: nodeaffinity.Name, Reason: nodeaffinity.ErrReasonPod})
		if !includeAllFailures {
			return admissionResults
		}
	}
	if !nodename.Fits(pod, nodeInfo) {
		admissionResults = append(admissionResults, AdmissionResult{Name: nodename.Name, Reason: nodename.ErrReason})
		if !includeAllFailures {
			return admissionResults
		}
	}
	if !nodeports.Fits(pod, nodeInfo) {
		admissionResults = append(admissionResults, AdmissionResult{Name: nodeports.Name, Reason: nodeports.ErrReason})
		if !includeAllFailures {
			return admissionResults
		}
	}
	return admissionResults
}

// AdmissionResult describes the reason why Scheduler can't admit the pod.
// If the reason is a resource fit one, then AdmissionResult.InsufficientResource includes the details.
type AdmissionResult struct {
	Name                 string
	Reason               string
	InsufficientResource *noderesources.InsufficientResource
}
