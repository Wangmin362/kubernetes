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

package config

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	"k8s.io/kubernetes/pkg/util/config"
)

// PodConfigNotificationMode describes how changes are sent to the update channel.
// TODO 如何理解这里的Pod配置通知模式？
type PodConfigNotificationMode int

const (
	// PodConfigNotificationUnknown is the default value for
	// PodConfigNotificationMode when uninitialized.
	PodConfigNotificationUnknown PodConfigNotificationMode = iota
	// PodConfigNotificationSnapshot delivers the full configuration as a SET whenever
	// any change occurs.
	// TODO 如何理解该模式？
	PodConfigNotificationSnapshot
	// PodConfigNotificationSnapshotAndUpdates delivers an UPDATE and DELETE message whenever pods are
	// changed, and a SET message if there are any additions or removals.
	// TODO 如何理解该模式？
	PodConfigNotificationSnapshotAndUpdates
	// PodConfigNotificationIncremental delivers ADD, UPDATE, DELETE, REMOVE, RECONCILE to the update channel.
	// TODO 如何理解该模式？
	PodConfigNotificationIncremental
)

// TODO 这个接口抽象来干嘛的？
type podStartupSLIObserver interface {
	ObservedPodOnWatch(pod *v1.Pod, when time.Time)
}

// PodConfig is a configuration mux that merges many sources of pod configuration into a single
// consistent structure, and then delivers incremental change notifications to listeners
// in order.
// 1、PodConfig用于抽象Pod变更的变更，并合并了URL, FILE, APIServer这三个源的变更，同时会把这些变更派送给监听者
// 2、URL, File, APIServer这三个来源的变更来自于用户，APIServer会把这些数据更新到ETCD同时，这些变更需要让底层真正干活的组件知道，也就是
// kubelet。kubelet会监听updates通道，一旦发现有Pod变更了就需要把底层真正运行的Pod reconcile 为用户期望的状态。同时，pod reconcile
// 过程中的状态转换也需要上报上来，让用户知道底层的pod变更事件。
// 3、TODO 目前看来所有源的变更都是SET事件，并没有其它事件
type PodConfig struct {
	// 1、Pod存储，存储了来自于不同来源的Pod
	// 2、这里的缓存相当重要，对于APIServer, HTTP, File, 这里缓存的数据就是上一个时刻的数据，也就是说我们可以对比缓存中的数据以及当前
	// 变更数据，从而知道当前Pod发生了什么变化，是更新、删除还是创建
	pods *podStorage
	// 1、这个Mux比较有意思，这个Mux不是普通HTTP的路由，而是不同来源的Pod的路由
	// 2、Mux的主要职责就是合并某个源的缓存数据与当前变更数据，从而得出Pod的变更事件
	mux *config.Mux

	// the channel of denormalized changes passed to listeners
	// 1、这里应该也是采用了异步接收Pod的模式，通过channel可以实现一个简单且高效的消息中间件，从而解耦PodUpdate事件的生产端和消费端
	// 2、updates的数据来源有：1、StaticPod, 2、HTTP  3、APIServer
	// 3、外部的组件通过这个channel获取Pod的变更
	updates chan kubetypes.PodUpdate

	// contains the list of all configured sources
	sourcesLock sync.Mutex
	// K8S目前支持HTTP, File, APIServer这三种来源
	sources sets.String
}

// NewPodConfig creates an object that can merge many configuration sources into a stream
// of normalized updates to a pod configuration.
func NewPodConfig(
	mode PodConfigNotificationMode, // 默认为PodConfigNotificationIncremental类型
	recorder record.EventRecorder,
	startupSLIObserver podStartupSLIObserver,
) *PodConfig {
	// 消息队列的长度为50个
	updates := make(chan kubetypes.PodUpdate, 50)
	// 1、实例化PodStorage，实际上就是一个二级map，第一级Key为Source，第二级Key为Pod.UID
	// 2、PodStorage同时是一个Merge,
	storage := newPodStorage(updates, mode, recorder, startupSLIObserver)
	podConfig := &PodConfig{
		pods:    storage,
		mux:     config.NewMux(storage),
		updates: updates,
		sources: sets.String{},
	}
	return podConfig
}

