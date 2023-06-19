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

package logs

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/wait"
	internalapi "k8s.io/cri-api/pkg/apis"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/utils/clock"
)

const (
	// logMonitorPeriod is the period container log manager monitors
	// container logs and performs log rotation.
	logMonitorPeriod = 10 * time.Second
	// timestampFormat is format of the timestamp suffix for rotated log.
	// See https://golang.org/pkg/time/#Time.Format.
	timestampFormat = "20060102-150405"
	// compressSuffix is the suffix for compressed log.
	compressSuffix = ".gz"
	// tmpSuffix is the suffix for temporary file.
	tmpSuffix = ".tmp"
)

// ContainerLogManager manages lifecycle of all container logs.
//
// Implementation is thread-safe.
type ContainerLogManager interface {
	// Start TODO(random-liu): Add RotateLogs function and call it under disk pressure.
	// Start container log manager.
	// 1、每隔十秒钟，通过CRI接口调用底层的容器运行时获取到容器，然后遍历每隔容器的日志（也是通过CRI接口查询容器状态），如果发现容器当前使用的日志
	// 大小大于日志切割策略的最大值，那么就压缩容器日志，同时调用CRI接口要求容器把日志写入到新的文件当中。
	// 2、在遍历容器日志的同时，ContainerLogManager会删除.tmp以及已经压缩过的容器日志。实际上，如果是程序正常执行，是不会有.tmp以及已经压缩过的
	// 日志的，因为这些日志会在ContainerLogManager压缩完日志之后删除
	// TODO 3、为什么在实现的时候先压缩日志然后在调用CRI接口要求容器写入新的日志到新的文件当中？
	Start()
	// Clean removes all logs of specified container.
	// 移除指定容器的所有所有日志
	Clean(ctx context.Context, containerID string) error
}

// LogRotatePolicy is a policy for container log rotation. The policy applies to all
// containers managed by kubelet.
// 日志切割策略，容器的日志不应该全部写入到一个文件中。很多时候，我们需要按照一定的策略来保存日志，譬如每天的日志保存在一个文件中，一到凌晨十二点
// 就从新写入一个文件。又比如，有些人可能需要按照固定大小保存日志，譬如每个日志文件最多保存50M的日志，一旦超过50M日志，就从新写入新的文件。
type LogRotatePolicy struct {
	// MaxSize in bytes of the container log file before it is rotated. Negative
	// number means to disable container log rotation.
	// TODO 默认策略是多大？
	MaxSize int64
	// MaxFiles is the maximum number of log files that can be present.
	// If rotating the logs creates excess files, the oldest file is removed.
	// TODO 默认策略是多少个文件？
	MaxFiles int
}

// GetAllLogs gets all inuse (rotated/compressed) logs for a specific container log.
// Returned logs are sorted in oldest to newest order.
// TODO(#59902): Leverage this function to support log rotation in `kubectl logs`.
func GetAllLogs(log string) ([]string, error) {
	// pattern is used to match all rotated files.
	pattern := fmt.Sprintf("%s.*", log)
	logs, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("failed to list all log files with pattern %q: %v", pattern, err)
	}
	inuse, _ := filterUnusedLogs(logs)
	sort.Strings(inuse)
	return append(inuse, log), nil
}

// compressReadCloser wraps gzip.Reader with a function to close file handler.
type compressReadCloser struct {
	f *os.File
	*gzip.Reader
}

func (rc *compressReadCloser) Close() error {
	ferr := rc.f.Close()
	rerr := rc.Reader.Close()
	if ferr != nil {
		return ferr
	}
	if rerr != nil {
		return rerr
	}
	return nil
}

// UncompressLog compresses a compressed log and return a readcloser for the
// stream of the uncompressed content.
// TODO(#59902): Leverage this function to support log rotation in `kubectl logs`.
func UncompressLog(log string) (_ io.ReadCloser, retErr error) {
	if !strings.HasSuffix(log, compressSuffix) {
		return nil, fmt.Errorf("log is not compressed")
	}
	f, err := os.Open(log)
	if err != nil {
		return nil, fmt.Errorf("failed to open log: %v", err)
	}
	defer func() {
		if retErr != nil {
			f.Close()
		}
	}()
	r, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("failed to create gzip reader: %v", err)
	}
	return &compressReadCloser{f: f, Reader: r}, nil
}

// parseMaxSize parses quantity string to int64 max size in bytes.
func parseMaxSize(size string) (int64, error) {
	quantity, err := resource.ParseQuantity(size)
	if err != nil {
		return 0, err
	}
	maxSize, ok := quantity.AsInt64()
	if !ok {
		return 0, fmt.Errorf("invalid max log size")
	}
	return maxSize, nil
}

