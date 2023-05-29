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

package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	configv1 "k8s.io/kube-scheduler/config/v1"
	schedulerapi "k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/scheme"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/parallelize"
	frameworkplugins "k8s.io/kubernetes/pkg/scheduler/framework/plugins"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	internalcache "k8s.io/kubernetes/pkg/scheduler/internal/cache"
	cachedebugger "k8s.io/kubernetes/pkg/scheduler/internal/cache/debugger"
	internalqueue "k8s.io/kubernetes/pkg/scheduler/internal/queue"
	"k8s.io/kubernetes/pkg/scheduler/metrics"
	"k8s.io/kubernetes/pkg/scheduler/profile"
)

const (
	// Duration the scheduler will wait before expiring an assumed pod.
	// See issue #106361 for more details about this parameter and its value.
	durationToExpireAssumedPod time.Duration = 0
)

// ErrNoNodesAvailable is used to describe the error that no nodes available to schedule pods.
var ErrNoNodesAvailable = fmt.Errorf("no nodes available to schedule pods")

// Scheduler watches for new unscheduled pods. It attempts to find
// nodes that they fit on and writes bindings back to the api server.
// TODO 如何理解这个结构体的抽象？
type Scheduler struct {
	// It is expected that changes made via Cache will be observed
	// by NodeLister and Algorithm.
	// 1、Cache中缓存了已经调度的Pod
	// 2、Cache中缓存了Node的信息，以及Node中每个Pod的亲和性、反亲和性、Node已经使用的端口、分配的资源以及请求的资源情况
	Cache internalcache.Cache

	// TODO K8S是如何使用Extender的？
	Extenders []framework.Extender

	// NextPod should be a function that blocks until the next pod
	// is available. We don't use a channel for this, because scheduling
	// a pod may take some amount of time and we don't want pods to get
	// stale while they sit in a channel.
	// 用于获取下一个需要调度的Pod,实际上就是从SchedulerQueue中的activeQ中获取
	NextPod func() *framework.QueuedPodInfo

	// FailureHandler is called upon a scheduling failure.
	// 当Pod在调度失败时，需要调用此方法
	FailureHandler FailureHandlerFn

	// SchedulePod tries to schedule the given pod to one of the nodes in the node list.
	// Return a struct of ScheduleResult with the name of suggested host on success,
	// otherwise will return a FitError with reasons.
	// 调度一个Pod
	SchedulePod func(ctx context.Context, fwk framework.Framework, state *framework.CycleState, pod *v1.Pod) (ScheduleResult, error)

	// Close this to shut down the scheduler.
	StopEverything <-chan struct{}

	// SchedulingQueue holds pods to be scheduled
	// 1、SchedulingQueue缓存的Pod都是还未调度的Pod
	SchedulingQueue internalqueue.SchedulingQueue

	// Profiles are the scheduling profiles.
	// 用于记录当前支持的调度框架，key为调度框架的名字（用户通过pod.Spec.SchedulerName指定使用哪个调度框架），value为调度框架
	Profiles profile.Map

	// ClientSet
	client clientset.Interface

	// 1、K8S调度Pod的最终结果就是找到一个合适的Node，然后在这个Node上创建Pod
	// 2、这里的Node快照就是保存了当前K8S所有的node信息，其中主要记录了当前一共有几个可用的Node，每个Node的具体信息、每个Node之上都运行了
	// 哪些Pod，Node之上运行的Pod的亲和性、反亲和性、每个Node的资源使用情况等等
	nodeInfoSnapshot *internalcache.Snapshot

	// 一个Pod只需要调度到一个Node之上运行就可以了，但是如果有5000个Node，经过筛选，发现有4500个节点可以使用，此时我们不需要把4500
	// 个所有可用的节点都计算一次得分，而是计算其中一小部分就可以，譬如取其中的100个Node来计算得分。这里的百分比就是干这个事情的，在所有可用的
	// Node节点中，选取一定百分比的Node节点用于排分计算
	percentageOfNodesToScore int32

	// TODO 这玩意干嘛的？
	nextStartNodeIndex int
}

type schedulerOptions struct {
	componentConfigVersion            string
	kubeConfig                        *restclient.Config
	percentageOfNodesToScore          int32
	podInitialBackoffSeconds          int64
	podMaxBackoffSeconds              int64
	podMaxInUnschedulablePodsDuration time.Duration
	// Contains out-of-tree plugins to be merged with the in-tree registry.
	frameworkOutOfTreeRegistry frameworkruntime.Registry
	profiles                   []schedulerapi.KubeSchedulerProfile
	// TODO 这个参数的作用
	extenders         []schedulerapi.Extender
	frameworkCapturer FrameworkCapturer
	parallelism       int32
	// 是否开启使用K8S默认的调度器
	applyDefaultProfile bool
}

