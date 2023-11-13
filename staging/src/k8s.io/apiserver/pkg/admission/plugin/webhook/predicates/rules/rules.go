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

package rules

import (
	"strings"

	"k8s.io/api/admissionregistration/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
)

// Matcher determines if the Attr matches the Rule.
// Rule匹配器，通过WebhookConfiguration.rules指定规则
type Matcher struct {
	Rule v1.RuleWithOperations // 通过WebhookConfiguration.rules指定规则
	Attr admission.Attributes
}

// Matches returns if the Attr matches the Rule.
func (r *Matcher) Matches() bool {
	return r.scope() &&
		r.operation() &&
		r.group() &&
		r.version() &&
		r.resource()
}

func exactOrWildcard(items []string, requested string) bool {
	for _, item := range items {
		if item == "*" {
			return true
		}
		if item == requested {
			return true
		}
	}

	return false
}

var namespaceResource = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "namespaces"}

func (r *Matcher) scope() bool {
	// 如果当前没有指定资源作用域，认为匹配成功
	if r.Rule.Scope == nil || *r.Rule.Scope == v1.AllScopes {
		return true
	}
	// attr.GetNamespace() is set to the name of the namespace for requests of the namespace object itself.
	// 如果指定了资源作用域，那么当前请求的作用域必须匹配
	switch *r.Rule.Scope {
	case v1.NamespacedScope:
		// first make sure that we are not requesting a namespace object (namespace objects are cluster-scoped)
		return r.Attr.GetResource() != namespaceResource && r.Attr.GetNamespace() != metav1.NamespaceNone
	case v1.ClusterScope:
		// also return true if the request is for a namespace object (namespace objects are cluster-scoped)
		return r.Attr.GetResource() == namespaceResource || r.Attr.GetNamespace() == metav1.NamespaceNone
	default:
		return false
	}
}

// 匹配组
func (r *Matcher) group() bool {
	return exactOrWildcard(r.Rule.APIGroups, r.Attr.GetResource().Group)
}

// 匹配版本
func (r *Matcher) version() bool {
	return exactOrWildcard(r.Rule.APIVersions, r.Attr.GetResource().Version)
}

// 匹配Operation
func (r *Matcher) operation() bool {
	attrOp := r.Attr.GetOperation()
	for _, op := range r.Rule.Operations {
		if op == v1.OperationAll {
			return true
		}
		// The constants are the same such that this is a valid cast (and this
		// is tested).
		if op == v1.OperationType(attrOp) {
			return true
		}
	}
	return false
}

func splitResource(resSub string) (res, sub string) {
	parts := strings.SplitN(resSub, "/", 2)
	if len(parts) == 2 {
		return parts[0], parts[1]
	}
	return parts[0], ""
}

// 匹配资源
func (r *Matcher) resource() bool {
	opRes, opSub := r.Attr.GetResource().Resource, r.Attr.GetSubresource()
	for _, res := range r.Rule.Resources {
		res, sub := splitResource(res)
		resMatch := res == "*" || res == opRes
		subMatch := sub == "*" || sub == opSub
		if resMatch && subMatch {
			return true
		}
	}
	return false
}

// IsExemptAdmissionConfigurationResource determines if an admission.Attributes object is describing
// the admission of a ValidatingWebhookConfiguration or a MutatingWebhookConfiguration or a ValidatingAdmissionPolicy or a ValidatingAdmissionPolicyBinding
func IsExemptAdmissionConfigurationResource(attr admission.Attributes) bool {
	gvk := attr.GetKind()
	if gvk.Group == "admissionregistration.k8s.io" {
		if gvk.Kind == "ValidatingWebhookConfiguration" || gvk.Kind == "MutatingWebhookConfiguration" ||
			gvk.Kind == "ValidatingAdmissionPolicy" || gvk.Kind == "ValidatingAdmissionPolicyBinding" {
			return true
		}
	}
	return false
}
