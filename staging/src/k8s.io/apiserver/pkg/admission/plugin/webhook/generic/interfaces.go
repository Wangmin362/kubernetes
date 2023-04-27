/*
Copyright 2018 The Kubernetes Authors.

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

package generic

import (
	"context"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/plugin/webhook"
)

// Source can list dynamic webhook plugins.
// 每一个 webhook.WebhookAccessor 就是一个webhook配置，所谓的Source，实际上就是为了拿到所有的webhook配置
type Source interface {
	// Webhooks 可以理解为webhook的配置，只不过是一个列表
	Webhooks() []webhook.WebhookAccessor
	// HasSynced 当前Source是否已经同步完成，由于Source一般都是通过informer获取到的，因此这里的HasSynced
	// 一般指的是informer是否已同步完成
	HasSynced() bool
}

// VersionedAttributes is a wrapper around the original admission attributes, adding versioned
// variants of the object and old object.
type VersionedAttributes struct {
	// Attributes holds the original admission attributes
	admission.Attributes
	// VersionedOldObject holds Attributes.OldObject (if non-nil), converted to VersionedKind.
	// It must never be mutated.
	VersionedOldObject runtime.Object
	// VersionedObject holds Attributes.Object (if non-nil), converted to VersionedKind.
	// If mutated, Dirty must be set to true by the mutator.
	VersionedObject runtime.Object
	// VersionedKind holds the fully qualified kind
	VersionedKind schema.GroupVersionKind
	// Dirty indicates VersionedObject has been modified since being converted from Attributes.Object
	Dirty bool
}

// GetObject overrides the Attributes.GetObject()
func (v *VersionedAttributes) GetObject() runtime.Object {
	if v.VersionedObject != nil {
		return v.VersionedObject
	}
	return v.Attributes.GetObject()
}

// WebhookInvocation describes how to call a webhook, including the resource and subresource the webhook registered for,
// and the kind that should be sent to the webhook.
type WebhookInvocation struct {
	Webhook     webhook.WebhookAccessor
	Resource    schema.GroupVersionResource
	Subresource string
	Kind        schema.GroupVersionKind
}

// Dispatcher dispatches webhook call to a list of webhooks with admission attributes as argument.
// TODO 如何理解这个接口的设计？
type Dispatcher interface {
	// Dispatch a request to the webhooks. Dispatcher may choose not to
	// call a hook, either because the rules of the hook does not match, or
	// the namespaceSelector or the objectSelector of the hook does not
	// match. A non-nil error means the request is rejected.
	// 把当前的请求代理到合适的webhook上
	Dispatch(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces, hooks []webhook.WebhookAccessor) error
}