// Option configures a Scheduler
// 这种写法，外部无法实现这个类型的变量，因为schedulerOptions是私有的
type Option func(*schedulerOptions)

// ScheduleResult represents the result of scheduling a pod.
type ScheduleResult struct {
	// Name of the selected node.
	SuggestedHost string
	// The number of nodes the scheduler evaluated the pod against in the filtering
	// phase and beyond.
	EvaluatedNodes int
	// The number of nodes out of the evaluated ones that fit the pod.
	FeasibleNodes int
}

// WithComponentConfigVersion sets the component config version to the
// KubeSchedulerConfiguration version used. The string should be the full
// scheme group/version of the external type we converted from (for example
// "kubescheduler.config.k8s.io/v1")
func WithComponentConfigVersion(apiVersion string) Option {
	return func(o *schedulerOptions) {
		o.componentConfigVersion = apiVersion
	}
}

// WithKubeConfig sets the kube config for Scheduler.
func WithKubeConfig(cfg *restclient.Config) Option {
	return func(o *schedulerOptions) {
		o.kubeConfig = cfg
	}
}

// WithProfiles sets profiles for Scheduler. By default, there is one profile
// with the name "default-scheduler".
func WithProfiles(p ...schedulerapi.KubeSchedulerProfile) Option {
	return func(o *schedulerOptions) {
		o.profiles = p
		o.applyDefaultProfile = false
	}
}

// WithParallelism sets the parallelism for all scheduler algorithms. Default is 16.
func WithParallelism(threads int32) Option {
	return func(o *schedulerOptions) {
		o.parallelism = threads
	}
}

// WithPercentageOfNodesToScore sets percentageOfNodesToScore for Scheduler, the default value is 50
func WithPercentageOfNodesToScore(percentageOfNodesToScore int32) Option {
	return func(o *schedulerOptions) {
		o.percentageOfNodesToScore = percentageOfNodesToScore
	}
}

// WithFrameworkOutOfTreeRegistry sets the registry for out-of-tree plugins. Those plugins
// will be appended to the default registry.
func WithFrameworkOutOfTreeRegistry(registry frameworkruntime.Registry) Option {
	return func(o *schedulerOptions) {
		o.frameworkOutOfTreeRegistry = registry
	}
}

// WithPodInitialBackoffSeconds sets podInitialBackoffSeconds for Scheduler, the default value is 1
func WithPodInitialBackoffSeconds(podInitialBackoffSeconds int64) Option {
	return func(o *schedulerOptions) {
		o.podInitialBackoffSeconds = podInitialBackoffSeconds
	}
}

// WithPodMaxBackoffSeconds sets podMaxBackoffSeconds for Scheduler, the default value is 10
func WithPodMaxBackoffSeconds(podMaxBackoffSeconds int64) Option {
	return func(o *schedulerOptions) {
		o.podMaxBackoffSeconds = podMaxBackoffSeconds
	}
}

// WithPodMaxInUnschedulablePodsDuration sets podMaxInUnschedulablePodsDuration for PriorityQueue.
func WithPodMaxInUnschedulablePodsDuration(duration time.Duration) Option {
	return func(o *schedulerOptions) {
		o.podMaxInUnschedulablePodsDuration = duration
	}
}

// WithExtenders sets extenders for the Scheduler
func WithExtenders(e ...schedulerapi.Extender) Option {
	return func(o *schedulerOptions) {
		o.extenders = e
	}
}

// FrameworkCapturer is used for registering a notify function in building framework.
type FrameworkCapturer func(schedulerapi.KubeSchedulerProfile)

// WithBuildFrameworkCapturer sets a notify function for getting buildFramework details.
func WithBuildFrameworkCapturer(fc FrameworkCapturer) Option {
	return func(o *schedulerOptions) {
		o.frameworkCapturer = fc
	}
}

var defaultSchedulerOptions = schedulerOptions{
	percentageOfNodesToScore:          schedulerapi.DefaultPercentageOfNodesToScore,
	podInitialBackoffSeconds:          int64(internalqueue.DefaultPodInitialBackoffDuration.Seconds()),
	podMaxBackoffSeconds:              int64(internalqueue.DefaultPodMaxBackoffDuration.Seconds()),
	podMaxInUnschedulablePodsDuration: internalqueue.DefaultPodMaxInUnschedulablePodsDuration,
	parallelism:                       int32(parallelize.DefaultParallelism),
	// Ideally we would statically set the default profile here, but we can't because
	// creating the default profile may require testing feature gates, which may get
	// set dynamically in tests. Therefore, we delay creating it until New is actually
	// invoked.
	// 默认使用K8S内部实现的调度器
	applyDefaultProfile: true,
}

