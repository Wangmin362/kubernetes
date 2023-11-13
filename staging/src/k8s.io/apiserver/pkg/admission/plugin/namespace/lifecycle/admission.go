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

package lifecycle

import (
	"context"
	"fmt"
	"io"
	"time"

	"k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilcache "k8s.io/apimachinery/pkg/util/cache"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/utils/clock"
)

const (
	// PluginName indicates the name of admission plug-in
	PluginName = "NamespaceLifecycle"
	// how long a namespace stays in the force live lookup cache before expiration.
	forceLiveLookupTTL = 30 * time.Second
	// how long to wait for a missing namespace before re-checking the cache (and then doing a live lookup)
	// this accomplishes two things:
	// 1. It allows a watch-fed cache time to observe a namespace creation event
	// 2. It allows time for a namespace creation to distribute to members of a storage cluster,
	//    so the live lookup has a better chance of succeeding even if it isn't performed against the leader.
	missingNamespaceWait = 50 * time.Millisecond
)

// Register registers a plugin
func Register(plugins *admission.Plugins) {
	plugins.Register(PluginName, func(config io.Reader) (admission.Interface, error) {
		return NewLifecycle(sets.NewString(metav1.NamespaceDefault, metav1.NamespaceSystem, metav1.NamespacePublic))
	})
}

// Lifecycle is an implementation of admission.Interface.
// It enforces life-cycle constraints around a Namespace depending on its Phase
// 1、不允许删除default, kube-system, kube-public名称空间
// 2、阻止向不存在的名称空间中创建资源
// 3、组织向正在删除的名称空间中创建资源
// 3、此插件默认启用。
type Lifecycle struct {
	*admission.Handler
	client             kubernetes.Interface        // APIServer客户端
	immortalNamespaces sets.String                 // 所谓永存的名称空间，其实就是default, kube-system, kube-public名称空间
	namespaceLister    corelisters.NamespaceLister // 查询名称空间
	// forceLiveLookupCache holds a list of entries for namespaces that we have a strong reason to believe are stale in our local cache.
	// if a namespace is in this cache, then we will ignore our local state and always fetch latest from api server.
	forceLiveLookupCache *utilcache.LRUExpireCache // LRU缓存
}

// 准入控制插件初始化器
var _ = initializer.WantsExternalKubeInformerFactory(&Lifecycle{})
var _ = initializer.WantsExternalKubeClientSet(&Lifecycle{})

