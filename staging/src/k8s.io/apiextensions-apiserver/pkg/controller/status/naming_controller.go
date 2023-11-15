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

package status

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	apiextensionshelpers "k8s.io/apiextensions-apiserver/pkg/apihelpers"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	client "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	informers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions/apiextensions/v1"
	listers "k8s.io/apiextensions-apiserver/pkg/client/listers/apiextensions/v1"
)

// NamingConditionController This controller is reserving names. To avoid conflicts, be sure to run only one instance of the worker at a time.
// This could eventually be lifted, but starting simple.
// 1、NamingConditionController的主要职责就是保证用户新添加的CRD不会和集群中已经存在的CRD有明明冲突问题，譬如单数名称、附属名称、缩写名称等等
type NamingConditionController struct {
	crdClient client.CustomResourceDefinitionsGetter

	crdLister listers.CustomResourceDefinitionLister
	crdSynced cache.InformerSynced
	// crdMutationCache backs our lister and keeps track of committed updates to avoid racy
	// write/lookup cycles.  It's got 100 slots by default, so it unlikely to overrun
	// TODO to revisit this if naming conflicts are found to occur in the wild
	// TODO 分析这玩意的工作原理
	crdMutationCache cache.MutationCache

	// To allow injection for testing.
	syncFn func(key string) error

	// 缓存的CRD的名字， CRD本身是Cluster级别的资源，但是CR可能是名称空间级别的，也可能是集群级别的资源
	queue workqueue.RateLimitingInterface
}

func NewNamingConditionController(
	crdInformer informers.CustomResourceDefinitionInformer,
	crdClient client.CustomResourceDefinitionsGetter,
) *NamingConditionController {
	c := &NamingConditionController{
		crdClient: crdClient,
		crdLister: crdInformer.Lister(),
		crdSynced: crdInformer.Informer().HasSynced,
		queue:     workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "crd_naming_condition_controller"),
	}

	informerIndexer := crdInformer.Informer().GetIndexer()
	c.crdMutationCache = cache.NewIntegerResourceVersionMutationCache(informerIndexer, informerIndexer, 60*time.Second, false)

	// 监听CRD的变化
	crdInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addCustomResourceDefinition,
		UpdateFunc: c.updateCustomResourceDefinition,
		DeleteFunc: c.deleteCustomResourceDefinition,
	})

	c.syncFn = c.sync

	return c
}

// 1、allResources：保存CRD的单数名称、复数名称、缩写名称
// 2、allKinds：// 保存Kind, ListKind类型
func (c *NamingConditionController) getAcceptedNamesForGroup(group string) (allResources sets.String, allKinds sets.String) {
	// 保存CRD的单数名称、复数名称、缩写名称
	allResources = sets.String{}
	// 保存Kind, ListKind类型
	allKinds = sets.String{}

	// 遍历所有的CRD
	list, err := c.crdLister.List(labels.Everything())
	if err != nil {
		panic(err)
	}

	for _, curr := range list {
		if curr.Spec.Group != group {
			continue
		}

		// for each item here, see if we have a mutation cache entry that is more recent
		// this makes sure that if we tight loop on update and run, our mutation cache will show
		// us the version of the objects we just updated to.
		item := curr
		obj, exists, err := c.crdMutationCache.GetByKey(curr.Name)
		if exists && err == nil {
			item = obj.(*apiextensionsv1.CustomResourceDefinition)
		}

		allResources.Insert(item.Status.AcceptedNames.Plural)
		allResources.Insert(item.Status.AcceptedNames.Singular)
		allResources.Insert(item.Status.AcceptedNames.ShortNames...)

		allKinds.Insert(item.Status.AcceptedNames.Kind)
		allKinds.Insert(item.Status.AcceptedNames.ListKind)
	}

	return allResources, allKinds
}

