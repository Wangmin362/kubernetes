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

package admission

import (
	"context"
	"io"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	auditinternal "k8s.io/apiserver/pkg/apis/audit"
	"k8s.io/apiserver/pkg/authentication/user"
)

// Attributes is an interface used by AdmissionController to get information about a request
// that is used to make an admission decision.
// 1、用于获取当前请求的各种属性
type Attributes interface {
	// GetName returns the name of the object as presented in the request.  On a CREATE operation, the client
	// may omit name and rely on the server to generate the name.  If that is the case, this method will return
	// the empty string
	// 获取当前资源的名字，对于Create操作，有可能是由APIServer产生名字，因此这个字段可能是空的
	GetName() string
	// GetNamespace is the namespace associated with the request (if any)
	// 获取当前资源操作的名称空间
	GetNamespace() string
	// GetResource is the name of the resource being requested.  This is not the kind.  For example: pods
	// 获取当前资源请求的GVR
	GetResource() schema.GroupVersionResource
	// GetSubresource is the name of the subresource being requested.  This is a different resource, scoped to the parent resource, but it may have a different kind.
	// For instance, /pods has the resource "pods" and the kind "Pod", while /pods/foo/status has the resource "pods", the sub resource "status", and the kind "Pod"
	// (because status operates on pods). The binding resource for a pod though may be /pods/foo/binding, which has resource "pods", subresource "binding", and kind "Binding".
	// 获取当前子资源名，如果不是操作子资源，这里肯定为空
	GetSubresource() string
	// GetOperation is the operation being performed
	// 获取当前资源的操作，CREATE, UPDATE, DELETE, CONNECT
	GetOperation() Operation
	// GetOperationOptions is the options for the operation being performed
	// 获取当前资源操作的参数配置
	GetOperationOptions() runtime.Object
	// IsDryRun indicates that modifications will definitely not be persisted for this request. This is to prevent
	// admission controllers with side effects and a method of reconciliation from being overwhelmed.
	// However, a value of false for this does not mean that the modification will be persisted, because it
	// could still be rejected by a subsequent validation step.
	// 当前请求是否是DryRun?
	IsDryRun() bool
	// GetObject is the object from the incoming request prior to default values being applied
	// 获取当前资源
	GetObject() runtime.Object
	// GetOldObject is the existing object. Only populated for UPDATE and DELETE requests.
	// 获取老资源，这个属性仅针对于UPDATE, DELETE接口
	GetOldObject() runtime.Object
	// GetKind is the type of object being manipulated.  For example: Pod
	GetKind() schema.GroupVersionKind
	// GetUserInfo is information about the requesting user
	// 获取当前请求的用户信息
	GetUserInfo() user.Info

	// AddAnnotation sets annotation according to key-value pair. The key should be qualified, e.g., podsecuritypolicy.admission.k8s.io/admit-policy, where
	// "podsecuritypolicy" is the name of the plugin, "admission.k8s.io" is the name of the organization, "admit-policy" is the key name.
	// An error is returned if the format of key is invalid. When trying to overwrite annotation with a new value, an error is returned.
	// Both ValidationInterface and MutationInterface are allowed to add Annotations.
	// By default, an annotation gets logged into audit event if the request's audit level is greater or
	// equal to Metadata.
	// 添加Annotation
	AddAnnotation(key, value string) error

	// AddAnnotationWithLevel sets annotation according to key-value pair with additional intended audit level.
	// An Annotation gets logged into audit event if the request's audit level is greater or equal to the
	// intended audit level.
	AddAnnotationWithLevel(key, value string, level auditinternal.Level) error

	// GetReinvocationContext tracks the admission request information relevant to the re-invocation policy.
	// 获取重新调用上下文
	GetReinvocationContext() ReinvocationContext
}

// ObjectInterfaces is an interface used by AdmissionController to get object interfaces
// such as Converter or Defaulter. These interfaces are normally coming from Request Scope
// to handle special cases like CRDs.
// TODO 这玩意估计是通过Scheme实现的
type ObjectInterfaces interface {
	// GetObjectCreater is the ObjectCreator appropriate for the requested object.
	GetObjectCreater() runtime.ObjectCreater
	// GetObjectTyper is the ObjectTyper appropriate for the requested object.
	GetObjectTyper() runtime.ObjectTyper
	// GetObjectDefaulter is the ObjectDefaulter appropriate for the requested object.
	GetObjectDefaulter() runtime.ObjectDefaulter
	// GetObjectConvertor is the ObjectConvertor appropriate for the requested object.
	GetObjectConvertor() runtime.ObjectConvertor
	// GetEquivalentResourceMapper is the EquivalentResourceMapper appropriate for finding equivalent resources and expected kind for the requested object.
	GetEquivalentResourceMapper() runtime.EquivalentResourceMapper
}

// privateAnnotationsGetter is a private interface which allows users to get annotations from Attributes.
type privateAnnotationsGetter interface {
	getAnnotations(maxLevel auditinternal.Level) map[string]string
}

