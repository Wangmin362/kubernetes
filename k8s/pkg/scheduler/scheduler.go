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
	// TODO 缓存的是啥？
	Cache internalcache.Cache

	Extenders []framework.Extender

	// NextPod should be a function that blocks until the next pod
	// is available. We don't use a channel for this, because scheduling
	// a pod may take some amount of time and we don't want pods to get
	// stale while they sit in a channel.
	NextPod func() *framework.QueuedPodInfo

	// FailureHandler is called upon a scheduling failure.
	FailureHandler FailureHandlerFn

	// SchedulePod tries to schedule the given pod to one of the nodes in the node list.
	// Return a struct of ScheduleResult with the name of suggested host on success,
	// otherwise will return a FitError with reasons.
	SchedulePod func(ctx context.Context, fwk framework.Framework, state *framework.CycleState, pod *v1.Pod) (ScheduleResult, error)

	// Close this to shut down the scheduler.
	StopEverything <-chan struct{}

	// SchedulingQueue holds pods to be scheduled
	SchedulingQueue internalqueue.SchedulingQueue

	// Profiles are the scheduling profiles.
	// 用于记录当前支持的调度框架，key为调度框架的名字（用户通过pod.Spec.SchedulerName指定使用哪个调度框架），value为调度框架
	Profiles profile.Map

	client clientset.Interface

	// 1、K8S调度Pod的最终结果就是找到一个合适的Node，然后在这个Node上创建Pod
	// 2、这里的Node快照就是保存了当前K8S所有的node信息，其中主要记录了当前一共有几个可用的Node，每个Node的具体信息、每个Node之上都运行了
	// 哪些Pod，Node之上运行的Pod的亲和性、反亲和性、每个Node的资源使用情况等等
	nodeInfoSnapshot *internalcache.Snapshot

	// TODO 一个Pod只需要调度到一个Node之上运行就可以了，但是如果有5000个Node，经过筛选，发现有4500个节点可以使用，此时我们不需要把4500
	// 个所有可用的节点都计算一次得分，而是计算其中一小部分就可以，譬如取其中的100个Node来计算得分。这里的百分比就是干这个事情的，在所有可用的
	// Node节点中，选取一定百分比的Node节点用于排分计算
	percentageOfNodesToScore int32

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

	// TODO 这里缓存的是啥？
	schedulerCache := internalcache.New(durationToExpireAssumedPod, stopEverything)

	// Setup cache debugger.
	// TODO CacheDebugger是如何支持Debug的
	debugger := cachedebugger.New(nodeLister, podLister, schedulerCache, podQueue)
	debugger.ListenForSignal(stopEverything)

	// TODO 实例化Scheduler, 重点分析schedulePod
	sched := newScheduler(
		schedulerCache, // TODO 暂时不清楚这玩意干嘛的
		extenders,
		internalqueue.MakeNextPodFunc(podQueue), // 用于获取下一个需要调度的Pod
		stopEverything,                          // 停止Channel
		podQueue,                                // 是一个优先级队列
		profiles,                                // 实际上就是调度框架，也就是我们所谓的调度器
		client,                                  // ClientSet
		snapshot,                                // TODO 似乎是缓存了Node信息
		options.percentageOfNodesToScore,        // 每次调度一个Pod不需要找到所有可用的Node，而是找到其中的一部分就可以了
	)

	// TODO 监听Pod, Node的增删改查
	addAllEventHandlers(sched, informerFactory, dynInformerFactory, unionedGVKs(clusterEventMap))

	return sched, nil
}

// Run begins watching and scheduling. It starts scheduling and blocked until the context is done.
func (sched *Scheduler) Run(ctx context.Context) {
	// TODO 启动优先级队列
	sched.SchedulingQueue.Run()

	// We need to start scheduleOne loop in a dedicated goroutine,
	// because scheduleOne function hangs on getting the next item
	// from the SchedulingQueue.
	// If there are no new pods to schedule, it will be hanging there
	// and if done in this goroutine it will be blocking closing
	// SchedulingQueue, in effect causing a deadlock on shutdown.
	// 调度Pod
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