func (c *NamingConditionController) calculateNamesAndConditions(in *apiextensionsv1.CustomResourceDefinition) (
	apiextensionsv1.CustomResourceDefinitionNames, apiextensionsv1.CustomResourceDefinitionCondition, apiextensionsv1.CustomResourceDefinitionCondition) {
	// Get the names that have already been claimed
	// 获取当前CRD所在组的所有CRD的名字（单数名称、复数名称、缩写名称）以及Kind、LindList类型
	allResources, allKinds := c.getAcceptedNamesForGroup(in.Spec.Group)

	// 默认设置CRD NamesAccepted=Unknown的Condition，后续会根据冲突情况修改
	namesAcceptedCondition := apiextensionsv1.CustomResourceDefinitionCondition{
		Type:   apiextensionsv1.NamesAccepted,
		Status: apiextensionsv1.ConditionUnknown,
	}

	requestedNames := in.Spec.Names          // 当前CRD的名称
	acceptedNames := in.Status.AcceptedNames // 当前CRD接受的名称
	newNames := in.Status.AcceptedNames      // 当前CRD最终会被接受的名称，这里主要是为了获取指针，后面会陆续修改其中的属性

	// Check each name for mismatches.  If there's a mismatch between spec and status, then try to deconflict.
	// Continue on errors so that the status is the best match possible
	// 1、如果当前CRD的以及该接受的复数名称和CRD定义的复数名称一致，那么说明当前CRD的复数数名称肯定和当前系统已经存在的CRD名称没有冲突
	// 2、如果当前CRD定义的复数名称还没有被接受，那么判断当前CRD定义的复数名称是否和当前系统中所有资源的单数名称、复数名称、缩写名称冲突
	// 3、如果有冲突，那么给这个CRD打上NamesAccepted=False的Condition，原因为PluralConflict
	if err := equalToAcceptedOrFresh(requestedNames.Plural, acceptedNames.Plural, allResources); err != nil {
		namesAcceptedCondition.Status = apiextensionsv1.ConditionFalse
		namesAcceptedCondition.Reason = "PluralConflict"
		namesAcceptedCondition.Message = err.Error()
	} else {
		// 说明当前CRD的复数名称和当前系统所有的CRD的单数名称、复数名称、缩写名称没有冲突，可以使用
		newNames.Plural = requestedNames.Plural
	}
	// 1、如果当前CRD的以及该接受的单数名称和CRD定义的单数名称一致，那么说明当前CRD的单数数名称肯定和当前系统已经存在的CRD名称没有冲突
	// 2、如果当前CRD定义的单数名称还没有被接受，那么判断当前CRD定义的单数名称是否和当前系统中所有资源的单数名称、复数名称、缩写名称冲突
	// 3、如果有冲突，那么给这个CRD打上NamesAccepted=False的Condition，原因为SingularConflict
	if err := equalToAcceptedOrFresh(requestedNames.Singular, acceptedNames.Singular, allResources); err != nil {
		namesAcceptedCondition.Status = apiextensionsv1.ConditionFalse
		namesAcceptedCondition.Reason = "SingularConflict"
		namesAcceptedCondition.Message = err.Error()
	} else {
		// 说明当前CRD的单数名称和当前系统所有的CRD的单数名称、复数名称、缩写名称没有冲突，可以使用
		newNames.Singular = requestedNames.Singular
	}
	// 如果当前CRD被接受的缩写名称和CRD定义的缩写名称不相等，说明CRD定义的缩写名称可能存在冲突，因此还需要检测
	if !reflect.DeepEqual(requestedNames.ShortNames, acceptedNames.ShortNames) {
		var errs []error
		// 获取CRD已经被接受的说些名称
		existingShortNames := sets.NewString(acceptedNames.ShortNames...)
		// 遍历CRD定义的缩写名称
		for _, shortName := range requestedNames.ShortNames {
			// if the shortname is already ours, then we're fine
			// 如果当前缩写名称已经被接受，那么肯定不存在冲突，直接跳过
			if existingShortNames.Has(shortName) {
				continue
			}
			// 否则，看看当前CRD定义的缩写名称和当前系统所有的CRD资源定义的单数名称、复数名称、缩写名称是否有冲突
			if err := equalToAcceptedOrFresh(shortName, "", allResources); err != nil {
				errs = append(errs, err)
			}

		}

		// 如果错误非空，那么肯定说明至少有一个缩写名称冲突，那么给当前CRD打上NamesAccepted=False的Condition，原因为ShortNamesConflict
		if err := utilerrors.NewAggregate(errs); err != nil {
			namesAcceptedCondition.Status = apiextensionsv1.ConditionFalse
			namesAcceptedCondition.Reason = "ShortNamesConflict"
			namesAcceptedCondition.Message = err.Error()
		} else {
			// 说明当前CRD的缩写名称和当前系统所有的CRD的单数名称、复数名称、缩写名称没有冲突，可以使用
			newNames.ShortNames = requestedNames.ShortNames
		}
	}

	// 1、如果当前CRD定义的Kind已经被接受，那么肯定没有冲突
	// 2、如果当前CRD定义的Kind没有被接受，那么需要判断当前CRD定义的Kind是否和当前系统定义的所有资源的Kind, KindList是否有冲突
	// 3、如果有冲突，那么这个CRD会被打上NamesAccepted=False的Condition，原因为KindConflict
	if err := equalToAcceptedOrFresh(requestedNames.Kind, acceptedNames.Kind, allKinds); err != nil {
		namesAcceptedCondition.Status = apiextensionsv1.ConditionFalse
		namesAcceptedCondition.Reason = "KindConflict"
		namesAcceptedCondition.Message = err.Error()
	} else {
		// 说明当前CRD定义的Kind没有和当前系统定义的所有资源的Kind, KindList冲突
		newNames.Kind = requestedNames.Kind
	}

	// 1、如果当前CRD定义的ListKind已经被接受，那么肯定没有冲突
	// 2、如果当前CRD定义的ListKind没有被接受，那么需要判断当前CRD定义的Kind是否和当前系统定义的所有资源的Kind, KindList是否有冲突
	// 3、如果有冲突，那么这个CRD会被打上NamesAccepted=False的Condition，原因为ListKindConflict
	if err := equalToAcceptedOrFresh(requestedNames.ListKind, acceptedNames.ListKind, allKinds); err != nil {
		namesAcceptedCondition.Status = apiextensionsv1.ConditionFalse
		namesAcceptedCondition.Reason = "ListKindConflict"
		namesAcceptedCondition.Message = err.Error()
	} else {
		// 说明当前CRD定义的ListKind没有和当前系统定义的所有资源的Kind, KindList冲突
		newNames.ListKind = requestedNames.ListKind
	}

	// Categories不存在冲突问题
	newNames.Categories = requestedNames.Categories

	// if we haven't changed the condition, then our names must be good.
	// 如果此时NamesAccepted Condition的状态还是Unknow，说明当前CRD定义的单数名称、复数名称、缩写名称、Kind, ListKind和系统已经存在的
	// CRD名称肯定没有冲突，因此修改NamesAccepted=True
	if namesAcceptedCondition.Status == apiextensionsv1.ConditionUnknown {
		namesAcceptedCondition.Status = apiextensionsv1.ConditionTrue
		namesAcceptedCondition.Reason = "NoConflicts"
		namesAcceptedCondition.Message = "no conflicts found"
	}

	// set EstablishedCondition initially to false, then set it to true in establishing controller.
	// The Establishing Controller will see the NamesAccepted condition when it arrives through the shared informer.
	// At that time the API endpoint handler will serve the endpoint, avoiding a race
	// which we had if we set Established to true here.
	// 初始化Established Condition
	establishedCondition := apiextensionsv1.CustomResourceDefinitionCondition{
		Type:    apiextensionsv1.Established,
		Status:  apiextensionsv1.ConditionFalse,
		Reason:  "NotAccepted",
		Message: "not all names are accepted",
	}
	if old := apiextensionshelpers.FindCRDCondition(in, apiextensionsv1.Established); old != nil {
		establishedCondition = *old
	}
	if establishedCondition.Status != apiextensionsv1.ConditionTrue && namesAcceptedCondition.Status == apiextensionsv1.ConditionTrue {
		establishedCondition = apiextensionsv1.CustomResourceDefinitionCondition{
			Type:    apiextensionsv1.Established,
			Status:  apiextensionsv1.ConditionFalse,
			Reason:  "Installing",
			Message: "the initial names have been accepted",
		}
	}

	return newNames, namesAcceptedCondition, establishedCondition
}

