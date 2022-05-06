/*
Copyright 2014 The Kubernetes Authors.

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

package cache

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/util/sets"
)

// Indexer extends Store with multiple indices and restricts each
// accumulator to simply hold the current object (and be empty after
// Delete).
//
// There are three kinds of strings here:
// 1. a storage key, as defined in the Store interface,
// 2. a name of an index, and
// 3. an "indexed value", which is produced by an IndexFunc and
//    can be a field value or any other string computed from the object.
// 索引器的底层实际上就是一个存储器，也就是索引器在存储的基础上增加了索引的功能，使得查询速度加快
type Indexer interface {
	Store
	// Index returns the stored objects whose set of indexed values
	// intersects the set of indexed values of the given object, for
	// the named index
	// 获取obj对象所在的IndexName维度下的所有资源对象
	// 譬如indexName=namespace,如果obj.metadata.namespace=gator-cloud，那么该函数就是获取gator-cloud中所有K8S资源对象
	// 对于indexName=node来说，如果obj.spec.nodeName=node5，那么就是在获取node5上所有的K8S资源对象
	Index(indexName string, obj interface{}) ([]interface{}, error)
	// IndexKeys returns the storage keys of the stored objects whose
	// set of indexed values for the named index includes the given
	// indexed value
	// 获取indexName维度下的indexValue分类的所有K8S资源对象
	// 譬如这里的indexName=namespace, 如果indexValues=gator-cloud,那么就是在获取gator-cloud空间下所有对象的对象键
	// 如果indexName=node, 并且indexValue=node5,那么这里就是在获取node5上的所有资源的对象键
	IndexKeys(indexName, indexedValue string) ([]string, error)
	// ListIndexFuncValues returns all the indexed values of the given index
	// 譬如indexName=namespace, 那么这里就是在获取namesapce维度下的所有分类，譬如有default, gator-cloud, kube-system, gator-ucss分类
	ListIndexFuncValues(indexName string) []string
	// ByIndex returns the stored objects whose set of indexed values
	// for the named index includes the given indexed value
	// ByIndex 获取indexName维度下的indexValue分类的所有K8S资源对象
	// 譬如这里的indexName=namespace, 如果indexValues=gator-cloud,那么就是在获取gator-cloud空间下所有资源对象
	// 如果indexName=node, 并且indexValue=node5,那么这里就是在获取node5上的所有资源对象
	ByIndex(indexName, indexedValue string) ([]interface{}, error)
	// GetIndexers return the indexers
	// 获取分类维度，譬如以namespace, node维度进行划分
	GetIndexers() Indexers

	// AddIndexers adds more indexers to this store.  If you call this after you already have data
	// in the store, the results are undefined.
	// 添加索引划分维度
	AddIndexers(newIndexers Indexers) error
}

// IndexFunc knows how to compute the set of indexed values for an object.
// 对象的索引键函数，根据传入的对象计算出对象的索引键
type IndexFunc func(obj interface{}) ([]string, error)

// IndexFuncToKeyFuncAdapter adapts an indexFunc to a keyFunc.  This is only useful if your index function returns
// unique values for every object.  This conversion can create errors when more than one key is found.  You
// should prefer to make proper key and index functions.
func IndexFuncToKeyFuncAdapter(indexFunc IndexFunc) KeyFunc {
	return func(obj interface{}) (string, error) {
		indexKeys, err := indexFunc(obj)
		if err != nil {
			return "", err
		}
		if len(indexKeys) > 1 {
			return "", fmt.Errorf("too many keys: %v", indexKeys)
		}
		if len(indexKeys) == 0 {
			return "", fmt.Errorf("unexpected empty indexKeys")
		}
		return indexKeys[0], nil
	}
}

const (
	// NamespaceIndex is the lookup name for the most common index function, which is to index by the namespace field.
	NamespaceIndex string = "namespace"
)

// MetaNamespaceIndexFunc is a default index function that indexes based on an object's namespace
// indexer的key就是资源所在的名称空间，一般都是使用名称空间划分，也就是LocalStorage先按照名称空间划分不同的资源
func MetaNamespaceIndexFunc(obj interface{}) ([]string, error) {
	meta, err := meta.Accessor(obj)
	if err != nil {
		return []string{""}, fmt.Errorf("object has no meta: %v", err)
	}
	return []string{meta.GetNamespace()}, nil
}

// Index maps the indexed value to a set of keys in the store that match on that value
// key为索引键， value为对象键，这里为什么对象键是一个数组呢？原因是因为不同对象的索引键可能是相同的，而这里是
// 根据对象键找到对象的索引键; Index的Key可以理解为不同的维度中的具体的分类，譬如以namespace进行划分，那么这里的Key就是default,
// kube-system, gator-cloud, ops, gator-ucss等名称空间，value就是这些名称空间下的Pod对象
type Index map[string]sets.String

// Indexers maps a name to an IndexFunc
// 计算索引函数可以有多个，使用名字进行分类，一般是使用名称空间，实际上可以理解为一个分类函数，我们需要把K8S中的资源按照不同的分类函数进行分类。
// 试想一下如下的使用场景：1、获取kube-system名称空间下所有的pod  2、获取node1上所有的Pod
// 我们当然可以直接调用apiServer提供的API，但是既然本地都有缓存了，当然需要缓存支持按照不同的分类维度获取相同维度的所有资源，这种操作在K8S当中
// 是非常常见的。
// map[string]IndexFunc{"namespace": getObjNamespace, "node": getObjNode}
type Indexers map[string]IndexFunc

// Indices maps a name to an Index
// Indices实际上就是我们所谓的索引，这里的索引就是根据不同的分类维度建立索引的，譬如如上的两个需求：
//                                                 Indices
//                             /                                         \
//                           /                                            \
//                     namespace                                         node                 这里就是Indexers，其实就是各个维度，以及各个维度的计算函数
//            /      |     \           \                         /      /    \      \
//          /        |       \           \                     /       /      \      \
//    default  kube-system gator-cloud    ops                node1   node2   node3    node4
//    /  |  \       |   \        |  \      \  \               |       |      |  \       \  \     这里就是Index, key为上层不同的分类维度
//   /   |   \      |    \       |   \      \  \              |       |      |   \       \   \     value为下层的pod
//	pod pod pod    pop  pod     pod pod     pod pod           pod    pod     pod  pod   pod   pod
type Indices map[string]Index
