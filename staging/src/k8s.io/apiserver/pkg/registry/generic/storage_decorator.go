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

package generic

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/storage"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	"k8s.io/apiserver/pkg/storage/storagebackend/factory"
	"k8s.io/client-go/tools/cache"
)

// StorageDecorator is a function signature for producing a storage.Interface
// and an associated DestroyFunc from given parameters.
// TODO 如何理解这个方法的设计？ 如何理解这个方法的命名？
// 1、config：K8S资源的存储后端配置，即当K8S需要存储一个资源的时候，这个资源到底该怎么存储？前缀是啥？如何序列化？这个资源的存储后端是谁？
// 2、resourcePrefix：当前资源的存储前缀是啥
// 3、keyFunc：用于生成当前资源对象的key,一般都是 namespace/name
// 4、newFunc：用于实例化当前资源
// 5、newListFunc：用于实例化当前资源列表
// 6、getAttrsFunc：TODO
// 7、trigger：TODO
// 8、indexer：TODO
// 9、storage.Interface：当前资源是如何存储的，可以理解为标准存储
// 10、factory.DestroyFunc
type StorageDecorator func(
	config *storagebackend.ConfigForResource,
	resourcePrefix string,
	keyFunc func(obj runtime.Object) (string, error),
	newFunc func() runtime.Object,
	newListFunc func() runtime.Object,
	getAttrsFunc storage.AttrFunc,
	trigger storage.IndexerFuncs,
	indexers *cache.Indexers,
) (storage.Interface, factory.DestroyFunc, error)

// UndecoratedStorage returns the given a new storage from the given config
// without any decoration.
// TODO 为啥这里是非装饰的存储，从搜索结果来看这个方法主要是用来测试使用的
func UndecoratedStorage(
	config *storagebackend.ConfigForResource,
	resourcePrefix string,
	keyFunc func(obj runtime.Object) (string, error),
	newFunc func() runtime.Object,
	newListFunc func() runtime.Object,
	getAttrsFunc storage.AttrFunc,
	trigger storage.IndexerFuncs,
	indexers *cache.Indexers) (storage.Interface, factory.DestroyFunc, error) {
	return NewRawStorage(config, newFunc)
}

// NewRawStorage creates the low level kv storage. This is a work-around for current
// two layer of same storage interface.
// TODO: Once cacher is enabled on all registries (event registry is special), we will remove this method.
// TODO 如何理解这里所谓的装饰？
func NewRawStorage(config *storagebackend.ConfigForResource,
	newFunc func() runtime.Object) (storage.Interface, factory.DestroyFunc, error) {
	return factory.Create(*config, newFunc)
}
