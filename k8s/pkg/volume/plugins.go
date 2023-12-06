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

package volume

import (
	"fmt"
	"net"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/mount-utils"
	"k8s.io/utils/exec"

	authenticationv1 "k8s.io/api/authentication/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/informers"
	clientset "k8s.io/client-go/kubernetes"
	storagelistersv1 "k8s.io/client-go/listers/storage/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	cloudprovider "k8s.io/cloud-provider"
	proxyutil "k8s.io/kubernetes/pkg/proxy/util"
	"k8s.io/kubernetes/pkg/volume/util/hostutil"
	"k8s.io/kubernetes/pkg/volume/util/recyclerclient"
	"k8s.io/kubernetes/pkg/volume/util/subpath"
)

type ProbeOperation uint32
type ProbeEvent struct {
	Plugin     VolumePlugin // VolumePlugin that was added/updated/removed. if ProbeEvent.Op is 'ProbeRemove', Plugin should be nil
	PluginName string
	Op         ProbeOperation // The operation to the plugin
}

const (
	// Common parameter which can be specified in StorageClass to specify the desired FSType
	// Provisioners SHOULD implement support for this if they are block device based
	// Must be a filesystem type supported by the host operating system.
	// Ex. "ext4", "xfs", "ntfs". Default value depends on the provisioner
	VolumeParameterFSType = "fstype"

	ProbeAddOrUpdate ProbeOperation = 1 << iota
	ProbeRemove
)

// VolumeOptions contains option information about a volume.
type VolumeOptions struct {
	// The attributes below are required by volume.Provisioner
	// TODO: refactor all of this out of volumes when an admin can configure
	// many kinds of provisioners.

	// Reclamation policy for a persistent volume
	PersistentVolumeReclaimPolicy v1.PersistentVolumeReclaimPolicy
	// Mount options for a persistent volume
	// TODO 1、挂载参数是如何使用的？
	MountOptions []string
	// Suggested PV.Name of the PersistentVolume to provision.
	// This is a generated name guaranteed to be unique in Kubernetes cluster.
	// If you choose not to use it as volume name, ensure uniqueness by either
	// combining it with your value or create unique values of your own.
	PVName string
	// PVC is reference to the claim that lead to provisioning of a new PV.
	// Provisioners *must* create a PV that would be matched by this PVC,
	// i.e. with required capacity, accessMode, labels matching PVC.Selector and
	// so on.
	PVC *v1.PersistentVolumeClaim
	// Unique name of Kubernetes cluster.
	ClusterName string
	// Tags to attach to the real volume in the cloud provider - e.g. AWS EBS
	CloudTags *map[string]string
	// Volume provisioning parameters from StorageClass
	Parameters map[string]string
}

// NodeResizeOptions contain options to be passed for node expansion.
type NodeResizeOptions struct {
	VolumeSpec *Spec

	// DevicePath - location of actual device on the node. In case of CSI
	// this just could be volumeID
	DevicePath string

	// DeviceMountPath location where device is mounted on the node. If volume type
	// is attachable - this would be global mount path otherwise
	// it would be location where volume was mounted for the pod
	DeviceMountPath string

	// DeviceStagingPath stores location where the volume is staged
	DeviceStagePath string

	NewSize resource.Quantity
	OldSize resource.Quantity
}

// DynamicPluginProber
// TODO 这玩意是用来干嘛的？
// TODO 什么叫做动态插件？
type DynamicPluginProber interface {
	Init() error

	// Probe aggregates events for successful drivers and errors for failed drivers
	Probe() (events []ProbeEvent, err error)
}

// VolumePlugin is an interface to volume plugins that can be used on a
// kubernetes node (e.g. by kubelet) to instantiate and manage volumes.
// TODO 什么叫做卷插件? 干嘛用的？ K8S是如何抽象的？
type VolumePlugin interface {
	// Init initializes the plugin.  This will be called exactly once
	// before any New* calls are made - implementations of plugins may
	// depend on this.
	// 1、初始化插件，注入VolumeHost依赖
	Init(host VolumeHost) error

	// GetPluginName Name returns the plugin's name.  Plugins must use namespaced names
	// such as "example.com/volume" and contain exactly one '/' character.
	// The "kubernetes.io" namespace is reserved for plugins which are
	// bundled with kubernetes.
	// 获取当前卷插件的名字，譬如kubernetes.io/nfs
	GetPluginName() string

	// GetVolumeName returns the name/ID to uniquely identifying the actual
	// backing device, directory, path, etc. referenced by the specified volume
	// spec.
	// For Attachable volumes, this value must be able to be passed back to
	// volume Detach methods to identify the device to act on.
	// If the plugin does not support the given spec, this returns an error.
	GetVolumeName(spec *Spec) (string, error)

	// CanSupport tests whether the plugin supports a given volume
	// specification from the API.  The spec pointer should be considered
	// const.
	// 1、对于NFS插件来说，只要指定的NFS地址不为空即可
	CanSupport(spec *Spec) bool

	// RequiresRemount returns true if this plugin requires mount calls to be
	// reexecuted. Atomically updating volumes, like Downward API, depend on
	// this to update the contents of the volume.
	RequiresRemount(spec *Spec) bool

	// NewMounter creates a new volume.Mounter from an API specification.
	// Ownership of the spec pointer in *not* transferred.
	// - spec: The v1.Volume spec
	// - pod: The enclosing pod
	// 1、用于挂载指定的卷
	NewMounter(spec *Spec, podRef *v1.Pod, opts VolumeOptions) (Mounter, error)

	// NewUnmounter creates a new volume.Unmounter from recoverable state.
	// - name: The volume name, as per the v1.Volume spec.
	// - podUID: The UID of the enclosing pod
	// 用于卸载指定卷
	NewUnmounter(name string, podUID types.UID) (Unmounter, error)

	// ConstructVolumeSpec constructs a volume spec based on the given volume name
	// and volumePath. The spec may have incomplete information due to limited
	// information from input. This function is used by volume manager to reconstruct
	// volume spec by reading the volume directories from disk
	// TODO 这玩意有啥用？
	ConstructVolumeSpec(volumeName, volumePath string) (ReconstructedVolume, error)

	// SupportsMountOption returns true if volume plugins supports Mount options
	// Specifying mount options in a volume plugin that doesn't support
	// user specified mount options will result in error creating persistent volumes
	// TODO 这个参数有何用？
	SupportsMountOption() bool

	// SupportsBulkVolumeVerification checks if volume plugin type is capable
	// of enabling bulk polling of all nodes. This can speed up verification of
	// attached volumes by quite a bit, but underlying pluging must support it.
	// 是否支持批量卷的校验
	SupportsBulkVolumeVerification() bool

	// SupportsSELinuxContextMount returns true if volume plugins supports
	// mount -o context=XYZ for a given volume.
	SupportsSELinuxContextMount(spec *Spec) (bool, error)
}

