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

package endpoints

import (
	"path"
	"time"

	restful "github.com/emicklei/go-restful/v3"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/endpoints/discovery"
	"k8s.io/apiserver/pkg/endpoints/handlers/fieldmanager"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storageversion"
	openapiproto "k8s.io/kube-openapi/pkg/util/proto"
)

// ConvertabilityChecker indicates what versions a GroupKind is available in.
type ConvertabilityChecker interface {
	// VersionsForGroupKind indicates what versions are available to convert a group kind. This determines
	// what our decoding abilities are.
	VersionsForGroupKind(gk schema.GroupKind) []schema.GroupVersion
}

// APIGroupVersion is a helper for exposing rest.Storage objects as http.Handlers via go-restful
// It handles URLs of the form:
// /${storage_key}[/${object_name}]
// Where 'storage_key' points to a rest.Storage object stored in storage.
// This object should contain all parameterization necessary for running a particular API version
// 1、实际上APIGroupVersion就是APIGroupInfo的子集，ApiGroupInfo中保存的是关于一个组下的所有版本的所有资源的增删改查Handler,而
// APIGroupVersion则保存的是关于一个组的某个具体的所有资源的增删改查Handler
// 2、TODO 详细分析APIGroupVersion中所有属性的作用
type APIGroupVersion struct {
	// key为resource，譬如pods, deployments，value则是各个资源的存储，实际上可以理解为资源的增删改查的Handler
	Storage map[string]rest.Storage

	// 当前组的API前缀是啥，其实在K8S当中只有两种，第一种是/api，也就是所谓的Legacy API,另外一种就是/apis
	Root string

	// GroupVersion is the external group version
	// 当前API组信息是哪个组的
	GroupVersion schema.GroupVersion

	// OptionsExternalVersion controls the Kubernetes APIVersion used for common objects in the apiserver
	// schema like api.Status, api.DeleteOptions, and metav1.ListOptions. Other implementors may
	// define a version "v1beta1" but want to use the Kubernetes "v1" internal objects. If
	// empty, defaults to GroupVersion.
	OptionsExternalVersion *schema.GroupVersion
	// MetaGroupVersion defaults to "meta.k8s.io/v1" and is the scheme group version used to decode
	// common API implementations like ListOptions. Future changes will allow this to vary by group
	// version (for when the inevitable meta/v2 group emerges).
	MetaGroupVersion *schema.GroupVersion

	// RootScopedKinds are the root scoped kinds for the primary GroupVersion
	RootScopedKinds sets.String

	// Serializer is used to determine how to convert responses from API methods into bytes to send over
	// the wire.
	// BODY编解码器
	Serializer runtime.NegotiatedSerializer
	// 参数编解码器 TODO 为什么参数编解码器可以做到通用？
	ParameterCodec runtime.ParameterCodec

	Typer   runtime.ObjectTyper
	Creater runtime.ObjectCreater
	// TODO Convertor应该是负责ExternalVersion和Internal Version之间的转换
	Convertor             runtime.ObjectConvertor
	ConvertabilityChecker ConvertabilityChecker
	Defaulter             runtime.ObjectDefaulter
	Namer                 runtime.Namer
	UnsafeConvertor       runtime.ObjectConvertor
	TypeConverter         fieldmanager.TypeConverter

	EquivalentResourceRegistry runtime.EquivalentResourceRegistry

	// Authorizer determines whether a user is allowed to make a certain request. The Handler does a preliminary
	// authorization check using the request URI but it may be necessary to make additional checks, such as in
	// the create-on-update case
	// TODO 仔细分析
	Authorizer authorizer.Authorizer

	// TODO 仔细分析
	Admit admission.Interface

	MinRequestTimeout time.Duration

	// OpenAPIModels exposes the OpenAPI models to each individual handler.
	OpenAPIModels openapiproto.Models

	// The limit on the request body size that would be accepted and decoded in a write request.
	// 0 means no limit.
	MaxRequestBodyBytes int64
}

// InstallREST registers the REST handlers (storage, watch, proxy and redirect) into a restful Container.
// It is expected that the provided path root prefix will serve all operations. Root MUST NOT end
// in a slash.
// 把APIGroupVersion中保存的的资源注册到container当中
func (g *APIGroupVersion) InstallREST(container *restful.Container) ([]*storageversion.ResourceInfo, error) {
	// 拼接路由前缀 /<prefix>/<group>/<version>, 如果当前注册的是Legacy资源，那么它的group为""
	// 由于APIGroupVersion是一个组下某本的所有资源，因此prefix前缀势必是同样的
	prefix := path.Join(g.Root, g.GroupVersion.Group, g.GroupVersion.Version)
	// 委托给APIInstaller完成信息的注册
	installer := &APIInstaller{
		group:             g,
		prefix:            prefix, // /<prefix>/<group>/<version>
		minRequestTimeout: g.MinRequestTimeout,
	}

	// TODO 为APIInstaller生成路由信息
	apiResources, resourceInfos, ws, registrationErrors := installer.Install()

	// 向ws中增加/<prefix>/<group>/<version>/路由信息，返回当前gv下的所有资源信息，用于当用户请求/<prefix>/<group>/<version>/路径时，
	// 返回当前组下的某个组的所有资源信息
	versionDiscoveryHandler := discovery.NewAPIVersionHandler(g.Serializer, g.GroupVersion, staticLister{apiResources})
	versionDiscoveryHandler.AddToWebService(ws)

	// 添加路由信息到container当中
	container.Add(ws)
	return removeNonPersistedResources(resourceInfos), utilerrors.NewAggregate(registrationErrors)
}

func removeNonPersistedResources(infos []*storageversion.ResourceInfo) []*storageversion.ResourceInfo {
	var filtered []*storageversion.ResourceInfo
	for _, info := range infos {
		// if EncodingVersion is empty, then the apiserver does not
		// need to register this resource via the storage version API,
		// thus we can remove it.
		if info != nil && len(info.EncodingVersion) > 0 {
			filtered = append(filtered, info)
		}
	}
	return filtered
}

// staticLister implements the APIResourceLister interface
type staticLister struct {
	list []metav1.APIResource
}

func (s staticLister) ListAPIResources() []metav1.APIResource {
	return s.list
}

var _ discovery.APIResourceLister = &staticLister{}