// Channel creates or returns a config source channel.  The channel
// only accepts PodUpdates
// 1、添加某个来源的Pod数据，PodConfig将会持续监听这个来源的数据
// 2、监听当前指定的源，并返回一个零缓冲channel，当外部的组件发现这个源的Pod发生了变更，应该向这个channel写入变更数据。于此同时，这里会启动一个
// 协程监听这个channel，当外部的组件向这这个channel写入Pod变更数据时，Mux会把这个源的缓存Pod数据与变更的Pod数据做合并，对比出每个Pod应该执行
// ADD, DELETE, REMOVE, UPDATE, RECONCILE当作当中的其中一个，还是当前Pod没有发生任何改变
func (c *PodConfig) Channel(ctx context.Context, source string) chan<- interface{} {
	c.sourcesLock.Lock()
	defer c.sourcesLock.Unlock()
	// 添加新的Pod来源
	c.sources.Insert(source)
	return c.mux.ChannelWithContext(ctx, source)
}

// SeenAllSources returns true if seenSources contains all sources in the
// config, and also this config has received a SET message from each source.
// PodConfig是否已经看到过所有的源
func (c *PodConfig) SeenAllSources(seenSources sets.String) bool {
	if c.pods == nil {
		return false
	}
	c.sourcesLock.Lock()
	defer c.sourcesLock.Unlock()
	klog.V(5).InfoS("Looking for sources, have seen", "sources", c.sources.List(), "seenSources", seenSources)
	return seenSources.HasAll(c.sources.List()...) && c.pods.seenSources(c.sources.List()...)
}

// Updates returns a channel of updates to the configuration, properly denormalized.
// 对外暴露Pod的变更，外部组件可以监听当前Pod的变更
func (c *PodConfig) Updates() <-chan kubetypes.PodUpdate {
	return c.updates
}

// Sync requests the full configuration be delivered to the update channel.
// 用于重置当前所有的Pod
func (c *PodConfig) Sync() {
	c.pods.Sync()
}

// podStorage manages the current pod state at any point in time and ensures updates
// to the channel are delivered in order.  Note that this object is an in-memory source of
// "truth" and on creation contains zero entries.  Once all previously read sources are
// available, then this object should be considered authoritative.
type podStorage struct {
	podLock sync.RWMutex
	// map of source name to pod uid to pod reference
	// Pod存储，第一级Key为Source，即当前Pod来自于那里，目前主要有三种数据来源，分别是：HTTP，StaticPod, APIServer
	pods map[string]map[types.UID]*v1.Pod
	// TODO 如何理解这个属性？
	mode PodConfigNotificationMode

	// ensures that updates are delivered in strict order
	// on the updates channel
	updateLock sync.Mutex
	// 来自于HTTP, File, APIServer的Pod变更会被放入到这个channel当中
	updates chan<- kubetypes.PodUpdate

	// contains the set of all sources that have sent at least one SET
	sourcesSeenLock sync.RWMutex
	// 当前K8S有且仅支持HTTP, PodStatic, APIServer三种来源
	sourcesSeen sets.String

	// the EventRecorder to use
	recorder record.EventRecorder

	startupSLIObserver podStartupSLIObserver
}

// TODO: PodConfigNotificationMode could be handled by a listener to the updates channel
// in the future, especially with multiple listeners.
// TODO: allow initialization of the current state of the store with snapshotted version.
func newPodStorage(
	updates chan<- kubetypes.PodUpdate,
	mode PodConfigNotificationMode,
	recorder record.EventRecorder,
	startupSLIObserver podStartupSLIObserver,
) *podStorage {
	return &podStorage{
		pods:               make(map[string]map[types.UID]*v1.Pod),
		mode:               mode,
		updates:            updates,
		sourcesSeen:        sets.String{},
		recorder:           recorder,
		startupSLIObserver: startupSLIObserver,
	}
}