// PersistentVolumePlugin is an extended interface of VolumePlugin and is used
// by volumes that want to provide long term persistence of data
type PersistentVolumePlugin interface {
	VolumePlugin
	// GetAccessModes describes the ways a given volume can be accessed/mounted.
	// 获取持久卷的访问模式
	GetAccessModes() []v1.PersistentVolumeAccessMode
}

// RecyclableVolumePlugin is an extended interface of VolumePlugin and is used
// by persistent volumes that want to be recycled before being made available
// again to new claims
type RecyclableVolumePlugin interface {
	VolumePlugin

	// Recycle knows how to reclaim this
	// resource after the volume's release from a PersistentVolumeClaim.
	// Recycle will use the provided recorder to write any events that might be
	// interesting to user. It's expected that caller will pass these events to
	// the PV being recycled.
	// TODO 一个存储卷如何循环使用？
	Recycle(pvName string, spec *Spec, eventRecorder recyclerclient.RecycleEventRecorder) error
}

// DeletableVolumePlugin is an extended interface of VolumePlugin and is used
// by persistent volumes that want to be deleted from the cluster after their
// release from a PersistentVolumeClaim.
// 1、删除一个持久卷
type DeletableVolumePlugin interface {
	VolumePlugin
	// NewDeleter creates a new volume.Deleter which knows how to delete this
	// resource in accordance with the underlying storage provider after the
	// volume's release from a claim
	NewDeleter(logger klog.Logger, spec *Spec) (Deleter, error)
}

// ProvisionableVolumePlugin is an extended interface of VolumePlugin and is
// used to create volumes for the cluster.
// 1、创建一个持久卷
type ProvisionableVolumePlugin interface {
	VolumePlugin
	// NewProvisioner creates a new volume.Provisioner which knows how to
	// create PersistentVolumes in accordance with the plugin's underlying
	// storage provider
	NewProvisioner(logger klog.Logger, options VolumeOptions) (Provisioner, error)
}

// AttachableVolumePlugin is an extended interface of VolumePlugin and is used for volumes that require attachment
// to a node before mounting.
// 1、支持卷的Attach/Detach操作
type AttachableVolumePlugin interface {
	DeviceMountableVolumePlugin
	NewAttacher() (Attacher, error)
	NewDetacher() (Detacher, error)
	// CanAttach tests if provided volume spec is attachable
	CanAttach(spec *Spec) (bool, error)
}

// DeviceMountableVolumePlugin is an extended interface of VolumePlugin and is used
// for volumes that requires mount device to a node before binding to volume to pod.
// TODO 1、什么叫做MountDevice
type DeviceMountableVolumePlugin interface {
	VolumePlugin
	NewDeviceMounter() (DeviceMounter, error)
	NewDeviceUnmounter() (DeviceUnmounter, error)
	GetDeviceMountRefs(deviceMountPath string) ([]string, error)
	// CanDeviceMount determines if device in volume.Spec is mountable
	CanDeviceMount(spec *Spec) (bool, error)
}

// ExpandableVolumePlugin is an extended interface of VolumePlugin and is used for volumes that can be
// expanded via control-plane ExpandVolumeDevice call.
// 1、支持卷的扩容
type ExpandableVolumePlugin interface {
	VolumePlugin
	ExpandVolumeDevice(spec *Spec, newSize resource.Quantity, oldSize resource.Quantity) (resource.Quantity, error)
	RequiresFSResize() bool
}

// NodeExpandableVolumePlugin is an expanded interface of VolumePlugin and is used for volumes that
// require expansion on the node via NodeExpand call.
type NodeExpandableVolumePlugin interface {
	VolumePlugin
	RequiresFSResize() bool
	// NodeExpand expands volume on given deviceMountPath and returns true if resize is successful.
	NodeExpand(resizeOptions NodeResizeOptions) (bool, error)
}

// VolumePluginWithAttachLimits is an extended interface of VolumePlugin that restricts number of
// volumes that can be attached to a node.
type VolumePluginWithAttachLimits interface {
	VolumePlugin
	// GetVolumeLimits Return maximum number of volumes that can be attached to a node for this plugin.
	// The key must be same as string returned by VolumeLimitKey function. The returned
	// map may look like:
	//     - { "storage-limits-aws-ebs": 39 }
	//     - { "storage-limits-gce-pd": 10 }
	// A volume plugin may return error from this function - if it can not be used on a given node or not
	// applicable in given environment (where environment could be cloudprovider or any other dependency)
	// For example - calling this function for EBS volume plugin on a GCE node should
	// result in error.
	// The returned values are stored in node allocatable property and will be used
	// by scheduler to determine how many pods with volumes can be scheduled on given node.
	GetVolumeLimits() (map[string]int64, error)
	// VolumeLimitKey Return volume limit key string to be used in node capacity constraints
	// The key must start with prefix storage-limits-. For example:
	//    - storage-limits-aws-ebs
	//    - storage-limits-csi-cinder
	// The key should respect character limit of ResourceName type
	// This function may be called by kubelet or scheduler to identify node allocatable property
	// which stores volumes limits.
	VolumeLimitKey(spec *Spec) string
}

