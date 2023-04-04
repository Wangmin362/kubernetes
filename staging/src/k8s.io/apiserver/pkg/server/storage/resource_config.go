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
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// APIResourceConfigSource is the interface to determine which groups and versions are enabled
// APIResourceConfigSource 用于判断某个资源或者某个组的所有资源是否启用
type APIResourceConfigSource interface {
	// ResourceEnabled 资源是否启用
	ResourceEnabled(resource schema.GroupVersionResource) bool
	// AnyResourceForGroupEnabled 这个接口应该是上面的接口快速判断吧，如果一个组下的所有资源都是启用状态，那么当前API就应该返回true
	AnyResourceForGroupEnabled(group string) bool
}

var _ APIResourceConfigSource = &ResourceConfig{}

type ResourceConfig struct {
	GroupVersionConfigs map[schema.GroupVersion]bool
	ResourceConfigs     map[schema.GroupVersionResource]bool
}

func NewResourceConfig() *ResourceConfig {
	return &ResourceConfig{GroupVersionConfigs: map[schema.GroupVersion]bool{}, ResourceConfigs: map[schema.GroupVersionResource]bool{}}
}

// DisableMatchingVersions disables all group/versions for which the matcher function returns true.
// This will remove any preferences previously set on individual resources.
func (o *ResourceConfig) DisableMatchingVersions(matcher func(gv schema.GroupVersion) bool) {
	for groupVersion := range o.GroupVersionConfigs {
		// 如果匹配上了，说明需要禁用这个组
		if matcher(groupVersion) {
			// 禁用这个组
			o.GroupVersionConfigs[groupVersion] = false
			// 既然组被禁用了，那么组下的每一个资源都应该被禁用 TODO 没有搞懂的是，为啥是组删除所有的资源
			o.removeMatchingResourcePreferences(resourceMatcherForVersion(groupVersion))
		}
	}
}

// EnableMatchingVersions enables all group/versions for which the matcher function returns true.
// This will remove any preferences previously set on individual resources.
func (o *ResourceConfig) EnableMatchingVersions(matcher func(gv schema.GroupVersion) bool) {
	for version := range o.GroupVersionConfigs {
		if matcher(version) {
			o.GroupVersionConfigs[version] = true
			o.removeMatchingResourcePreferences(resourceMatcherForVersion(version))
		}
	}
}

// resourceMatcherForVersion matches resources in the specified version
func resourceMatcherForVersion(gv schema.GroupVersion) func(gvr schema.GroupVersionResource) bool {
	return func(gvr schema.GroupVersionResource) bool {
		return gv == gvr.GroupVersion()
	}
}

// removeMatchingResourcePreferences removes individual resource preferences that match.  This is useful when an override of a version or level enablement should
// override the previously individual preferences.
func (o *ResourceConfig) removeMatchingResourcePreferences(matcher func(gvr schema.GroupVersionResource) bool) {
	var keysToRemove []schema.GroupVersionResource
	for k := range o.ResourceConfigs {
		// 如果当前资源的匹配上了组
		if matcher(k) {
			keysToRemove = append(keysToRemove, k)
		}
	}
	// TODO 卧槽，这里为啥是移除，上面直接设置为false不可以么？
	for _, k := range keysToRemove {
		delete(o.ResourceConfigs, k)
	}
}

// DisableVersions disables the versions entirely.
// This will remove any preferences previously set on individual resources.
func (o *ResourceConfig) DisableVersions(versions ...schema.GroupVersion) {
	for _, version := range versions {
		o.GroupVersionConfigs[version] = false

		// a preference about a version takes priority over the previously set resources
		o.removeMatchingResourcePreferences(resourceMatcherForVersion(version))
	}
}

// EnableVersions enables all resources in a given groupVersion.
// This will remove any preferences previously set on individual resources.
func (o *ResourceConfig) EnableVersions(versions ...schema.GroupVersion) {
	for _, version := range versions {
		o.GroupVersionConfigs[version] = true

		// a preference about a version takes priority over the previously set resources
		o.removeMatchingResourcePreferences(resourceMatcherForVersion(version))
	}

}

// TODO this must be removed and we enable/disable individual resources.
func (o *ResourceConfig) versionEnabled(version schema.GroupVersion) bool {
	enabled, _ := o.GroupVersionConfigs[version]
	return enabled
}

func (o *ResourceConfig) DisableResources(resources ...schema.GroupVersionResource) {
	for _, resource := range resources {
		o.ResourceConfigs[resource] = false
	}
}

func (o *ResourceConfig) EnableResources(resources ...schema.GroupVersionResource) {
	for _, resource := range resources {
		o.ResourceConfigs[resource] = true
	}
}

func (o *ResourceConfig) ResourceEnabled(resource schema.GroupVersionResource) bool {
	// if a resource is explicitly set, that takes priority over the preference of the version.
	resourceEnabled, explicitlySet := o.ResourceConfigs[resource]
	if explicitlySet {
		return resourceEnabled
	}

	// 如果整个组都是禁用的，那么就认为此资源是禁用状态
	if !o.versionEnabled(resource.GroupVersion()) {
		return false
	}
	// they are enabled by default.
	return true
}

// AnyResourceForGroupEnabled 用于判断整个组是否启用
func (o *ResourceConfig) AnyResourceForGroupEnabled(group string) bool {
	for groupVersion := range o.GroupVersionConfigs {
		if groupVersion.Group == group {
			if o.versionEnabled(groupVersion) {
				return true
			}
		}
	}
	for resource := range o.ResourceConfigs {
		if resource.Group == group && o.ResourceEnabled(resource) {
			return true
		}
	}

	return false
}