func equalToAcceptedOrFresh(requestedName, acceptedName string, usedNames sets.String) error {
	if requestedName == acceptedName {
		return nil
	}
	if !usedNames.Has(requestedName) {
		return nil
	}

	return fmt.Errorf("%q is already in use", requestedName)
}

// key = <name>
func (c *NamingConditionController) sync(key string) error {
	// 查询当前CRD
	inCustomResourceDefinition, err := c.crdLister.Get(key)
	if apierrors.IsNotFound(err) {
		// CRD was deleted and has freed its names.
		// Reconsider all other CRDs in the same group.
		// TODO
		if err := c.requeueAllOtherGroupCRDs(key); err != nil {
			return err
		}
		return nil
	}
	if err != nil {
		return err
	}

	// Skip checking names if Spec and Status names are same.
	// 如果当前CRD的所有名字全部被接受，直接退出
	if equality.Semantic.DeepEqual(inCustomResourceDefinition.Spec.Names, inCustomResourceDefinition.Status.AcceptedNames) {
		return nil
	}

	// 1、否则，判断当前CRD可以被接受的名字，其实就是看看当前CRD的单数名称、附属名称、缩写名称、Kind、ListKind是否和其它CRD有冲突？
	// 2、acceptedNames就是当前CRD可以被接受的名字
	acceptedNames, namingCondition, establishedCondition := c.calculateNamesAndConditions(inCustomResourceDefinition)

	// nothing to do if accepted names and NamesAccepted condition didn't change
	// 如果CRD的所有名称已经被接受，并且CRD被打上了NamesAccepted=true的Condition，那么就无需更新当前CRD
	if reflect.DeepEqual(inCustomResourceDefinition.Status.AcceptedNames, acceptedNames) &&
		apiextensionshelpers.IsCRDConditionEquivalent(&namingCondition, apiextensionshelpers.FindCRDCondition(inCustomResourceDefinition,
			apiextensionsv1.NamesAccepted)) {
		return nil
	}

	// 更新CRD的状态，主要是设置Condition
	crd := inCustomResourceDefinition.DeepCopy()
	crd.Status.AcceptedNames = acceptedNames
	apiextensionshelpers.SetCRDCondition(crd, namingCondition)
	apiextensionshelpers.SetCRDCondition(crd, establishedCondition)

	// 更新CRD状态
	updatedObj, err := c.crdClient.CustomResourceDefinitions().UpdateStatus(context.TODO(), crd, metav1.UpdateOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		// deleted or changed in the meantime, we'll get called again
		return nil
	}
	if err != nil {
		return err
	}

	// if the update was successful, go ahead and add the entry to the mutation cache
	// TODO 这里是在干嘛？
	c.crdMutationCache.Mutation(updatedObj)

	// we updated our status, so we may be releasing a name.  When this happens, we need to rekick everything in our group
	// if we fail to rekick, just return as normal.  We'll get everything on a resync
	// TODO ?
	if err := c.requeueAllOtherGroupCRDs(key); err != nil {
		return err
	}

	return nil
}