// New returns a Scheduler
func New(client clientset.Interface,
	informerFactory informers.SharedInformerFactory,
	dynInformerFactory dynamicinformer.DynamicSharedInformerFactory,
	recorderFactory profile.RecorderFactory,
	stopCh <-chan struct{},
	opts ...Option) (*Scheduler, error) {

	stopEverything := stopCh
	if stopEverything == nil {
		stopEverything = wait.NeverStop
	}

	// 默认参数开启了使用K8S内部实现的调度器
	options := defaultSchedulerOptions
	// 修改Scheduler配置参数
	for _, opt := range opts {
		opt(&options)
	}

	// TODO 这里一定是创建了默认调度器的配置
	if options.applyDefaultProfile {
		var versionedCfg configv1.KubeSchedulerConfiguration
		scheme.Scheme.Default(&versionedCfg)
		cfg := schedulerapi.KubeSchedulerConfiguration{}
		if err := scheme.Scheme.Convert(&versionedCfg, &cfg, nil); err != nil {
			return nil, err
		}
		options.profiles = cfg.Profiles
	}

	// InTree调度插件为K8S内部定义的插件，而OutOf调度插件为用户自定义开发的插件
	registry := frameworkplugins.NewInTreeRegistry()
	// 合并外部注册的插件和内部的插件，实际上就是合并两个Map，同时检测有没有同名的插件，如果有同名插件，就直接报错，认为插件注册冲突
	if err := registry.Merge(options.frameworkOutOfTreeRegistry); err != nil {
		return nil, err
	}

	// 注册指标
	metrics.Register()

	// TODO 详细分析
	extenders, err := buildExtenders(options.extenders, options.profiles)
	if err != nil {
		return nil, fmt.Errorf("couldn't build extenders: %w", err)
	}

	// Scheduler核心目的就是为了调度Pod
	podLister := informerFactory.Core().V1().Pods().Lister()
	// Node是Pod的载体，因此需要知道当前到底有多少Node，以及每个Node的资源情况
	nodeLister := informerFactory.Core().V1().Nodes().Lister()

	// The nominator will be passed all the way to framework instantiation.
	nominator := internalqueue.NewPodNominator(podLister)
	// 用于获取Node相关的信息以及PVC相关信息
	snapshot := internalcache.NewEmptySnapshot()
	// TODO 这玩意干嘛的？
	clusterEventMap := make(map[framework.ClusterEvent]sets.String)

	// 1、K8S支持用户开发自己的调度器，然后通过pod.Spec.SchedulerName指定调度器的名字
	// 2、这里的Profile实际上就是一个Map,用户缓存K8S所有支持的调度器；key为调度器的名字，value为调度框架
	profiles, err := profile.NewMap(options.profiles, registry, recorderFactory, stopCh,
		frameworkruntime.WithComponentConfigVersion(options.componentConfigVersion),
		frameworkruntime.WithClientSet(client),
		frameworkruntime.WithKubeConfig(options.kubeConfig),
		frameworkruntime.WithInformerFactory(informerFactory),
		frameworkruntime.WithSnapshotSharedLister(snapshot),
		frameworkruntime.WithPodNominator(nominator),
		frameworkruntime.WithCaptureProfile(frameworkruntime.CaptureProfile(options.frameworkCapturer)),
		frameworkruntime.WithClusterEventMap(clusterEventMap),
		frameworkruntime.WithParallelism(int(options.parallelism)),
		frameworkruntime.WithExtenders(extenders),
	)
	if err != nil {
		return nil, fmt.Errorf("initializing profiles: %v", err)
	}
	// 如果一个调度器都没有创建出来，肯定是有问题的，因为无法调度Pod
	if len(profiles) == 0 {
		return nil, errors.New("at least one profile is required")
	}

	// TODO 如何理解这个队列？
	podQueue := internalqueue.NewSchedulingQueue(
		profiles[options.profiles[0].SchedulerName].QueueSortFunc(),
		informerFactory,
		internalqueue.WithPodInitialBackoffDuration(time.Duration(options.podInitialBackoffSeconds)*time.Second),
		internalqueue.WithPodMaxBackoffDuration(time.Duration(options.podMaxBackoffSeconds)*time.Second),
		internalqueue.WithPodNominator(nominator),
		internalqueue.WithClusterEventMap(clusterEventMap),
		internalqueue.WithPodMaxInUnschedulablePodsDuration(options.podMaxInUnschedulablePodsDuration),
	)

	// 1、schedulerCache主要缓存了已经调度的Pod信息还有AssumedPod信息，以及每个Node信息，其中包括每个Node都运行了那些Pod，亲和性、反亲和性是
	// 怎样的，资源申请、资源分配情况
	// 2、之所以记录这些资源，是因为调度Pod的时候需要考虑这些因素
	schedulerCache := internalcache.New(durationToExpireAssumedPod, stopEverything)

	// Setup cache debugger.
	// TODO CacheDebugger是如何支持Debug的
	debugger := cachedebugger.New(nodeLister, podLister, schedulerCache, podQueue)
	debugger.ListenForSignal(stopEverything)

	// TODO 实例化Scheduler,挨个调度Pod
	sched := newScheduler(
		schedulerCache,
		extenders,
		internalqueue.MakeNextPodFunc(podQueue), // 用于获取下一个需要调度的Pod
		stopEverything,                          // 停止Channel
		podQueue,                                // 是一个优先级队列
		profiles,                                // 实际上就是调度框架，也就是我们所谓的调度器
		client,                                  // ClientSet
		snapshot,                                // TODO 似乎是缓存了Node信息
		options.percentageOfNodesToScore,        // 每次调度一个Pod不需要找到所有可用的Node，而是找到其中的一部分就可以了
	)

	// 1、监听Pod资源
	// 1.1、监听未调度的Pod，维护SchedulerQueue(也就是PriorityQueue)
	// 1.2、监听已经成功调度的Pod，维护Cache，主要是更新每个Node上所有运行的Pod信息，包括资源、亲和性、反亲和性、端口分配
	// 2、监听Node资源，维护Node信息
	// 3、监听CSINode,CSIDriver,CSIStorageCapacity,PersistentVolume,PersistentVolumeClaim,StorageClass资源，因为在Pod调度
	// 过程中，可能会有由于PVC缺失导致调度失败，而这些资源的变动可能会导致那些调度失败的Pod可以重新调度
	addAllEventHandlers(sched, informerFactory, dynInformerFactory, unionedGVKs(clusterEventMap))

	return sched, nil
}

