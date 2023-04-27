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
	"sync/atomic"

	"k8s.io/api/admissionregistration/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/admission/plugin/webhook"
	"k8s.io/apiserver/pkg/admission/plugin/webhook/generic"
	"k8s.io/client-go/informers"
	admissionregistrationlisters "k8s.io/client-go/listers/admissionregistration/v1"
	"k8s.io/client-go/tools/cache"
)

// mutatingWebhookConfigurationManager collects the mutating webhook objects so that they can be called.
type mutatingWebhookConfigurationManager struct {
	configuration *atomic.Value
	lister        admissionregistrationlisters.MutatingWebhookConfigurationLister
	hasSynced     func() bool
	// initialConfigurationSynced stores a boolean value, which tracks if
	// the existing webhook configs have been synced (honored) by the
	// manager at startup-- the informer has synced and either has no items
	// or has finished executing updateConfiguration() once.
	initialConfigurationSynced *atomic.Value
}

var _ generic.Source = &mutatingWebhookConfigurationManager{}

func NewMutatingWebhookConfigurationManager(f informers.SharedInformerFactory) generic.Source {
	// MutatingWebhookConfiguration资源的Informer
	informer := f.Admissionregistration().V1().MutatingWebhookConfigurations()
	manager := &mutatingWebhookConfigurationManager{
		configuration:              &atomic.Value{},
		lister:                     informer.Lister(),
		hasSynced:                  informer.Informer().HasSynced,
		initialConfigurationSynced: &atomic.Value{},
	}

	// Start with an empty list
	manager.configuration.Store([]webhook.WebhookAccessor{})
	manager.initialConfigurationSynced.Store(false)

	// On any change, rebuild the config
	informer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(_ interface{}) { manager.updateConfiguration() },
		UpdateFunc: func(_, _ interface{}) { manager.updateConfiguration() },
		DeleteFunc: func(_ interface{}) { manager.updateConfiguration() },
	})

	return manager
}

// Webhooks returns the merged MutatingWebhookConfiguration.
func (m *mutatingWebhookConfigurationManager) Webhooks() []webhook.WebhookAccessor {
	return m.configuration.Load().([]webhook.WebhookAccessor)
}

// HasSynced returns true when the manager is synced with existing webhookconfig
// objects at startup-- which means the informer is synced and either has no items
// or updateConfiguration() has completed.
func (m *mutatingWebhookConfigurationManager) HasSynced() bool {
	if !m.hasSynced() {
		return false
	}
	if m.initialConfigurationSynced.Load().(bool) {
		// the informer has synced and configuration has been updated
		return true
	}
	if configurations, err := m.lister.List(labels.Everything()); err == nil && len(configurations) == 0 {
		// the empty list we initially stored is valid to use.
		// Setting initialConfigurationSynced to true, so subsequent checks
		// would be able to take the fast path on the atomic boolean in a
		// cluster without any admission webhooks configured.
		m.initialConfigurationSynced.Store(true)
		// the informer has synced and we don't have any items
		return true
	}
	return false
}

func (m *mutatingWebhookConfigurationManager) updateConfiguration() {
	// 拿到所有的MutationWebhookConfiguration
	configurations, err := m.lister.List(labels.Everything())
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("error updating configuration: %v", err))
		return
	}
	// 把拿到的所有webhook配置封装为WebhookAccessor保存起来
	m.configuration.Store(mergeMutatingWebhookConfigurations(configurations))
	m.initialConfigurationSynced.Store(true)
}

// TODO 这里干了啥？
func mergeMutatingWebhookConfigurations(configurations []*v1.MutatingWebhookConfiguration) []webhook.WebhookAccessor {
	// The internal order of webhooks for each configuration is provided by the user
	// but configurations themselves can be in any order. As we are going to run these
	// webhooks in serial, they are sorted here to have a deterministic order.
	// TODO 为什么要通过MutationWebhookConfiguration的名字排序？
	sort.SliceStable(configurations, MutatingWebhookConfigurationSorter(configurations).ByName)
	var accessors []webhook.WebhookAccessor
	for _, c := range configurations {
		// webhook names are not validated for uniqueness, so we check for duplicates and
		// add a int suffix to distinguish between them
		names := map[string]int{}
		for i := range c.Webhooks {
			n := c.Webhooks[i].Name
			uid := fmt.Sprintf("%s/%s/%d", c.Name, n, names[n])
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