// Merge normalizes a set of incoming changes from different sources into a map of all Pods
// and ensures that redundant changes are filtered out, and then pushes zero or more minimal
// updates onto the update channel.  Ensures that updates are delivered in order.
// 1、change参数实际上就是一个Pod的变动，实际上就是PodUpdate类型
// 2、Merge的功能是把PodUpdate类型的数据分门别类整理处理再次分装为PodUpdate类型，然后放入到updates channel当中
func (s *podStorage) Merge(source string, change interface{}) error {
	s.updateLock.Lock()
	defer s.updateLock.Unlock()

	// 判断之前是否看见过这个来源
	seenBefore := s.sourcesSeen.Has(source)
	// 主要目的是为了对比当前源已经存在的pod和当前变更的pod，从而得出需要对Pod进行的操作
	adds, updates, deletes, removes, reconciles := s.merge(source, change)
	// 如果之前没有处理过，并且经过merger以后，就处理过这个来源的Pod变更，说明是第一次处理这个来源的Pod变更
	firstSet := !seenBefore && s.sourcesSeen.Has(source)

	// deliver update notifications
	switch s.mode {
	case PodConfigNotificationIncremental:
		if len(removes.Pods) > 0 {
			s.updates <- *removes
		}
		if len(adds.Pods) > 0 {
			s.updates <- *adds
		}
		if len(updates.Pods) > 0 {
			s.updates <- *updates
		}
		if len(deletes.Pods) > 0 {
			s.updates <- *deletes
		}
		if firstSet && len(adds.Pods) == 0 && len(updates.Pods) == 0 && len(deletes.Pods) == 0 {
			// Send an empty update when first seeing the source and there are
			// no ADD or UPDATE or DELETE pods from the source. This signals kubelet that
			// the source is ready.
			s.updates <- *adds
		}
		// Only add reconcile support here, because kubelet doesn't support Snapshot update now.
		if len(reconciles.Pods) > 0 {
			s.updates <- *reconciles
		}

	case PodConfigNotificationSnapshotAndUpdates:
		if len(removes.Pods) > 0 || len(adds.Pods) > 0 || firstSet {
			s.updates <- kubetypes.PodUpdate{Pods: s.MergedState().([]*v1.Pod), Op: kubetypes.SET, Source: source}
		}
		if len(updates.Pods) > 0 {
			s.updates <- *updates
		}
		if len(deletes.Pods) > 0 {
			s.updates <- *deletes
		}

	case PodConfigNotificationSnapshot:
		if len(updates.Pods) > 0 || len(deletes.Pods) > 0 || len(adds.Pods) > 0 || len(removes.Pods) > 0 || firstSet {
			s.updates <- kubetypes.PodUpdate{Pods: s.MergedState().([]*v1.Pod), Op: kubetypes.SET, Source: source}
		}

	case PodConfigNotificationUnknown:
		fallthrough
	default:
		panic(fmt.Sprintf("unsupported PodConfigNotificationMode: %#v", s.mode))
	}

	return nil
}

