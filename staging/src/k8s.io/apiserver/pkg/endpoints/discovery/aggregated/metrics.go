/*
Copyright 2023 The Kubernetes Authors.

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

package aggregated

import (
	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

var (
	// 用于计数聚合发现被聚合了多少次
	regenerationCounter = metrics.NewCounter(
		&metrics.CounterOpts{
			Name:           "aggregator_discovery_aggregation_count_total",
			Help:           "Counter of number of times discovery was aggregated",
			StabilityLevel: metrics.ALPHA,
		},
	)
)

func init() {
	legacyregistry.MustRegister(regenerationCounter)
}
