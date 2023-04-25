/*
Copyright 2017 The Kubernetes Authors.

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

package autoregister

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"time"

	"k8s.io/klog/v2"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	v1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	apiregistrationclient "k8s.io/kube-aggregator/pkg/client/clientset_generated/clientset/typed/apiregistration/v1"
	informers "k8s.io/kube-aggregator/pkg/client/informers/externalversions/apiregistration/v1"
	listers "k8s.io/kube-aggregator/pkg/client/listers/apiregistration/v1"
	"k8s.io/kube-aggregator/pkg/controllers"
)

const (
	// AutoRegisterManagedLabel is a label attached to the APIService that identifies how the APIService wants to be synced.
	// TODO 这个注解有啥用？ 答：通过ApiServer以及ExtesionServer的API创建的APIService会打赏这个标签，值为onstart以及true其中的一个
	AutoRegisterManagedLabel = "kube-aggregator.kubernetes.io/automanaged"

	// manageOnStart is a value for the AutoRegisterManagedLabel that indicates the APIService wants to be synced one time when the controller starts.
	// TODO 通过APIServer的API生成的APIService会打上 kube-aggregator.kubernetes.io/automanaged=onstart的注解
	manageOnStart = "onstart"
	// manageContinuously is a value for the AutoRegisterManagedLabel that indicates the APIService wants to be synced continuously.
	// TODO 通过ExtensionServer的API生成的APIService会打上kube-aggregator.kubernetes.io/automanaged=true的注解
	manageContinuously = "true"
)

// AutoAPIServiceRegistration is an interface which callers can re-declare locally and properly cast to for
// adding and removing APIServices
type AutoAPIServiceRegistration interface {
	// AddAPIServiceToSyncOnStart adds an API service to sync on start.
	// TODO 看名字，这个接口应该是程序启动的时候就需要执行
	// TODO 从代码流程上来看，这个API应该是专门给APIServer用的，因为APIServer的API是固定的，只需要在刚开始的时候把所有的
	// TODO API转换为APIService放入到AutoAPIServiceRegistration即可
	AddAPIServiceToSyncOnStart(in *v1.APIService)

	// AddAPIServiceToSync adds an API service to sync continuously.
	// TODO 从代码执行的流程上来看，下面两个API是专门给ExtensionServer使用的，因为CRD是动态变化的，可能新增、可能删除、也可能修改
	// TODO 所以需要不停的去同步
	AddAPIServiceToSync(in *v1.APIService)
	// RemoveAPIServiceToSync removes an API service to auto-register.
	RemoveAPIServiceToSync(name string)
}

// autoRegisterController is used to keep a particular set of APIServices present in the API.  It is useful
// for cases where you want to auto-register APIs like TPRs or groups from the core kube-apiserver
type autoRegisterController struct {
	apiServiceLister listers.APIServiceLister
	apiServiceSynced cache.InformerSynced
	apiServiceClient apiregistrationclient.APIServicesGetter

	apiServicesToSyncLock sync.RWMutex
	// TODO 核心就是这里了 key为APIService的名字 这个属性中记录的是所有需要同步创建的APIService
	// TODO 这里的来源只有两种：1、来自于APIServer的API  2、来自于ExtensionServer的API
	apiServicesToSync map[string]*v1.APIService

	syncHandler func(apiServiceName string) error

	// track which services we have synced
	syncedSuccessfullyLock *sync.RWMutex
	// 判断APIService是否已经同步成功，key为APIService的名字
	// TODO Sync表达了什么含义？意思是路由注册成功？
	// 答：Sync表示已经成功创建了APIService, 并不表表示路由注册成功
	syncedSuccessfully map[string]bool

	// remember names of services that existed when we started
	// 记录aggregator server启动时就存在的APIService
	// TODO 这玩意有啥用？为啥要记录这个？
	apiServicesAtStart map[string]bool

	// queue is where incoming work is placed to de-dup and to allow "easy" rate limited requeues on errors
	// TODO 数据来源：
	// 1、APIServer，APIServer支持的每个路径都会包转为一个Service为nil的APIService
	// 2、ExtensionServer 每个CRD也会转换为Service属性为nil的APIService
	queue workqueue.RateLimitingInterface
}

// NewAutoRegisterController creates a new autoRegisterController.
// TODO 为啥这里不直接返回AutoAPIServiceRegistration接口？
func NewAutoRegisterController(apiServiceInformer informers.APIServiceInformer,
	apiServiceClient apiregistrationclient.APIServicesGetter) *autoRegisterController {
	c := &autoRegisterController{
		apiServiceLister:  apiServiceInformer.Lister(),
		apiServiceSynced:  apiServiceInformer.Informer().HasSynced,
		apiServiceClient:  apiServiceClient,
		apiServicesToSync: map[string]*v1.APIService{},

		apiServicesAtStart: map[string]bool{},

		syncedSuccessfullyLock: &sync.RWMutex{},
		syncedSuccessfully:     map[string]bool{},

		// 存放的是APIService
		queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "autoregister"),
	}
	c.syncHandler = c.checkAPIService

	// 监听APIService资源的变更
	apiServiceInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			cast := obj.(*v1.APIService)
			// APIService是Cluster级别的资源，直接放名字即可
			c.queue.Add(cast.Name)
		},
		// TODO 为什么这里又不需要关心oldObj
		UpdateFunc: func(_, obj interface{}) {
			cast := obj.(*v1.APIService)
			c.queue.Add(cast.Name)
		},
		DeleteFunc: func(obj interface{}) {
			cast, ok := obj.(*v1.APIService)
			if !ok {
				tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
				if !ok {
					klog.V(2).Infof("Couldn't get object from tombstone %#v", obj)
					return
				}
				cast, ok = tombstone.Obj.(*v1.APIService)
				if !ok {
					klog.V(2).Infof("Tombstone contained unexpected object: %#v", obj)
					return
				}
			}
			c.queue.Add(cast.Name)
		},
	})

	return c
}

// Run starts the autoregister controller in a loop which syncs API services until stopCh is closed.
func (c *autoRegisterController) Run(workers int, stopCh <-chan struct{}) {
	// don't let panics crash the process
	defer utilruntime.HandleCrash()
	// make sure the work queue is shutdown which will trigger workers to end
	defer c.queue.ShutDown()

	klog.Info("Starting autoregister controller")
	defer klog.Info("Shutting down autoregister controller")

	// wait for your secondary caches to fill before starting your work
	// 等待APIService同步完成
	if !controllers.WaitForCacheSync("autoregister", stopCh, c.apiServiceSynced) {
		return
	}

	// record APIService objects that existed when we started
	if services, err := c.apiServiceLister.List(labels.Everything()); err == nil {
		for _, service := range services {
			c.apiServicesAtStart[service.Name] = true
		}
	}

	// start up your worker threads based on workers.  Some controllers have multiple kinds of workers
	for i := 0; i < workers; i++ {
		// runWorker will loop until "something bad" happens.  The .Until will then rekick the worker
		// after one second
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	// wait until we're told to stop
	<-stopCh
}

func (c *autoRegisterController) runWorker() {
	// hot loop until we're told to stop.  processNextWorkItem will automatically wait until there's work
	// available, so we don't worry about secondary waits
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem deals with one key off the queue.  It returns false when it's time to quit.
func (c *autoRegisterController) processNextWorkItem() bool {
	// pull the next work item from queue.  It should be a key we use to lookup something in a cache
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	// you always have to indicate to the queue that you've completed a piece of work
	defer c.queue.Done(key)

	// do your work on the key.  This method will contains your "do stuff" logic
	err := c.syncHandler(key.(string))
	if err == nil {
		// if you had no error, tell the queue to stop tracking history for your key.  This will
		// reset things like failure counts for per-item rate limiting
		c.queue.Forget(key)
		return true
	}

	// there was a failure so be sure to report it.  This method allows for pluggable error handling
	// which can be used for things like cluster-monitoring
	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))
	// since we failed, we should requeue the item to work on later.  This method will add a backoff
	// to avoid hotlooping on particular items (they're probably still not going to work right away)
	// and overall controller protection (everything I've done is broken, this controller needs to
	// calm down or it can starve other useful work) cases.
	c.queue.AddRateLimited(key)

	return true
}

// checkAPIService syncs the current APIService against a list of desired APIService objects
//
//	| A. desired: not found | B. desired: sync on start | C. desired: sync always
//
// ------------------------------------------------|-----------------------|---------------------------|------------------------
// 1. current: lookup error                        | error                 | error                     | error
// 2. current: not found                           | -                     | create once               | create
// 3. current: no sync                             | -                     | -                         | -
// 4. current: sync on start, not present at start | -                     | -                         | -
// 5. current: sync on start, present at start     | delete once           | update once               | update once
// 6. current: sync always                         | delete                | update once               | update
func (c *autoRegisterController) checkAPIService(name string) (err error) {
	// 如何desired为非空，说明当前APIService是通过APIServer或者ExtensionServer生成的
	desired := c.GetAPIServiceToSync(name)
	// 查询informer，通过名字查询APIService，实际上可以理解为就是查询的apiserver
	curr, err := c.apiServiceLister.Get(name)

	// if we've never synced this service successfully, record a successful sync.
	// 通过 syncedSuccessfully判断是否同步成功
	hasSynced := c.hasSyncedSuccessfully(name)
	if !hasSynced {
		defer func() {
			if err == nil { // 没有错误，说明Sync成功
				c.setSyncedSuccessfully(name)
			}
		}()
	}

	switch {
	// we had a real error, just return it (1A,1B,1C)
	case err != nil && !apierrors.IsNotFound(err):
		// 如果在ETCD中查询APIService资源出错，并且错误还不是NotFound错误，那只能报错了
		return err

	// we don't have an entry and we don't want one (2A)
	case apierrors.IsNotFound(err) && desired == nil:
		// 如果在ETCD中没有找到APIService资源，但是desired也不存在，当然啥事情也不需要干
		return nil

	// the local object only wants to sync on start and has already synced (2B,5B,6B "once" enforcement)
	case isAutomanagedOnStart(desired) && hasSynced:
		// 如果是当前APIService是通过APIServer的API生成的，并且已经通不过了，那么直接退出，不需要做任何事情
		return nil

	// we don't have an entry and we do want one (2B,2C)
	case apierrors.IsNotFound(err) && desired != nil:
		// 如果在ETCD中没有找到当前APIService资源，并且desired非空，就说明通过APIServer或者ExtensionServer生成的APIService
		// 还没有同步过，此时需要创建对应的APIService资源
		_, err := c.apiServiceClient.APIServices().Create(context.TODO(), desired, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			// created in the meantime, we'll get called again
			return nil
		}
		return err

	// we aren't trying to manage this APIService (3A,3B,3C)
	case !isAutomanaged(curr):
		// 如果当前的APIService不是通过APIServer或者ExtensionServer生成的，那么不需要管理这些APIService
		// TODO 实际上，正常情况下能进入到这里的APIService要么是通过APIServer生成的，要么是通过ExtensionServer生成的
		return nil

	// the remote object only wants to sync on start, but was added after we started (4A,4B,4C)
	case isAutomanagedOnStart(curr) && !c.apiServicesAtStart[name]:
		// 如果当前的APIServer是通过APIServer的API生成的，
		return nil

	// the remote object only wants to sync on start and has already synced (5A,5B,5C "once" enforcement)
	case isAutomanagedOnStart(curr) && hasSynced:
		// 如果是当前APIService是通过APIServer的API生成的，并且已经通不过了，那么直接退出，不需要做任何事情
		return nil

	// we have a spurious APIService that we're managing, delete it (5A,6A)
	case desired == nil:
		// 这里因该是对于CRD的支持，因为CRD存在被删除的情况，因此这里也需要删除对应的APIService
		opts := metav1.DeleteOptions{Preconditions: metav1.NewUIDPreconditions(string(curr.UID))}
		err := c.apiServiceClient.APIServices().Delete(context.TODO(), curr.Name, opts)
		if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
			// deleted or changed in the meantime, we'll get called again
			return nil
		}
		return err

	// if the specs already match, nothing for us to do
	case reflect.DeepEqual(curr.Spec, desired.Spec):
		return nil
	}

	// we have an entry and we have a desired, now we deconflict.  Only a few fields matter. (5B,5C,6B,6C)
	apiService := curr.DeepCopy()
	apiService.Spec = desired.Spec
	_, err = c.apiServiceClient.APIServices().Update(context.TODO(), apiService, metav1.UpdateOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		// deleted or changed in the meantime, we'll get called again
		return nil
	}
	return err
}

// GetAPIServiceToSync gets a single API service to sync.
func (c *autoRegisterController) GetAPIServiceToSync(name string) *v1.APIService {
	c.apiServicesToSyncLock.RLock()
	defer c.apiServicesToSyncLock.RUnlock()

	return c.apiServicesToSync[name]
}

// AddAPIServiceToSyncOnStart registers an API service to sync only when the controller starts.
func (c *autoRegisterController) AddAPIServiceToSyncOnStart(in *v1.APIService) {
	c.addAPIServiceToSync(in, manageOnStart)
}

// AddAPIServiceToSync registers an API service to sync continuously.
func (c *autoRegisterController) AddAPIServiceToSync(in *v1.APIService) {
	c.addAPIServiceToSync(in, manageContinuously)
}

func (c *autoRegisterController) addAPIServiceToSync(in *v1.APIService, syncType string) {
	c.apiServicesToSyncLock.Lock()
	defer c.apiServicesToSyncLock.Unlock()

	apiService := in.DeepCopy()
	if apiService.Labels == nil {
		apiService.Labels = map[string]string{}
	}
	apiService.Labels[AutoRegisterManagedLabel] = syncType

	c.apiServicesToSync[apiService.Name] = apiService
	c.queue.Add(apiService.Name)
}

// RemoveAPIServiceToSync deletes a registered APIService.
func (c *autoRegisterController) RemoveAPIServiceToSync(name string) {
	c.apiServicesToSyncLock.Lock()
	defer c.apiServicesToSyncLock.Unlock()

	delete(c.apiServicesToSync, name)
	c.queue.Add(name)
}

func (c *autoRegisterController) hasSyncedSuccessfully(name string) bool {
	c.syncedSuccessfullyLock.RLock()
	defer c.syncedSuccessfullyLock.RUnlock()
	return c.syncedSuccessfully[name]
}

func (c *autoRegisterController) setSyncedSuccessfully(name string) {
	c.syncedSuccessfullyLock.Lock()
	defer c.syncedSuccessfullyLock.Unlock()
	c.syncedSuccessfully[name] = true
}

func automanagedType(service *v1.APIService) string {
	if service == nil {
		return ""
	}
	return service.Labels[AutoRegisterManagedLabel]
}

func isAutomanagedOnStart(service *v1.APIService) bool {
	return automanagedType(service) == manageOnStart
}

func isAutomanaged(service *v1.APIService) bool {
	managedType := automanagedType(service)
	return managedType == manageOnStart || managedType == manageContinuously
}
