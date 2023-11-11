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

	apidiscoveryv2beta1 "k8s.io/api/apidiscovery/v2beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/managedfields"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/endpoints/discovery"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/apiserver/pkg/storageversion"
)

// ConvertabilityChecker indicates what versions a GroupKind is available in.
// 找到一个GK的所有版本，并且按照优先级排序，显然，这玩意一定是从Scheme当中获取的，因为只有在Scheme中保存了一个资源不同版本的优先级
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
// TODO 为什么需要抽象出这个对象？
// 1、APIGroupVersion包含了/<group>/<version>下所有资源的增删改查操作，group, version都是确定的
type APIGroupVersion struct {
	// 1、这里的Key应该就是资源，而Value则是每个资源对应的增删改查操作。这里的key可能是资源（譬如deployment），也有可能是子资源，譬如（deployment/scale）
	// 2、其实就是/<group>/<version>下面的所有资源
	Storage map[string]rest.Storage

	// 1、路由前缀，目前K8S中只存在两种前缀，对于核心资源而言，前缀为/api（核心资源也被称之为Legacy资源），而对于其它资源
	// 前缀为/apis
	Root string

	// GroupVersion is the external group version
	// 当前的Group, Version信息
	// TODO GroupVersion,OptionsExternalVersion,MetaGroupVersion有何不同？
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
	// TODO 这玩意干嘛的？
	RootScopedKinds sets.String

	// Serializer is used to determine how to convert responses from API methods into bytes to send over
	// the wire.
	Serializer     runtime.NegotiatedSerializer
	ParameterCodec runtime.ParameterCodec

	Typer                 runtime.ObjectTyper     // 用于获取资源的GVK，以及判断资源是否是可识别资源
	Creater               runtime.ObjectCreater   // 用于创建资源
	Convertor             runtime.ObjectConvertor // 用于资源的转换、以及判断资源的某个标签是否可以作为标签选择器
	ConvertabilityChecker ConvertabilityChecker   // 用于获取一个资源的的所有版本，并且按照优先级排序。最前面的版本优先级最高
	Defaulter             runtime.ObjectDefaulter // 用于为资源设置默认值
	Namer                 runtime.Namer           // 用于获取资源名
	UnsafeConvertor       runtime.ObjectConvertor // TODO Converter和UnsafeConverter有何不同？
	TypeConverter         managedfields.TypeConverter

	// TODO 等效资源注册中心
	EquivalentResourceRegistry runtime.EquivalentResourceRegistry

	// Authorizer determines whether a user is allowed to make a certain request. The Handler does a preliminary
	// authorization check using the request URI but it may be necessary to make additional checks, such as in
	// the create-on-update case
	// TODO 鉴权 意思是每种资源可以单独设置鉴权？
	Authorizer authorizer.Authorizer

	// TODO 可以对每个组进行准入控制？
	Admit admission.Interface

	MinRequestTimeout time.Duration

	// The limit on the request body size that would be accepted and decoded in a write request.
	// 0 means no limit.
	// 请求提最多可以接受的字节数，默认一般设置为3MB
	MaxRequestBodyBytes int64
}

// InstallREST registers the REST handlers (storage, watch, proxy and redirect) into a restful Container.
// It is expected that the provided path root prefix will serve all operations. Root MUST NOT end
// in a slash.
func (g *APIGroupVersion) InstallREST(container *restful.Container) (
	[]apidiscoveryv2beta1.APIResourceDiscovery,
	[]*storageversion.ResourceInfo,
	error) {
	// 1、路由前缀为：/<root>/<group>/<version>，对于核心资源（也被称之为Legacy资源），Root就是/api，除了核心资源，
	// 其余资源的Root为/apis
	prefix := path.Join(g.Root, g.GroupVersion.Group, g.GroupVersion.Version)
	installer := &APIInstaller{
		group:             g,
		prefix:            prefix,
		minRequestTimeout: g.MinRequestTimeout,
	}

	// 路由当前资源组下所有资源的路由
	apiResources, resourceInfos, ws, registrationErrors := installer.Install()

	// 注册/<root>/<group>/<version>路由，此路由用于返回某个组的某个版本下的所有可用资源信息
	versionDiscoveryHandler := discovery.NewAPIVersionHandler(g.Serializer, g.GroupVersion, staticLister{apiResources})
	versionDiscoveryHandler.AddToWebService(ws)
	container.Add(ws)

	aggregatedDiscoveryResources, err := ConvertGroupVersionIntoToDiscovery(apiResources)
	if err != nil {
		registrationErrors = append(registrationErrors, err)
	}
	return aggregatedDiscoveryResources, removeNonPersistedResources(resourceInfos), utilerrors.NewAggregate(registrationErrors)
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
