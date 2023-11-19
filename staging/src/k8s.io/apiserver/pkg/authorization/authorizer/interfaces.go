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

package authorizer

import (
	"context"
	"net/http"

	"k8s.io/apiserver/pkg/authentication/user"
)

// Attributes is an interface used by an Authorizer to get information about a request
// that is used to make an authorization decision.
type Attributes interface {
	// GetUser returns the user.Info object to authorize
	GetUser() user.Info

	// GetVerb returns the kube verb associated with API requests (this includes get, list, watch, create, update, patch, delete, deletecollection, and proxy),
	// or the lowercased HTTP verb associated with non-API requests (this includes get, put, post, patch, and delete)
	GetVerb() string

	// IsReadOnly When IsReadOnly() == true, the request has no side effects, other than
	// caching, logging, and other incidentals.
	IsReadOnly() bool

	// The namespace of the object, if a request is for a REST object.
	GetNamespace() string

	// The kind of object, if a request is for a REST object.
	GetResource() string

	// GetSubresource returns the subresource being requested, if present
	GetSubresource() string

	// GetName returns the name of the object as parsed off the request.  This will not be present for all request types, but
	// will be present for: get, update, delete
	GetName() string

	// The group of the resource, if a request is for a REST object.
	GetAPIGroup() string

	// GetAPIVersion returns the version of the group requested, if a request is for a REST object.
	GetAPIVersion() string

	// IsResourceRequest returns true for requests to API resources, like /api/v1/nodes,
	// and false for non-resource endpoints like /api, /healthz
	IsResourceRequest() bool

	// GetPath returns the path of the request
	GetPath() string
}

// Authorizer makes an authorization decision based on information gained by making
// zero or more calls to methods of the Attributes interface.  It returns nil when an action is
// authorized, otherwise it returns an error.
// 1、APIServer中实现了：Node, ABAC, RBAC, Webhook, AlwaysDeny, AlwaysAllow这集中鉴权器
// 2、APIServer在初始化过程中，会根据用户启用的鉴权模式，对所有启用的鉴权模式实例化一个鉴权器
// 3、APIServer最终使用的鉴权器实际上是UnionAuthorizer，这玩意就一个鉴权器数组，在实际鉴权的时候，UnionAuthorizer把请求挨个
// 交给其中的每一个鉴权器进行鉴权，只要其中一个鉴权器鉴权成功，就认为本次请求鉴权成功。
type Authorizer interface {
	Authorize(ctx context.Context, a Attributes) (authorized Decision, reason string, err error)
}

type AuthorizerFunc func(ctx context.Context, a Attributes) (Decision, string, error)

func (f AuthorizerFunc) Authorize(ctx context.Context, a Attributes) (Decision, string, error) {
	return f(ctx, a)
}

// RuleResolver provides a mechanism for resolving the list of rules that apply to a given user within a namespace.
// TODO 如何理解RuleResolver这种设计？
type RuleResolver interface {
	// RulesFor get the list of cluster wide rules, the list of rules in the specific namespace, incomplete status and errors.
	RulesFor(user user.Info, namespace string) ([]ResourceRuleInfo, []NonResourceRuleInfo, bool, error)
}

// RequestAttributesGetter provides a function that extracts Attributes from an http.Request
// 用于从请求信息当中抽取某些关键属性用于鉴权
type RequestAttributesGetter interface {
	GetRequestAttributes(user.Info, *http.Request) Attributes
}

// AttributesRecord implements Attributes interface.
type AttributesRecord struct {
	// 用户信息，想要对于当前的请求进行鉴权，那么肯定要知道发出当前请求的用户是谁？
	// 当APIServer对当前请求认证通过之后，用户信息会被填充进来
	User            user.Info
	Verb            string // 当前请求想要执行的动作，譬如get, list, create, watch, patch, delete等动作
	Namespace       string // 当前请求要操作的资源所在的名称空间
	APIGroup        string // 当前请求要操作资源的group
	APIVersion      string // 当前请求要操作资源的版本
	Resource        string // 当前请求要操作的资源
	Subresource     string // 当前请求要操作的子资源
	Name            string // TODO 这个字段代表着谁的名字？
	ResourceRequest bool   // TODO 这个字段是啥意思？
	Path            string // 这里应该是URL路径
}

func (a AttributesRecord) GetUser() user.Info {
	return a.User
}

func (a AttributesRecord) GetVerb() string {
	return a.Verb
}

func (a AttributesRecord) IsReadOnly() bool {
	return a.Verb == "get" || a.Verb == "list" || a.Verb == "watch"
}

func (a AttributesRecord) GetNamespace() string {
	return a.Namespace
}

func (a AttributesRecord) GetResource() string {
	return a.Resource
}

func (a AttributesRecord) GetSubresource() string {
	return a.Subresource
}

func (a AttributesRecord) GetName() string {
	return a.Name
}

func (a AttributesRecord) GetAPIGroup() string {
	return a.APIGroup
}

func (a AttributesRecord) GetAPIVersion() string {
	return a.APIVersion
}

func (a AttributesRecord) IsResourceRequest() bool {
	return a.ResourceRequest
}

func (a AttributesRecord) GetPath() string {
	return a.Path
}

type Decision int

const (
	// DecisionDeny means that an authorizer decided to deny the action.
	DecisionDeny Decision = iota
	// DecisionAllow means that an authorizer decided to allow the action.
	DecisionAllow
	// DecisionNoOpionion means that an authorizer has no opinion on whether
	// to allow or deny an action.
	DecisionNoOpinion
)
