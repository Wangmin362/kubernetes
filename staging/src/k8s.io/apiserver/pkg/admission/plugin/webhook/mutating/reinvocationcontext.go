/*
Copyright 2019 The Kubernetes Authors.

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
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
)

// 1、重新调用上下文
// 2、所谓重新调用，指的是一个Webhook调用失败之后需要再次调用，而再次调用的上下文就是这里。
// 3、一个Webhook是否能够被重新调用，取决于WebhookConfiguration配置的reinvocationPolicy是否设置了允许调用
type webhookReinvokeContext struct {
	// lastWebhookOutput holds the result of the last webhook admission plugin call
	lastWebhookOutput runtime.Object
	// previouslyInvokedReinvocableWebhooks holds the set of webhooks that have been invoked and
	// should be reinvoked if a later mutation occurs
	previouslyInvokedReinvocableWebhooks sets.String
	// reinvokeWebhooks holds the set of webhooks that should be reinvoked
	reinvokeWebhooks sets.String
}

func (rc *webhookReinvokeContext) ShouldReinvokeWebhook(webhook string) bool {
	return rc.reinvokeWebhooks.Has(webhook)
}

// IsOutputChangedSinceLastWebhookInvocation 判断当前对象相比于上次一次调用是否已经发生改变
func (rc *webhookReinvokeContext) IsOutputChangedSinceLastWebhookInvocation(object runtime.Object) bool {
	return !apiequality.Semantic.DeepEqual(rc.lastWebhookOutput, object)
}

func (rc *webhookReinvokeContext) SetLastWebhookInvocationOutput(object runtime.Object) {
	if object == nil {
		rc.lastWebhookOutput = nil
		return
	}
	rc.lastWebhookOutput = object.DeepCopyObject()
}

func (rc *webhookReinvokeContext) AddReinvocableWebhookToPreviouslyInvoked(webhook string) {
	if rc.previouslyInvokedReinvocableWebhooks == nil {
		rc.previouslyInvokedReinvocableWebhooks = sets.NewString()
	}
	rc.previouslyInvokedReinvocableWebhooks.Insert(webhook)
}

func (rc *webhookReinvokeContext) RequireReinvokingPreviouslyInvokedPlugins() {
	if len(rc.previouslyInvokedReinvocableWebhooks) > 0 {
		if rc.reinvokeWebhooks == nil {
			rc.reinvokeWebhooks = sets.NewString()
		}
		for s := range rc.previouslyInvokedReinvocableWebhooks {
			rc.reinvokeWebhooks.Insert(s)
		}
		rc.previouslyInvokedReinvocableWebhooks = sets.NewString()
	}
}