type containerLogManager struct {
	// 容器运行时接口
	runtimeService internalapi.RuntimeService
	osInterface    kubecontainer.OSInterface
	// 日志切分策略
	policy LogRotatePolicy
	clock  clock.Clock
	mutex  sync.Mutex
}

// NewContainerLogManager creates a new container log manager.
func NewContainerLogManager(runtimeService internalapi.RuntimeService, osInterface kubecontainer.OSInterface,
	maxSize string, maxFiles int) (ContainerLogManager, error) {
	if maxFiles <= 1 {
		return nil, fmt.Errorf("invalid MaxFiles %d, must be > 1", maxFiles)
	}
	// 每个日志文件保存的最大大小
	parsedMaxSize, err := parseMaxSize(maxSize)
	if err != nil {
		return nil, fmt.Errorf("failed to parse container log max size %q: %v", maxSize, err)
	}
	// Negative number means to disable container log rotation
	if parsedMaxSize < 0 {
		return NewStubContainerLogManager(), nil
	}
	// policy LogRotatePolicy
	return &containerLogManager{
		osInterface:    osInterface,
		runtimeService: runtimeService,
		policy: LogRotatePolicy{
			MaxSize:  parsedMaxSize,
			MaxFiles: maxFiles,
		},
		clock: clock.RealClock{},
		mutex: sync.Mutex{},
	}, nil
}

// Start the container log manager.
func (c *containerLogManager) Start() {
	ctx := context.Background()
	// Start a goroutine periodically does container log rotation.
	go wait.Forever(func() {
		// 切分日志，每10秒钟运行一次
		if err := c.rotateLogs(ctx); err != nil {
			klog.ErrorS(err, "Failed to rotate container logs")
		}
	}, logMonitorPeriod)
}

// Clean removes all logs of specified container (including rotated one).
// 移除指定容器的所有日志
func (c *containerLogManager) Clean(ctx context.Context, containerID string) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	resp, err := c.runtimeService.ContainerStatus(ctx, containerID, false)
	if err != nil {
		return fmt.Errorf("failed to get container status %q: %v", containerID, err)
	}
	if resp.GetStatus() == nil {
		return fmt.Errorf("container status is nil for %q", containerID)
	}
	pattern := fmt.Sprintf("%s*", resp.GetStatus().GetLogPath())
	logs, err := c.osInterface.Glob(pattern)
	if err != nil {
		return fmt.Errorf("failed to list all log files with pattern %q: %v", pattern, err)
	}

	for _, l := range logs {
		if err := c.osInterface.Remove(l); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove container %q log %q: %v", containerID, l, err)
		}
	}

	return nil
}

func (c *containerLogManager) rotateLogs(ctx context.Context) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	// TODO(#59998): Use kubelet pod cache.
	// 通过CRI接口调用底层的容器运行时获取所有的容器
	containers, err := c.runtimeService.ListContainers(ctx, &runtimeapi.ContainerFilter{})
	if err != nil {
		return fmt.Errorf("failed to list containers: %v", err)
	}
	// NOTE(random-liu): Figure out whether we need to rotate container logs in parallel.
	for _, container := range containers {
		// Only rotate logs for running containers. Non-running containers won't
		// generate new output, it doesn't make sense to keep an empty latest log.
		// 如果容器并没有处于运行中的状态，就忽略该容器
		if container.GetState() != runtimeapi.ContainerState_CONTAINER_RUNNING {
			continue
		}
		id := container.GetId()
		// Note that we should not block log rotate for an error of a single container.
		// 获取容器的状态
		resp, err := c.runtimeService.ContainerStatus(ctx, id, false)
		if err != nil {
			klog.ErrorS(err, "Failed to get container status", "containerID", id)
			continue
		}
		if resp.GetStatus() == nil { // 如果容器的状态为空，那就忽略这个容器
			klog.ErrorS(err, "Container status is nil", "containerID", id)
			continue
		}
		// 获取到容器保存日志的路径
		path := resp.GetStatus().GetLogPath()
		info, err := c.osInterface.Stat(path)
		if err != nil {
			if !os.IsNotExist(err) {
				klog.ErrorS(err, "Failed to stat container log", "path", path)
				continue
			}
			// In rotateLatestLog, there are several cases that we may
			// lose original container log after ReopenContainerLog fails.
			// We try to recover it by reopening container log.
			if err := c.runtimeService.ReopenContainerLog(ctx, id); err != nil {
				klog.ErrorS(err, "Container log doesn't exist, reopen container log failed", "containerID", id, "path", path)
				continue
			}
			// The container log should be recovered.
			info, err = c.osInterface.Stat(path)
			if err != nil {
				klog.ErrorS(err, "Failed to stat container log after reopen", "path", path)
				continue
			}
		}
		// 如果当前的容器的日志文件的大小并没有超过策略的最大值，就忽略这个容器
		if info.Size() < c.policy.MaxSize {
			continue
		}
		// Perform log rotation.
		// 切割容器的日志
		if err := c.rotateLog(ctx, id, path); err != nil {
			klog.ErrorS(err, "Failed to rotate log for container", "path", path, "containerID", id)
			continue
		}
	}
	return nil
}

