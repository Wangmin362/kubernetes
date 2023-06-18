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

package images

import (
	"context"
	goerrors "errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/util/sliceutils"
)

// instrumentationScope is OpenTelemetry instrumentation scope name
const instrumentationScope = "k8s.io/kubernetes/pkg/kubelet/images"

// StatsProvider is an interface for fetching stats used during image garbage
// collection.
// 用于返回当前镜像所占用文件系统的详情信息
type StatsProvider interface {
	// ImageFsStats returns the stats of the image filesystem.
	ImageFsStats(ctx context.Context) (*statsapi.FsStats, error)
}

// ImageGCManager is an interface for managing lifecycle of all images.
// Implementation is thread-safe.
// ImageGCManger的原理非常简单，通过Start方法启动，并不停的调用CRI接口维护imageRecord以及imageCache。并且通过GarbageCollect删除掉
// 无用的镜像，直到删除到ImageGCPolicy.LowThresholdPercent水线以下就可以了。而通过DeleteUnusedImage方法直接删除所有无用的镜像
type ImageGCManager interface {
	// GarbageCollect Applies the garbage collection policy. Errors include being unable to free
	// enough space as per the garbage collection policy.
	// 和DeleteUnusedImages方法非常类似，只不过DeleteUnusedImages是直接删除当前节点所有无用的镜像，只有被Pod使用的镜像才算是有用的镜像
	// 而GarbageCollect方法则是根据GCPolicy来删除镜像，此方法虽然也是删除镜像，但是并不会删除所有无用的镜像，而是只需要把把镜像占用的空间
	// 释放到ImageGCPolicy.LowThresholdPercent水线以下就可以了
	GarbageCollect(ctx context.Context) error

	// Start async garbage collection of images.
	// 异步执行镜像的收集
	Start()

	// GetImageList 获取当前节点所下载的镜像，这个功能实现起来比较简单，直接通过CRI接口调用底层的容器运行时即可
	GetImageList() ([]container.Image, error)

	// DeleteUnusedImages Delete all unused images.
	// 这个功能应该是通过GetImageList方法获取当前节点的所有镜像信息，然后通过遍历所有的Pod的所有容器的镜像信息，就可以找到
	// 无用的镜像。不过根据kubelet之前的尿性来说，肯定是缓存了当前节点的镜像信息
	DeleteUnusedImages(ctx context.Context) error
}

// ImageGCPolicy is a policy for garbage collecting images. Policy defines an allowed band in
// which garbage collection will be run.
// 镜像垃圾收集策略
type ImageGCPolicy struct {
	// Any usage above this threshold will always trigger garbage collection.
	// This is the highest usage we will allow.
	// 只要触发文件系统的最大使用水线，就开始收集镜像
	HighThresholdPercent int

	// Any usage below this threshold will never trigger garbage collection.
	// This is the lowest threshold we will try to garbage collect to.
	// 文件系统的使用量的最低水线，只要文件系统的使用量小于这个值，就不需要收集镜像
	LowThresholdPercent int

	// Minimum age at which an image can be garbage collected.
	// 镜像最小生存时间
	MinAge time.Duration
}

type realImageGCManager struct {
	// Container runtime
	runtime container.Runtime

	// Records of images and their use.
	// Key为镜像ID
	imageRecords     map[string]*imageRecord
	imageRecordsLock sync.Mutex

	// The image garbage collection policy in use.
	// 镜像收集策略，这是ImageGCManager收集镜像的指导方针
	policy ImageGCPolicy

	// statsProvider provides stats used during image garbage collection.
	// 用于统计当前文件系统的使用信息
	statsProvider StatsProvider

	// Recorder for Kubernetes events.
	// 用于生成某些事件
	recorder record.EventRecorder

	// Reference to this node.
	// 当前Kubelet一定是运行在某个节点上的
	nodeRef *v1.ObjectReference

	// Track initialization
	initialized bool

	// imageCache is the cache of latest image list.
	// 缓存当前节点的所有镜像信息，实际上就是通过CRI接口查询到的
	imageCache imageCache

	// sandbox image exempted from GC
	// 沙箱使用的镜像信息，一般来说沙箱镜像不会被回收，除非再使用K8S集群的过程当中更换了镜像信息
	sandboxImage string

	// tracer for recording spans
	tracer trace.Tracer
}