// BlockVolumePlugin is an extend interface of VolumePlugin and is used for block volumes support.
// 1、似乎是对于块设备的支持
type BlockVolumePlugin interface {
	VolumePlugin
	// NewBlockVolumeMapper creates a new volume.BlockVolumeMapper from an API specification.
	// Ownership of the spec pointer in *not* transferred.
	// - spec: The v1.Volume spec
	// - pod: The enclosing pod
	NewBlockVolumeMapper(spec *Spec, podRef *v1.Pod, opts VolumeOptions) (BlockVolumeMapper, error)
	// NewBlockVolumeUnmapper creates a new volume.BlockVolumeUnmapper from recoverable state.
	// - name: The volume name, as per the v1.Volume spec.
	// - podUID: The UID of the enclosing pod
	NewBlockVolumeUnmapper(name string, podUID types.UID) (BlockVolumeUnmapper, error)
	// ConstructBlockVolumeSpec constructs a volume spec based on the given
	// podUID, volume name and a pod device map path.
	// The spec may have incomplete information due to limited information
	// from input. This function is used by volume manager to reconstruct
	// volume spec by reading the volume directories from disk.
	ConstructBlockVolumeSpec(podUID types.UID, volumeName, volumePath string) (*Spec, error)
}

// TODO(#14217)
// As part of the Volume Host refactor we are starting to create Volume Hosts
// for specific hosts. New methods for each specific host can be added here.
// Currently consumers will do type assertions to get the specific type of Volume
// Host; however, the end result should be that specific Volume Hosts are passed
// to the specific functions they are needed in (instead of using a catch-all
// VolumeHost interface)

// KubeletVolumeHost is a Kubelet specific interface that plugins can use to access the kubelet.
type KubeletVolumeHost interface {
	// SetKubeletError lets plugins set an error on the Kubelet runtime status
	// that will cause the Kubelet to post NotReady status with the error message provided
	SetKubeletError(err error)

	// GetInformerFactory returns the informer factory for CSIDriverLister
	GetInformerFactory() informers.SharedInformerFactory
	// CSIDriverLister returns the informer lister for the CSIDriver API Object
	CSIDriverLister() storagelistersv1.CSIDriverLister
	// CSIDriverSynced returns the informer synced for the CSIDriver API Object
	CSIDriversSynced() cache.InformerSynced
	// WaitForCacheSync is a helper function that waits for cache sync for CSIDriverLister
	WaitForCacheSync() error
	// Returns hostutil.HostUtils
	GetHostUtil() hostutil.HostUtils
}

// AttachDetachVolumeHost is a AttachDetach Controller specific interface that plugins can use
// to access methods on the Attach Detach Controller.
type AttachDetachVolumeHost interface {
	// CSINodeLister returns the informer lister for the CSINode API Object
	CSINodeLister() storagelistersv1.CSINodeLister

	// CSIDriverLister returns the informer lister for the CSIDriver API Object
	CSIDriverLister() storagelistersv1.CSIDriverLister

	// VolumeAttachmentLister returns the informer lister for the VolumeAttachment API Object
	VolumeAttachmentLister() storagelistersv1.VolumeAttachmentLister
	// IsAttachDetachController is an interface marker to strictly tie AttachDetachVolumeHost
	// to the attachDetachController
	IsAttachDetachController() bool
}

