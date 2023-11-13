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

package namespace

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apiserver/pkg/admission"
	clientset "k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
)

// NamespaceSelectorProvider 获取当前请求资源的名称空间，通过WebhookConfiguration.namespaceSelector指定
type NamespaceSelectorProvider interface {
	// GetParsedNamespaceSelector GetNamespaceSelector gets the webhook NamespaceSelector field.
	GetParsedNamespaceSelector() (labels.Selector, error)
}

// Matcher decides if a request is exempted by the NamespaceSelector of a
// webhook configuration.
// 名称空间匹配器
type Matcher struct {
	// 获取名称空间
	NamespaceLister corelisters.NamespaceLister
	Client          clientset.Interface
}

// Validate checks if the Matcher has a NamespaceLister and Client.
func (m *Matcher) Validate() error {
	var errs []error
	if m.NamespaceLister == nil {
		errs = append(errs, fmt.Errorf("the namespace matcher requires a namespaceLister"))
	}
	if m.Client == nil {
		errs = append(errs, fmt.Errorf("the namespace matcher requires a client"))
	}
	return utilerrors.NewAggregate(errs)
}

// GetNamespaceLabels gets the labels of the namespace related to the attr.
func (m *Matcher) GetNamespaceLabels(attr admission.Attributes) (map[string]string, error) {
	// If the request itself is creating or updating a namespace, then get the
	// labels from attr.Object, because namespaceLister doesn't have the latest
	// namespace yet.
	//
	// However, if the request is deleting a namespace, then get the label from
	// the namespace in the namespaceLister, because a delete request is not
	// going to change the object, and attr.Object will be a DeleteOptions
	// rather than a namespace object.
	// 1、如果当前请求资源即使Namespace,并且不是Namespace的子资源，并且当前操作为创建或者更新，那么就直接返回当前资源的标签。因为当前资源就是
	// 名称空间资源，它的标签当然是名称空间的标签
	if attr.GetResource().Resource == "namespaces" &&
		len(attr.GetSubresource()) == 0 &&
		(attr.GetOperation() == admission.Create || attr.GetOperation() == admission.Update) {
		accessor, err := meta.Accessor(attr.GetObject())
		if err != nil {
			return nil, err
		}
		return accessor.GetLabels(), nil
	}

	// 获取资源说在名称空间
	namespaceName := attr.GetNamespace()
	// 根据名字获取名称空间资源
	namespace, err := m.NamespaceLister.Get(namespaceName)
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if apierrors.IsNotFound(err) {
		// in case of latency in our caches, make a call direct to storage to verify that it truly exists or not
		// 如果没有找到，有可能是Informer没有，但是APIServer中确实存在，此时尝试向APIServer请求获取名称空间资源
		namespace, err = m.Client.CoreV1().Namespaces().Get(context.TODO(), namespaceName, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
	}
	return namespace.Labels, nil
}

// MatchNamespaceSelector decideds whether the request matches the
// namespaceSelctor of the webhook. Only when they match, the webhook is called.
func (m *Matcher) MatchNamespaceSelector(
	p NamespaceSelectorProvider, // 为用户在WebhookConfiguration中配置的选择器
	attr admission.Attributes, // 当前请求的资源，通过Attributes可以获取各种属性
) (bool, *apierrors.StatusError) {
	// 获取请求资源所在名称空间
	namespaceName := attr.GetNamespace()
	// 1、如果当前资源没有指定名称空间，并且当前资源不是Namespace资源，说明当前资源是一个Cluster级别的资源，名称空间选择器肯定没啥卵用，直接认为是匹配
	// 2、之所以认为是匹配的，是因为用户很有可能设置WebhookConfiguration的Scope为*，也就是无论资源是Cluster级别的还是名称空间级别的，都需要
	// 进行一次准入控制。此时，如果当前请求是Cluster级别的，那么用户指定的名称空间选择器只能认为匹配上
	if len(namespaceName) == 0 && attr.GetResource().Resource != "namespaces" {
		// If the request is about a cluster scoped resource, and it is not a
		// namespace, it is never exempted.
		// TODO: figure out a way selective exempt cluster scoped resources.
		// Also update the comment in types.go
		return true, nil
	}
	// 获取用户指定的名称空间选择器
	selector, err := p.GetParsedNamespaceSelector()
	if err != nil {
		return false, apierrors.NewInternalError(err)
	}
	// 如果用户没有指定，认为匹配成功
	if selector.Empty() {
		return true, nil
	}

	// 获取当前请求资源的名称空间标签
	namespaceLabels, err := m.GetNamespaceLabels(attr)
	// this means the namespace is not found, for backwards compatibility,
	// return a 404
	if apierrors.IsNotFound(err) {
		status, ok := err.(apierrors.APIStatus)
		if !ok {
			return false, apierrors.NewInternalError(err)
		}
		return false, &apierrors.StatusError{ErrStatus: status.Status()}
	}
	if err != nil {
		return false, apierrors.NewInternalError(err)
	}
	// 比较两个名称空间选择器是否匹配
	return selector.Matches(labels.Set(namespaceLabels)), nil
}