func (c *NamingConditionController) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Info("Starting NamingConditionController")
	defer klog.Info("Shutting down NamingConditionController")

	if !cache.WaitForCacheSync(stopCh, c.crdSynced) {
		return
	}

	// only start one worker thread since its a slow moving API and the naming conflict resolution bits aren't thread-safe
	go wait.Until(c.runWorker, time.Second, stopCh)

	<-stopCh
}

func (c *NamingConditionController) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem deals with one key off the queue.  It returns false when it's time to quit.
func (c *NamingConditionController) processNextWorkItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.syncFn(key.(string))
	if err == nil {
		c.queue.Forget(key)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with: %v", key, err))
	c.queue.AddRateLimited(key)

	return true
}

func (c *NamingConditionController) enqueue(obj *apiextensionsv1.CustomResourceDefinition) {
	// 1、如果当前资源存在Namespace，那么返回<namespace>/<name>，否则返回<name>
	// 2、由于CRD资源是没有名称空间的，因此ojb=name
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", obj, err))
		return
	}

	c.queue.Add(key)
}

func (c *NamingConditionController) addCustomResourceDefinition(obj interface{}) {
	castObj := obj.(*apiextensionsv1.CustomResourceDefinition)
	klog.V(4).Infof("Adding %s", castObj.Name)
	c.enqueue(castObj)
}

func (c *NamingConditionController) updateCustomResourceDefinition(obj, _ interface{}) {
	castObj := obj.(*apiextensionsv1.CustomResourceDefinition)
	klog.V(4).Infof("Updating %s", castObj.Name)
	c.enqueue(castObj)
}

func (c *NamingConditionController) deleteCustomResourceDefinition(obj interface{}) {
	castObj, ok := obj.(*apiextensionsv1.CustomResourceDefinition)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			klog.Errorf("Couldn't get object from tombstone %#v", obj)
			return
		}
		castObj, ok = tombstone.Obj.(*apiextensionsv1.CustomResourceDefinition)
		if !ok {
			klog.Errorf("Tombstone contained object that is not expected %#v", obj)
			return
		}
	}
	klog.V(4).Infof("Deleting %q", castObj.Name)
	c.enqueue(castObj)
}

func (c *NamingConditionController) requeueAllOtherGroupCRDs(name string) error {
	pluralGroup := strings.SplitN(name, ".", 2)
	list, err := c.crdLister.List(labels.Everything())
	if err != nil {
		return err
	}
	for _, curr := range list {
		if curr.Spec.Group == pluralGroup[1] && curr.Name != name {
			c.queue.Add(curr.Name)
		}
	}
	return nil
}
