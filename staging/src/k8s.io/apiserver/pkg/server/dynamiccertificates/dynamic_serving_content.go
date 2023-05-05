/*
Copyright 2019 The Kubernetes Authors.

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

package dynamiccertificates

import (
	"context"
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

// DynamicCertKeyPairContent provides a CertKeyContentProvider that can dynamically react to new file content
// 监听证书和私钥文件的变化，一旦证书或者私钥其中任何一个发生改变，就通知所有的Listener
type DynamicCertKeyPairContent struct {
	// 值为：Purpose::CertFile::KeyFile
	name string

	// certFile is the name of the certificate file to read.
	// 数字证书
	certFile string
	// keyFile is the name of the key file to read.
	// 私钥
	keyFile string

	// certKeyPair is a certKeyContent that contains the last read, non-zero length content of the key and cert
	// 用于保存数字证书和私钥
	certKeyPair atomic.Value

	// 当证书和私钥其中任何一个发生变化，就通知所有的Listener
	listeners []Listener

	// queue only ever has one item, but it has nice error handling backoff/retry semantics
	// 队列中方式的元素是没有意义的，因此可以随意放，目的是一个元素的放入代表certFile以及keyFile发生了变化
	queue workqueue.RateLimitingInterface
}

var _ CertKeyContentProvider = &DynamicCertKeyPairContent{}
var _ ControllerRunner = &DynamicCertKeyPairContent{}

// NewDynamicServingContentFromFiles returns a dynamic CertKeyContentProvider based on a cert and key filename
func NewDynamicServingContentFromFiles(purpose, certFile, keyFile string) (*DynamicCertKeyPairContent, error) {
	if len(certFile) == 0 || len(keyFile) == 0 {
		return nil, fmt.Errorf("missing filename for serving cert")
	}
	name := fmt.Sprintf("%s::%s::%s", purpose, certFile, keyFile)

	ret := &DynamicCertKeyPairContent{
		name:     name,
		certFile: certFile,
		keyFile:  keyFile,
		queue:    workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), fmt.Sprintf("DynamicCABundle-%s", purpose)),
	}
	// 启动的时候就加载一次certFile，keyFile文件
	if err := ret.loadCertKeyPair(); err != nil {
		return nil, err
	}

	return ret, nil
}

// AddListener adds a listener to be notified when the serving cert content changes.
func (c *DynamicCertKeyPairContent) AddListener(listener Listener) {
	c.listeners = append(c.listeners, listener)
}

// loadCertKeyPair determines the next set of content for the file.
func (c *DynamicCertKeyPairContent) loadCertKeyPair() error {
	// 读取证书
	cert, err := ioutil.ReadFile(c.certFile)
	if err != nil {
		return err
	}
	// 读取私钥
	key, err := ioutil.ReadFile(c.keyFile)
	if err != nil {
		return err
	}
	if len(cert) == 0 || len(key) == 0 {
		return fmt.Errorf("missing content for serving cert %q", c.Name())
	}

	// Ensure that the key matches the cert and both are valid
	// 用于保证证书和私钥是配对并且是有效的
	_, err = tls.X509KeyPair(cert, key)
	if err != nil {
		return err
	}

	newCertKey := &certKeyContent{
		cert: cert,
		key:  key,
	}

	// check to see if we have a change. If the values are the same, do nothing.
	existing, ok := c.certKeyPair.Load().(*certKeyContent)
	// 对比之前加载过的证书和私钥，如果没有发生改变就直接退出
	if ok && existing != nil && existing.Equal(newCertKey) {
		return nil
	}

	// 否则，保存最新的数字证书以及私钥
	c.certKeyPair.Store(newCertKey)
	klog.V(2).InfoS("Loaded a new cert/key pair", "name", c.Name())

	// 由于证书和私钥发生改变，因此需要通知所有的Listener
	for _, listener := range c.listeners {
		listener.Enqueue()
	}

	return nil
}

// RunOnce runs a single sync loop
func (c *DynamicCertKeyPairContent) RunOnce(ctx context.Context) error {
	return c.loadCertKeyPair()
}

// Run starts the controller and blocks until context is killed.
func (c *DynamicCertKeyPairContent) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.InfoS("Starting controller", "name", c.name)
	defer klog.InfoS("Shutting down controller", "name", c.name)

	// doesn't matter what workers say, only start one.
	go wait.Until(c.runWorker, time.Second, ctx.Done())

	// start the loop that watches the cert and key files until stopCh is closed.
	go wait.Until(func() {
		if err := c.watchCertKeyFile(ctx.Done()); err != nil {
			klog.ErrorS(err, "Failed to watch cert and key file, will retry later")
		}
	}, time.Minute, ctx.Done())

	<-ctx.Done()
}

func (c *DynamicCertKeyPairContent) watchCertKeyFile(stopCh <-chan struct{}) error {
	// Trigger a check here to ensure the content will be checked periodically even if the following watch fails.
	// TODO 为啥这里需要入队一个元素？
	c.queue.Add(workItemKey)

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("error creating fsnotify watcher: %v", err)
	}
	defer w.Close()

	// 监听证书和私钥文件
	if err := w.Add(c.certFile); err != nil {
		return fmt.Errorf("error adding watch for file %s: %v", c.certFile, err)
	}
	if err := w.Add(c.keyFile); err != nil {
		return fmt.Errorf("error adding watch for file %s: %v", c.keyFile, err)
	}
	// Trigger a check in case the file is updated before the watch starts.
	// TODO 为啥这里需要入队一个元素？
	c.queue.Add(workItemKey)

	for {
		select {
		case e := <-w.Events:
			if err := c.handleWatchEvent(e, w); err != nil {
				return err
			}
		case err := <-w.Errors:
			return fmt.Errorf("received fsnotify error: %v", err)
		case <-stopCh:
			return nil
		}
	}
}

// handleWatchEvent triggers reloading the cert and key file, and restarts a new watch if it's a Remove or Rename event.
// If one file is updated before the other, the loadCertKeyPair method will catch the mismatch and will not apply the
// change. When an event of the other file is received, it will trigger reloading the files again and the new content
// will be loaded and used.
func (c *DynamicCertKeyPairContent) handleWatchEvent(e fsnotify.Event, w *fsnotify.Watcher) error {
	// This should be executed after restarting the watch (if applicable) to ensure no file event will be missing.
	// 入队，触发loadCertKeyPair被执行，从而加载最新的证书和私钥
	defer c.queue.Add(workItemKey)
	// 只要证书或者私钥不是删除或者重命名，就可以直接读取文件内容
	if e.Op&(fsnotify.Remove|fsnotify.Rename) == 0 {
		return nil
	}
	// 否则就删除文件之后重新监听
	if err := w.Remove(e.Name); err != nil {
		klog.InfoS("Failed to remove file watch, it may have been deleted", "file", e.Name, "err", err)
	}
	if err := w.Add(e.Name); err != nil {
		return fmt.Errorf("error adding watch for file %s: %v", e.Name, err)
	}
	return nil
}

func (c *DynamicCertKeyPairContent) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *DynamicCertKeyPairContent) processNextWorkItem() bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	err := c.loadCertKeyPair()
	if err == nil {
		c.queue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.queue.AddRateLimited(dsKey)

	return true
}

// Name is just an identifier
func (c *DynamicCertKeyPairContent) Name() string {
	return c.name
}

// CurrentCertKeyContent provides cert and key byte content
func (c *DynamicCertKeyPairContent) CurrentCertKeyContent() ([]byte, []byte) {
	certKeyContent := c.certKeyPair.Load().(*certKeyContent)
	return certKeyContent.cert, certKeyContent.key
}
