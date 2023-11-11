/*
Copyright 2015 The Kubernetes Authors.

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

package admission

import (
	"time"

	"k8s.io/apimachinery/pkg/util/sets"
)

const (
	// timeToWaitForReady is the amount of time to wait to let an admission controller to be ready to satisfy a request.
	// this is useful when admission controllers need to warm their caches before letting requests through.
	timeToWaitForReady = 10 * time.Second
)

// ReadyFunc is a function that returns true if the admission controller is ready to handle requests.
type ReadyFunc func() bool

// Handler is a base for admission control handlers that
// support a predefined set of operations
// 1、Handler是准入控制插件的基础实现，一般直接基于Handler实现自己的准入控制插件
// 2、此Handler实现的比较抽象，并没有什么业务相关的含义，同时实现了一般的准入控制插件需要的功能，譬如是否支持某个操作，以及判断插件是否就绪的标准
type Handler struct {
	operations sets.String // 支持的操作
	readyFunc  ReadyFunc   // 准入控制插件是否就绪
}

// Handles returns true for methods that this handler supports
// 判断当前的准入控制插件是否支持某种操作
func (h *Handler) Handles(operation Operation) bool {
	return h.operations.Has(string(operation))
}

// NewHandler creates a new base handler that handles the passed
// in operations
// 实例化准入控制插件
func NewHandler(ops ...Operation) *Handler {
	operations := sets.NewString()
	for _, op := range ops {
		operations.Insert(string(op))
	}
	return &Handler{
		operations: operations,
	}
}

// SetReadyFunc allows late registration of a ReadyFunc to know if the handler is ready to process requests.
// 设置准入控制插件的就绪函数
func (h *Handler) SetReadyFunc(readyFunc ReadyFunc) {
	h.readyFunc = readyFunc
}

// WaitForReady will wait for the readyFunc (if registered) to return ready, and in case of timeout, will return false.
// 1、从这里可以看出，K8S假设准入控制插件必须在10秒钟之内继续，否则不再等待插件就绪
func (h *Handler) WaitForReady() bool {
	// there is no ready func configured, so we return immediately
	if h.readyFunc == nil {
		return true
	}

	timeout := time.After(timeToWaitForReady)
	for !h.readyFunc() {
		select {
		case <-time.After(100 * time.Millisecond): // 每100毫秒判断一次插件是否就绪
		case <-timeout: // 插件必须在10秒钟之内就绪
			return h.readyFunc()
		}
	}
	return true
}