// 1、source为Pod不同的来源，目前支持HTTP, File, APIServer
// 2、change的数据类型为PodUpdate
// 3、主要目的是为了对比当前源已经存在的pod和当前变更的pod，从而得出需要对Pod进行的操作
func (s *podStorage) merge(source string, change interface{}) (adds, updates, deletes, removes, reconciles *kubetypes.PodUpdate) {
	s.podLock.Lock()
	defer s.podLock.Unlock()

	var addPods []*v1.Pod       // 当前Pod变更有哪些Pod是新增？
	var updatePods []*v1.Pod    // 当前Pod变更有哪些Pod是更新？
	var deletePods []*v1.Pod    // 当前Pod变更有哪些Pod是删除？
	var removePods []*v1.Pod    // 当前Pod变更有哪些Pod是删除？ 和DeletePod有啥区别？
	var reconcilePods []*v1.Pod // 当前Pod变更有哪些Pod是新增？

	// 获取当前来源的所有Pod
	pods := s.pods[source]
	if pods == nil {
		pods = make(map[types.UID]*v1.Pod)
	}

	// updatePodFunc is the local function which updates the pod cache *oldPods* with new pods *newPods*.
	// After updated, new pod will be stored in the pod cache *pods*.
	// Notice that *pods* and *oldPods* could be the same cache.
	// 第一个参数为新变更的Pod,第二个参数为以前的pod，第三个参数为某个来源的所有pod缓存
	updatePodsFunc := func(newPods []*v1.Pod, oldPods, pods map[types.UID]*v1.Pod) {
		// 1、过滤掉重复的pod，所谓重复的Pod指的是FullName相同的Pod，FullName=<name>_<namespace>
		// 2、因为是Pod的变更，所以对于同一个Pod多次变更，我们只需要保留一个Pod变更记录即可
		filtered := filterInvalidPods(newPods, source, s.recorder)
		for _, ref := range filtered {
			// Annotate the pod with the source before any comparison.
			if ref.Annotations == nil {
				ref.Annotations = make(map[string]string)
			}
			// 给Pod新增kubernetes.io/config.source=source的注解，标记当前Pod来自于哪里
			ref.Annotations[kubetypes.ConfigSourceAnnotationKey] = source
			// ignore static pods
			// 1、来自于File, HTTP源的Pod被称之为StaticPod，来自于APIServer的Pod被称之为非StaticPod
			// 2、给这种类型的Pod增加一个时间，后便后续统计Pod的启动时间
			if !kubetypes.IsStaticPod(ref) {
				// TODO 分析这里
				s.startupSLIObserver.ObservedPodOnWatch(ref, time.Now())
			}

			// 如果当前缓存中已经存在这个pod
			if existing, found := oldPods[ref.UID]; found {
				// 更新缓存
				pods[ref.UID] = existing
				// 1、判断两个Pod是否在语义上相同，所谓语义相同，即pod的spec, label, deletionTimestamp, deletionGracePeriodSeconds, annotation相同，
				// 其余字段不需要关心，只要其中的任意一个字段不同，我们就认为这两个Pod不同
				// 2、如果Pod在语义上是相同的，并且状态也一样，说明Pod没有发生任何变化，不需要做任何操作。
				// 3、如果Pod在语义上是相同的，但是状态不一样，那么这个Pod需要做reconcile
				// 4、如果Pod在语义上是不同的，并且DeletionTimestamp为非空，说明Pod需要删除
				// 5、如果Pod在语义上是不同的，并且DeletionTimestamp为空，说明Pod需要更新
				needUpdate, needReconcile, needGracefulDelete := checkAndUpdatePod(existing, ref)
				if needUpdate {
					updatePods = append(updatePods, existing)
				} else if needReconcile {
					reconcilePods = append(reconcilePods, existing)
				} else if needGracefulDelete {
					deletePods = append(deletePods, existing)
				}
				continue
			}
			// 如果当前pod是第一次被kubelet观测到，那么需要记录第一次被kubelet观测到的时间
			recordFirstSeenTime(ref)
			// 缓存记录这个pod
			pods[ref.UID] = ref
			addPods = append(addPods, ref)
		}
	}

	update := change.(kubetypes.PodUpdate)
	switch update.Op {
	case kubetypes.ADD, kubetypes.UPDATE, kubetypes.DELETE:
		if update.Op == kubetypes.ADD {
			klog.V(4).InfoS("Adding new pods from source", "source", source, "pods", klog.KObjSlice(update.Pods))
		} else if update.Op == kubetypes.DELETE {
			klog.V(4).InfoS("Gracefully deleting pods from source", "source", source, "pods", klog.KObjSlice(update.Pods))
		} else {
			klog.V(4).InfoS("Updating pods from source", "source", source, "pods", klog.KObjSlice(update.Pods))
		}
		updatePodsFunc(update.Pods, pods, pods)

	case kubetypes.REMOVE:
		klog.V(4).InfoS("Removing pods from source", "source", source, "pods", klog.KObjSlice(update.Pods))
		for _, value := range update.Pods {
			// 从缓存中移除这个pod
			if existing, found := pods[value.UID]; found {
				// this is a delete
				delete(pods, value.UID)
				removePods = append(removePods, existing)
				continue
			}
			// this is a no-op
		}

	case kubetypes.SET:
		klog.V(4).InfoS("Setting pods for source", "source", source)
		s.markSourceSet(source)
		// Clear the old map entries by just creating a new map
		oldPods := pods
		pods = make(map[types.UID]*v1.Pod)
		updatePodsFunc(update.Pods, oldPods, pods)
		for uid, existing := range oldPods {
			if _, found := pods[uid]; !found {
				// this is a delete
				removePods = append(removePods, existing)
			}
		}

	default:
		klog.InfoS("Received invalid update type", "type", update)

	}

	// 更新当前source的pod缓存
	s.pods[source] = pods

	adds = &kubetypes.PodUpdate{Op: kubetypes.ADD, Pods: copyPods(addPods), Source: source}
	updates = &kubetypes.PodUpdate{Op: kubetypes.UPDATE, Pods: copyPods(updatePods), Source: source}
	deletes = &kubetypes.PodUpdate{Op: kubetypes.DELETE, Pods: copyPods(deletePods), Source: source}
	removes = &kubetypes.PodUpdate{Op: kubetypes.REMOVE, Pods: copyPods(removePods), Source: source}
	reconciles = &kubetypes.PodUpdate{Op: kubetypes.RECONCILE, Pods: copyPods(reconcilePods), Source: source}

	return adds, updates, deletes, removes, reconciles
}