// Run begins watching and scheduling. It starts scheduling and blocked until the context is done.
func (sched *Scheduler) Run(ctx context.Context) {
	// 1、用于把PodBackoffQueue当中的pod元素放入到ActiveQ当中，当然，只有那些到了备份时间的Pod才会被放入到activeQ当中。由于PodBackoffQ
	// 是以堆的数据结构存储元素，而且是按照Pod的备份时间进行排序，因此只需要从堆顶判断是否已经到了备份时间，如果堆顶Pod都没有到备份时间，
	// 那么剩下的元素就不需要判断了。如果堆顶Pod到了备份时间，就把堆顶Pod弹出来放入到ActiveQ当中。同时，剩下的元素按照相同的逻辑进行判断。
	// 2、检测PodBackoffQ中的Pod元素是否需要进行备份放到activeQ中，每秒钟检测一次
	// 3、找到已经在UnschedulingPod缓存中待了超过五分钟的元素
	// 4、依次遍历这些元素，然后进行以下判断：
	// 3.1、如果当前Pod的UnschedulablePlugins不为空，并且不是因为长时间没有调度造成的，就直接跳过这个元素。因为如果是因为其它原因造成
	// 当前Pod无法调度，此时再去调度也无济于事，依然无法正常调度
	// 3.2、如果当前Pod还没有过备份时间，那就把当前Pod加入到PodBackoffQ当中，然后删除UnschedulablePlugin中的对应元素
	// 3.3、如果当前Pod已经过了备份时间，那就把当前Pod加入到ActiveQ当中，然后删除UnschedulablePlugin中的对应元素
	sched.SchedulingQueue.Run()

	// We need to start scheduleOne loop in a dedicated goroutine,
	// because scheduleOne function hangs on getting the next item
	// from the SchedulingQueue.
	// If there are no new pods to schedule, it will be hanging there
	// and if done in this goroutine it will be blocking closing
	// SchedulingQueue, in effect causing a deadlock on shutdown.
	// TODO 调度Pod
	go wait.UntilWithContext(ctx, sched.scheduleOne, 0)

	<-ctx.Done()
	sched.SchedulingQueue.Close()
}

