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

package configuration

import (
	"fmt"
	"sort"

	"k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/admission/plugin/webhook"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/generic"
	"k8s.io/client-go/informers"
	admissionregistrationlisters "k8s.io/client-go/listers/admissionregistration/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/cache/synctrack"
)

// mutatingWebhookConfigurationManager collects the mutating webhook objects so that they can be called.
// 1、用于获取Source，本质上是为了获取Webhook准入插件
type mutatingWebhookConfigurationManager struct {
	lister    admissionregistrationlisters.MutatingWebhookConfigurationLister // 用于获取MutatingWebhook配置
	hasSynced func() bool                                                     // Informer是否同步完成
	// 之所以这玩意看起来这么复杂，就是考虑了并发竞争，其实这里可以简单的认为是[]WebhookAccessor数组
	lazy synctrack.Lazy[[]webhook.WebhookAccessor]
}

var _ generic.Source = &mutatingWebhookConfigurationManager{}

func NewMutatingWebhookConfigurationManager(f informers.SharedInformerFactory) generic.Source {
	// 监听K8S集群中所有的MutatingWebhookConfiguration
	informer := f.Admissionregistration().V1().MutatingWebhookConfigurations()
	manager := &mutatingWebhookConfigurationManager{
		lister: informer.Lister(),
	}
	// manager.getConfiguration获取所有的Webhook
	manager.lazy.Evaluate = manager.getConfiguration

	// 一旦WebhookConfiguration发生了变化，那么重新获取Webhook
	handle, _ := informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ interface{}) { manager.lazy.Notify() },
		UpdateFunc: func(_, _ interface{}) { manager.lazy.Notify() },
		DeleteFunc: func(_ interface{}) { manager.lazy.Notify() },
	})
	manager.hasSynced = handle.HasSynced

	return manager
}

// Webhooks returns the merged MutatingWebhookConfiguration.
func (m *mutatingWebhookConfigurationManager) Webhooks() []webhook.WebhookAccessor {
	// 获取Webhook数据
	out, err := m.lazy.Get()
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("error getting webhook configuration: %v", err))
	}
	return out
}

// HasSynced returns true if the initial set of mutating webhook configurations
// has been loaded.
// 用于判断Informer是否同步完成
func (m *mutatingWebhookConfigurationManager) HasSynced() bool { return m.hasSynced() }

func (m *mutatingWebhookConfigurationManager) getConfiguration() ([]webhook.WebhookAccessor, error) {
	// 获取所有的MutatingWebhook
	configurations, err := m.lister.List(labels.Everything())
	if err != nil {
		return []webhook.WebhookAccessor{}, err
	}
	return mergeMutatingWebhookConfigurations(configurations), nil
}

func mergeMutatingWebhookConfigurations(configurations []*v1.MutatingWebhookConfiguration) []webhook.WebhookAccessor {
	// The internal order of webhooks for each configuration is provided by the user
	// but configurations themselves can be in any order. As we are going to run these
	// webhooks in serial, they are sorted here to have a deterministic order.
	// 排序
	sort.SliceStable(configurations, MutatingWebhookConfigurationSorter(configurations).ByName)
	var accessors []webhook.WebhookAccessor
	for _, c := range configurations {
		// webhook names are not validated for uniqueness, so we check for duplicates and
		// add a int suffix to distinguish between them
		names := map[string]int{}
		for i := range c.Webhooks {
			n := c.Webhooks[i].Name
			uid := fmt.Sprintf("%s/%s/%d", c.Name, n, names[n])
			// 名字相同的webhook，通过后缀来区分
			names[n]++
			accessors = append(accessors, webhook.NewMutatingWebhookAccessor(uid, c.Name, &c.Webhooks[i]))
		}
	}
	return accessors
}

type MutatingWebhookConfigurationSorter []*v1.MutatingWebhookConfiguration

func (a MutatingWebhookConfigurationSorter) ByName(i, j int) bool {
	return a[i].Name < a[j].Name
}
