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

package options

import (
	"fmt"
	"strings"

	"github.com/spf13/pflag"

	"k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/resourceconfig"
	serverstore "k8s.io/apiserver/pkg/server/storage"
	cliflag "k8s.io/component-base/cli/flag"
)

// APIEnablementOptions contains the options for which resources to turn on and off.
// Given small aggregated API servers, this option isn't required for "normal" API servers
// 用于用户设置启用/禁用API
type APIEnablementOptions struct {
	RuntimeConfig cliflag.ConfigurationMap
}

func NewAPIEnablementOptions() *APIEnablementOptions {
	return &APIEnablementOptions{
		RuntimeConfig: make(cliflag.ConfigurationMap),
	}
}

// AddFlags adds flags for a specific APIServer to the specified FlagSet
func (s *APIEnablementOptions) AddFlags(fs *pflag.FlagSet) {
	fs.Var(&s.RuntimeConfig, "runtime-config", ""+
		"A set of key=value pairs that enable or disable built-in APIs. Supported options are:\n"+
		"v1=true|false for the core API group\n"+
		"<group>/<version>=true|false for a specific API group and version (e.g. apps/v1=true)\n"+
		"api/all=true|false controls all API versions\n"+
		"api/ga=true|false controls all API versions of the form v[0-9]+\n"+
		"api/beta=true|false controls all API versions of the form v[0-9]+beta[0-9]+\n"+
		"api/alpha=true|false controls all API versions of the form v[0-9]+alpha[0-9]+\n"+
		"api/legacy is deprecated, and will be removed in a future version")
}

// Validate validates RuntimeConfig with a list of registries.
// Usually this list only has one element, the apiserver registry of the process.
// But in the advanced (and usually not recommended) case of delegated apiservers there can be more.
// Validate will filter out the known groups of each registry.
// If anything is left over after that, an error is returned.
func (s *APIEnablementOptions) Validate(registries ...GroupRegistry) []error {
	if s == nil {
		return nil
	}

	errors := []error{}
	if s.RuntimeConfig[resourceconfig.APIAll] == "false" && len(s.RuntimeConfig) == 1 {
		// Do not allow only set api/all=false, in such case apiserver startup has no meaning.
		return append(errors, fmt.Errorf("invalid key with only %v=false", resourceconfig.APIAll))
	}

	groups, err := resourceconfig.ParseGroups(s.RuntimeConfig)
	if err != nil {
		return append(errors, err)
	}

	for _, registry := range registries {
		// filter out known groups
		groups = unknownGroups(groups, registry)
	}
	if len(groups) != 0 {
		errors = append(errors, fmt.Errorf("unknown api groups %s", strings.Join(groups, ",")))
	}

	return errors
}

// ApplyTo override MergedResourceConfig with defaults and registry
// 这里其实就是需要根据注册中心、K8S默认启用/禁用的资源、用户设置禁用/启用的资源做一个合并的动作，最终合并出启用/禁用的资源
func (s *APIEnablementOptions) ApplyTo(
	c *server.Config, // GenericServer配置
	defaultResourceConfig *serverstore.ResourceConfig, // 默认启用/禁用的资源
	registry resourceconfig.GroupVersionRegistry, // 所谓GV注册中心，其实就是用于判断group是否已经注册、GV是否已经注册、所有组的GV优先级顺序
) error {

	if s == nil {
		return nil
	}

	// 1、所谓GV注册中心，其实就是用于判断group是否已经注册、GV是否已经注册、所有组的GV优先级顺序。注册中心仅仅是提供信息的组件
	// 2、这里其实就是需要根据注册中心、K8S默认启用/禁用的资源、用户设置禁用/启用的资源做一个合并的动作，最终合并出启用/禁用的资源
	mergedResourceConfig, err := resourceconfig.MergeAPIResourceConfigs(
		defaultResourceConfig, // K8S默认启用、禁用的资源
		s.RuntimeConfig,       // 用于设置的启用/禁用的资源
		registry,              // 所谓GV注册中心，其实就是用于判断group是否已经注册、GV是否已经注册、所有组的GV优先级顺序
	)
	c.MergedResourceConfig = mergedResourceConfig

	return err
}

func unknownGroups(groups []string, registry GroupRegistry) []string {
	unknownGroups := []string{}
	for _, group := range groups {
		if !registry.IsGroupRegistered(group) {
			unknownGroups = append(unknownGroups, group)
		}
	}
	return unknownGroups
}

// GroupRegistry provides a method to check whether given group is registered.
// 返回当前的组是否已经注册
type GroupRegistry interface {
	// IsRegistered returns true if given group is registered.
	IsGroupRegistered(group string) bool
}
