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
	"errors"
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"
)

// PopProcessFunc is passed to Pop() method of Queue interface.
// It is supposed to process the accumulator popped from the queue.
type PopProcessFunc func(interface{}) error

// ErrRequeue may be returned by a PopProcessFunc to safely requeue
// the current item. The value of Err will be returned from Pop.
type ErrRequeue struct {
	// Err is returned by the Pop function
	Err error
}

// ErrFIFOClosed used when FIFO is closed
var ErrFIFOClosed = errors.New("DeltaFIFO: manipulating with closed queue")

func (e ErrRequeue) Error() string {
	if e.Err == nil {
		return "the popped item should be requeued without returning an error"
	}
	return e.Err.Error()
}

// Queue extends Store with a collection of Store keys to "process".
// Every Add, Update, or Delete may put the object's key in that collection.
// A Queue has a way to derive the corresponding key given an accumulator.
// A Queue can be accessed concurrently from multiple goroutines.
// A Queue can be "closed", after which Pop operations return an error.
type Queue interface {
	Store

	// Pop blocks until there is at least one key to process or the
	// Queue is closed.  In the latter case Pop returns with an error.
	// In the former case Pop atomically picks one key to process,
	// removes that (key, accumulator) association from the Store, and
	// processes the accumulator.  Pop returns the accumulator that
	// was processed and the result of processing.  The PopProcessFunc
	// may return an ErrRequeue{inner} and in this case Pop will (a)
	// return that (key, accumulator) association to the Queue as part
	// of the atomic processing and (b) return the inner error from
	// Pop.
	// 从队列当中弹出一个元素，符合队列的名字，一般来说就是先进先出或者先进后出
	Pop(PopProcessFunc) (interface{}, error)

	// AddIfNotPresent puts the given accumulator into the Queue (in
	// association with the accumulator's key) if and only if that key
	// is not already associated with a non-empty accumulator.
	// 这个接口的实现，显然需要判断是否已经存在该元素，而判断的标准就是_,exist :=map[key],
	// exist是否为false,只有当为false的时候才会添加元素，其中的key肯定是对象的唯一键
	AddIfNotPresent(interface{}) error

	// HasSynced returns true if the first batch of keys have all been
	// popped.  The first batch of keys are those of the first Replace
	// operation if that happened before any Add, AddIfNotPresent,
	// Update, or Delete; otherwise the first batch is empty.
	HasSynced() bool

	// Close the queue
	Close()
}

// Pop is helper function for popping from Queue.
// WARNING: Do NOT use this function in non-test code to avoid races
// unless you really really really really know what you are doing.
func Pop(queue Queue) interface{} {
	var result interface{}
	queue.Pop(func(obj interface{}) error {
		result = obj
		return nil
	})
	return result
}

// FIFO is a Queue in which (a) each accumulator is simply the most
// recently provided object and (b) the collection of keys to process
// is a FIFO.  The accumulators all start out empty, and deleting an
// object from its accumulator empties the accumulator.  The Resync
// operation is a no-op.
//
// Thus: if multiple adds/updates of a single object happen while that
// object's key is in the queue before it has been processed then it
// will only be processed once, and when it is processed the most
// recent version will be processed. This can't be done with a channel
//
// FIFO solves this use case:
//  * You want to process every object (exactly) once.
//  * You want to process the most recent version of the object when you process it.
//  * You do not want to process deleted objects, they should be removed from the queue.
//  * You do not want to periodically reprocess objects.
// Compare with DeltaFIFO for other use cases.
type FIFO struct {
	// 读写互斥锁
	lock sync.RWMutex
	// 条件同步
	cond sync.Cond
	// We depend on the property that every key in `items` is also in `queue`
	// 用map来存储元素
	items map[string]interface{}
	// 用queue来保证元素的顺序，显然，保存的内容是每个元素的Key，key通过keyFunc计算出来
	queue []string

	// populated is true if the first batch of items inserted by Replace() has been populated
	// or Delete/Add/Update was called first.
	// TODO 这玩意到底是用来干啥的？
	populated bool
	// initialPopulationCount is the number of items inserted by the first call of Replace()
	initialPopulationCount int

	// keyFunc is used to make the key used for queued item insertion and retrieval, and
	// should be deterministic.
	// 用于计算对象键
	keyFunc KeyFunc

	// Indication the queue is closed.
	// Used to indicate a queue is closed so a control loop can exit when a queue is empty.
	// Currently, not used to gate any of CRUD operations.
	closed bool
}

