/*
Copyright 2017 The Kubernetes Authors.

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

package devicemanager

import (
	"sync"

	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/util/sets"
	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
	"k8s.io/kubernetes/pkg/kubelet/cm/devicemanager/checkpoint"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
)

// 设备分配信息
type deviceAllocateInfo struct {
	// deviceIds contains device Ids allocated to this container for the given resourceName.
	deviceIds checkpoint.DevicesPerNUMA
	// allocResp contains cached rpc AllocateResponse.
	allocResp *pluginapi.ContainerAllocateResponse
}

type resourceAllocateInfo map[string]deviceAllocateInfo // Keyed by resourceName.
type containerDevices map[string]resourceAllocateInfo   // Keyed by containerName.
type podDevices struct {
	sync.RWMutex
	devs map[string]containerDevices // Keyed by podUID.
}

// NewPodDevices is a function that returns object of podDevices type with its own guard
// RWMutex and a map where key is a pod UID and value contains
// container devices information of type containerDevices.
func newPodDevices() *podDevices {
	return &podDevices{devs: make(map[string]containerDevices)}
}

func (pdev *podDevices) pods() sets.String {
	pdev.RLock()
	defer pdev.RUnlock()
	ret := sets.NewString()
	for k := range pdev.devs {
		ret.Insert(k)
	}
	return ret
}

func (pdev *podDevices) size() int {
	pdev.RLock()
	defer pdev.RUnlock()
	return len(pdev.devs)
}

func (pdev *podDevices) hasPod(podUID string) bool {
	pdev.RLock()
	defer pdev.RUnlock()
	_, podExists := pdev.devs[podUID]
	return podExists
}

func (pdev *podDevices) insert(
	podUID, // 当前pod
	contName, // pod中的容器
	resource string, // 容器申请的资源名
	devices checkpoint.DevicesPerNUMA, // 分配的设备（也就是资源）
	resp *pluginapi.ContainerAllocateResponse, // kubelet设备插件给的响应
) {
	pdev.Lock()
	defer pdev.Unlock()
	if _, podExists := pdev.devs[podUID]; !podExists {
		pdev.devs[podUID] = make(containerDevices)
	}
	if _, contExists := pdev.devs[podUID][contName]; !contExists {
		pdev.devs[podUID][contName] = make(resourceAllocateInfo)
	}
	pdev.devs[podUID][contName][resource] = deviceAllocateInfo{
		deviceIds: devices,
		allocResp: resp,
	}
}

func (pdev *podDevices) delete(pods []string) {
	pdev.Lock()
	defer pdev.Unlock()
	for _, uid := range pods {
		delete(pdev.devs, uid)
	}
}

// Returns list of device Ids allocated to the given pod for the given resource.
// Returns nil if we don't have cached state for the given <podUID, resource>.
func (pdev *podDevices) podDevices(podUID, resource string) sets.String {
	pdev.RLock()
	defer pdev.RUnlock()

	ret := sets.NewString()
	for contName := range pdev.devs[podUID] {
		ret = ret.Union(pdev.containerDevices(podUID, contName, resource))
	}
	return ret
}

// Returns list of device Ids allocated to the given container for the given resource.
// Returns nil if we don't have cached state for the given <podUID, contName, resource>.
// 返回当前容器已经分配的设备
func (pdev *podDevices) containerDevices(podUID, contName, resource string) sets.String {
	pdev.RLock()
	defer pdev.RUnlock()
	if _, podExists := pdev.devs[podUID]; !podExists {
		return nil
	}
	if _, contExists := pdev.devs[podUID][contName]; !contExists {
		return nil
	}
	devs, resourceExists := pdev.devs[podUID][contName][resource]
	if !resourceExists {
		return nil
	}
	return devs.deviceIds.Devices()
}

// Populates allocatedResources with the device resources allocated to the specified <podUID, contName>.
func (pdev *podDevices) addContainerAllocatedResources(podUID, contName string, allocatedResources map[string]sets.String) {
	pdev.RLock()
	defer pdev.RUnlock()
	containers, exists := pdev.devs[podUID]
	if !exists {
		return
	}
	resources, exists := containers[contName]
	if !exists {
		return
	}
	for resource, devices := range resources {
		allocatedResources[resource] = allocatedResources[resource].Union(devices.deviceIds.Devices())
	}
}

// Removes the device resources allocated to the specified <podUID, contName> from allocatedResources.
func (pdev *podDevices) removeContainerAllocatedResources(podUID, contName string, allocatedResources map[string]sets.String) {
	pdev.RLock()
	defer pdev.RUnlock()
	containers, exists := pdev.devs[podUID]
	if !exists {
		return
	}
	resources, exists := containers[contName]
	if !exists {
		return
	}
	for resource, devices := range resources {
		allocatedResources[resource] = allocatedResources[resource].Difference(devices.deviceIds.Devices())
	}
}

// Returns all of devices allocated to the pods being tracked, keyed by resourceName.
// 返回当前节点已经分配的资源
func (pdev *podDevices) devices() map[string]sets.String {
	ret := make(map[string]sets.String)
	pdev.RLock()
	defer pdev.RUnlock()
	for _, containerDevices := range pdev.devs {
		for _, resources := range containerDevices {
			for resource, devices := range resources {
				if _, exists := ret[resource]; !exists {
					ret[resource] = sets.NewString()
				}
				if devices.allocResp != nil {
					ret[resource] = ret[resource].Union(devices.deviceIds.Devices())
				}
			}
		}
	}
	return ret
}

// Turns podDevices to checkpointData.
func (pdev *podDevices) toCheckpointData() []checkpoint.PodDevicesEntry {
	var data []checkpoint.PodDevicesEntry
	pdev.RLock()
	defer pdev.RUnlock()
	for podUID, containerDevices := range pdev.devs {
		for conName, resources := range containerDevices {
			for resource, devices := range resources {
				if devices.allocResp == nil {
					klog.ErrorS(nil, "Can't marshal allocResp, allocation response is missing", "podUID", podUID, "containerName", conName, "resourceName", resource)
					continue
				}

				allocResp, err := devices.allocResp.Marshal()
				if err != nil {
					klog.ErrorS(err, "Can't marshal allocResp", "podUID", podUID, "containerName", conName, "resourceName", resource)
					continue
				}
				data = append(data, checkpoint.PodDevicesEntry{
					PodUID:        podUID,
					ContainerName: conName,
					ResourceName:  resource,
					DeviceIDs:     devices.deviceIds,
					AllocResp:     allocResp})
			}
		}
	}
	return data
}

// Populates podDevices from the passed in checkpointData.
// 缓存各个Pod之前分配的设备信息
func (pdev *podDevices) fromCheckpointData(data []checkpoint.PodDevicesEntry) {
	// 遍历每一个pod分配的device
	for _, entry := range data {
		klog.V(2).InfoS("Get checkpoint entry",
			"podUID", entry.PodUID, "containerName", entry.ContainerName,
			"resourceName", entry.ResourceName, "deviceIDs", entry.DeviceIDs, "allocated", entry.AllocResp)
		allocResp := &pluginapi.ContainerAllocateResponse{}
		// 反序列化
		err := allocResp.Unmarshal(entry.AllocResp)
		if err != nil {
			klog.ErrorS(err, "Can't unmarshal allocResp", "podUID", entry.PodUID, "containerName", entry.ContainerName, "resourceName", entry.ResourceName)
			continue
		}
		// 缓存当前Pod分配的设备
		pdev.insert(entry.PodUID, entry.ContainerName, entry.ResourceName, entry.DeviceIDs, allocResp)
	}
}

// Returns combined container runtime settings to consume the container's allocated devices.
func (pdev *podDevices) deviceRunContainerOptions(podUID, contName string) *DeviceRunContainerOptions {
	pdev.RLock()
	defer pdev.RUnlock()

	containers, exists := pdev.devs[podUID]
	if !exists {
		return nil
	}
	resources, exists := containers[contName]
	if !exists {
		return nil
	}
	opts := &DeviceRunContainerOptions{}
	// Maps to detect duplicate settings.
	devsMap := make(map[string]string)
	mountsMap := make(map[string]string)
	envsMap := make(map[string]string)
	annotationsMap := make(map[string]string)
	// Loops through AllocationResponses of all cached device resources.
	for _, devices := range resources {
		resp := devices.allocResp
		// Each Allocate response has the following artifacts.
		// Environment variables
		// Mount points
		// Device files
		// Container annotations
		// These artifacts are per resource per container.
		// Updates RunContainerOptions.Envs.
		for k, v := range resp.Envs {
			if e, ok := envsMap[k]; ok {
				klog.V(4).InfoS("Skip existing env", "envKey", k, "envValue", v)
				if e != v {
					klog.ErrorS(nil, "Environment variable has conflicting setting", "envKey", k, "expected", v, "got", e)
				}
				continue
			}
			klog.V(4).InfoS("Add env", "envKey", k, "envValue", v)
			envsMap[k] = v
			opts.Envs = append(opts.Envs, kubecontainer.EnvVar{Name: k, Value: v})
		}

		// Updates RunContainerOptions.Devices.
		for _, dev := range resp.Devices {
			if d, ok := devsMap[dev.ContainerPath]; ok {
				klog.V(4).InfoS("Skip existing device", "containerPath", dev.ContainerPath, "hostPath", dev.HostPath)
				if d != dev.HostPath {
					klog.ErrorS(nil, "Container device has conflicting mapping host devices",
						"containerPath", dev.ContainerPath, "got", d, "expected", dev.HostPath)
				}
				continue
			}
			klog.V(4).InfoS("Add device", "containerPath", dev.ContainerPath, "hostPath", dev.HostPath)
			devsMap[dev.ContainerPath] = dev.HostPath
			opts.Devices = append(opts.Devices, kubecontainer.DeviceInfo{
				PathOnHost:      dev.HostPath,
				PathInContainer: dev.ContainerPath,
				Permissions:     dev.Permissions,
			})
		}

		// Updates RunContainerOptions.Mounts.
		for _, mount := range resp.Mounts {
			if m, ok := mountsMap[mount.ContainerPath]; ok {
				klog.V(4).InfoS("Skip existing mount", "containerPath", mount.ContainerPath, "hostPath", mount.HostPath)
				if m != mount.HostPath {
					klog.ErrorS(nil, "Container mount has conflicting mapping host mounts",
						"containerPath", mount.ContainerPath, "conflictingPath", m, "hostPath", mount.HostPath)
				}
				continue
			}
			klog.V(4).InfoS("Add mount", "containerPath", mount.ContainerPath, "hostPath", mount.HostPath)
			mountsMap[mount.ContainerPath] = mount.HostPath
			opts.Mounts = append(opts.Mounts, kubecontainer.Mount{
				Name:          mount.ContainerPath,
				ContainerPath: mount.ContainerPath,
				HostPath:      mount.HostPath,
				ReadOnly:      mount.ReadOnly,
				// TODO: This may need to be part of Device plugin API.
				SELinuxRelabel: false,
			})
		}

		// Updates for Annotations
		for k, v := range resp.Annotations {
			if e, ok := annotationsMap[k]; ok {
				klog.V(4).InfoS("Skip existing annotation", "annotationKey", k, "annotationValue", v)
				if e != v {
					klog.ErrorS(nil, "Annotation has conflicting setting", "annotationKey", k, "expected", e, "got", v)
				}
				continue
			}
			klog.V(4).InfoS("Add annotation", "annotationKey", k, "annotationValue", v)
			annotationsMap[k] = v
			opts.Annotations = append(opts.Annotations, kubecontainer.Annotation{Name: k, Value: v})
		}
	}
	return opts
}

// getContainerDevices returns the devices assigned to the provided container for all ResourceNames
func (pdev *podDevices) getContainerDevices(podUID, contName string) ResourceDeviceInstances {
	pdev.RLock()
	defer pdev.RUnlock()

	if _, podExists := pdev.devs[podUID]; !podExists {
		return nil
	}
	if _, contExists := pdev.devs[podUID][contName]; !contExists {
		return nil
	}
	resDev := NewResourceDeviceInstances()
	for resource, allocateInfo := range pdev.devs[podUID][contName] {
		if len(allocateInfo.deviceIds) == 0 {
			continue
		}
		devicePluginMap := make(map[string]pluginapi.Device)
		for numaid, devlist := range allocateInfo.deviceIds {
			for _, devID := range devlist {
				var topology *pluginapi.TopologyInfo
				if numaid != nodeWithoutTopology {
					NUMANodes := []*pluginapi.NUMANode{{ID: numaid}}
					if pDev, ok := devicePluginMap[devID]; ok && pDev.Topology != nil {
						if nodes := pDev.Topology.GetNodes(); nodes != nil {
							NUMANodes = append(NUMANodes, nodes...)
						}
					}

					// ID and Healthy are not relevant here.
					topology = &pluginapi.TopologyInfo{Nodes: NUMANodes}
				}
				devicePluginMap[devID] = pluginapi.Device{
					Topology: topology,
				}
			}
		}
		resDev[resource] = devicePluginMap
	}
	return resDev
}

// DeviceInstances is a mapping device name -> plugin device data
// key为设备名
type DeviceInstances map[string]pluginapi.Device

// ResourceDeviceInstances is a mapping resource name -> DeviceInstances
// key为资源名
type ResourceDeviceInstances map[string]DeviceInstances

// NewResourceDeviceInstances returns a new ResourceDeviceInstances
func NewResourceDeviceInstances() ResourceDeviceInstances {
	return make(ResourceDeviceInstances)
}

// Clone returns a clone of ResourceDeviceInstances
func (rdev ResourceDeviceInstances) Clone() ResourceDeviceInstances {
	clone := NewResourceDeviceInstances()
	for resourceName, resourceDevs := range rdev {
		clone[resourceName] = make(map[string]pluginapi.Device)
		for devID, dev := range resourceDevs {
			clone[resourceName][devID] = dev
		}
	}
	return clone
}

// Filter takes a condition set expressed as map[string]sets.String and returns a new
// ResourceDeviceInstances with only the devices matching the condition set.
func (rdev ResourceDeviceInstances) Filter(cond map[string]sets.String) ResourceDeviceInstances {
	filtered := NewResourceDeviceInstances()
	for resourceName, filterIDs := range cond {
		if _, exists := rdev[resourceName]; !exists {
			continue
		}
		filtered[resourceName] = DeviceInstances{}
		for instanceID, instance := range rdev[resourceName] {
			if filterIDs.Has(instanceID) {
				filtered[resourceName][instanceID] = instance
			}
		}
	}
	return filtered
}
