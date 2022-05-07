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

package workqueue

import (
	"sync"
	"time"

	"k8s.io/utils/clock"
)

type Interface interface {
	// Add 像队列当中添加元素
	Add(item interface{})
	// Len 队列的长度
	Len() int
	// Get 从队列中获取一个元素，该元素并不会从队列中删除，只有在执行Done(item)方法之后，队列中的item元素才会删除
	// 获取元素的时候可能会被阻塞，因为队列可能为空
	Get() (item interface{}, shutdown bool)
	// Done 告诉队列item元素已经处理完毕
	Done(item interface{})
	// ShutDown 关闭队列
	ShutDown()
	ShutDownWithDrain()
	// ShuttingDown 查询队列是否正在关闭
	ShuttingDown() bool
}

// New constructs a new work queue (see the package comment).
func New() *Type {
	return NewNamed("")
}

func NewNamed(name string) *Type {
	rc := clock.RealClock{}
	return newQueue(
		rc,
		globalMetricsFactory.newQueueMetrics(name, rc),
		defaultUnfinishedWorkUpdatePeriod,
	)
}

func newQueue(c clock.WithTicker, metrics queueMetrics, updatePeriod time.Duration) *Type {
	t := &Type{
		clock:                      c,
		dirty:                      set{},
		processing:                 set{},
		cond:                       sync.NewCond(&sync.Mutex{}),
		metrics:                    metrics,
		unfinishedWorkUpdatePeriod: updatePeriod,
	}

	// Don't start the goroutine for a type of noMetrics so we don't consume
	// resources unnecessarily
	if _, ok := metrics.(noMetrics); !ok {
		go t.updateUnfinishedWorkLoop()
	}

	return t
}

const defaultUnfinishedWorkUpdatePeriod = 500 * time.Millisecond

// Type is a work queue (see the package comment).
type Type struct {
	// queue defines the order in which we will work on items. Every
	// element of queue should be in the dirty set and not in the processing set.
	// 队列中的元素，底层为切片，说明元素的取出是有序的
	queue []t

	// dirty defines all of the items that need to be processed.
	// dirty元素集合,元素放入队列当中的同时也会放在dirty集合当中，为什么需要这么设计呢？
	// 原因是判断一个元素是否已经放入到对且当中怎么判断呢？ 如果是数组必须要使用O(n)的时间复杂度
	// 去判断，而通过集合可以在O(1)的时间复杂度就知道元素是否已经添加进入到队列当中了
	dirty set

	// Things that are currently being processed are in the processing set.
	// These things may be simultaneously in the dirty set. When we finish
	// processing something and remove it from this set, we'll check if
	// it's in the dirty set, and if so, add it to the queue.
	// 正在处理的元素集合
	processing set

	// 条件同步，如果在获取元素的时候，队列为空，显然该协程就应该被阻塞
	cond *sync.Cond

	// 队列已经被关闭的标记
	shuttingDown bool
	drain        bool

	metrics queueMetrics

	unfinishedWorkUpdatePeriod time.Duration
	clock                      clock.WithTicker
}

type empty struct{}
type t interface{}
type set map[t]empty

func (s set) has(item t) bool {
	_, exists := s[item]
	return exists
}

func (s set) insert(item t) {
	s[item] = empty{}
}

func (s set) delete(item t) {
	delete(s, item)
}

func (s set) len() int {
	return len(s)
}

// Add marks item as needing processing.
func (q *Type) Add(item interface{}) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	// 若队列已经关闭，则不再允许添加数据
	if q.shuttingDown {
		return
	}
	// 若dirty集合中已经存在了，也是直接返回，其实就是队列中已经添加该元素了
	if q.dirty.has(item) {
		return
	}
	// 通知metrics添加了新的元素，这个是用来做监控的
	q.metrics.add(item)

	q.dirty.insert(item)
	// 元素正在被处理，则直接返回
	// 被处理的元素是如何拿到的？ 似乎不是直接从queue中拿到的  什么情况下一个元素还未被添加到queue中，就直接能在processing中获取到
	// 有一种情况可能会进入到这里，那就是某个协程调用了Get方法，但是改写成还未调用Done方法，此时另外一个协程调用了Add方法，此时就会进入这个分支
	// 只有之前的item元素被处理完成之后，也就是协程调用了Done方法，新的item元素才会被正常入队重新处理
	if q.processing.has(item) {
		return
	}

	// 把新元素添加到队列尾部
	q.queue = append(q.queue, item)
	// 通知有新元素到了，此时被阻塞的协程就会被唤醒，而且有且仅有一个协程被唤醒
	q.cond.Signal()
}