// imageCache caches latest result of ListImages.
type imageCache struct {
	// sync.Mutex is the mutex protects the image cache.
	sync.Mutex
	// images is the image cache.
	images []container.Image
}

// set sorts the input list and updates image cache.
// 'i' takes ownership of the list, you should not reference the list again
// after calling this function.
// 之所以set方法是全量覆盖，是因为当前节点所有的镜像可以通过CRI接口一次性获取到，所以每一次都是全量覆盖，因为CRI接口获取的镜像信息就是
// 最新的
func (i *imageCache) set(images []container.Image) {
	i.Lock()
	defer i.Unlock()
	// The image list needs to be sorted when it gets read and used in
	// setNodeStatusImages. We sort the list on write instead of on read,
	// because the image cache is more often read than written
	sort.Sort(sliceutils.ByImageSize(images))
	i.images = images
}

// get gets image list from image cache.
// NOTE: The caller of get() should not do mutating operations on the
// returned list that could cause data race against other readers (e.g.
// in-place sorting the returned list)
func (i *imageCache) get() []container.Image {
	i.Lock()
	defer i.Unlock()
	return i.images
}

// Information about the images we track.
type imageRecord struct {
	// Time when this image was first detected.
	firstDetected time.Time

	// Time when we last saw this image being used.
	lastUsed time.Time

	// Size of the image in bytes.
	size int64

	// Pinned status of the image
	pinned bool
}

// NewImageGCManager instantiates a new ImageGCManager object.
func NewImageGCManager(runtime container.Runtime, statsProvider StatsProvider, recorder record.EventRecorder, nodeRef *v1.ObjectReference,
	policy ImageGCPolicy, sandboxImage string, tracerProvider trace.TracerProvider) (ImageGCManager, error) {
	// Validate policy.
	if policy.HighThresholdPercent < 0 || policy.HighThresholdPercent > 100 {
		return nil, fmt.Errorf("invalid HighThresholdPercent %d, must be in range [0-100]", policy.HighThresholdPercent)
	}
	if policy.LowThresholdPercent < 0 || policy.LowThresholdPercent > 100 {
		return nil, fmt.Errorf("invalid LowThresholdPercent %d, must be in range [0-100]", policy.LowThresholdPercent)
	}
	if policy.LowThresholdPercent > policy.HighThresholdPercent {
		return nil, fmt.Errorf("LowThresholdPercent %d can not be higher than HighThresholdPercent %d", policy.LowThresholdPercent, policy.HighThresholdPercent)
	}
	tracer := tracerProvider.Tracer(instrumentationScope)
	im := &realImageGCManager{
		runtime:       runtime,
		policy:        policy,
		imageRecords:  make(map[string]*imageRecord),
		statsProvider: statsProvider,
		recorder:      recorder,
		nodeRef:       nodeRef,
		initialized:   false,
		sandboxImage:  sandboxImage,
		tracer:        tracer,
	}

	return im, nil
}

func (im *realImageGCManager) Start() {
	ctx := context.Background()
	go wait.Until(func() {
		// Initial detection make detected time "unknown" in the past.
		var ts time.Time // TODO 如果这个时间没有初始化，那是多少？
		if im.initialized {
			ts = time.Now()
		}
		// 1、通过CRI接口获取镜像信息更新imageRecords信息
		// 2、通过CRI接口获取Pod当前节点运行的Pod信息获取正在使用中的镜像，这不过这个返回值在这里没有作用
		_, err := im.detectImages(ctx, ts)
		if err != nil {
			klog.InfoS("Failed to monitor images", "err", err)
		} else {
			im.initialized = true
		}
	}, 5*time.Minute, wait.NeverStop)

	// Start a goroutine periodically updates image cache.
	go wait.Until(func() {
		// 通过CRI接口获取所有的镜像信息
		images, err := im.runtime.ListImages(ctx)
		if err != nil {
			klog.InfoS("Failed to update image list", "err", err)
		} else {
			im.imageCache.set(images)
		}
	}, 30*time.Second, wait.NeverStop)

}

