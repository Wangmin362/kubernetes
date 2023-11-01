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

package server

import (
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apiserver/pkg/server/healthz"
	restclient "k8s.io/client-go/rest"
	"k8s.io/klog/v2"
)

// PostStartHookFunc is a function that is called after the server has started.
// It must properly handle cases like:
//  1. asynchronous start in multiple API server processes
//  2. conflicts between the different processes all trying to perform the same action
//  3. partially complete work (API server crashes while running your hook)
//  4. API server access **BEFORE** your hook has completed
//
// Think of it like a mini-controller that is super privileged and gets to run in-process
// If you use this feature, tag @deads2k on github who has promised to review code for anyone's PostStartHook
// until it becomes easier to use.
type PostStartHookFunc func(context PostStartHookContext) error

// PreShutdownHookFunc is a function that can be added to the shutdown logic.
type PreShutdownHookFunc func() error

// PostStartHookContext provides information about this API server to a PostStartHookFunc
type PostStartHookContext struct {
	// LoopbackClientConfig is a config for a privileged loopback connection to the API server
	LoopbackClientConfig *restclient.Config
	// StopCh is the channel that will be closed when the server stops
	StopCh <-chan struct{}
}

// PostStartHookProvider is an interface in addition to provide a post start hook for the api server
type PostStartHookProvider interface {
	PostStartHook() (string, PostStartHookFunc, error)
}

type postStartHookEntry struct {
	hook PostStartHookFunc
	// originatingStack holds the stack that registered postStartHooks. This allows us to show a more helpful message
	// for duplicate registration.
	originatingStack string

	// done will be closed when the postHook is finished
	done chan struct{}
}

type PostStartHookConfigEntry struct {
	hook PostStartHookFunc
	// originatingStack holds the stack that registered postStartHooks. This allows us to show a more helpful message
	// for duplicate registration.
	originatingStack string
}

type preShutdownHookEntry struct {
	hook PreShutdownHookFunc
}

// AddPostStartHook allows you to add a PostStartHook.
// 1、被禁用PostStartHook不再允许添加
// 2、同名的PostStartHook不允许添加
func (s *GenericAPIServer) AddPostStartHook(name string, hook PostStartHookFunc) error {
	if len(name) == 0 {
		return fmt.Errorf("missing name")
	}
	if hook == nil {
		return fmt.Errorf("hook func may not be nil: %q", name)
	}
	if s.disabledPostStartHooks.Has(name) {
		klog.V(1).Infof("skipping %q because it was explicitly disabled", name)
		return nil
	}

	s.postStartHookLock.Lock()
	defer s.postStartHookLock.Unlock()

	// 1、用于标识postStartHook是否被调用，如果已经被调用，那么不能再向GenericServer添加PostStartHook，PostStartHook被调用，说明了
	// GenericServer基本已经启动完成了，此时添加的PostStartHook极有可能无法被执行，因此是一旦postStartHook被执行，就不再允许添加
	// PostStartHook
	if s.postStartHooksCalled {
		return fmt.Errorf("unable to add %q because PostStartHooks have already been called", name)
	}
	// 不能重复添加PostStartHook，这里需要报错提醒用户，否则要么用户添加的PostStartHook把别人的覆盖了，要么用户想要添加的PostStartHook
	// 被忽略，这两种情况下的任意一种都不是我们想看到的。用明确的错误提示用户才是正确的做法。
	if postStartHook, exists := s.postStartHooks[name]; exists {
		// this is programmer error, but it can be hard to debug
		return fmt.Errorf("unable to add %q because it was already registered by: %s", name, postStartHook.originatingStack)
	}

	// done is closed when the poststarthook is finished.  This is used by the health check to be able to indicate
	// that the poststarthook is finished
	// done实际上是为了两个协程之间的通信，当PostStartHook执行完成之后，BootSequenceHealthCheck就会检测通过
	done := make(chan struct{})
	// TODO 猜测是GenericServer启动完成之后会启动健康检测
	if err := s.AddBootSequenceHealthChecks(postStartHookHealthz{name: "poststarthook/" + name, done: done}); err != nil {
		return err
	}
	// 添加PostStartHook，就是一个简单的Map添加元素
	s.postStartHooks[name] = postStartHookEntry{hook: hook, originatingStack: string(debug.Stack()), done: done}

	return nil
}

