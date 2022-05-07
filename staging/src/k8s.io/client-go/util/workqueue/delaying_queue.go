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

package workqueue

import (
	"container/heap"
	"sync"
	"time"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/utils/clock"
)

// DelayingInterface is an Interface that can Add an item at a later time. This makes it easier to
// requeue items after failures without ending up in a hot-loop.
type DelayingInterface interface {
	Interface
	// AddAfter adds an item to the workqueue after the indicated duration has passed
	// 延时添加，即指定在duration时间之后加入到队列当中
	AddAfter(item interface{}, duration time.Duration)
}

// NewDelayingQueue constructs a new workqueue with delayed queuing ability
func NewDelayingQueue() DelayingInterface {
	return NewDelayingQueueWithCustomClock(clock.RealClock{}, "")
}

// NewDelayingQueueWithCustomQueue constructs a new workqueue with ability to
// inject custom queue Interface instead of the default one
func NewDelayingQueueWithCustomQueue(q Interface, name string) DelayingInterface {
	return newDelayingQueue(clock.RealClock{}, q, name)
}

// NewNamedDelayingQueue constructs a new named workqueue with delayed queuing ability
func NewNamedDelayingQueue(name string) DelayingInterface {
	return NewDelayingQueueWithCustomClock(clock.RealClock{}, name)
}

// NewDelayingQueueWithCustomClock constructs a new named workqueue
// with ability to inject real or fake clock for testing purposes
func NewDelayingQueueWithCustomClock(clock clock.WithTicker, name string) DelayingInterface {
	return newDelayingQueue(clock, NewNamed(name), name)
}

func newDelayingQueue(clock clock.WithTicker, q Interface, name string) *delayingType {
	ret := &delayingType{
		Interface: q,
		clock:     clock,
		// 如果10秒钟只能都没有元素，waitingLoop协程就会被唤醒，看看队列当中的元素是否有已经Ready的
		heartbeat: clock.NewTicker(maxWait),
		stopCh:    make(chan struct{}),
		// 延时队列最多只能放1000个元素，队列满了之后调用AddAfter函数就会阻塞
		waitingForAddCh: make(chan *waitFor, 1000),
		metrics:         newRetryMetrics(name),
	}

	// 在new一个延时队列之后，马上就启动一个协程处理延时队列当中的channel,当时间到了之后添加到队列当中
	go ret.waitingLoop()
	return ret
}

// delayingType wraps an Interface and provides delayed re-enquing
type delayingType struct {
	// 这里传入的实现就是通用队列Type
	Interface

	// clock tracks time for delayed firing
	// 时钟，用于获取时间
	clock clock.Clock

	// stopCh lets us signal a shutdown to the waiting loop
	// 延时以为着异步，就需要有另外一个协程处理，所以需要有推出信号
	stopCh chan struct{}
	// stopOnce guarantees we only signal shutdown a single time
	stopOnce sync.Once

	// heartbeat ensures we wait no more than maxWait before firing
	// 定时器，在没有任何数据操作的时候可以定时的唤醒处理协程
	heartbeat clock.Ticker

	// waitingForAddCh is a buffered channel that feeds waitingForAdd
	// 所有延迟添加的元素都被封装为waitFor放入到channel当中
	// 从这里可以推测出，延时队列的原理就是一个典型的生产者消费者模型，调用AddAfter的协程为生产者，
	// 而从waitingForAddCh中获取元素的为消费者
	waitingForAddCh chan *waitFor

	// metrics counts the number of retries
	metrics retryMetrics
}

// waitFor holds the data to add and the time it should be added
type waitFor struct {
	// 要添加到队列中的元素
	data t
	// 在什么时候这个元素被添加到队列当中
	//无非有两种指定形式：1、指定在某个时间点添加，2、指定在几秒钟后添加
	readyAt time.Time
	// index in the priority queue (heap)
	index int
}

// waitForPriorityQueue implements a priority queue for waitFor items.
//
// waitForPriorityQueue implements heap.Interface. The item occurring next in
// time (i.e., the item with the smallest readyAt) is at the root (index 0).
// Peek returns this minimum item at index 0. Pop returns the minimum item after
// it has been removed from the queue and placed at index Len()-1 by
// container/heap. Push adds an item at index Len(), and container/heap
// percolates it into the correct location.
// waitForPriorityQueue是一个二叉堆的实现，堆顶的元素是等待时间最小的元素
type waitForPriorityQueue []*waitFor

func (pq waitForPriorityQueue) Len() int {
	return len(pq)
}
func (pq waitForPriorityQueue) Less(i, j int) bool {
	return pq[i].readyAt.Before(pq[j].readyAt)
}
func (pq waitForPriorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].index = i
	pq[j].index = j
}

// Push adds an item to the queue. Push should not be called directly; instead,
// use `heap.Push`.
func (pq *waitForPriorityQueue) Push(x interface{}) {
	n := len(*pq)
	item := x.(*waitFor)
	item.index = n
	*pq = append(*pq, item)
}

// Pop removes an item from the queue. Pop should not be called directly;
// instead, use `heap.Pop`.
func (pq *waitForPriorityQueue) Pop() interface{} {
	n := len(*pq)
	item := (*pq)[n-1]
	item.index = -1
	*pq = (*pq)[0:(n - 1)]
	return item
}

// Peek returns the item at the beginning of the queue, without removing the
// item or otherwise mutating the queue. It is safe to call directly.
func (pq waitForPriorityQueue) Peek() interface{} {
	return pq[0]
}