func (s *podStorage) markSourceSet(source string) {
	s.sourcesSeenLock.Lock()
	defer s.sourcesSeenLock.Unlock()
	s.sourcesSeen.Insert(source)
}

func (s *podStorage) seenSources(sources ...string) bool {
	s.sourcesSeenLock.RLock()
	defer s.sourcesSeenLock.RUnlock()
	return s.sourcesSeen.HasAll(sources...)
}

// 过滤掉重复的pod，所谓重复的Pod指的是FullName相同的Pod，FullName=<name>_<namespace>
func filterInvalidPods(pods []*v1.Pod, source string, recorder record.EventRecorder) (filtered []*v1.Pod) {
	names := sets.String{}
	for i, pod := range pods {
		// Pods from each source are assumed to have passed validation individually.
		// This function only checks if there is any naming conflict.
		// TODO 这里是否需要考虑对于Pod按照事件排序，最新的Pod应该在前面，这样我们保留的就是最新的Pod，如果不排序可能保存的不是最新的Pod
		name := kubecontainer.GetPodFullName(pod)
		if names.Has(name) {
			klog.InfoS("Pod failed validation due to duplicate pod name, ignoring", "index", i, "pod", klog.KObj(pod), "source", source)
			recorder.Eventf(pod, v1.EventTypeWarning, events.FailedValidation, "Error validating pod %s from %s due to duplicate pod name %q, ignoring", format.Pod(pod), source, pod.Name)
			continue
		} else {
			names.Insert(name)
		}

		filtered = append(filtered, pod)
	}
	return
}

// Annotations that the kubelet adds to the pod.
var localAnnotations = []string{
	kubetypes.ConfigSourceAnnotationKey,
	kubetypes.ConfigMirrorAnnotationKey,
	kubetypes.ConfigFirstSeenAnnotationKey,
}

func isLocalAnnotationKey(key string) bool {
	for _, localKey := range localAnnotations {
		if key == localKey {
			return true
		}
	}
	return false
}

// isAnnotationMapEqual returns true if the existing annotation Map is equal to candidate except
// for local annotations.
func isAnnotationMapEqual(existingMap, candidateMap map[string]string) bool {
	if candidateMap == nil {
		candidateMap = make(map[string]string)
	}
	for k, v := range candidateMap {
		if isLocalAnnotationKey(k) {
			continue
		}
		if existingValue, ok := existingMap[k]; ok && existingValue == v {
			continue
		}
		return false
	}
	for k := range existingMap {
		if isLocalAnnotationKey(k) {
			continue
		}
		// stale entry in existing map.
		if _, exists := candidateMap[k]; !exists {
			return false
		}
	}
	return true
}

// recordFirstSeenTime records the first seen time of this pod.
func recordFirstSeenTime(pod *v1.Pod) {
	klog.V(4).InfoS("Receiving a new pod", "pod", klog.KObj(pod))
	pod.Annotations[kubetypes.ConfigFirstSeenAnnotationKey] = kubetypes.NewTimestamp().GetString()
}

// updateAnnotations returns an Annotation map containing the api annotation map plus
// locally managed annotations
func updateAnnotations(existing, ref *v1.Pod) {
	annotations := make(map[string]string, len(ref.Annotations)+len(localAnnotations))
	for k, v := range ref.Annotations {
		annotations[k] = v
	}
	for _, k := range localAnnotations {
		if v, ok := existing.Annotations[k]; ok {
			annotations[k] = v
		}
	}
	existing.Annotations = annotations
}

// 判断两个Pod是否在语义上相同，所谓语义相同，即pod的spec, label, deletionTimestamp, deletionGracePeriodSeconds, annotation相同，其余字段不需要关心
// 只要其中的任意一个字段不同，我们就认为这两个Pod不同
func podsDifferSemantically(existing, ref *v1.Pod) bool {
	if reflect.DeepEqual(existing.Spec, ref.Spec) &&
		reflect.DeepEqual(existing.Labels, ref.Labels) &&
		reflect.DeepEqual(existing.DeletionTimestamp, ref.DeletionTimestamp) &&
		reflect.DeepEqual(existing.DeletionGracePeriodSeconds, ref.DeletionGracePeriodSeconds) &&
		isAnnotationMapEqual(existing.Annotations, ref.Annotations) {
		return false
	}
	return true
}

