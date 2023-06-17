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

package checkpointmanager

import (
	"fmt"
	"sync"

	"k8s.io/kubernetes/pkg/kubelet/checkpointmanager/errors"
	utilstore "k8s.io/kubernetes/pkg/kubelet/util/store"
	utilfs "k8s.io/kubernetes/pkg/util/filesystem"
)

// Checkpoint provides the process checkpoint data
// TODO 如何理解Checkpoint接口设计？
type Checkpoint interface {
	MarshalCheckpoint() ([]byte, error)
	UnmarshalCheckpoint(blob []byte) error
	VerifyChecksum() error
}

// CheckpointManager provides the interface to manage checkpoint
// 1、实际上，从这两个接口定义上并看不出来checkpoint是干嘛用的。checkpoint接口仅仅定义了如何序列化与反序列化以及校验checkpoint,
// 而checkpointManager也仅仅定义了一个checkpoint的缓存，不过从目前的实现上来说，其实现是一个文件持久化的checkpoint管理器
type CheckpointManager interface {
	// CreateCheckpoint persists checkpoint in CheckpointStore. checkpointKey is the key for utilstore to locate checkpoint.
	// For file backed utilstore, checkpointKey is the file name to write the checkpoint data.
	CreateCheckpoint(checkpointKey string, checkpoint Checkpoint) error
	// GetCheckpoint retrieves checkpoint from CheckpointStore.
	GetCheckpoint(checkpointKey string, checkpoint Checkpoint) error
	// WARNING: RemoveCheckpoint will not return error if checkpoint does not exist.
	RemoveCheckpoint(checkpointKey string) error
	// ListCheckpoint returns the list of existing checkpoints.
	ListCheckpoints() ([]string, error)
}

// impl is an implementation of CheckpointManager. It persists checkpoint in CheckpointStore
type impl struct {
	path string
	// 一个简单的KV型数据库
	store utilstore.Store
	mutex sync.Mutex
}

// NewCheckpointManager returns a new instance of a checkpoint manager
// 1、CheckpointManager保存checkpoint的方式是以文件的形式进行持久化的
func NewCheckpointManager(checkpointDir string) (CheckpointManager, error) {
	fstore, err := utilstore.NewFileStore(checkpointDir, &utilfs.DefaultFs{})
	if err != nil {
		return nil, err
	}

	return &impl{path: checkpointDir, store: fstore}, nil
}

// CreateCheckpoint persists checkpoint in CheckpointStore.
func (manager *impl) CreateCheckpoint(checkpointKey string, checkpoint Checkpoint) error {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	// 把checkpoint序列化然后存储到文件当中
	blob, err := checkpoint.MarshalCheckpoint()
	if err != nil {
		return err
	}
	return manager.store.Write(checkpointKey, blob)
}

// GetCheckpoint retrieves checkpoint from CheckpointStore.
func (manager *impl) GetCheckpoint(checkpointKey string, checkpoint Checkpoint) error {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	// 通过key，获取checkpoint的序列化数据
	blob, err := manager.store.Read(checkpointKey)
	if err != nil {
		if err == utilstore.ErrKeyNotFound {
			return errors.ErrCheckpointNotFound
		}
		return err
	}
	// 然后进行反序列化，显然checkpoint必须是一个指针，否则无法更改checkpoint
	err = checkpoint.UnmarshalCheckpoint(blob)
	if err == nil {
		// 检查校验和
		err = checkpoint.VerifyChecksum()
	}
	return err
}

// RemoveCheckpoint will not return error if checkpoint does not exist.
func (manager *impl) RemoveCheckpoint(checkpointKey string) error {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	// 删除Checkopint
	return manager.store.Delete(checkpointKey)
}

// ListCheckpoints returns the list of existing checkpoints.
// 查看当前有哪些checkpoint
func (manager *impl) ListCheckpoints() ([]string, error) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	keys, err := manager.store.List()
	if err != nil {
		return []string{}, fmt.Errorf("failed to list checkpoint store: %v", err)
	}
	return keys, nil
}