// GetImageList 获取当前节点所有的镜像信息
func (im *realImageGCManager) GetImageList() ([]container.Image, error) {
	return im.imageCache.get(), nil
}

// 首先通过CRI接口获取当前节点所有的镜像，然后通过CRI接口获取当前节点所有运行的Pod，显然所有正在运行的Pod说使用的镜像一定是不能删除的，所以
// detectImages会更新正在使用镜像的最后使用时间，并记录镜像大小信息，以及是否能够被删除的信息。
// 1、通过CRI接口获取镜像信息更新imageRecords信息
// 2、通过CRI接口获取Pod当前节点运行的Pod信息获取正在使用中的镜像
func (im *realImageGCManager) detectImages(ctx context.Context, detectTime time.Time) (sets.String, error) {
	// 用于记录当前节点使用中的镜像名
	imagesInUse := sets.NewString()

	// Always consider the container runtime pod sandbox image in use
	// 通过CRI接口调用容器运行时获取沙箱使用的镜像，如果此接口没有错误，并且成功获取到了沙箱使用镜像的ID了，就说明当前的沙箱镜像还在使用
	imageRef, err := im.runtime.GetImageRef(ctx, container.ImageSpec{Image: im.sandboxImage})
	if err == nil && imageRef != "" {
		imagesInUse.Insert(imageRef)
	}

	// 获取当前节点使用的所有镜像信息
	images, err := im.runtime.ListImages(ctx)
	if err != nil {
		return imagesInUse, err
	}
	// 获取所有的Pod，当前存在的Pod所使用的镜像一定是不能回收的了，这些镜像都认为时使用中的镜像
	pods, err := im.runtime.GetPods(ctx, true)
	if err != nil {
		return imagesInUse, err
	}

	// Make a set of images in use by containers.
	for _, pod := range pods {
		for _, container := range pod.Containers {
			klog.V(5).InfoS("Container uses image", "pod", klog.KRef(pod.Namespace, pod.Name), "containerName", container.Name, "containerImage", container.Image, "imageID", container.ImageID)
			imagesInUse.Insert(container.ImageID)
		}
	}

	// Add new images and record those being used.
	now := time.Now()
	currentImages := sets.NewString()
	im.imageRecordsLock.Lock()
	defer im.imageRecordsLock.Unlock()
	for _, image := range images {
		klog.V(5).InfoS("Adding image ID to currentImages", "imageID", image.ID)
		currentImages.Insert(image.ID)

		// 如果再ImageRecord缓存中没有找到，说明这个镜像是一个新的镜像
		if _, ok := im.imageRecords[image.ID]; !ok {
			klog.V(5).InfoS("Image ID is new", "imageID", image.ID)
			im.imageRecords[image.ID] = &imageRecord{
				firstDetected: detectTime,
			}
		}

		// Set last used time to now if the image is being used.
		// 如果镜像还在使用，那么更新镜像最后还在使用的时间
		if isImageUsed(image.ID, imagesInUse) {
			klog.V(5).InfoS("Setting Image ID lastUsed", "imageID", image.ID, "lastUsed", now)
			im.imageRecords[image.ID].lastUsed = now
		}

		klog.V(5).InfoS("Image ID has size", "imageID", image.ID, "size", image.Size)
		im.imageRecords[image.ID].size = image.Size

		klog.V(5).InfoS("Image ID is pinned", "imageID", image.ID, "pinned", image.Pinned)
		im.imageRecords[image.ID].pinned = image.Pinned
	}

	// 遍历imageRecords缓存，如果当前镜像不存在，说明镜像已经删除，直接移除imageRecords缓存中的信息
	for image := range im.imageRecords {
		if !currentImages.Has(image) {
			klog.V(5).InfoS("Image ID is no longer present; removing from imageRecords", "imageID", image)
			delete(im.imageRecords, image)
		}
	}

	return imagesInUse, nil
}

