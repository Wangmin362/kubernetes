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

package generic

import (
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	flowcontrolrequest "k8s.io/apiserver/pkg/util/flowcontrol/request"
	"k8s.io/client-go/tools/cache"
)

// RESTOptions is set of resource-specific configuration options to generic registries.
type RESTOptions struct {
	// 存储配置，其中包含非常重要的编解码器
	StorageConfig *storagebackend.ConfigForResource
	// 存储后端，实际上可以理解为ETCD的客户端
	Decorator StorageDecorator

	EnableGarbageCollection   bool
	DeleteCollectionWorkers   int
	ResourcePrefix            string
	CountMetricPollPeriod     time.Duration
	StorageObjectCountTracker flowcontrolrequest.StorageObjectCountTracker
}

// GetRESTOptions Implement RESTOptionsGetter so that RESTOptions can directly be used when available (i.e. tests)
func (opts RESTOptions) GetRESTOptions(schema.GroupResource) (RESTOptions, error) {
	return opts, nil
}

// RESTOptionsGetter 可以获取每个资源的存储配置
type RESTOptionsGetter interface {
	GetRESTOptions(resource schema.GroupResource) (RESTOptions, error)
}

// StoreOptions is set of configuration options used to complete generic registries.
type StoreOptions struct {
	RESTOptions RESTOptionsGetter
	TriggerFunc storage.IndexerFuncs
	AttrFunc    storage.AttrFunc
	Indexers    *cache.Indexers
}