var (
	_ = Queue(&FIFO{}) // FIFO is a Queue
)

// Close the queue.
func (f *FIFO) Close() {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.closed = true
	// 唤醒所有阻塞的协程，队列已经被关闭
	f.cond.Broadcast()
}

// HasSynced returns true if an Add/Update/Delete/AddIfNotPresent are called first,
// or the first batch of items inserted by Replace() has been popped.
func (f *FIFO) HasSynced() bool {
	f.lock.Lock()
	defer f.lock.Unlock()
	return f.populated && f.initialPopulationCount == 0
}

// Add inserts an item, and puts it in the queue. The item is only enqueued
// if it doesn't already exist in the set.
func (f *FIFO) Add(obj interface{}) error {
	// 获取对象键
	id, err := f.keyFunc(obj)
	if err != nil {
		return KeyError{obj, err}
	}
	f.lock.Lock()
	defer f.lock.Unlock()
	f.populated = true
	if _, exists := f.items[id]; !exists {
		// 队列中不存在该元素，直接入队
		f.queue = append(f.queue, id)
	}
	// 这里是幂等的，不管元素存不存在，都是直接替换
	f.items[id] = obj
	// TODO 这里为什么是唤醒所有的协程，而不是使用f.cond.Signal()唤醒其中的一个协程
	f.cond.Broadcast()
	return nil
}

// AddIfNotPresent inserts an item, and puts it in the queue. If the item is already
// present in the set, it is neither enqueued nor added to the set.
//
// This is useful in a single producer/consumer scenario so that the consumer can
// safely retry items without contending with the producer and potentially enqueueing
// stale items.
func (f *FIFO) AddIfNotPresent(obj interface{}) error {
	// 获取对象键
	id, err := f.keyFunc(obj)
	if err != nil {
		return KeyError{obj, err}
	}
	f.lock.Lock()
	defer f.lock.Unlock()
	f.addIfNotPresent(id, obj)
	return nil
}

// addIfNotPresent assumes the fifo lock is already held and adds the provided
// item to the queue under id if it does not already exist.
// id就是obj的对象键
func (f *FIFO) addIfNotPresent(id string, obj interface{}) {
	f.populated = true
	if _, exists := f.items[id]; exists {
		// 已经存在，则直接返回
		return
	}

	// 不存在，直接append到队列当中，记录入队顺序
	f.queue = append(f.queue, id)
	// Map保存数据
	f.items[id] = obj
	// TODO 这里为什么是唤醒所有的协程，而不是使用f.cond.Signal()唤醒其中的一个协程
	f.cond.Broadcast()
}

// Update is the same as Add in this implementation.
func (f *FIFO) Update(obj interface{}) error {
	return f.Add(obj)
}

// Delete removes an item. It doesn't add it to the queue, because
// this implementation assumes the consumer only cares about the objects,
// not the order in which they were created/added.
func (f *FIFO) Delete(obj interface{}) error {
	id, err := f.keyFunc(obj)
	if err != nil {
		return KeyError{obj, err}
	}
	f.lock.Lock()
	defer f.lock.Unlock()
	f.populated = true
	delete(f.items, id)
	return err
}

// List returns a list of all the items.
// 获取队列的所有元素
func (f *FIFO) List() []interface{} {
	f.lock.RLock()
	defer f.lock.RUnlock()
	list := make([]interface{}, 0, len(f.items))
	for _, item := range f.items {
		list = append(list, item)
	}
	return list
}

// ListKeys returns a list of all the keys of the objects currently
// in the FIFO.
// 获取存储数据的所有对象的对象键，对于KV存储来说就是获取Key值
func (f *FIFO) ListKeys() []string {
	f.lock.RLock()
	defer f.lock.RUnlock()
	list := make([]string, 0, len(f.items))
	for key := range f.items {
		list = append(list, key)
	}
	return list
}