// VolumeHost is an interface that plugins can use to access the kubelet.
// 此接口用于访问kubelet
type VolumeHost interface {
	// GetPluginDir returns the absolute path to a directory under which
	// a given plugin may store data.  This directory might not actually
	// exist on disk yet.  For plugin data that is per-pod, see
	// GetPodPluginDir().
	// 默认为 /var/lib/kubelet/plugins/<plugin-name>
	GetPluginDir(pluginName string) string

	// GetVolumeDevicePluginDir returns the absolute path to a directory
	// under which a given plugin may store data.
	// ex. plugins/kubernetes.io/{PluginName}/{DefaultKubeletVolumeDevicesDirName}/{volumePluginDependentPath}/
	// 默认为 /var/lib/kubelet/plugins/<plugin-name>/volumeDevices
	GetVolumeDevicePluginDir(pluginName string) string

	// GetPodsDir returns the absolute path to a directory where all the pods
	// information is stored
	// 默认为 /var/lib/kubelet/pods
	GetPodsDir() string

	// GetPodVolumeDir returns the absolute path a directory which
	// represents the named volume under the named plugin for the given
	// pod.  If the specified pod does not exist, the result of this call
	// might not exist.
	// 默认为 /var/lib/kubelet/pods/<pod-uid>/volumes/<plugin-name>/<volume-name>
	GetPodVolumeDir(podUID types.UID, pluginName string, volumeName string) string

	// GetPodPluginDir returns the absolute path to a directory under which
	// a given plugin may store data for a given pod.  If the specified pod
	// does not exist, the result of this call might not exist.  This
	// directory might not actually exist on disk yet.
	// 默认为 /var/lib/kubelet/pods/<pod-uid>/plugins/<plugin-name>
	GetPodPluginDir(podUID types.UID, pluginName string) string

	// GetPodVolumeDeviceDir returns the absolute path a directory which
	// represents the named plugin for the given pod.
	// If the specified pod does not exist, the result of this call
	// might not exist.
	// ex. pods/{podUid}/{DefaultKubeletVolumeDevicesDirName}/{escapeQualifiedPluginName}/
	// 默认为 /var/lib/kubelet/pods/<pod-uid>/volumeDevices/<plugin-name>
	GetPodVolumeDeviceDir(podUID types.UID, pluginName string) string

	// GetKubeClient returns a client interface
	// 用于获取ClientSet客户端
	GetKubeClient() clientset.Interface

	// NewWrapperMounter finds an appropriate plugin with which to handle
	// the provided spec.  This is used to implement volume plugins which
	// "wrap" other plugins.  For example, the "secret" volume is
	// implemented in terms of the "emptyDir" volume.
	NewWrapperMounter(volName string, spec Spec, pod *v1.Pod, opts VolumeOptions) (Mounter, error)

	// NewWrapperUnmounter finds an appropriate plugin with which to handle
	// the provided spec.  See comments on NewWrapperMounter for more
	// context.
	NewWrapperUnmounter(volName string, spec Spec, podUID types.UID) (Unmounter, error)

	// GetCloudProvider Get cloud provider from kubelet.
	GetCloudProvider() cloudprovider.Interface

	// GetMounter Get mounter interface.
	GetMounter(pluginName string) mount.Interface

	// GetHostName Returns the hostname of the host kubelet is running on
	// 获取kubelet所在节点的主机名字
	GetHostName() string

	// GetHostIP Returns host IP or nil in the case of error.
	// 获取kubelet所在节点的主机IP地址
	GetHostIP() (net.IP, error)

	// GetNodeAllocatable Returns node allocatable.
	// 获取kubelet所在节点可分配资源的大小
	GetNodeAllocatable() (v1.ResourceList, error)

	// GetSecretFunc zReturns a function that returns a secret.
	// 获取某个Secret
	GetSecretFunc() func(namespace, name string) (*v1.Secret, error)

	// GetConfigMapFunc Returns a function that returns a configmap.
	// 获取某个configmap
	GetConfigMapFunc() func(namespace, name string) (*v1.ConfigMap, error)

	// GetServiceAccountTokenFunc 获取Token
	GetServiceAccountTokenFunc() func(namespace, name string, tr *authenticationv1.TokenRequest) (*authenticationv1.TokenRequest, error)

	DeleteServiceAccountTokenFunc() func(podUID types.UID)

	// GetExec Returns an interface that should be used to execute any utilities in volume plugins
	// TODO 暂时不知道这玩意干嘛的
	GetExec(pluginName string) exec.Interface

	// GetNodeLabels Returns the labels on the node
	GetNodeLabels() (map[string]string, error)

	// GetNodeName Returns the name of the node
	GetNodeName() types.NodeName

	GetAttachedVolumesFromNodeStatus() (map[v1.UniqueVolumeName]string, error)

	// GetEventRecorder Returns the event recorder of kubelet.
	GetEventRecorder() record.EventRecorder

	// GetSubpather Returns an interface that should be used to execute subpath operations
	GetSubpather() subpath.Interface

	// GetFilteredDialOptions Returns options to pass for proxyutil filtered dialers.
	GetFilteredDialOptions() *proxyutil.FilteredDialOptions
}

// VolumePluginMgr tracks registered plugins.
// 1、卷管理器用于追踪、管理注册的卷插件。实际上就是一个Map, key为插件名，value为插件
type VolumePluginMgr struct {
	mutex sync.RWMutex
	// key为插件名, value为卷插件
	plugins map[string]VolumePlugin
	// TODO 分析这玩意
	prober DynamicPluginProber
	// TODO Probed插件和普通的卷插件有何不同
	probedPlugins map[string]VolumePlugin
	// 用于保存已经废弃API的警告信息
	loggedDeprecationWarnings sets.String
	Host                      VolumeHost
}

// Spec is an internal representation of a volume.  All API volume types translate to Spec.
type Spec struct {
	Volume                          *v1.Volume           // 用于表示一个挂载卷
	PersistentVolume                *v1.PersistentVolume // TODO 为什么需要这么一个东西？
	ReadOnly                        bool                 // 当前卷是否只读
	InlineVolumeSpecForCSIMigration bool                 // TODO 表示什么含义？
	Migrated                        bool                 // TODO 表示什么含义？
}

// Name returns the name of either Volume or PersistentVolume, one of which must not be nil.
func (spec *Spec) Name() string {
	switch {
	case spec.Volume != nil:
		return spec.Volume.Name
	case spec.PersistentVolume != nil:
		return spec.PersistentVolume.Name
	default:
		return ""
	}
}

// IsKubeletExpandable returns true for volume types that can be expanded only by the node
// and not the controller. Currently Flex volume is the only one in this category since
// it is typically not installed on the controller
func (spec *Spec) IsKubeletExpandable() bool {
	switch {
	case spec.Volume != nil:
		return spec.Volume.FlexVolume != nil
	case spec.PersistentVolume != nil:
		return spec.PersistentVolume.Spec.FlexVolume != nil
	default:
		return false
	}
}

// KubeletExpandablePluginName creates and returns a name for the plugin
// this is used in context on the controller where the plugin lookup fails
// as volume expansion on controller isn't supported, but a plugin name is
// required
func (spec *Spec) KubeletExpandablePluginName() string {
	switch {
	case spec.Volume != nil && spec.Volume.FlexVolume != nil:
		return spec.Volume.FlexVolume.Driver
	case spec.PersistentVolume != nil && spec.PersistentVolume.Spec.FlexVolume != nil:
		return spec.PersistentVolume.Spec.FlexVolume.Driver
	default:
		return ""
	}
}