// AnnotationsGetter allows users to get annotations from Attributes. An alternate Attribute should implement
// this interface.
type AnnotationsGetter interface {
	GetAnnotations(maxLevel auditinternal.Level) map[string]string
}

// ReinvocationContext provides access to the admission related state required to implement the re-invocation policy.
// 1、插件再次被调用的的上下文
type ReinvocationContext interface {
	// IsReinvoke returns true if the current admission check is a re-invocation.
	// 返回准入控制器当前的执行是否是再次调用
	IsReinvoke() bool
	// SetIsReinvoke sets the current admission check as a re-invocation.
	// 用于设置当前准入控制插件的执行是再次执行
	SetIsReinvoke()
	// ShouldReinvoke returns true if any plugin has requested a re-invocation.
	// 返回当前准入控制插件是否应该被再次调用？
	ShouldReinvoke() bool
	// SetShouldReinvoke signals that a re-invocation is desired.
	// 设置当前准入控制插件是否能够被再次调用
	SetShouldReinvoke()
	// SetValue set a value for a plugin name, possibly overriding a previous value.
	// 设置准入插件的民资
	SetValue(plugin string, v interface{})
	// Value reads a value for a webhook.
	Value(plugin string) interface{}
}

// Interface is an abstract, pluggable interface for Admission Control decisions.
// 1、准入控制插件顶级抽象
// 2、实际上我认为Interface并非是准入控制插件的顶级抽象，而是准入控制插件必备的一种属性。无论是对于哪一种准入控制插件，都需要实现这个特性。
// 真正的准入控制插件应该是MutationInterface以及ValidationInterface这两个顶级抽象。这里的Interface抽象出了最基本的特性，但是在实际使用
// 中还需要实际执行准入判断，准入判断的抽象就是MutationInterface以及ValidationInterface这两个接口。
// 3、需要注意的是，准入控制插件既可以是MutationInterface,也可以是ValidationInterface
type Interface interface {
	// Handles returns true if this admission controller can handle the given operation
	// where operation can be one of CREATE, UPDATE, DELETE, or CONNECT
	// TODO 对于插件来说，支持不同的插件以为着什么？
	Handles(operation Operation) bool
}

// MutationInterface
// 1、修改类型的准入控制插件顶级抽象，通过Admit方法来进行准入控制
// 2、在K8S当中，修改类型的准入控制插件是穿行执行的，因为一个准入插件的执行结构很有可能会影响另外一个准入插件的执行结果。
// TODO K8S是如何控制准入插件的执行顺序的？
type MutationInterface interface {
	Interface

	// Admit makes an admission decision based on the request attributes.
	// Context is used only for timeout/deadline/cancellation and tracing information.
	Admit(ctx context.Context, a Attributes, o ObjectInterfaces) (err error)
}

// ValidationInterface is an abstract, pluggable interface for Admission Control decisions.
// 1、验证类型的准入控制插件顶级抽象，通过Admit方法来进行准入控制
// 2、在K8S中，验证类型的准入插件是并行执行的，因为是验证类型的准入控制插件并不会修改原始的资源对象，因此一个准入控制插件的执行结果并不会影响
// 另外的准入控制插件的结果。显然，但凡有一个准入控制插件不允许通过，那么这个请求将会被拒绝。除非所有的准入控制插件都同意验证通过才认为验证通过
type ValidationInterface interface {
	Interface

	// Validate makes an admission decision based on the request attributes.  It is NOT allowed to mutate
	// Context is used only for timeout/deadline/cancellation and tracing information.
	Validate(ctx context.Context, a Attributes, o ObjectInterfaces) (err error)
}

// Operation is the type of resource operation being checked for admission control
// TODO 如何理解操作？
// 1、这里的操作实际上对应的资源的增删改查操作，对于K8S来说，GET, LIST, WATCH都是查询的操作，无需做准入控制。本来，准入控制插件的目的就是为了
// 对于修改类型的请求做修改或者验证。而查询类型的请求查询的是系统存好的数据，肯定是合法的。所以无需验证，也不能修改。
type Operation string

// Operation constants
const (
	Create  Operation = "CREATE"
	Update  Operation = "UPDATE"
	Delete  Operation = "DELETE"
	Connect Operation = "CONNECT"
)

// PluginInitializer is used for initialization of shareable resources between admission plugins.
// After initialization the resources have to be set separately
// 1、所谓的插件初始化器，实际上就是为了跟插件注入某些依赖，帮助插件完成初始化，这里取名为Initialization也算是比较合理
type PluginInitializer interface {
	Initialize(plugin Interface)
}

// InitializationValidator holds ValidateInitialization functions, which are responsible for validation of initialized
// shared resources and should be implemented on admission plugins
// 用于判断插件是否初始化完成
type InitializationValidator interface {
	ValidateInitialization() error
}

// ConfigProvider provides a way to get configuration for an admission plugin based on its name
// 1、用于向插件提供配置，有些插件可以提供配置文件来配置
// 2、这个配置其实是从插件配置文件当中获取的
type ConfigProvider interface {
	ConfigFor(pluginName string) (io.Reader, error)
}