func (im *realImageGCManager) GarbageCollect(ctx context.Context) error {
	ctx, otelSpan := im.tracer.Start(ctx, "Images/GarbageCollect")
	defer otelSpan.End()
	// Get disk usage on disk holding images.
	// 获取当前镜像使用文件系统的情况
	fsStats, err := im.statsProvider.ImageFsStats(ctx)
	if err != nil {
		return err
	}

	var capacity, available int64
	if fsStats.CapacityBytes != nil {
		capacity = int64(*fsStats.CapacityBytes)
	}
	if fsStats.AvailableBytes != nil {
		available = int64(*fsStats.AvailableBytes)
	}

	if available > capacity {
		klog.InfoS("Availability is larger than capacity", "available", available, "capacity", capacity)
		available = capacity
	}

	// Check valid capacity.
	if capacity == 0 {
		err := goerrors.New("invalid capacity 0 on image filesystem")
		im.recorder.Eventf(im.nodeRef, v1.EventTypeWarning, events.InvalidDiskCapacity, err.Error())
		return err
	}

	// If over the max threshold, free enough to place us at the lower threshold.
	// 获取当前节点镜像占用文件系统的百分比
	usagePercent := 100 - int(available*100/capacity)
	// 如果镜像使用的容量大于镜像回收策略的最大水线，就需要回收镜像
	if usagePercent >= im.policy.HighThresholdPercent {
		// 计算需要释放的容量大小，只有释放了这么的镜像，才能把镜像占用文件系统的百分比降到LowThresholdPercent以下
		amountToFree := capacity*int64(100-im.policy.LowThresholdPercent)/100 - available
		klog.InfoS("Disk usage on image filesystem is over the high threshold, trying to free bytes down to the low threshold", "usage", usagePercent, "highThreshold", im.policy.HighThresholdPercent, "amountToFree", amountToFree, "lowThreshold", im.policy.LowThresholdPercent)
		freed, err := im.freeSpace(ctx, amountToFree, time.Now())
		if err != nil {
			return err
		}

		// 如果实际上释放的空间小于需要释放的空间
		if freed < amountToFree {
			err := fmt.Errorf("Failed to garbage collect required amount of images. Attempted to free %d bytes, but only found %d bytes eligible to free.", amountToFree, freed)
			im.recorder.Eventf(im.nodeRef, v1.EventTypeWarning, events.FreeDiskSpaceFailed, err.Error())
			return err
		}
	}

	return nil
}

func (im *realImageGCManager) DeleteUnusedImages(ctx context.Context) error {
	klog.InfoS("Attempting to delete unused images")
	// 删除所有没有使用的镜像
	_, err := im.freeSpace(ctx, math.MaxInt64, time.Now())
	return err
}