// AddPostStartHookOrDie allows you to add a PostStartHook, but dies on failure
func (s *GenericAPIServer) AddPostStartHookOrDie(name string, hook PostStartHookFunc) {
	if err := s.AddPostStartHook(name, hook); err != nil {
		klog.Fatalf("Error registering PostStartHook %q: %v", name, err)
	}
}

// AddPreShutdownHook allows you to add a PreShutdownHook.
func (s *GenericAPIServer) AddPreShutdownHook(name string, hook PreShutdownHookFunc) error {
	if len(name) == 0 {
		return fmt.Errorf("missing name")
	}
	if hook == nil {
		return nil
	}

	s.preShutdownHookLock.Lock()
	defer s.preShutdownHookLock.Unlock()

	if s.preShutdownHooksCalled {
		return fmt.Errorf("unable to add %q because PreShutdownHooks have already been called", name)
	}
	if _, exists := s.preShutdownHooks[name]; exists {
		return fmt.Errorf("unable to add %q because it is already registered", name)
	}

	s.preShutdownHooks[name] = preShutdownHookEntry{hook: hook}

	return nil
}

// AddPreShutdownHookOrDie allows you to add a PostStartHook, but dies on failure
func (s *GenericAPIServer) AddPreShutdownHookOrDie(name string, hook PreShutdownHookFunc) {
	if err := s.AddPreShutdownHook(name, hook); err != nil {
		klog.Fatalf("Error registering PreShutdownHook %q: %v", name, err)
	}
}

// RunPostStartHooks runs the PostStartHooks for the server
func (s *GenericAPIServer) RunPostStartHooks(stopCh <-chan struct{}) {
	s.postStartHookLock.Lock()
	defer s.postStartHookLock.Unlock()
	s.postStartHooksCalled = true

	context := PostStartHookContext{
		LoopbackClientConfig: s.LoopbackClientConfig,
		StopCh:               stopCh,
	}

	for hookName, hookEntry := range s.postStartHooks {
		go runPostStartHook(hookName, hookEntry, context)
	}
}

// RunPreShutdownHooks runs the PreShutdownHooks for the server
// 运行GenericServer的PreShutdownHook
func (s *GenericAPIServer) RunPreShutdownHooks() error {
	var errorList []error

	s.preShutdownHookLock.Lock()
	defer s.preShutdownHookLock.Unlock()
	s.preShutdownHooksCalled = true

	for hookName, hookEntry := range s.preShutdownHooks {
		if err := runPreShutdownHook(hookName, hookEntry); err != nil {
			errorList = append(errorList, err)
		}
	}
	// 聚合错误
	return utilerrors.NewAggregate(errorList)
}

// isPostStartHookRegistered checks whether a given PostStartHook is registered
func (s *GenericAPIServer) isPostStartHookRegistered(name string) bool {
	s.postStartHookLock.Lock()
	defer s.postStartHookLock.Unlock()
	_, exists := s.postStartHooks[name]
	return exists
}

func runPostStartHook(name string, entry postStartHookEntry, context PostStartHookContext) {
	var err error
	func() {
		// don't let the hook *accidentally* panic and kill the server
		defer utilruntime.HandleCrash()
		err = entry.hook(context)
	}()
	// if the hook intentionally wants to kill server, let it.
	if err != nil {
		klog.Fatalf("PostStartHook %q failed: %v", name, err)
	}
	close(entry.done)
}

func runPreShutdownHook(name string, entry preShutdownHookEntry) error {
	var err error
	func() {
		// don't let the hook *accidentally* panic and kill the server
		defer utilruntime.HandleCrash()
		err = entry.hook()
	}()
	if err != nil {
		return fmt.Errorf("PreShutdownHook %q failed: %v", name, err)
	}
	return nil
}

// postStartHookHealthz implements a healthz check for poststarthooks.  It will return a "hookNotFinished"
// error until the poststarthook is finished.
type postStartHookHealthz struct {
	name string

	// done will be closed when the postStartHook is finished
	done chan struct{}
}

var _ healthz.HealthChecker = postStartHookHealthz{}

func (h postStartHookHealthz) Name() string {
	return h.name
}

var errHookNotFinished = errors.New("not finished")

func (h postStartHookHealthz) Check(req *http.Request) error {
	select {
	case <-h.done:
		return nil
	default:
		return errHookNotFinished
	}
}
