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
	// 注册MutationWebhook准入控制器
	plugins.Register(PluginName, func(configFile io.Reader) (admission.Interface, error) {
		plugin, err := NewMutatingWebhook(configFile)
		if err != nil {
			return nil, err
		}

		return plugin, nil
	})
}

// Plugin is an implementation of admission.Interface.
// TODO Webhook也是准入控制插件的一种，只不过Webhook是K8S留给用户动态增加准入控制的一种方式
type Plugin struct {
	*generic.Webhook
}

var _ admission.MutationInterface = &Plugin{}

// NewMutatingWebhook returns a generic admission webhook plugin.
// 实例化MutatingWebhook, configFile为MutatingWebhook的配置文件
func NewMutatingWebhook(configFile io.Reader) (*Plugin, error) {
	// MutationWebhook支持Connect, Create, Delete, Update
	handler := admission.NewHandler(admission.Connect, admission.Create, admission.Delete, admission.Update)
	p := &Plugin{}
	var err error

	// configuration.NewMutatingWebhookConfigurationManager是一个sourceFactory，是为了拿到所有的webhook
	//
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
// TODO webhook实现了准入控制插件的准入(Admit)方法发
func (a *Plugin) Admit(ctx context.Context, attr admission.Attributes, o admission.ObjectInterfaces) error {
	return a.Webhook.Dispatch(ctx, attr, o)
}
