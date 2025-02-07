/*
Copyright 2016 The Kubernetes Authors.

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

package storage

import (
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// ResourceEncodingConfig
// TODO 如何理解这里所谓的编码配置
type ResourceEncodingConfig interface {
	// StorageEncodingFor returns the serialization format for the resource.
	// TODO this should actually return a GroupVersionKind since you can logically have multiple "matching" Kinds
	// For now, it returns just the GroupVersion for consistency with old behavior
	// 返回资源优先选择的版本
	StorageEncodingFor(schema.GroupResource) (schema.GroupVersion, error)

	// InMemoryEncodingFor returns the groupVersion for the in memory representation the storage should convert to.
	// 返回资源的内部版本，Version=__internal
	InMemoryEncodingFor(schema.GroupResource) (schema.GroupVersion, error)
}

type DefaultResourceEncodingConfig struct {
	// resources records the overriding encoding configs for individual resources.
	// TODO 这玩意似乎专门针对特殊资源的设置
	resources map[schema.GroupResource]*OverridingResourceEncoding
	scheme    *runtime.Scheme
}

type OverridingResourceEncoding struct {
	ExternalResourceEncoding schema.GroupVersion
	InternalResourceEncoding schema.GroupVersion
}

var _ ResourceEncodingConfig = &DefaultResourceEncodingConfig{}

func NewDefaultResourceEncodingConfig(scheme *runtime.Scheme) *DefaultResourceEncodingConfig {
	return &DefaultResourceEncodingConfig{
		resources: map[schema.GroupResource]*OverridingResourceEncoding{},
		scheme:    scheme,
	}
}

// SetResourceEncoding 注册外部资源和内部资源的映射关系，普通用户使用的都是外部资源，在K8S中使用内部资源(__internal)
/*
  目前只有下面几个资源进行了单独的配置：
	admissionregistration.Resource("validatingadmissionpolicies").WithVersion("v1alpha1"),
	admissionregistration.Resource("validatingadmissionpolicybindings").WithVersion("v1alpha1"),
	networking.Resource("clustercidrs").WithVersion("v1alpha1"),
	networking.Resource("ipaddresses").WithVersion("v1alpha1"),
	certificates.Resource("clustertrustbundles").WithVersion("v1alpha1"),
*/
func (o *DefaultResourceEncodingConfig) SetResourceEncoding(
	resourceBeingStored schema.GroupResource,
	externalEncodingVersion,
	internalVersion schema.GroupVersion,
) {
	o.resources[resourceBeingStored] = &OverridingResourceEncoding{
		ExternalResourceEncoding: externalEncodingVersion,
		InternalResourceEncoding: internalVersion,
	}
}

func (o *DefaultResourceEncodingConfig) StorageEncodingFor(resource schema.GroupResource) (schema.GroupVersion, error) {
	// 先判断当前资源所在的是否存在，如果不存在，那肯定找不到与之对应的外部版本
	if !o.scheme.IsGroupRegistered(resource.Group) {
		return schema.GroupVersion{}, fmt.Errorf("group %q is not registered in scheme", resource.Group)
	}

	// 如果当前资源进行了特殊设置，那么直接返回外部资源配置
	resourceOverride, resourceExists := o.resources[resource]
	if resourceExists {
		return resourceOverride.ExternalResourceEncoding, nil
	}

	// return the most preferred external version for the group
	// 如果不存在，那么直接从scheme中获取组的所有GV，并取出第一个
	return o.scheme.PrioritizedVersionsForGroup(resource.Group)[0], nil
}

func (o *DefaultResourceEncodingConfig) InMemoryEncodingFor(resource schema.GroupResource) (schema.GroupVersion, error) {
	// 先判断当前资源所在的是否存在，如果不存在，那肯定找不到与之对应的内部版本
	if !o.scheme.IsGroupRegistered(resource.Group) {
		return schema.GroupVersion{}, fmt.Errorf("group %q is not registered in scheme", resource.Group)
	}

	resourceOverride, resourceExists := o.resources[resource]
	if resourceExists {
		return resourceOverride.InternalResourceEncoding, nil
	}
	return schema.GroupVersion{Group: resource.Group, Version: runtime.APIVersionInternal}, nil
}