// VolumeConfig is how volume plugins receive configuration.  An instance
// specific to the plugin will be passed to the plugin's
// ProbeVolumePlugins(config) func.  Reasonable defaults will be provided by
// the binary hosting the plugins while allowing override of those default
// values.  Those config values are then set to an instance of VolumeConfig
// and passed to the plugin.
//
// Values in VolumeConfig are intended to be relevant to several plugins, but
// not necessarily all plugins.  The preference is to leverage strong typing
// in this struct.  All config items must have a descriptive but non-specific
// name (i.e, RecyclerMinimumTimeout is OK but RecyclerMinimumTimeoutForNFS is
// !OK).  An instance of config will be given directly to the plugin, so
// config names specific to plugins are unneeded and wrongly expose plugins in
// this VolumeConfig struct.
//
// OtherAttributes is a map of string values intended for one-off
// configuration of a plugin or config that is only relevant to a single
// plugin.  All values are passed by string and require interpretation by the
// plugin. Passing config as strings is the least desirable option but can be
// used for truly one-off configuration. The binary should still use strong
// typing for this value when binding CLI values before they are passed as
// strings in OtherAttributes.
// 1、TODO 如何理解这里的配置？
type VolumeConfig struct {
	// RecyclerPodTemplate is pod template that understands how to scrub clean
	// a persistent volume after its release. The template is used by plugins
	// which override specific properties of the pod in accordance with that
	// plugin. See NewPersistentVolumeRecyclerPodTemplate for the properties
	// that are expected to be overridden.
	RecyclerPodTemplate *v1.Pod

	// RecyclerMinimumTimeout is the minimum amount of time in seconds for the
	// recycler pod's ActiveDeadlineSeconds attribute. Added to the minimum
	// timeout is the increment per Gi of capacity.
	RecyclerMinimumTimeout int

	// RecyclerTimeoutIncrement is the number of seconds added to the recycler
	// pod's ActiveDeadlineSeconds for each Gi of capacity in the persistent
	// volume. Example: 5Gi volume x 30s increment = 150s + 30s minimum = 180s
	// ActiveDeadlineSeconds for recycler pod
	RecyclerTimeoutIncrement int

	// PVName is name of the PersistentVolume instance that is being recycled.
	// It is used to generate unique recycler pod name.
	PVName string

	// OtherAttributes stores config as strings.  These strings are opaque to
	// the system and only understood by the binary hosting the plugin and the
	// plugin itself.
	OtherAttributes map[string]string

	// ProvisioningEnabled configures whether provisioning of this plugin is
	// enabled or not. Currently used only in host_path plugin.
	ProvisioningEnabled bool
}

// ReconstructedVolume contains information about a volume reconstructed by
// ConstructVolumeSpec().
type ReconstructedVolume struct {
	// Spec is the volume spec of a mounted volume
	Spec *Spec
	// SELinuxMountContext is value of -o context=XYZ mount option.
	// If empty, no such mount option is used.
	SELinuxMountContext string
}

// NewSpecFromVolume creates an Spec from an v1.Volume
func NewSpecFromVolume(vs *v1.Volume) *Spec {
	return &Spec{
		Volume: vs,
	}
}

// NewSpecFromPersistentVolume creates an Spec from an v1.PersistentVolume
func NewSpecFromPersistentVolume(pv *v1.PersistentVolume, readOnly bool) *Spec {
	return &Spec{
		PersistentVolume: pv,
		ReadOnly:         readOnly,
	}
}

// InitPlugins initializes each plugin.  All plugins must have unique names.
// This must be called exactly once before any New* methods are called on any
// plugins.
// 1、遍历传入的卷插件，挨个注册，同时调用每个卷插件的Init(host)方法进行卷插件的初始化
func (pm *VolumePluginMgr) InitPlugins(plugins []VolumePlugin, prober DynamicPluginProber, host VolumeHost) error {
	pm.mutex.Lock()
	defer pm.mutex.Unlock()

	pm.Host = host
	pm.loggedDeprecationWarnings = sets.NewString()

	if prober == nil {
		// Use a dummy prober to prevent nil deference.
		pm.prober = &dummyPluginProber{}
	} else {
		pm.prober = prober
	}
	// TODO 初始化卷探测器
	if err := pm.prober.Init(); err != nil {
		// Prober init failure should not affect the initialization of other plugins.
		klog.ErrorS(err, "Error initializing dynamic plugin prober")
		pm.prober = &dummyPluginProber{}
	}

	if pm.plugins == nil {
		pm.plugins = map[string]VolumePlugin{}
	}
	if pm.probedPlugins == nil {
		pm.probedPlugins = map[string]VolumePlugin{}
	}

	var allErrs []error
	// 遍历所有的注册的卷插件
	for _, plugin := range plugins {
		name := plugin.GetPluginName()
		// 校验卷插件的名字是否有效
		if errs := validation.IsQualifiedName(name); len(errs) != 0 {
			allErrs = append(allErrs, fmt.Errorf("volume plugin has invalid name: %q: %s", name, strings.Join(errs, ";")))
			continue
		}

		// 已经注册过的卷不允许再注册
		if _, found := pm.plugins[name]; found {
			allErrs = append(allErrs, fmt.Errorf("volume plugin %q was registered more than once", name))
			continue
		}

		// 初始化卷插件，注意VolumeHost依赖
		err := plugin.Init(host)
		if err != nil {
			klog.ErrorS(err, "Failed to load volume plugin", "pluginName", name)
			allErrs = append(allErrs, err)
			continue
		}
		// 保存注册过的卷插件
		pm.plugins[name] = plugin
		klog.V(1).InfoS("Loaded volume plugin", "pluginName", name)
	}
	return utilerrors.NewAggregate(allErrs)
}