// checkAndUpdatePod updates existing, and:
//   - if ref makes a meaningful change, returns needUpdate=true
//   - if ref makes a meaningful change, and this change is graceful deletion, returns needGracefulDelete=true
//   - if ref makes no meaningful change, but changes the pod status, returns needReconcile=true
//   - else return all false
//     Now, needUpdate, needGracefulDelete and needReconcile should never be both true
//
// 1、判断两个Pod是否在语义上相同，所谓语义相同，即pod的spec, label, deletionTimestamp, deletionGracePeriodSeconds, annotation相同，
// 其余字段不需要关心，只要其中的任意一个字段不同，我们就认为这两个Pod不同
// 2、如果Pod在语义上是相同的，并且状态也一样，说明Pod没有发生任何变化，不需要做任何操作。
// 3、如果Pod在语义上是相同的，但是状态不一样，那么这个Pod需要做reconcile
// 4、如果Pod在语义上是不同的，并且DeletionTimestamp为非空，说明Pod需要删除
// 5、如果Pod在语义上是不同的，并且DeletionTimestamp为空，说明Pod需要更新
func checkAndUpdatePod(existing, ref *v1.Pod) (needUpdate, needReconcile, needGracefulDelete bool) {

	// 1. this is a reconcile
	// TODO: it would be better to update the whole object and only preserve certain things
	//       like the source annotation or the UID (to ensure safety)
	// 判断两个Pod是否在语义上相同，所谓语义相同，即pod的spec, label, deletionTimestamp, deletionGracePeriodSeconds, annotation相同，其余字段不需要关心
	// 只要其中的任意一个字段不同，我们就认为这两个Pod不同
	if !podsDifferSemantically(existing, ref) {
		// this is not an update
		// Only check reconcile when it is not an update, because if the pod is going to
		// be updated, an extra reconcile is unnecessary
		// 如果两个Pod语义相同，那么我们需要判断这两个Pod的状态是相同，如果状态不同，那么这个Pod需要Reconcile
		if !reflect.DeepEqual(existing.Status, ref.Status) {
			// Pod with changed pod status needs reconcile, because kubelet should
			// be the source of truth of pod status.
			existing.Status = ref.Status
			needReconcile = true
		}
		return
	}

	// Overwrite the first-seen time with the existing one. This is our own
	// internal annotation, there is no need to update.
	// 保存当前Pod第一次被看到的时间
	ref.Annotations[kubetypes.ConfigFirstSeenAnnotationKey] = existing.Annotations[kubetypes.ConfigFirstSeenAnnotationKey]

	// 由于Pod在语义上已经不等了，那么这个Pod一定发生了改变
	existing.Spec = ref.Spec
	existing.Labels = ref.Labels
	existing.DeletionTimestamp = ref.DeletionTimestamp
	existing.DeletionGracePeriodSeconds = ref.DeletionGracePeriodSeconds
	existing.Status = ref.Status
	updateAnnotations(existing, ref)

	// 2. this is an graceful delete
	if ref.DeletionTimestamp != nil {
		// 说明Pod被删除了
		needGracefulDelete = true
	} else {
		// 3. this is an update
		needUpdate = true
	}

	return
}

// Sync sends a copy of the current state through the update channel.
// 用于重置当前所有的Pod
func (s *podStorage) Sync() {
	s.updateLock.Lock()
	defer s.updateLock.Unlock()
	s.updates <- kubetypes.PodUpdate{Pods: s.MergedState().([]*v1.Pod), Op: kubetypes.SET, Source: kubetypes.AllSource}
}

// MergedState Object implements config.Accessor
// 获取PodStorage当前缓存的所有源的所有Pod
func (s *podStorage) MergedState() interface{} {
	s.podLock.RLock()
	defer s.podLock.RUnlock()
	pods := make([]*v1.Pod, 0)
	for _, sourcePods := range s.pods {
		for _, podRef := range sourcePods {
			pods = append(pods, podRef.DeepCopy())
		}
	}
	return pods
}

func copyPods(sourcePods []*v1.Pod) []*v1.Pod {
	var pods []*v1.Pod
	for _, source := range sourcePods {
		// Use a deep copy here just in case
		pods = append(pods, source.DeepCopy())
	}
	return pods
}