// Admit makes an admission decision based on the request attributes
func (l *Lifecycle) Admit(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) error {
	// prevent deletion of immortal namespaces
	// 不允许删除default, kube-system, kube-public名称空间
	if a.GetOperation() == admission.Delete && a.GetKind().GroupKind() == v1.SchemeGroupVersion.WithKind("Namespace").GroupKind() &&
		l.immortalNamespaces.Has(a.GetName()) {
		return errors.NewForbidden(a.GetResource().GroupResource(), a.GetName(), fmt.Errorf("this namespace may not be deleted"))
	}

	// always allow non-namespaced resources
	// 当前Lifecycle仅针对名称空间资源起作用，对于其它任意资源都不起作用，直接放过
	if len(a.GetNamespace()) == 0 && a.GetKind().GroupKind() != v1.SchemeGroupVersion.WithKind("Namespace").GroupKind() {
		return nil
	}

	if a.GetKind().GroupKind() == v1.SchemeGroupVersion.WithKind("Namespace").GroupKind() {
		// if a namespace is deleted, we want to prevent all further creates into it
		// while it is undergoing termination.  to reduce incidences where the cache
		// is slow to update, we add the namespace into a force live lookup list to ensure
		// we are not looking at stale state.
		// 记录删除的名称空间，如果一个名称空间已经被删除，此时用户在名称空间被删除的过程当当中创建新的资源，此时需要直接通过缓存阻止这个操作
		if a.GetOperation() == admission.Delete {
			l.forceLiveLookupCache.Add(a.GetName(), true, forceLiveLookupTTL)
		}
		// allow all operations to namespaces
		return nil
	}

	// always allow deletion of other resources
	// 对于其它资源默认
	if a.GetOperation() == admission.Delete {
		return nil
	}

	// always allow access review checks.  Returning status about the namespace would be leaking information
	if isAccessReview(a) {
		return nil
	}

	// we need to wait for our caches to warm
	// 等待准入控制插件就绪
	if !l.WaitForReady() {
		return admission.NewForbidden(a, fmt.Errorf("not yet ready to handle request"))
	}

	var (
		exists bool
		err    error
	)

	// 获取当前资源所在名称空间
	namespace, err := l.namespaceLister.Get(a.GetNamespace())
	if err != nil {
		if !errors.IsNotFound(err) {
			return errors.NewInternalError(err)
		}
	} else {
		exists = true
	}

	// 当前资源的名称空间不存在，并且是一个创建资源的操作
	if !exists && a.GetOperation() == admission.Create {
		// give the cache time to observe the namespace before rejecting a create.
		// this helps when creating a namespace and immediately creating objects within it.
		// 重新检测名称空间是否存在
		time.Sleep(missingNamespaceWait)
		namespace, err = l.namespaceLister.Get(a.GetNamespace())
		switch {
		case errors.IsNotFound(err):
			// no-op
		case err != nil:
			return errors.NewInternalError(err)
		default:
			exists = true
		}
		if exists {
			klog.V(4).InfoS("Namespace existed in cache after waiting", "namespace", klog.KRef("", a.GetNamespace()))
		}
	}

	// forceLiveLookup if true will skip looking at local cache state and instead always make a live call to server.
	forceLiveLookup := false
	if _, ok := l.forceLiveLookupCache.Get(a.GetNamespace()); ok {
		// we think the namespace was marked for deletion, but our current local cache says otherwise, we will force a live lookup.
		forceLiveLookup = exists && namespace.Status.Phase == v1.NamespaceActive
	}

	// refuse to operate on non-existent namespaces
	if !exists || forceLiveLookup {
		// as a last resort, make a call directly to storage
		// 当前请求的名称空间不存在，直接拒绝这个请求
		namespace, err = l.client.CoreV1().Namespaces().Get(context.TODO(), a.GetNamespace(), metav1.GetOptions{})
		switch {
		case errors.IsNotFound(err):
			return err
		case err != nil:
			return errors.NewInternalError(err)
		}

		klog.V(4).InfoS("Found namespace via storage lookup", "namespace", klog.KRef("", a.GetNamespace()))
	}

	// ensure that we're not trying to create objects in terminating namespaces
	if a.GetOperation() == admission.Create {
		// 如果当前请求准备创建资源，并且资源所在名称空间没有处于删除状态，那么允许此操作
		if namespace.Status.Phase != v1.NamespaceTerminating {
			return nil
		}

		// 否则，禁止向正在删除的名称空间中创建任意资源
		err := admission.NewForbidden(a, fmt.Errorf("unable to create new content in namespace %s because it is being terminated", a.GetNamespace()))
		if apierr, ok := err.(*errors.StatusError); ok {
			apierr.ErrStatus.Details.Causes = append(apierr.ErrStatus.Details.Causes, metav1.StatusCause{
				Type:    v1.NamespaceTerminatingCause,
				Message: fmt.Sprintf("namespace %s is being terminated", a.GetNamespace()),
				Field:   "metadata.namespace",
			})
		}
		return err
	}

	return nil
}

// NewLifecycle creates a new namespace Lifecycle admission control handler
func NewLifecycle(immortalNamespaces sets.String) (*Lifecycle, error) {
	return newLifecycleWithClock(immortalNamespaces, clock.RealClock{})
}

func newLifecycleWithClock(immortalNamespaces sets.String, clock utilcache.Clock) (*Lifecycle, error) {
	forceLiveLookupCache := utilcache.NewLRUExpireCacheWithClock(100, clock)
	return &Lifecycle{
		// 支持CREATE, UPDATE, DELETE操作
		Handler:              admission.NewHandler(admission.Create, admission.Update, admission.Delete),
		immortalNamespaces:   immortalNamespaces,
		forceLiveLookupCache: forceLiveLookupCache,
	}, nil
}

// SetExternalKubeInformerFactory implements the WantsExternalKubeInformerFactory interface.
func (l *Lifecycle) SetExternalKubeInformerFactory(f informers.SharedInformerFactory) {
	namespaceInformer := f.Core().V1().Namespaces()
	l.namespaceLister = namespaceInformer.Lister()
	// Informer同步完成就认为Livecycle准入控制插件就绪
	l.SetReadyFunc(namespaceInformer.Informer().HasSynced)
}

// SetExternalKubeClientSet implements the WantsExternalKubeClientSet interface.
func (l *Lifecycle) SetExternalKubeClientSet(client kubernetes.Interface) {
	l.client = client
}

// ValidateInitialization implements the InitializationValidator interface.
func (l *Lifecycle) ValidateInitialization() error {
	if l.namespaceLister == nil {
		return fmt.Errorf("missing namespaceLister")
	}
	if l.client == nil {
		return fmt.Errorf("missing client")
	}
	return nil
}

// accessReviewResources are resources which give a view into permissions in a namespace.  Users must be allowed to create these
// resources because returning "not found" errors allows someone to search for the "people I'm going to fire in 2017" namespace.
var accessReviewResources = map[schema.GroupResource]bool{
	{Group: "authorization.k8s.io", Resource: "localsubjectaccessreviews"}: true,
}

func isAccessReview(a admission.Attributes) bool {
	return accessReviewResources[a.GetResource().GroupResource()]
}