func (pm *VolumePluginMgr) initProbedPlugin(probedPlugin VolumePlugin) error {
	// 获取卷的名字
	name := probedPlugin.GetPluginName()
	// 校验卷名是否符合规范
	if errs := validation.IsQualifiedName(name); len(errs) != 0 {
		return fmt.Errorf("volume plugin has invalid name: %q: %s", name, strings.Join(errs, ";"))
	}

	// 初始化ProbedVolumePlugin
	err := probedPlugin.Init(pm.Host)
	if err != nil {
		return fmt.Errorf("failed to load volume plugin %s, error: %s", name, err.Error())
	}

	klog.V(1).InfoS("Loaded volume plugin", "pluginName", name)
	return nil
}

// FindPluginBySpec looks for a plugin that can support a given volume
// specification.  If no plugins can support or more than one plugin can
// support it, return error.
// 1、根据调用方指定的卷，找到其对应的卷插件
func (pm *VolumePluginMgr) FindPluginBySpec(spec *Spec) (VolumePlugin, error) {
	pm.mutex.RLock()
	defer pm.mutex.RUnlock()

	if spec == nil {
		return nil, fmt.Errorf("could not find plugin because volume spec is nil")
	}

	var match VolumePlugin
	var matchedPluginNames []string
	for _, v := range pm.plugins {
		if v.CanSupport(spec) {
			match = v
			// 保存支持当前卷的卷插件名字
			matchedPluginNames = append(matchedPluginNames, v.GetPluginName())
		}
	}

	// TODO 刷新Probed卷插件
	pm.refreshProbedPlugins()
	for _, plugin := range pm.probedPlugins {
		if plugin.CanSupport(spec) {
			match = plugin
			matchedPluginNames = append(matchedPluginNames, plugin.GetPluginName())
		}
	}

	if len(matchedPluginNames) == 0 {
		return nil, fmt.Errorf("no volume plugin matched")
	}
	// 看来有多个卷插件支持，也不得行；显然，这会引起歧义
	if len(matchedPluginNames) > 1 {
		return nil, fmt.Errorf("multiple volume plugins matched: %s", strings.Join(matchedPluginNames, ","))
	}

	return match, nil
}

// FindPluginByName fetches a plugin by name or by legacy name.  If no plugin
// is found, returns error.
// 通过名字找到卷插件
func (pm *VolumePluginMgr) FindPluginByName(name string) (VolumePlugin, error) {
	pm.mutex.RLock()
	defer pm.mutex.RUnlock()

	// Once we can get rid of legacy names we can reduce this to a map lookup.
	var match VolumePlugin
	if v, found := pm.plugins[name]; found {
		match = v
	}

	pm.refreshProbedPlugins()
	if plugin, found := pm.probedPlugins[name]; found {
		if match != nil {
			return nil, fmt.Errorf("multiple volume plugins matched: %s and %s", match.GetPluginName(), plugin.GetPluginName())
		}
		match = plugin
	}

	if match == nil {
		return nil, fmt.Errorf("no volume plugin matched name: %s", name)
	}
	return match, nil
}

// Check if probedPlugin cache update is required.
// If it is, initialize all probed plugins and replace the cache with them.
func (pm *VolumePluginMgr) refreshProbedPlugins() {
	events, err := pm.prober.Probe()

	if err != nil {
		klog.ErrorS(err, "Error dynamically probing plugins")
	}

	// because the probe function can return a list of valid plugins
	// even when an error is present we still must add the plugins
	// or they will be skipped because each event only fires once
	for _, event := range events {
		if event.Op == ProbeAddOrUpdate {
			if err := pm.initProbedPlugin(event.Plugin); err != nil {
				klog.ErrorS(err, "Error initializing dynamically probed plugin",
					"pluginName", event.Plugin.GetPluginName())
				continue
			}
			// 保存Probed卷插件
			pm.probedPlugins[event.Plugin.GetPluginName()] = event.Plugin
		} else if event.Op == ProbeRemove {
			// Plugin is not available on ProbeRemove event, only PluginName
			delete(pm.probedPlugins, event.PluginName)
		} else {
			klog.ErrorS(nil, "Unknown Operation on PluginName.",
				"pluginName", event.Plugin.GetPluginName())
		}
	}
}

// ListVolumePluginWithLimits returns plugins that have volume limits on nodes
// 获取支持VolumeLimit的卷插件
func (pm *VolumePluginMgr) ListVolumePluginWithLimits() []VolumePluginWithAttachLimits {
	pm.mutex.RLock()
	defer pm.mutex.RUnlock()

	var matchedPlugins []VolumePluginWithAttachLimits
	for _, v := range pm.plugins {
		if plugin, ok := v.(VolumePluginWithAttachLimits); ok {
			matchedPlugins = append(matchedPlugins, plugin)
		}
	}
	return matchedPlugins
}

// FindPersistentPluginBySpec looks for a persistent volume plugin that can
// support a given volume specification.  If no plugin is found, return an
// error
// 获取持久卷
func (pm *VolumePluginMgr) FindPersistentPluginBySpec(spec *Spec) (PersistentVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		return nil, fmt.Errorf("could not find volume plugin for spec: %#v", spec)
	}
	if persistentVolumePlugin, ok := volumePlugin.(PersistentVolumePlugin); ok {
		return persistentVolumePlugin, nil
	}
	return nil, fmt.Errorf("no persistent volume plugin matched")
}