// ShutDown stops the queue. After the queue drains, the returned shutdown bool
// on Get() will be true. This method may be invoked more than once.
func (q *delayingType) ShutDown() {
	q.stopOnce.Do(func() {
		q.Interface.ShutDown()
		// 关闭队列
		close(q.stopCh)
		// 清理心跳定时器
		q.heartbeat.Stop()
	})
}

// AddAfter adds the given item to the work queue after the given delay
func (q *delayingType) AddAfter(item interface{}, duration time.Duration) {
	// don't add if we're already shutting down
	if q.ShuttingDown() {
		return
	}

	q.metrics.retry()

	// immediately add things with no delay
	// 如果指定的元素延时时间小于等于零，就直接添加
	if duration <= 0 {
		q.Add(item)
		return
	}

	select {
	case <-q.stopCh:
		// unblock if ShutDown() is called
	// 往channel中添加延时元素
	case q.waitingForAddCh <- &waitFor{data: item, readyAt: q.clock.Now().Add(duration)}:
	}
}

// maxWait keeps a max bound on the wait time. It's just insurance against weird things happening.
// Checking the queue every 10 seconds isn't expensive and we know that we'll never end up with an
// expired item sitting for more than 10 seconds.
const maxWait = 10 * time.Second

// waitingLoop runs until the workqueue is shutdown and keeps a check on the list of items to be added.
func (q *delayingType) waitingLoop() {
	defer utilruntime.HandleCrash()

	// Make a placeholder channel to use when there are no items in our list
	never := make(<-chan time.Time)

	// Make a timer that expires when the item at the head of the waiting queue is ready
	var nextReadyAtTimer clock.Timer

	waitingForQueue := &waitForPriorityQueue{}
	// 这里实际上在做二叉堆化
	heap.Init(waitingForQueue)

	waitingEntryByData := map[t]*waitFor{}

	for {
		if q.Interface.ShuttingDown() {
			return
		}

		now := q.clock.Now()

		// Add ready entries
		for waitingForQueue.Len() > 0 {
			// 拿出堆顶元素看一眼，如果堆顶元素添加的时间大于当前时间，就说明还没到时间
			entry := waitingForQueue.Peek().(*waitFor)
			if entry.readyAt.After(now) {
				// 由于waitingForQueue是一个二叉堆，堆顶的元素都比当前时间大，那么其余的元素等待的时间
				// 肯定也比当前时间安大，因此只能退出等待
				break
			}

			// 如果堆顶元素比当前时间小，就说明已经到时间了，可以直接添加到队列当中
			entry = heap.Pop(waitingForQueue).(*waitFor)
			q.Add(entry.data)
			delete(waitingEntryByData, entry.data)
		}

		// Set up a wait for the first item's readyAt (if one exists)
		nextReadyAt := never
		if waitingForQueue.Len() > 0 {
			if nextReadyAtTimer != nil {
				nextReadyAtTimer.Stop()
			}
			// 拿出堆顶元素
			entry := waitingForQueue.Peek().(*waitFor)
			// 计算堆顶元素添加到队列当中还需要等待的时间，然后定个时间
			nextReadyAtTimer = q.clock.NewTimer(entry.readyAt.Sub(now))
			nextReadyAt = nextReadyAtTimer.C()
		}

		select {
		case <-q.stopCh:
			// 队列已经被关闭，waitingLoop协程直接退出
			return

		case <-q.heartbeat.C():
			// continue the loop, which will add ready items
			// 即使队列中没有元素，waitingLoop这个协程也会定期苏醒
		case <-nextReadyAt:
			// continue the loop, which will add ready items
			// 如果堆顶元素的等待时间到了，此时waitingLoop协程就会被唤醒
		case waitEntry := <-q.waitingForAddCh:
			// 如果waitingForAddCh channel中有元素
			if waitEntry.readyAt.After(q.clock.Now()) {
				// 当前元素的时间还没有到，直接把元素放入堆中，并且用一个map做记录
				insert(waitingForQueue, waitingEntryByData, waitEntry)
			} else {
				// 当前元素的等待时间已经到了，那么直接添加到队列当中
				q.Add(waitEntry.data)
			}

			// 一旦waitingLoop协程拿到数据，这里就会把waitingForAddCh通道中的所有数据取出来
			// 这里能够加快数据的处理，防止有些元素在channel中放了很久还是没有被处理
			drained := false
			for !drained {
				select {
				case waitEntry := <-q.waitingForAddCh:
					if waitEntry.readyAt.After(q.clock.Now()) {
						insert(waitingForQueue, waitingEntryByData, waitEntry)
					} else {
						q.Add(waitEntry.data)
					}
				default:
					drained = true
				}
			}
		}
	}
}

// insert adds the entry to the priority queue, or updates the readyAt if it already exists in the queue
func insert(q *waitForPriorityQueue, knownEntries map[t]*waitFor, entry *waitFor) {
	// if the entry already exists, update the time only if it would cause the item to be queued sooner
	existing, exists := knownEntries[entry.data]
	if exists {
		// 元素已经存在，说明之前被添加过，由于map的key就是元素数据，所以数据肯定是一样的，不一样的只能是延时时间
		// 所以只需要更新一下之前已经存在元素的延时时间
		if existing.readyAt.After(entry.readyAt) {
			existing.readyAt = entry.readyAt
			// 重新进行二叉堆化
			heap.Fix(q, existing.index)
		}

		return
	}

	// 元素不存在，就直接放到二叉堆中，按照时间从小到大排序
	heap.Push(q, entry)
	// 同时使用map记录当前元素是否已经添加过
	knownEntries[entry.data] = entry
}