func (c *containerLogManager) rotateLog(ctx context.Context, id, log string) error {
	// pattern is used to match all rotated files.
	pattern := fmt.Sprintf("%s.*", log)
	// 获取到容器的所有日志
	logs, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("failed to list all log files with pattern %q: %v", pattern, err)
	}

	// 清理掉无用的文件，所有的.gz文件以及普通的日志文件都认为是无用的文件，而以.tmp以及已经被压缩过的日志文件都被认为是无用的日志文件，
	// 这类文件是需要被删除的
	logs, err = c.cleanupUnusedLogs(logs)
	if err != nil {
		return fmt.Errorf("failed to cleanup logs: %v", err)
	}

	// 1、从cleanupUnusedLogs方法中得到的logs都是还需要使用的文件，只不过里面包含.gz压缩文件以及普通的日志文件
	// 2、由于后面压缩日志的时候，ContainerLogManager会生成.gz以及.tmp日志，而如果用户指定的日志切割策略，那么不能应为.gz以及.tmp文件
	// 而超出这个限制。显然，如果此时日志文件的数量已经达到了最大值，我们就需要把一些老的日志文件删除，从而给最终要生成的.gz文件以及临时文件
	// .tmp文件腾出位置
	logs, err = c.removeExcessLogs(logs)
	if err != nil {
		return fmt.Errorf("failed to remove excess logs: %v", err)
	}

	// Compress uncompressed log files.
	for _, l := range logs {
		// 如果当前日志文件已经是压缩文件了，就不需要要再次压缩文件
		if strings.HasSuffix(l, compressSuffix) {
			continue
		}
		// 使用gzip压缩日志文件为.gz文件，然后删除原始日志文件
		if err := c.compressLog(l); err != nil {
			return fmt.Errorf("failed to compress log %q: %v", l, err)
		}
	}

	// 1、重命名当前使用的日志文件
	// 2、通过gRPC调用容器运行时要求容器重新打开一个文件写入文件日志
	// 3、TODO 为什么不是先让容器重新打开一个文件，然后再压缩日志文件么？ 目前这种顺序不会导致容器丢失日志？
	if err := c.rotateLatestLog(ctx, id, log); err != nil {
		return fmt.Errorf("failed to rotate log %q: %v", log, err)
	}

	return nil
}

// cleanupUnusedLogs cleans up temporary or unused log files generated by previous log rotation
// failure.
func (c *containerLogManager) cleanupUnusedLogs(logs []string) ([]string, error) {
	// 从一堆日志文件中根据文件名后缀找出正在使用中的文件以及不再使用的日志，其中
	// .gz文件和普通日志文件都认为是正在使用中的日志文件，.tmp以及日志文件已经被压缩的日志文件都认为是无用的日志文件
	inuse, unused := filterUnusedLogs(logs)
	for _, l := range unused {
		if err := c.osInterface.Remove(l); err != nil {
			return nil, fmt.Errorf("failed to remove unused log %q: %v", l, err)
		}
	}
	return inuse, nil
}

// filterUnusedLogs splits logs into 2 groups, the 1st group is in used logs,
// the second group is unused logs.
func filterUnusedLogs(logs []string) (inuse []string, unused []string) {
	for _, l := range logs {
		if isInUse(l, logs) {
			inuse = append(inuse, l)
		} else {
			unused = append(unused, l)
		}
	}
	return inuse, unused
}

// isInUse checks whether a container log file is still inuse.
// 1、日志文件名以.tmp结尾的文件，都认为是没有使用中的文件
// 2、日志文件名以.gz结尾的文件，都是认为在使用中的文件，因为这个日志文件是kubelet压缩过的文件，肯定是有用的
// 3、如果当前的日志文件名字拼接上.gz出现在日志文件中，说明当前日志文件已经被压缩过，所以此日志文件可以被删除
// 4、其它的日志文件都认为是在使用中的文件
func isInUse(l string, logs []string) bool {
	// All temporary files are not in use.
	if strings.HasSuffix(l, tmpSuffix) {
		return false
	}
	// All compressed logs are in use.
	if strings.HasSuffix(l, compressSuffix) {
		return true
	}
	// Files has already been compressed are not in use.
	// 实际上，这里表达的意思是当前日志文件已经被压缩到了.gz文件当中，因此当前日志文件被认为是无用的文件
	for _, another := range logs {
		if l+compressSuffix == another {
			return false
		}
	}
	return true
}

