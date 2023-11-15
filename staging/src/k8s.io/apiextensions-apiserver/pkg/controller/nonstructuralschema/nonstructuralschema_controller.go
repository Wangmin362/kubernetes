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

package nonstructuralschema

import (
	"context"
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	apiextensionshelpers "k8s.io/apiextensions-apiserver/pkg/apihelpers"
	apiextensionsinternal "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	client "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1"
	informers "k8s.io/apiextensions-apiserver/pkg/client/informers/externalversions/apiextensions/v1"
	listers "k8s.io/apiextensions-apiserver/pkg/client/listers/apiextensions/v1"
)

// ConditionController is maintaining the NonStructuralSchema condition.
type ConditionController struct {
	crdClient client.CustomResourceDefinitionsGetter

	crdLister listers.CustomResourceDefinitionLister
	crdSynced cache.InformerSynced

	// To allow injection for testing.
	// key为CRD的名字
	syncFn func(key string) error

	// 保存的CRD的名字
	queue workqueue.RateLimitingInterface

	// last generation this controller updated the condition per CRD name (to avoid two
	// different version of the apiextensions-apiservers in HA to fight for the right message)
	lastSeenGenerationLock sync.Mutex
	// TODO 干嘛的？
	lastSeenGeneration map[string]int64
}

// NewConditionController constructs a non-structural schema condition controller.
func NewConditionController(
	crdInformer informers.CustomResourceDefinitionInformer,
	crdClient client.CustomResourceDefinitionsGetter,
) *ConditionController {
	c := &ConditionController{
		crdClient:          crdClient,
		crdLister:          crdInformer.Lister(),
		crdSynced:          crdInformer.Informer().HasSynced,
		queue:              workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "non_structural_schema_condition_controller"),
		lastSeenGeneration: map[string]int64{},
	}

	crdInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    c.addCustomResourceDefinition,
		UpdateFunc: c.updateCustomResourceDefinition,
		DeleteFunc: c.deleteCustomResourceDefinition,
	})

	c.syncFn = c.sync

	return c
}

func calculateCondition(in *apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.CustomResourceDefinitionCondition {
	cond := &apiextensionsv1.CustomResourceDefinitionCondition{
		Type:   apiextensionsv1.NonStructuralSchema,
		Status: apiextensionsv1.ConditionUnknown,
	}

	allErrs := field.ErrorList{}

	// TODO 这里说明这个字段必须是False
	if in.Spec.PreserveUnknownFields {
		allErrs = append(allErrs, field.Invalid(field.NewPath("spec", "preserveUnknownFields"),
			in.Spec.PreserveUnknownFields,
			fmt.Sprint("must be false")))
	}

	// 遍历当前CRD所有的版本
	for i, v := range in.Spec.Versions {
		// 如果当前CRD没有定义OpenAPI，直接跳过。这玩意其实就是用于定义CRD的的字段，以及每个字段的类型
		if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			continue
		}

		internalSchema := &apiextensionsinternal.CustomResourceValidation{}
		if err := apiextensionsv1.Convert_v1_CustomResourceValidation_To_apiextensions_CustomResourceValidation(v.Schema, internalSchema, nil); err != nil {
			klog.Errorf("failed to convert CRD validation to internal version: %v", err)
			continue
		}
		// 判断当前的CRD到底是否是非结构化的
		s, err := schema.NewStructural(internalSchema.OpenAPIV3Schema)
		if err != nil {
			cond.Reason = "StructuralError"
			cond.Message = fmt.Sprintf("failed to check validation schema for version %s: %v", v.Name, err)
			return cond
		}

		pth := field.NewPath("spec", "versions").Index(i).Child("schema", "openAPIV3Schema")

		allErrs = append(allErrs, schema.ValidateStructural(pth, s)...)
	}

	if len(allErrs) == 0 {
		return nil
	}

	cond.Status = apiextensionsv1.ConditionTrue
	cond.Reason = "Violations"
	cond.Message = allErrs.ToAggregate().Error()

	return cond
}

func (c *ConditionController) sync(key string) error {
	// 根据CRD名字查询当前CRD
	inCustomResourceDefinition, err := c.crdLister.Get(key)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}

	// avoid repeated calculation for the same generation
	c.lastSeenGenerationLock.Lock()
	lastSeen, seenBefore := c.lastSeenGeneration[inCustomResourceDefinition.Name]
	c.lastSeenGenerationLock.Unlock()
	// 如果之前已经看过这个CRD，并且CRD的结果没有发生改变，那么无需再次处理这个CRD。换言之，如果CRD的结构发生改变，那么Generation一定会发生改变
	if seenBefore && inCustomResourceDefinition.Generation <= lastSeen {
		return nil
	}

	// check old condition
	// 判断当前CRD是否是非结构化的，如果是那么返回Condition NonStructuralSchema=True
	cond := calculateCondition(inCustomResourceDefinition)
	old := apiextensionshelpers.FindCRDCondition(inCustomResourceDefinition, apiextensionsv1.NonStructuralSchema)

	if cond == nil && old == nil {
		return nil
	}
	if cond != nil && old != nil && old.Status == cond.Status && old.Reason == cond.Reason && old.Message == cond.Message {
		return nil
	}

	// update condition
	crd := inCustomResourceDefinition.DeepCopy()
	if cond == nil {
		apiextensionshelpers.RemoveCRDCondition(crd, apiextensionsv1.NonStructuralSchema)
	} else {
		cond.LastTransitionTime = metav1.NewTime(time.Now())
		apiextensionshelpers.SetCRDCondition(crd, *cond)
	}

	// 更新这个CRD的状态
	_, err = c.crdClient.CustomResourceDefinitions().UpdateStatus(context.TODO(), crd, metav1.UpdateOptions{})
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		// deleted or changed in the meantime, we'll get called again
		return nil
	}
	if err != nil {
		return err
	}

	// store generation in order to avoid repeated updates for the same generation (and potential
	// fights of API server in HA environments).
	c.lastSeenGenerationLock.Lock()
	defer c.lastSeenGenerationLock.Unlock()
	// 记录下来，说明当前已经处理过这个CRD了，下次不需要再处理
	c.lastSeenGeneration[crd.Name] = crd.Generation

	return nil
}

// Run starts the controller.
func (c *ConditionController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting NonStructuralSchemaConditionController")
	defer klog.Infof("Shutting down NonStructuralSchemaConditionController")

	if !cache.WaitForCacheSync(stopCh, c.crdSynced) {
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
}

func (c *ConditionController) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem deals with one key off the queue.  It returns false when it's time to quit.
func (c *ConditionController) processNextWorkItem() bool {
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

func (c *ConditionController) enqueue(obj *apiextensionsv1.CustomResourceDefinition) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", obj, err))
		return
	}

	c.queue.Add(key)
}

func (c *ConditionController) addCustomResourceDefinition(obj interface{}) {
	castObj := obj.(*apiextensionsv1.CustomResourceDefinition)
	klog.V(4).Infof("Adding %s", castObj.Name)
	c.enqueue(castObj)
}

func (c *ConditionController) updateCustomResourceDefinition(obj, _ interface{}) {
	castObj := obj.(*apiextensionsv1.CustomResourceDefinition)
	klog.V(4).Infof("Updating %s", castObj.Name)
	c.enqueue(castObj)
}

func (c *ConditionController) deleteCustomResourceDefinition(obj interface{}) {
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

	c.lastSeenGenerationLock.Lock()
	defer c.lastSeenGenerationLock.Unlock()
	delete(c.lastSeenGeneration, castObj.Name)
}