// FindVolumePluginWithLimitsBySpec returns volume plugin that has a limit on how many
// of them can be attached to a node
// 获取卷的Limit
func (pm *VolumePluginMgr) FindVolumePluginWithLimitsBySpec(spec *Spec) (VolumePluginWithAttachLimits, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		return nil, fmt.Errorf("could not find volume plugin for spec : %#v", spec)
	}

	if limitedPlugin, ok := volumePlugin.(VolumePluginWithAttachLimits); ok {
		return limitedPlugin, nil
	}
	return nil, fmt.Errorf("no plugin with limits found")
}

// FindPersistentPluginByName fetches a persistent volume plugin by name.  If
// no plugin is found, returns error.
// 获取持久卷
func (pm *VolumePluginMgr) FindPersistentPluginByName(name string) (PersistentVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginByName(name)
	if err != nil {
		return nil, err
	}
	if persistentVolumePlugin, ok := volumePlugin.(PersistentVolumePlugin); ok {
		return persistentVolumePlugin, nil
	}
	return nil, fmt.Errorf("no persistent volume plugin matched")
}

// FindRecyclablePluginBySpec fetches a persistent volume plugin by name.  If
// no plugin is found, returns error.
// 获取支持循环使用的卷
func (pm *VolumePluginMgr) FindRecyclablePluginBySpec(spec *Spec) (RecyclableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		return nil, err
	}
	if recyclableVolumePlugin, ok := volumePlugin.(RecyclableVolumePlugin); ok {
		return recyclableVolumePlugin, nil
	}
	return nil, fmt.Errorf("no recyclable volume plugin matched")
}

// FindProvisionablePluginByName fetches  a persistent volume plugin by name.  If
// no plugin is found, returns error.
// 获取卷
func (pm *VolumePluginMgr) FindProvisionablePluginByName(name string) (ProvisionableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginByName(name)
	if err != nil {
		return nil, err
	}
	if provisionableVolumePlugin, ok := volumePlugin.(ProvisionableVolumePlugin); ok {
		return provisionableVolumePlugin, nil
	}
	return nil, fmt.Errorf("no provisionable volume plugin matched")
}

// FindDeletablePluginBySpec fetches a persistent volume plugin by spec.  If
// no plugin is found, returns error.
func (pm *VolumePluginMgr) FindDeletablePluginBySpec(spec *Spec) (DeletableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		return nil, err
	}
	if deletableVolumePlugin, ok := volumePlugin.(DeletableVolumePlugin); ok {
		return deletableVolumePlugin, nil
	}
	return nil, fmt.Errorf("no deletable volume plugin matched")
}

// FindDeletablePluginByName fetches a persistent volume plugin by name.  If
// no plugin is found, returns error.
func (pm *VolumePluginMgr) FindDeletablePluginByName(name string) (DeletableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginByName(name)
	if err != nil {
		return nil, err
	}
	if deletableVolumePlugin, ok := volumePlugin.(DeletableVolumePlugin); ok {
		return deletableVolumePlugin, nil
	}
	return nil, fmt.Errorf("no deletable volume plugin matched")
}

// FindCreatablePluginBySpec fetches a persistent volume plugin by name.  If
// no plugin is found, returns error.
func (pm *VolumePluginMgr) FindCreatablePluginBySpec(spec *Spec) (ProvisionableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		return nil, err
	}
	if provisionableVolumePlugin, ok := volumePlugin.(ProvisionableVolumePlugin); ok {
		return provisionableVolumePlugin, nil
	}
	return nil, fmt.Errorf("no creatable volume plugin matched")
}

// FindAttachablePluginBySpec fetches a persistent volume plugin by spec.
// Unlike the other "FindPlugin" methods, this does not return error if no
// plugin is found.  All volumes require a mounter and unmounter, but not
// every volume will have an attacher/detacher.
func (pm *VolumePluginMgr) FindAttachablePluginBySpec(spec *Spec) (AttachableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		return nil, err
	}
	if attachableVolumePlugin, ok := volumePlugin.(AttachableVolumePlugin); ok {
		if canAttach, err := attachableVolumePlugin.CanAttach(spec); err != nil {
			return nil, err
		} else if canAttach {
			return attachableVolumePlugin, nil
		}
	}
	return nil, nil
}

// FindAttachablePluginByName fetches an attachable volume plugin by name.
// Unlike the other "FindPlugin" methods, this does not return error if no
// plugin is found.  All volumes require a mounter and unmounter, but not
// every volume will have an attacher/detacher.
func (pm *VolumePluginMgr) FindAttachablePluginByName(name string) (AttachableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginByName(name)
	if err != nil {
		return nil, err
	}
	if attachablePlugin, ok := volumePlugin.(AttachableVolumePlugin); ok {
		return attachablePlugin, nil
	}
	return nil, nil
}

// FindDeviceMountablePluginBySpec fetches a persistent volume plugin by spec.
func (pm *VolumePluginMgr) FindDeviceMountablePluginBySpec(spec *Spec) (DeviceMountableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		return nil, err
	}
	if deviceMountableVolumePlugin, ok := volumePlugin.(DeviceMountableVolumePlugin); ok {
		if canMount, err := deviceMountableVolumePlugin.CanDeviceMount(spec); err != nil {
			return nil, err
		} else if canMount {
			return deviceMountableVolumePlugin, nil
		}
	}
	return nil, nil
}

// FindDeviceMountablePluginByName fetches a devicemountable volume plugin by name.
func (pm *VolumePluginMgr) FindDeviceMountablePluginByName(name string) (DeviceMountableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginByName(name)
	if err != nil {
		return nil, err
	}
	if deviceMountableVolumePlugin, ok := volumePlugin.(DeviceMountableVolumePlugin); ok {
		return deviceMountableVolumePlugin, nil
	}
	return nil, nil
}