// NewInformerFactory creates a SharedInformerFactory and initializes a scheduler specific
// in-place podInformer.
func NewInformerFactory(cs clientset.Interface, resyncPeriod time.Duration) informers.SharedInformerFactory {
	informerFactory := informers.NewSharedInformerFactory(cs, resyncPeriod)
	informerFactory.InformerFor(&v1.Pod{}, newPodInformer)
	return informerFactory
}

func buildExtenders(extenders []schedulerapi.Extender, profiles []schedulerapi.KubeSchedulerProfile) ([]framework.Extender, error) {
	var fExtenders []framework.Extender
	if len(extenders) == 0 {
		return nil, nil
	}

	var ignoredExtendedResources []string
	var ignorableExtenders []framework.Extender
	for i := range extenders {
		klog.V(2).InfoS("Creating extender", "extender", extenders[i])
		extender, err := NewHTTPExtender(&extenders[i])
		if err != nil {
			return nil, err
		}
		if !extender.IsIgnorable() {
			fExtenders = append(fExtenders, extender)
		} else {
			ignorableExtenders = append(ignorableExtenders, extender)
		}
		for _, r := range extenders[i].ManagedResources {
			if r.IgnoredByScheduler {
				ignoredExtendedResources = append(ignoredExtendedResources, r.Name)
			}
		}
	}
	// place ignorable extenders to the tail of extenders
	fExtenders = append(fExtenders, ignorableExtenders...)

	// If there are any extended resources found from the Extenders, append them to the pluginConfig for each profile.
	// This should only have an effect on ComponentConfig, where it is possible to configure Extenders and
	// plugin args (and in which case the extender ignored resources take precedence).
	if len(ignoredExtendedResources) == 0 {
		return fExtenders, nil
	}

	for i := range profiles {
		prof := &profiles[i]
		var found = false
		for k := range prof.PluginConfig {
			if prof.PluginConfig[k].Name == noderesources.Name {
				// Update the existing args
				pc := &prof.PluginConfig[k]
				args, ok := pc.Args.(*schedulerapi.NodeResourcesFitArgs)
				if !ok {
					return nil, fmt.Errorf("want args to be of type NodeResourcesFitArgs, got %T", pc.Args)
				}
				args.IgnoredResources = ignoredExtendedResources
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("can't find NodeResourcesFitArgs in plugin config")
		}
	}
	return fExtenders, nil
}

type FailureHandlerFn func(ctx context.Context, fwk framework.Framework, podInfo *framework.QueuedPodInfo, err error, reason string, nominatingInfo *framework.NominatingInfo)

// newScheduler creates a Scheduler object.
func newScheduler(
	cache internalcache.Cache,
	extenders []framework.Extender,
	nextPod func() *framework.QueuedPodInfo,
	stopEverything <-chan struct{},
	schedulingQueue internalqueue.SchedulingQueue,
	profiles profile.Map,
	client clientset.Interface,
	nodeInfoSnapshot *internalcache.Snapshot,
	percentageOfNodesToScore int32) *Scheduler {
	sched := Scheduler{
		Cache:                    cache,
		Extenders:                extenders,
		NextPod:                  nextPod,
		StopEverything:           stopEverything,
		SchedulingQueue:          schedulingQueue,
		Profiles:                 profiles,
		client:                   client,
		nodeInfoSnapshot:         nodeInfoSnapshot,
		percentageOfNodesToScore: percentageOfNodesToScore,
	}
	// TODO 一个Pod到底是如何调度的？有哪些决定因素？
	sched.SchedulePod = sched.schedulePod
	// TODO 如果一个Pod调度失败了，该如何处理？
	sched.FailureHandler = sched.handleSchedulingFailure
	return &sched
}

func unionedGVKs(m map[framework.ClusterEvent]sets.String) map[framework.GVK]framework.ActionType {
	gvkMap := make(map[framework.GVK]framework.ActionType)
	for evt := range m {
		if _, ok := gvkMap[evt.Resource]; ok {
			gvkMap[evt.Resource] |= evt.ActionType
		} else {
			gvkMap[evt.Resource] = evt.ActionType
		}
	}
	return gvkMap
}

// newPodInformer creates a shared index informer that returns only non-terminal pods.
// The PodInformer allows indexers to be added, but note that only non-conflict indexers are allowed.
func newPodInformer(cs clientset.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	selector := fmt.Sprintf("status.phase!=%v,status.phase!=%v", v1.PodSucceeded, v1.PodFailed)
	tweakListOptions := func(options *metav1.ListOptions) {
		options.FieldSelector = selector
	}
	return coreinformers.NewFilteredPodInformer(cs, metav1.NamespaceAll, resyncPeriod, cache.Indexers{}, tweakListOptions)
}
