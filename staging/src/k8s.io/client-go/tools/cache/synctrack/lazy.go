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

package synctrack

import (
	"sync"
	"sync/atomic"
)

// Lazy defers the computation of `Evaluate` to when it is necessary. It is
// possible that Evaluate will be called in parallel from multiple goroutines.
type Lazy[T any] struct {
	// 用于实例化T类型的数据
	Evaluate func() (T, error)

	cache atomic.Pointer[cacheEntry[T]]
}

type cacheEntry[T any] struct {
	// 用于实例化T类型的数据
	eval   func() (T, error)
	lock   sync.RWMutex
	result *T
}

func (e *cacheEntry[T]) get() (T, error) {
	// 使用读锁读取数据
	if cur := func() *T {
		e.lock.RLock()
		defer e.lock.RUnlock()
		return e.result
	}(); cur != nil {
		return *cur, nil
	}

	e.lock.Lock()
	defer e.lock.Unlock()
	// 如果取出的数据非空，直接返回数据
	if e.result != nil {
		return *e.result, nil
	}
	// 如果取出的数据为空，那么通过eval函数实例化数据
	r, err := e.eval()
	if err == nil {
		// 缓存实例化数据
		e.result = &r
	}
	return r, err
}

func (z *Lazy[T]) newCacheEntry() *cacheEntry[T] {
	return &cacheEntry[T]{eval: z.Evaluate}
}

// Notify should be called when something has changed necessitating a new call
// to Evaluate.
// 存储新的CacheEntry
func (z *Lazy[T]) Notify() { z.cache.Swap(z.newCacheEntry()) }

// Get should be called to get the current result of a call to Evaluate. If the
// current cached value is stale (due to a call to Notify), then Evaluate will
// be called synchronously. If subsequent calls to Get happen (without another
// Notify), they will all wait for the same return value.
//
// Error returns are not cached and will cause multiple calls to evaluate!
func (z *Lazy[T]) Get() (T, error) {
	// 获取CacheEntry
	e := z.cache.Load()
	if e == nil {
		// Since we don't force a constructor, nil is a possible value.
		// If multiple Gets race to set this, the swap makes sure only
		// one wins.
		// 说明当前还没有缓存CacheEntry，此时实例化一个新的CacheEntry
		z.cache.CompareAndSwap(nil, z.newCacheEntry())
		e = z.cache.Load()
	}
	// 最所以称之为懒加载，其实就是这里，只有在真正获取数据的时候，才会执行eval函数实例化真正的数据
	return e.get()
}