// Get returns the requested item, or sets exists=false.
func (f *FIFO) Get(obj interface{}) (item interface{}, exists bool, err error) {
	key, err := f.keyFunc(obj)
	if err != nil {
		return nil, false, KeyError{obj, err}
	}
	return f.GetByKey(key)
}

// GetByKey returns the requested item, or sets exists=false.
func (f *FIFO) GetByKey(key string) (item interface{}, exists bool, err error) {
	f.lock.RLock()
	defer f.lock.RUnlock()
	item, exists = f.items[key]
	return item, exists, nil
}

// IsClosed checks if the queue is closed
func (f *FIFO) IsClosed() bool {
	f.lock.Lock()
	defer f.lock.Unlock()
	return f.closed
}

// Pop waits until an item is ready and processes it. If multiple items are
// ready, they are returned in the order in which they were added/updated.
// The item is removed from the queue (and the store) before it is processed,
// so if you don't successfully process it, it should be added back with
// AddIfNotPresent(). process function is called under lock, so it is safe
// update data structures in it that need to be in sync with the queue.
func (f *FIFO) Pop(process PopProcessFunc) (interface{}, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	for {
		for len(f.queue) == 0 {
			// When the queue is empty, invocation of Pop() is blocked until new item is enqueued.
			// When Close() is called, the f.closed is set and the condition is broadcasted.
			// Which causes this loop to continue and return from the Pop().
			if f.closed {
				return nil, ErrFIFOClosed
			}
			// 队列中没有元素，所以当前协程被阻塞，并释放锁
			f.cond.Wait()
		}
		id := f.queue[0]
		// TODO 这里为什么不考虑内存泄露？ 为什么不使用 f.queue[0] = nil 释放空间
		f.queue = f.queue[1:]
		if f.initialPopulationCount > 0 {
			f.initialPopulationCount--
		}
		item, ok := f.items[id]
		if !ok {
			// fixme 卧槽，这里的并发控制真牛逼，什么场景会进入到这里呢？
			// Item may have been deleted subsequently.
			continue
		}
		delete(f.items, id)
		// 处理获取到的元素
		err := process(item)
		if e, ok := err.(ErrRequeue); ok {
			// 如果是重新入队的错误，那么入队
			f.addIfNotPresent(id, item)
			err = e.Err
		}
		return item, err
	}
}

// Replace will delete the contents of 'f', using instead the given map.
// 'f' takes ownership of the map, you should not reference the map again
// after calling this function. f's queue is reset, too; upon return, it
// will contain the items in the map, in no particular order.
func (f *FIFO) Replace(list []interface{}, resourceVersion string) error {
	items := make(map[string]interface{}, len(list))
	for _, item := range list {
		key, err := f.keyFunc(item)
		if err != nil {
			return KeyError{item, err}
		}
		items[key] = item
	}

	f.lock.Lock()
	defer f.lock.Unlock()

	if !f.populated {
		f.populated = true
		f.initialPopulationCount = len(items)
	}

	f.items = items
	f.queue = f.queue[:0]
	for id := range items {
		f.queue = append(f.queue, id)
	}
	if len(f.queue) > 0 {
		// 队列中有元素了，唤醒所有协程
		f.cond.Broadcast()
	}
	return nil
}

// Resync will ensure that every object in the Store has its key in the queue.
// This should be a no-op, because that property is maintained by all operations.
func (f *FIFO) Resync() error {
	f.lock.Lock()
	defer f.lock.Unlock()

	inQueue := sets.NewString()
	for _, id := range f.queue {
		inQueue.Insert(id)
	}
	for id := range f.items {
		if !inQueue.Has(id) {
			f.queue = append(f.queue, id)
		}
	}
	if len(f.queue) > 0 {
		f.cond.Broadcast()
	}
	return nil
}

// NewFIFO returns a Store which can be used to queue up items to
// process.
func NewFIFO(keyFunc KeyFunc) *FIFO {
	// TODO 这里为什么不给一个默认的初始值大小
	f := &FIFO{
		items:   map[string]interface{}{},
		queue:   []string{},
		keyFunc: keyFunc,
	}
	f.cond.L = &f.lock
	return f
}