// compressLog compresses a log to log.gz with gzip.
func (c *containerLogManager) compressLog(log string) error {
	r, err := c.osInterface.Open(log)
	if err != nil {
		return fmt.Errorf("failed to open log %q: %v", log, err)
	}
	defer r.Close()
	tmpLog := log + tmpSuffix
	f, err := c.osInterface.OpenFile(tmpLog, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to create temporary log %q: %v", tmpLog, err)
	}
	defer func() {
		// Best effort cleanup of tmpLog.
		c.osInterface.Remove(tmpLog)
	}()
	defer f.Close()
	w := gzip.NewWriter(f)
	defer w.Close()
	// 把当前的日志文件拷贝到.tmp日志文件中
	if _, err := io.Copy(w, r); err != nil {
		return fmt.Errorf("failed to compress %q to %q: %v", log, tmpLog, err)
	}
	// The archive needs to be closed before renaming, otherwise an error will occur on Windows.
	w.Close()
	f.Close()
	// 拷贝完成之后，把.tmp文件重命名为.gz文件
	compressedLog := log + compressSuffix
	if err := c.osInterface.Rename(tmpLog, compressedLog); err != nil {
		return fmt.Errorf("failed to rename %q to %q: %v", tmpLog, compressedLog, err)
	}
	// Remove old log file.
	r.Close()
	// 移除当前日志文件
	if err := c.osInterface.Remove(log); err != nil {
		return fmt.Errorf("failed to remove log %q after compress: %v", log, err)
	}
	return nil
}

// removeExcessLogs removes old logs to make sure there are only at most MaxFiles log files.
// ContainerLogManager的最终目的是为了压缩日志，而压缩日志的时候会产生.gz以及.tmp文件，因此需要腾出两个位置给.gz以及.tmp文件。如果当前容器
// 的日志已经达到了日志切割策略指定的日志最大数量，此时需要删除一部分日志，显然，删除日志应该删除最老的日志
func (c *containerLogManager) removeExcessLogs(logs []string) ([]string, error) {
	// Sort log files in oldest to newest order.
	// 按照文件名创建的先后时间顺序排序，最先创建的日志文件在前面，最后创建的日志文件在后面
	sort.Strings(logs)
	// Container will create a new log file, and we'll rotate the latest log file.
	// Other than those 2 files, we can have at most MaxFiles-2 rotated log files.
	// Keep MaxFiles-2 files by removing old files.
	// We should remove from oldest to newest, so as not to break ongoing `kubectl logs`.
	maxRotatedFiles := c.policy.MaxFiles - 2
	if maxRotatedFiles < 0 {
		maxRotatedFiles = 0
	}
	i := 0
	for ; i < len(logs)-maxRotatedFiles; i++ {
		if err := c.osInterface.Remove(logs[i]); err != nil {
			return nil, fmt.Errorf("failed to remove old log %q: %v", logs[i], err)
		}
	}
	logs = logs[i:]
	return logs, nil
}

// rotateLatestLog rotates latest log without compression, so that container can still write
// and fluentd can finish reading.
func (c *containerLogManager) rotateLatestLog(ctx context.Context, id, log string) error {
	timestamp := c.clock.Now().Format(timestampFormat)
	rotated := fmt.Sprintf("%s.%s", log, timestamp)
	if err := c.osInterface.Rename(log, rotated); err != nil {
		return fmt.Errorf("failed to rotate log %q to %q: %v", log, rotated, err)
	}
	if err := c.runtimeService.ReopenContainerLog(ctx, id); err != nil {
		// Rename the rotated log back, so that we can try rotating it again
		// next round.
		// If kubelet gets restarted at this point, we'll lose original log.
		if renameErr := c.osInterface.Rename(rotated, log); renameErr != nil {
			// This shouldn't happen.
			// Report an error if this happens, because we will lose original
			// log.
			klog.ErrorS(renameErr, "Failed to rename rotated log", "rotatedLog", rotated, "newLog", log, "containerID", id)
		}
		return fmt.Errorf("failed to reopen container log %q: %v", id, err)
	}
	return nil
}
