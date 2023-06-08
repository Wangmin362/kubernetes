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

package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"k8s.io/klog/v2"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	api "k8s.io/kubernetes/pkg/apis/core"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	utilio "k8s.io/utils/io"
)

type podEventType int

const (
	podAdd podEventType = iota
	podModify
	podDelete

	eventBufferLen = 10
)

type watchEvent struct {
	fileName  string
	eventType podEventType
}

type sourceFile struct {
	path           string
	nodeName       types.NodeName
	period         time.Duration
	store          cache.Store
	fileKeyMapping map[string]string
	updates        chan<- interface{}
	watchEvents    chan *watchEvent
}

// NewSourceFile watches a config file for changes.
func NewSourceFile(path string, nodeName types.NodeName, period time.Duration, updates chan<- interface{}) {
	// "github.com/sigma/go-inotify" requires a path without trailing "/"
	// 去掉路径最后的斜杠
	path = strings.TrimRight(path, string(os.PathSeparator))

	config := newSourceFile(path, nodeName, period, updates)
	klog.V(1).InfoS("Watching path", "path", path)
	config.run()
}

func newSourceFile(path string, nodeName types.NodeName, period time.Duration, updates chan<- interface{}) *sourceFile {
	// 把Pod数组放入到updates channel通道当中
	send := func(objs []interface{}) {
		var pods []*v1.Pod
		for _, o := range objs {
			pods = append(pods, o.(*v1.Pod))
		}
		// 把变更的Pod写入到Channel当中，这个updates channel是一个无缓冲队列
		updates <- kubetypes.PodUpdate{Pods: pods, Op: kubetypes.SET, Source: kubetypes.FileSource}
	}
	// 1、cache.MetaNamespaceKeyFunc用于获取Pod的唯一Key，其实就是namespace + podName
	// 2、store应该是用来记录当前已经监听到的Pod,防止重复出发PLEG
	store := cache.NewUndeltaStore(send, cache.MetaNamespaceKeyFunc)
	return &sourceFile{
		path:           path,     // 监听的StaticPod路径
		nodeName:       nodeName, // 当前Node名字
		period:         period,   // 定时扫描StaticPod的周期
		store:          store,
		fileKeyMapping: map[string]string{},
		updates:        updates, // SourceFile会监听文件路径，并把Pod变更放入到这个channel当中
		watchEvents:    make(chan *watchEvent, eventBufferLen),
	}
}

func (s *sourceFile) run() {
	listTicker := time.NewTicker(s.period)

	go func() {
		// Read path immediately to speed up startup.
		if err := s.listConfig(); err != nil {
			klog.ErrorS(err, "Unable to read config path", "path", s.path)
		}
		for {
			select {
			case <-listTicker.C: // 定时扫描StaticPodPath，每秒钟扫描一次
				if err := s.listConfig(); err != nil {
					klog.ErrorS(err, "Unable to read config path", "path", s.path)
				}
			case e := <-s.watchEvents: // 一旦StaticPodPath路径中的文件发生变更，就会触发这里
				if err := s.consumeWatchEvent(e); err != nil {
					klog.ErrorS(err, "Unable to process watch event")
				}
			}
		}
	}()

	// 通过Linux的机制监听文件的变更，一旦发现文件变更就把变更事件写入到watchEvents channel当中
	s.startWatch()
}

func (s *sourceFile) applyDefaults(pod *api.Pod, source string) error {
	return applyDefaults(pod, source, true, s.nodeName)
}

func (s *sourceFile) listConfig() error {
	path := s.path
	statInfo, err := os.Stat(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		// Emit an update with an empty PodList to allow FileSource to be marked as seen
		s.updates <- kubetypes.PodUpdate{Pods: []*v1.Pod{}, Op: kubetypes.SET, Source: kubetypes.FileSource}
		return fmt.Errorf("path does not exist, ignoring")
	}

	switch {
	case statInfo.Mode().IsDir():
		// 如果当前路径是一个目录，那么从目录中的所有文件抽取定义的Pod
		pods, err := s.extractFromDir(path)
		if err != nil {
			return err
		}
		if len(pods) == 0 {
			// Emit an update with an empty PodList to allow FileSource to be marked as seen
			s.updates <- kubetypes.PodUpdate{Pods: pods, Op: kubetypes.SET, Source: kubetypes.FileSource}
			return nil
		}
		// 直接全量更新，每次扫描StaticPodPath都是以文件中的内容为准
		return s.replaceStore(pods...)

	case statInfo.Mode().IsRegular():
		// 如果当前路径是一个普通文件，那么从文件当中抽取Pod
		pod, err := s.extractFromFile(path)
		if err != nil {
			return err
		}
		// 直接全量更新，每次扫描StaticPodPath都是以文件中的内容为准
		return s.replaceStore(pod)

	default:
		return fmt.Errorf("path is not a directory or file")
	}
}

// Get as many pod manifests as we can from a directory. Return an error if and only if something
// prevented us from reading anything at all. Do not return an error if only some files
// were problematic.
func (s *sourceFile) extractFromDir(name string) ([]*v1.Pod, error) {
	dirents, err := filepath.Glob(filepath.Join(name, "[^.]*"))
	if err != nil {
		return nil, fmt.Errorf("glob failed: %v", err)
	}

	pods := make([]*v1.Pod, 0, len(dirents))
	if len(dirents) == 0 {
		return pods, nil
	}

	sort.Strings(dirents)
	for _, path := range dirents {
		statInfo, err := os.Stat(path)
		if err != nil {
			klog.ErrorS(err, "Could not get metadata", "path", path)
			continue
		}

		switch {
		case statInfo.Mode().IsDir():
			// 可以看到StaticPod所配置的路径中即使配置了目录也没啥卵用
			klog.ErrorS(nil, "Provided manifest path is a directory, not recursing into manifest path", "path", path)
		case statInfo.Mode().IsRegular():
			// 解析文件，抽取所有的Pod
			pod, err := s.extractFromFile(path)
			if err != nil {
				if !os.IsNotExist(err) {
					klog.ErrorS(err, "Could not process manifest file", "path", path)
				}
			} else {
				pods = append(pods, pod)
			}
		default:
			klog.ErrorS(nil, "Manifest path is not a directory or file", "path", path, "mode", statInfo.Mode())
		}
	}
	return pods, nil
}

// extractFromFile parses a file for Pod configuration information.
func (s *sourceFile) extractFromFile(filename string) (pod *v1.Pod, err error) {
	klog.V(3).InfoS("Reading config file", "path", filename)
	defer func() {
		if err == nil && pod != nil {
			objKey, keyErr := cache.MetaNamespaceKeyFunc(pod)
			if keyErr != nil {
				err = keyErr
				return
			}
			s.fileKeyMapping[filename] = objKey
		}
	}()

	file, err := os.Open(filename)
	if err != nil {
		return pod, err
	}
	defer file.Close()

	data, err := utilio.ReadAtMost(file, maxConfigLength)
	if err != nil {
		return pod, err
	}

	defaultFn := func(pod *api.Pod) error {
		return s.applyDefaults(pod, filename)
	}

	// 从文件当中解析Pod
	parsed, pod, podErr := tryDecodeSinglePod(data, defaultFn)
	if parsed {
		if podErr != nil {
			return pod, podErr
		}
		return pod, nil
	}

	return pod, fmt.Errorf("%v: couldn't parse as pod(%v), please check config file", filename, podErr)
}

func (s *sourceFile) replaceStore(pods ...*v1.Pod) (err error) {
	objs := []interface{}{}
	for _, pod := range pods {
		objs = append(objs, pod)
	}
	return s.store.Replace(objs, "")
}
