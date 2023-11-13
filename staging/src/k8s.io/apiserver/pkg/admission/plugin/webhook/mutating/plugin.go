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

package mutating

import (
	"context"
	"io"

	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/admission/configuration"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/generic"
)

const (
	// PluginName indicates the name of admission plug-in
	PluginName = "MutatingAdmissionWebhook"
)

// Register registers a plugin
func Register(plugins *admission.Plugins) {
	plugins.Register(PluginName, func(configFile io.Reader) (admission.Interface, error) {
		plugin, err := NewMutatingWebhook(configFile)
		if err != nil {
			return nil, err
		}

		return plugin, nil
	})
}

// Plugin is an implementation of admission.Interface.
// 1、MutatingWebhook本身并没有实际的业务含义，也并不会执行任何的准入控制，MutatingWebhook的准入控制是通过用户自定义的Webhook来完成准入
// 控制的，这里可以理解为MutatingWebhook的管理器
type Plugin struct {
	*generic.Webhook // 解决通用Webhook准入控制插件来实现自己的功能
}

var _ admission.MutationInterface = &Plugin{}

// NewMutatingWebhook returns a generic admission webhook plugin.
func NewMutatingWebhook(configFile io.Reader) (*Plugin, error) {
	// MutatingWebhook支持CREATE, DELETE, UPDATE, CONNECT配置
	handler := admission.NewHandler(admission.Connect, admission.Create, admission.Delete, admission.Update)
	p := &Plugin{}
	var err error
	p.Webhook, err = generic.NewWebhook(handler, configFile, configuration.NewMutatingWebhookConfigurationManager, newMutatingDispatcher(p))
	if err != nil {
		return nil, err
	}

	return p, nil
}

// ValidateInitialization implements the InitializationValidator interface.
func (a *Plugin) ValidateInitialization() error {
	if err := a.Webhook.ValidateInitialization(); err != nil {
		return err
	}
	return nil
}

// Admit makes an admission decision based on the request attributes.
func (a *Plugin) Admit(ctx context.Context, attr admission.Attributes, o admission.ObjectInterfaces) error {
	// 把任务委派到各个Webhook
	return a.Webhook.Dispatch(ctx, attr, o)
}