// Tries to free bytesToFree worth of images on the disk.
//
// Returns the number of bytes free and an error if any occurred. The number of
// bytes freed is always returned.
// Note that error may be nil and the number of bytes free may be less
// than bytesToFree.
func (im *realImageGCManager) freeSpace(ctx context.Context, bytesToFree int64, freeTime time.Time) (int64, error) {
	// 获取当前使用的镜像信息，显然如果当前镜像正在使用，那肯定不能释放
	imagesInUse, err := im.detectImages(ctx, freeTime)
	if err != nil {
		return 0, err
	}

	im.imageRecordsLock.Lock()
	defer im.imageRecordsLock.Unlock()

	// Get all images in eviction order.
	images := make([]evictionInfo, 0, len(im.imageRecords))
	for image, record := range im.imageRecords {
		// 如果当前镜像正在使用，那肯定时不能删除镜像，释放磁盘空间
		if isImageUsed(image, imagesInUse) {
			klog.V(5).InfoS("Image ID is being used", "imageID", image)
			continue
		}
		// Check if image is pinned, prevent garbage collection
		// 如果当前镜像虽然没有Pod正在使用，但是处于Pinned状态，也是不能删除的
		if record.pinned {
			klog.V(5).InfoS("Image is pinned, skipping garbage collection", "imageID", image)
			continue

		}
		// 否则，当前镜像就是可以删除的
		images = append(images, evictionInfo{
			id:          image,
			imageRecord: *record,
		})
	}
	// 现根据LastUsed排序，再根据Detected进行排序，原则就是做老的镜像最先被删除
	sort.Sort(byLastUsedAndDetected(images))

	// Delete unused images until we've freed up enough space.
	var deletionErrors []error
	spaceFreed := int64(0)
	for _, image := range images {
		klog.V(5).InfoS("Evaluating image ID for possible garbage collection", "imageID", image.id)
		// Images that are currently in used were given a newer lastUsed.
		// 如果当前镜像正在使用那就暂时不删除，防止后面再次使用该镜像
		if image.lastUsed.Equal(freeTime) || image.lastUsed.After(freeTime) {
			klog.V(5).InfoS("Image ID was used too recently, not eligible for garbage collection", "imageID", image.id, "lastUsed", image.lastUsed, "freeTime", freeTime)
			continue
		}

		// Avoid garbage collect the image if the image is not old enough.
		// In such a case, the image may have just been pulled down, and will be used by a container right away.
		// 如果当前镜像才生存了一小会儿，还没有超过最小生存时间，那么也暂时不进行回收
		if freeTime.Sub(image.firstDetected) < im.policy.MinAge {
			klog.V(5).InfoS("Image ID's age is less than the policy's minAge, not eligible for garbage collection", "imageID", image.id, "age", freeTime.Sub(image.firstDetected), "minAge", im.policy.MinAge)
			continue
		}

		// Remove image. Continue despite errors.
		klog.InfoS("Removing image to free bytes", "imageID", image.id, "size", image.size)
		// 通过调用CRI接口，移除镜像
		err := im.runtime.RemoveImage(ctx, container.ImageSpec{Image: image.id})
		if err != nil {
			deletionErrors = append(deletionErrors, err)
			continue
		}
		// 如果移除成功，那么需要把已经移除的镜像从ImageRecords缓存当中删除
		delete(im.imageRecords, image.id)
		// 记录已经释放的镜像大小
		spaceFreed += image.size

		// 如果已经释放了需要的空间，那就直接推出
		if spaceFreed >= bytesToFree {
			break
		}
	}

	if len(deletionErrors) > 0 {
		return spaceFreed, fmt.Errorf("wanted to free %d bytes, but freed %d bytes space with errors in image deletion: %v", bytesToFree, spaceFreed, errors.NewAggregate(deletionErrors))
	}
	return spaceFreed, nil
}

type evictionInfo struct {
	id string
	imageRecord
}

type byLastUsedAndDetected []evictionInfo

func (ev byLastUsedAndDetected) Len() int      { return len(ev) }
func (ev byLastUsedAndDetected) Swap(i, j int) { ev[i], ev[j] = ev[j], ev[i] }
func (ev byLastUsedAndDetected) Less(i, j int) bool {
	// Sort by last used, break ties by detected.
	if ev[i].lastUsed.Equal(ev[j].lastUsed) {
		return ev[i].firstDetected.Before(ev[j].firstDetected)
	}
	return ev[i].lastUsed.Before(ev[j].lastUsed)
}

func isImageUsed(imageID string, imagesInUse sets.String) bool {
	// Check the image ID.
	if _, ok := imagesInUse[imageID]; ok {
		return true
	}
	return false
}
