/*
Copyright 2018 The Kubernetes Authors.

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

package lease

import (
	"context"
	"fmt"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	coordclientset "k8s.io/client-go/kubernetes/typed/coordination/v1"
	"k8s.io/utils/clock"
	"k8s.io/utils/pointer"

	"k8s.io/klog/v2"
)

const (
	// maxUpdateRetries is the number of immediate, successive retries the controller will attempt
	// when renewing the lease before it waits for the renewal interval before trying again,
	// similar to what we do for node status retries
	maxUpdateRetries = 5
	// maxBackoff is the maximum sleep time during backoff (e.g. in backoffEnsureLease)
	maxBackoff = 7 * time.Second
)

// Controller manages creating and renewing the lease for this component (kube-apiserver, kubelet, etc.)
type Controller interface {
	Run(stopCh <-chan struct{})
}

// ProcessLeaseFunc processes the given lease in-place
type ProcessLeaseFunc func(*coordinationv1.Lease) error

type controller struct {
	// K8S ClientSet
	client clientset.Interface
	// Lease Client
	leaseClient                coordclientset.LeaseInterface
	holderIdentity             string
	leaseNamespace             string
	leaseDurationSeconds       int32
	renewInterval              time.Duration
	clock                      clock.Clock
	onRepeatedHeartbeatFailure func()

	// latestLease is the latest lease which the controller updated or created
	// 最新的Lease对象
	latestLease *coordinationv1.Lease

	// newLeasePostProcessFunc allows customizing a lease object (e.g. setting OwnerReference)
	// before every time the lease is created/refreshed(updated). Note that an error will block
	// a lease CREATE, causing the controller to retry next time, but an error won't block a
	// lease UPDATE.
	newLeasePostProcessFunc ProcessLeaseFunc
}

// NewController constructs and returns a controller
// 1、保证APIServer一定存在一个Lease对象，没有就创建一个Lease对象并调用Crate接口持久化到ETCD当中
// 2、如果APIServer中已存在Lease对象，那么更新这个Lease对象的reNew时间，为该Lease对象续期
func NewController(clock clock.Clock, client clientset.Interface, holderIdentity string, leaseDurationSeconds int32,
	onRepeatedHeartbeatFailure func(), renewInterval time.Duration, leaseNamespace string, newLeasePostProcessFunc ProcessLeaseFunc) Controller {
	var leaseClient coordclientset.LeaseInterface
	if client != nil {
		leaseClient = client.CoordinationV1().Leases(leaseNamespace)
	}
	return &controller{
		client:                     client,
		leaseClient:                leaseClient,
		holderIdentity:             holderIdentity,
		leaseNamespace:             leaseNamespace,
		leaseDurationSeconds:       leaseDurationSeconds,
		renewInterval:              renewInterval,
		clock:                      clock,
		onRepeatedHeartbeatFailure: onRepeatedHeartbeatFailure,
		newLeasePostProcessFunc:    newLeasePostProcessFunc,
	}
}

// Run runs the controller
func (c *controller) Run(stopCh <-chan struct{}) {
	if c.leaseClient == nil {
		klog.Infof("lease controller has nil lease client, will not claim or renew leases")
		return
	}
	wait.JitterUntil(c.sync, c.renewInterval, 0.04, true, stopCh)
}

func (c *controller) sync() {
	if c.latestLease != nil {
		// As long as the lease is not (or very rarely) updated by any other agent than the component itself,
		// we can optimistically assume it didn't change since our last update and try updating
		// based on the version from that time. Thanks to it we avoid GET call and reduce load
		// on etcd and kube-apiserver.
		// If at some point other agents will also be frequently updating the Lease object, this
		// can result in performance degradation, because we will end up with calling additional
		// GET/PUT - at this point this whole "if" should be removed.
		// 1、保证APIServer一定存在一个Lease对象，没有就创建一个Lease对象并调用Crate接口持久化到ETCD当中
		// 2、如果APIServer中已存在Lease对象，那么更新这个Lease对象的reNew时间，为该Lease对象续期
		err := c.retryUpdateLease(c.latestLease)
		if err == nil {
			return
		}
		klog.Infof("failed to update lease using latest lease, fallback to ensure lease, err: %v", err)
	}

	// 保证当前APIServer当中一定存在一个Lease对象
	lease, created := c.backoffEnsureLease()
	c.latestLease = lease
	// we don't need to update the lease if we just created it
	if !created && lease != nil {
		if err := c.retryUpdateLease(lease); err != nil {
			klog.Errorf("%v, will retry after %v", err, c.renewInterval)
		}
	}
}

// backoffEnsureLease attempts to create the lease if it does not exist,
// and uses exponentially increasing waits to prevent overloading the API server
// with retries. Returns the lease, and true if this call created the lease,
// false otherwise.
func (c *controller) backoffEnsureLease() (*coordinationv1.Lease, bool) {
	var (
		lease   *coordinationv1.Lease
		created bool
		err     error
	)
	sleep := 100 * time.Millisecond
	for {
		// 保证当前APIServer当中一定存在一个Lease对象
		lease, created, err = c.ensureLease()
		if err == nil {
			break
		}
		sleep = minDuration(2*sleep, maxBackoff)
		klog.Errorf("failed to ensure lease exists, will retry in %v, error: %v", sleep, err)
		// backoff wait
		c.clock.Sleep(sleep)
	}
	return lease, created
}

// ensureLease creates the lease if it does not exist. Returns the lease and
// a bool (true if this call created the lease), or any error that occurs.
func (c *controller) ensureLease() (*coordinationv1.Lease, bool, error) {
	// 从ETCD当中读取Lease对象
	lease, err := c.leaseClient.Get(context.TODO(), c.holderIdentity, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		// 如果当前APIServer当中还没有创建过Lease对象，那就实例化一个新的Lease对象
		leaseToCreate, err := c.newLease(nil)
		// An error occurred during allocating the new lease (likely from newLeasePostProcessFunc).
		// Given that we weren't able to set the lease correctly, we simply
		// not create it this time - we will retry in the next iteration.
		if err != nil {
			return nil, false, nil
		}
		// 持久化到ETCD当中
		lease, err := c.leaseClient.Create(context.TODO(), leaseToCreate, metav1.CreateOptions{})
		if err != nil {
			return nil, false, err
		}
		return lease, true, nil
	} else if err != nil {
		// unexpected error getting lease
		return nil, false, err
	}
	// lease already existed
	return lease, false, nil
}

// retryUpdateLease attempts to update the lease for maxUpdateRetries,
// call this once you're sure the lease has been created
// 1、保证APIServer一定存在一个Lease对象，没有就创建一个Lease对象并调用Crate接口持久化到ETCD当中
// 2、如果APIServer中已存在Lease对象，那么更新这个Lease对象的reNew时间，为该Lease对象续期
func (c *controller) retryUpdateLease(base *coordinationv1.Lease) error {
	// 最多尝试5此修改Lease
	for i := 0; i < maxUpdateRetries; i++ {
		// 如果LeaseController还没有刚开始运行还没有实例化Lease对象，那就实例化一个。否则更新租约
		leaseToUpdate, _ := c.newLease(base)
		// 把内存中更新的Lease对象持久化到ETCD当中
		// TODO 如果LeaseController刚开始运行，此时APIServer还没有Lease对象，这里调用Update接口肯定会报错，但是错误不应该是Conflict错误吧
		lease, err := c.leaseClient.Update(context.TODO(), leaseToUpdate, metav1.UpdateOptions{})
		if err == nil {
			c.latestLease = lease
			return nil
		}
		klog.Errorf("failed to update lease, error: %v", err)
		// OptimisticLockError requires getting the newer version of lease to proceed.
		if apierrors.IsConflict(err) {
			// 保证当前APIServer一定存在一个Lease对象，如果没有就创建一个Lease对象并持久化到ETCD当中
			base, _ = c.backoffEnsureLease()
			continue
		}
		if i > 0 && c.onRepeatedHeartbeatFailure != nil {
			c.onRepeatedHeartbeatFailure()
		}
	}
	return fmt.Errorf("failed %d attempts to update lease", maxUpdateRetries)
}

// newLease constructs a new lease if base is nil, or returns a copy of base
// with desired state asserted on the copy.
// Note that an error will block lease CREATE, causing the CREATE to be retried in
// the next iteration; but the error won't block lease refresh (UPDATE).
func (c *controller) newLease(base *coordinationv1.Lease) (*coordinationv1.Lease, error) {
	// Use the bare minimum set of fields; other fields exist for debugging/legacy,
	// but we don't need to make component heartbeats more complicated by using them.
	var lease *coordinationv1.Lease
	if base == nil {
		// 如果Base为空，说明LeaseController刚刚开始运行，还没有生成Lease对象，那么直接实例化一个Lease对象
		lease = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:      c.holderIdentity,
				Namespace: c.leaseNamespace,
			},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       pointer.StringPtr(c.holderIdentity),
				LeaseDurationSeconds: pointer.Int32Ptr(c.leaseDurationSeconds),
			},
		}
	} else {
		// 如果有，那么就深度拷贝一个Lease对象
		lease = base.DeepCopy()
	}
	// Lease对象续期
	lease.Spec.RenewTime = &metav1.MicroTime{Time: c.clock.Now()}

	if c.newLeasePostProcessFunc != nil {
		// 给Lease对象添加k8s.io/component=kube-apiserver的Label
		err := c.newLeasePostProcessFunc(lease)
		return lease, err
	}

	return lease, nil
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