// Len returns the current queue length, for informational purposes only. You
// shouldn't e.g. gate a call to Add() or Get() on Len() being a particular
// value, that can't be synchronized properly.
func (q *Type) Len() int {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	return len(q.queue)
}

// Get blocks until it can return an item to be processed. If shutdown = true,
// the caller should end their goroutine. You must call Done with item when you
// have finished processing it.
func (q *Type) Get() (item interface{}, shutdown bool) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	for len(q.queue) == 0 && !q.shuttingDown {
		// 如果队列中没有元素，那么当前协程就进入阻塞状态，此时协程会释放自己的锁
		q.cond.Wait()
	}
	// 协程被唤醒，但是没有数据，说明队列已经被关闭了
	if len(q.queue) == 0 {
		// We must be shutting down.
		return nil, true
	}

	// go语言中传递的是值，因此item是q.queue[0]的一份拷贝
	item = q.queue[0]
	// The underlying array still exists and reference this object, so the object will not be garbage collected.
	// 这里底层数组确实需要手动释放q.queue[0]的空间，否则该元素不会被gc
	q.queue[0] = nil
	// 移除元素
	q.queue = q.queue[1:]

	q.metrics.get(item)

	// 从队列中去除的元素会被添加到processing集合当中
	q.processing.insert(item)
	// 从dirty集合中删除
	q.dirty.delete(item)

	return item, false
}

// Done marks item as done processing, and if it has been marked as dirty again
// while it was being processed, it will be re-added to the queue for
// re-processing.
func (q *Type) Done(item interface{}) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()

	q.metrics.done(item)

	// item已经被处理完毕，因此需要从processing中删除
	q.processing.delete(item)
	// 如果dirty中还存在item元素，所以在一个协程处理item元素的过程中，另外一个协程调用了Add方法添加元素
	// 因此dirty集合当中存在该元素
	if q.dirty.has(item) {
		// 如果dirty中存在item元素，那么就需要把item元素重新入队，可以理解为
		q.queue = append(q.queue, item)
		// 唤醒阻塞的某个协程
		q.cond.Signal()
	} else if q.processing.len() == 0 {
		q.cond.Signal()
	}
}

// ShutDown will cause q to ignore all new items added to it and
// immediately instruct the worker goroutines to exit.
func (q *Type) ShutDown() {
	q.setDrain(false)
	q.shutdown()
}

// ShutDownWithDrain will cause q to ignore all new items added to it. As soon
// as the worker goroutines have "drained", i.e: finished processing and called
// Done on all existing items in the queue; they will be instructed to exit and
// ShutDownWithDrain will return. Hence: a strict requirement for using this is;
// your workers must ensure that Done is called on all items in the queue once
// the shut down has been initiated, if that is not the case: this will block
// indefinitely. It is, however, safe to call ShutDown after having called
// ShutDownWithDrain, as to force the queue shut down to terminate immediately
// without waiting for the drainage.
func (q *Type) ShutDownWithDrain() {
	q.setDrain(true)
	q.shutdown()
	for q.isProcessing() && q.shouldDrain() {
		q.waitForProcessing()
	}
}

// isProcessing indicates if there are still items on the work queue being
// processed. It's used to drain the work queue on an eventual shutdown.
func (q *Type) isProcessing() bool {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	return q.processing.len() != 0
}

// waitForProcessing waits for the worker goroutines to finish processing items
// and call Done on them.
func (q *Type) waitForProcessing() {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	// Ensure that we do not wait on a queue which is already empty, as that
	// could result in waiting for Done to be called on items in an empty queue
	// which has already been shut down, which will result in waiting
	// indefinitely.
	if q.processing.len() == 0 {
		return
	}
	q.cond.Wait()
}

func (q *Type) setDrain(shouldDrain bool) {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	q.drain = shouldDrain
}

func (q *Type) shouldDrain() bool {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	return q.drain
}

func (q *Type) shutdown() {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()
	q.shuttingDown = true
	// 唤醒所有被阻塞的协程
	q.cond.Broadcast()
}

func (q *Type) ShuttingDown() bool {
	q.cond.L.Lock()
	defer q.cond.L.Unlock()

	return q.shuttingDown
}

func (q *Type) updateUnfinishedWorkLoop() {
	t := q.clock.NewTicker(q.unfinishedWorkUpdatePeriod)
	defer t.Stop()
	for range t.C() {
		if !func() bool {
			q.cond.L.Lock()
			defer q.cond.L.Unlock()
			if !q.shuttingDown {
				q.metrics.updateUnfinishedWork()
				return true
			}
			return false

		}() {
			return
		}
	}
}