// FindExpandablePluginBySpec fetches a persistent volume plugin by spec.
func (pm *VolumePluginMgr) FindExpandablePluginBySpec(spec *Spec) (ExpandableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		if spec.IsKubeletExpandable() {
			// for kubelet expandable volumes, return a noop plugin that
			// returns success for expand on the controller
			klog.V(4).InfoS("FindExpandablePluginBySpec -> returning noopExpandableVolumePluginInstance", "specName", spec.Name())
			return &noopExpandableVolumePluginInstance{spec}, nil
		}
		klog.V(4).InfoS("FindExpandablePluginBySpec -> err", "specName", spec.Name(), "err", err)
		return nil, err
	}

	if expandableVolumePlugin, ok := volumePlugin.(ExpandableVolumePlugin); ok {
		return expandableVolumePlugin, nil
	}
	return nil, nil
}

// FindExpandablePluginByName fetches a persistent volume plugin by name.
func (pm *VolumePluginMgr) FindExpandablePluginByName(name string) (ExpandableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginByName(name)
	if err != nil {
		return nil, err
	}

	if expandableVolumePlugin, ok := volumePlugin.(ExpandableVolumePlugin); ok {
		return expandableVolumePlugin, nil
	}
	return nil, nil
}

// FindMapperPluginBySpec fetches a block volume plugin by spec.
func (pm *VolumePluginMgr) FindMapperPluginBySpec(spec *Spec) (BlockVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		return nil, err
	}

	if blockVolumePlugin, ok := volumePlugin.(BlockVolumePlugin); ok {
		return blockVolumePlugin, nil
	}
	return nil, nil
}

// FindMapperPluginByName fetches a block volume plugin by name.
func (pm *VolumePluginMgr) FindMapperPluginByName(name string) (BlockVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginByName(name)
	if err != nil {
		return nil, err
	}

	if blockVolumePlugin, ok := volumePlugin.(BlockVolumePlugin); ok {
		return blockVolumePlugin, nil
	}
	return nil, nil
}

// FindNodeExpandablePluginBySpec fetches a persistent volume plugin by spec
func (pm *VolumePluginMgr) FindNodeExpandablePluginBySpec(spec *Spec) (NodeExpandableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginBySpec(spec)
	if err != nil {
		return nil, err
	}
	if fsResizablePlugin, ok := volumePlugin.(NodeExpandableVolumePlugin); ok {
		return fsResizablePlugin, nil
	}
	return nil, nil
}

// FindNodeExpandablePluginByName fetches a persistent volume plugin by name
func (pm *VolumePluginMgr) FindNodeExpandablePluginByName(name string) (NodeExpandableVolumePlugin, error) {
	volumePlugin, err := pm.FindPluginByName(name)
	if err != nil {
		return nil, err
	}

	if fsResizablePlugin, ok := volumePlugin.(NodeExpandableVolumePlugin); ok {
		return fsResizablePlugin, nil
	}

	return nil, nil
}

func (pm *VolumePluginMgr) Run(stopCh <-chan struct{}) {
	kletHost, ok := pm.Host.(KubeletVolumeHost)
	if ok {
		// start informer for CSIDriver
		informerFactory := kletHost.GetInformerFactory()
		informerFactory.Start(stopCh)
		informerFactory.WaitForCacheSync(stopCh)
	}
}

// NewPersistentVolumeRecyclerPodTemplate creates a template for a recycler
// pod.  By default, a recycler pod simply runs "rm -rf" on a volume and tests
// for emptiness.  Most attributes of the template will be correct for most
// plugin implementations.  The following attributes can be overridden per
// plugin via configuration:
//
//  1. pod.Spec.Volumes[0].VolumeSource must be overridden.  Recycler
//     implementations without a valid VolumeSource will fail.
//  2. pod.GenerateName helps distinguish recycler pods by name.  Recommended.
//     Default is "pv-recycler-".
//  3. pod.Spec.ActiveDeadlineSeconds gives the recycler pod a maximum timeout
//     before failing.  Recommended.  Default is 60 seconds.
//
// See HostPath and NFS for working recycler examples
func NewPersistentVolumeRecyclerPodTemplate() *v1.Pod {
	timeout := int64(60)
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "pv-recycler-",
			Namespace:    metav1.NamespaceDefault,
		},
		Spec: v1.PodSpec{
			ActiveDeadlineSeconds: &timeout,
			RestartPolicy:         v1.RestartPolicyNever,
			Volumes: []v1.Volume{
				{
					Name: "vol",
					// IMPORTANT!  All plugins using this template MUST
					// override pod.Spec.Volumes[0].VolumeSource Recycler
					// implementations without a valid VolumeSource will fail.
					VolumeSource: v1.VolumeSource{},
				},
			},
			Containers: []v1.Container{
				{
					Name:    "pv-recycler",
					Image:   "registry.k8s.io/debian-base:v2.0.0",
					Command: []string{"/bin/sh"},
					Args:    []string{"-c", "test -e /scrub && rm -rf /scrub/..?* /scrub/.[!.]* /scrub/*  && test -z \"$(ls -A /scrub)\" || exit 1"},
					VolumeMounts: []v1.VolumeMount{
						{
							Name:      "vol",
							MountPath: "/scrub",
						},
					},
				},
			},
		},
	}
	return pod
}

// ValidateRecyclerPodTemplate
// Check validity of recycle pod template
// List of checks:
// - at least one volume is defined in the recycle pod template
// If successful, returns nil
// if unsuccessful, returns an error.
func ValidateRecyclerPodTemplate(pod *v1.Pod) error {
	if len(pod.Spec.Volumes) < 1 {
		return fmt.Errorf("does not contain any volume(s)")
	}
	return nil
}

type dummyPluginProber struct{}

func (*dummyPluginProber) Init() error                  { return nil }
func (*dummyPluginProber) Probe() ([]ProbeEvent, error) { return nil, nil }
