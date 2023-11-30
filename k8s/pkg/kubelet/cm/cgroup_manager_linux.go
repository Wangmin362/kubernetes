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

package cm

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	libcontainercgroups "github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/opencontainers/runc/libcontainer/cgroups/fscommon"
	"github.com/opencontainers/runc/libcontainer/cgroups/manager"
	cgroupsystemd "github.com/opencontainers/runc/libcontainer/cgroups/systemd"
	libcontainerconfigs "github.com/opencontainers/runc/libcontainer/configs"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
	v1helper "k8s.io/kubernetes/pkg/apis/core/v1/helper"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	cmutil "k8s.io/kubernetes/pkg/kubelet/cm/util"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
)

const (
	// systemdSuffix is the cgroup name suffix for systemd
	systemdSuffix string = ".slice"
	// MemoryMin is memory.min for cgroup v2
	MemoryMin string = "memory.min"
	// MemoryHigh is memory.high for cgroup v2
	MemoryHigh         string = "memory.high"
	Cgroup2MaxCpuLimit string = "max"
)

var RootCgroupName = CgroupName([]string{})

// NewCgroupName composes a new cgroup name.
// Use RootCgroupName as base to start at the root.
// This function does some basic check for invalid characters at the name.
func NewCgroupName(base CgroupName, components ...string) CgroupName {
	for _, component := range components {
		// Forbit using "_" in internal names. When remapping internal
		// names to systemd cgroup driver, we want to remap "-" => "_",
		// so we forbid "_" so that we can always reverse the mapping.
		if strings.Contains(component, "/") || strings.Contains(component, "_") {
			panic(fmt.Errorf("invalid character in component [%q] of CgroupName", component))
		}
	}
	return CgroupName(append(append([]string{}, base...), components...))
}

func escapeSystemdCgroupName(part string) string {
	return strings.Replace(part, "-", "_", -1)
}

func unescapeSystemdCgroupName(part string) string {
	return strings.Replace(part, "_", "-", -1)
}

// ToSystemd converts the internal cgroup name to a systemd name.
// For example, the name {"kubepods", "burstable", "pod1234-abcd-5678-efgh"} becomes
// "/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234_abcd_5678_efgh.slice"
// This function always expands the systemd name into the cgroupfs form. If only
// the last part is needed, use path.Base(...) on it to discard the rest.
// 如果cgroupName = {"kubepods", "burstable", "pod1234-abcd-5678-efgh"}, 那么此函数的返回值为：
// /kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234_abcd_5678_efgh.slice
func (cgroupName CgroupName) ToSystemd() string {
	if len(cgroupName) == 0 || (len(cgroupName) == 1 && cgroupName[0] == "") {
		return "/"
	}
	var newparts []string
	for _, part := range cgroupName {
		// 把所有的中划线转为下划线
		part = escapeSystemdCgroupName(part)
		newparts = append(newparts, part)
	}

	// 如果cgroupName = {"kubepods", "burstable", "pod1234-abcd-5678-efgh"}
	// 那么strings.Join(newparts, "-") = kubepods-burstable-pod1234_abcd_5678_efgh
	// 那么strings.Join(newparts, "-") + systemdSuffix = kubepods-burstable-pod1234_abcd_5678_efgh.slice
	// 那么返回值为：/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234_abcd_5678_efgh.slice
	result, err := cgroupsystemd.ExpandSlice(strings.Join(newparts, "-") + systemdSuffix)
	if err != nil {
		// Should never happen...
		panic(fmt.Errorf("error converting cgroup name [%v] to systemd format: %v", cgroupName, err))
	}
	return result
}

// ParseSystemdToCgroupName
// 如果name为/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234_abcd_5678_efgh.slice
// 那么返回值为：cgroupName = {"kubepods", "burstable", "pod1234-abcd-5678-efgh"}
func ParseSystemdToCgroupName(name string) CgroupName {
	// 返回name路径的最后一个元素
	driverName := path.Base(name)
	// 去除.slice后缀
	driverName = strings.TrimSuffix(driverName, systemdSuffix)
	parts := strings.Split(driverName, "-")
	var result []string
	for _, part := range parts {
		result = append(result, unescapeSystemdCgroupName(part))
	}
	return CgroupName(result)
}

// ToCgroupfs
// 如果cgroupName = {"kubepods", "burstable", "pod1234-abcd-5678-efgh"}, 那么此函数的返回值为：
// /kubepods/burstable/pod1234-abcd-5678-efgh
func (cgroupName CgroupName) ToCgroupfs() string {
	return "/" + path.Join(cgroupName...)
}

func ParseCgroupfsToCgroupName(name string) CgroupName {
	components := strings.Split(strings.TrimPrefix(name, "/"), "/")
	if len(components) == 1 && components[0] == "" {
		components = []string{}
	}
	return CgroupName(components)
}

func IsSystemdStyleName(name string) bool {
	return strings.HasSuffix(name, systemdSuffix)
}

// CgroupSubsystems holds information about the mounted cgroup subsystems
// TODO 这里说的cgroup子系统，似乎就是不同资源的控制
type CgroupSubsystems struct {
	// Cgroup subsystem mounts.
	// e.g.: "/sys/fs/cgroup/cpu" -> ["cpu", "cpuacct"]

	Mounts []libcontainercgroups.Mount
	// Cgroup subsystem to their mount location.
	// e.g.: "cpu" -> "/sys/fs/cgroup/cpu"
	// 每种资源的挂载点
	MountPoints map[string]string
}

// cgroupManagerImpl implements the CgroupManager interface.
// Its a stateless object which can be used to
// update,create or delete any number of cgroups
// It relies on runc/libcontainer cgroup managers.
type cgroupManagerImpl struct {
	// subsystems holds information about all the
	// mounted cgroup subsystems on the node
	subsystems *CgroupSubsystems

	// useSystemd tells if systemd cgroup manager should be used.
	// TODO 这玩意干嘛的？
	useSystemd bool
}

// Make sure that cgroupManagerImpl implements the CgroupManager interface
var _ CgroupManager = &cgroupManagerImpl{}

// NewCgroupManager is a factory method that returns a CgroupManager
// TODO cgroupDriver是什么意思？
func NewCgroupManager(cs *CgroupSubsystems, cgroupDriver string) CgroupManager {
	return &cgroupManagerImpl{
		subsystems: cs,
		useSystemd: cgroupDriver == "systemd",
	}
}

// Name converts the cgroup to the driver specific value in cgroupfs form.
// This always returns a valid cgroupfs path even when systemd driver is in use!
// 以cGroupName = {"kubepods", "burstable", "pod1234-abcd-5678-efgh"}为例，如果cgroupDriver指定为了systemd，那么返回值为
// /kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234_abcd_5678_efgh.slice，如果没有指定systemd，那么
// 返回值为/kubepods/burstable/pod1234-abcd-5678-efgh
func (m *cgroupManagerImpl) Name(name CgroupName) string {
	if m.useSystemd {
		// 如果cgroupName = {"kubepods", "burstable", "pod1234-abcd-5678-efgh"}, 那么此函数的返回值为：
		// /kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234_abcd_5678_efgh.slice
		return name.ToSystemd()
	}

	// 如果cgroupName = {"kubepods", "burstable", "pod1234-abcd-5678-efgh"}, 那么此函数的返回值为：
	// /kubepods/burstable/pod1234-abcd-5678-efgh
	return name.ToCgroupfs()
}

// CgroupName converts the literal cgroupfs name on the host to an internal identifier.
// 把一个cgroup路径合法的CgroupName
func (m *cgroupManagerImpl) CgroupName(name string) CgroupName {
	if m.useSystemd {
		// 如果name为/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234_abcd_5678_efgh.slice
		// 那么返回值为：cgroupName = {"kubepods", "burstable", "pod1234-abcd-5678-efgh"}
		return ParseSystemdToCgroupName(name)
	}

	// 如果name = /kubepods/burstable/pod1234-abcd-5678-efgh
	// 那么返回值为：cgroupName = {"kubepods", "burstable", "pod1234-abcd-5678-efgh"}
	return ParseCgroupfsToCgroupName(name)
}

// buildCgroupPaths builds a path to each cgroup subsystem for the specified name.
func (m *cgroupManagerImpl) buildCgroupPaths(name CgroupName) map[string]string {
	// 把CgroupName解析为路径
	cgroupFsAdaptedName := m.Name(name)
	cgroupPaths := make(map[string]string, len(m.subsystems.MountPoints))
	// 1、key为不同的cgroup子系统，譬如cpu, memeory, hugepage, cpuacct等等
	// 2、这里主要是在为不同的资源拼接出cgroup资源限制路径
	for key, val := range m.subsystems.MountPoints {
		cgroupPaths[key] = path.Join(val, cgroupFsAdaptedName)
	}
	return cgroupPaths
}

// buildCgroupUnifiedPath builds a path to the specified name.
func (m *cgroupManagerImpl) buildCgroupUnifiedPath(name CgroupName) string {
	cgroupFsAdaptedName := m.Name(name)
	return path.Join(cmutil.CgroupRoot, cgroupFsAdaptedName)
}

// libctCgroupConfig converts CgroupConfig to libcontainer's Cgroup config.
func (m *cgroupManagerImpl) libctCgroupConfig(in *CgroupConfig, needResources bool) *libcontainerconfigs.Cgroup {
	config := &libcontainerconfigs.Cgroup{
		Systemd: m.useSystemd,
	}
	if needResources {
		config.Resources = m.toResources(in.ResourceParameters)
	} else {
		config.Resources = &libcontainerconfigs.Resources{}
	}

	if !config.Systemd {
		// For fs cgroup manager, we can either set Path or Name and Parent.
		// Setting Path is easier.
		config.Path = in.Name.ToCgroupfs()

		return config
	}

	// For systemd, we have to set Name and Parent, as they are needed to talk to systemd.
	// Setting Path is optional as it can be deduced from Name and Parent.

	// TODO(filbranden): This logic belongs in libcontainer/cgroup/systemd instead.
	// It should take a libcontainerconfigs.Cgroup.Path field (rather than Name and Parent)
	// and split it appropriately, using essentially the logic below.
	// This was done for cgroupfs in opencontainers/runc#497 but a counterpart
	// for systemd was never introduced.
	dir, base := path.Split(in.Name.ToSystemd())
	if dir == "/" {
		dir = "-.slice"
	} else {
		dir = path.Base(dir)
	}
	config.Parent = dir
	config.Name = base

	return config
}

// Validate checks if all subsystem cgroups already exist
// 1、譬如CgroupName = {"kubepods", "burstable", "pod1234-abcd-5678-efgh"}
// 2、用于校验当前cgroup是否支持限制某些资源
func (m *cgroupManagerImpl) Validate(name CgroupName) error {
	// 判断是否是cgroup v2模式
	if libcontainercgroups.IsCgroup2UnifiedMode() {
		// 如果使用了systemd管理，那么cgroupPath = /sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234_abcd_5678_efgh.slice
		// 如果没有使用systemd管理，那么cgroupPath = /sys/fs/cgroup/kubepods/burstable/pod1234-abcd-5678-efgh
		cgroupPath := m.buildCgroupUnifiedPath(name)
		// 获取支持的资源子系统，一般来说对于cgroup v2版本来说，cpu, cpuset, memeory, hugetlb, pids都支持
		neededControllers := getSupportedUnifiedControllers()
		// 查看/sys/fs/cgroup/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod1234_abcd_5678_efgh.slice/cgroup.controllers
		// 文件中支持的资源子系统
		enabledControllers, err := readUnifiedControllers(cgroupPath)
		if err != nil {
			return fmt.Errorf("could not read controllers for cgroup %q: %w", name, err)
		}
		// 获取当前pod需要的子系统，但是当前cgroup不支持的资源子系统
		difference := neededControllers.Difference(enabledControllers)
		// 但凡长度大于零，就表明至少有一种以上的子系统不支持
		if difference.Len() > 0 {
			return fmt.Errorf("cgroup %q has some missing controllers: %v", name, strings.Join(difference.List(), ", "))
		}
		return nil // valid V2 cgroup
	}

	// 说明当前使用的是cgroup v1

	// Get map of all cgroup paths on the system for the particular cgroup
	// 获取每种资源的cgroup路径
	cgroupPaths := m.buildCgroupPaths(name)

	// the presence of alternative control groups not known to runc confuses
	// the kubelet existence checks.
	// ideally, we would have a mechanism in runc to support Exists() logic
	// scoped to the set control groups it understands.  this is being discussed
	// in https://github.com/opencontainers/runc/issues/1440
	// once resolved, we can remove this code.
	allowlistControllers := sets.NewString("cpu", "cpuacct", "cpuset", "memory", "systemd", "pids")

	if _, ok := m.subsystems.MountPoints["hugetlb"]; ok {
		allowlistControllers.Insert("hugetlb")
	}
	var missingPaths []string
	// If even one cgroup path doesn't exist, then the cgroup doesn't exist.
	for controller, path := range cgroupPaths {
		// ignore mounts we don't care about
		if !allowlistControllers.Has(controller) {
			continue
		}
		if !libcontainercgroups.PathExists(path) {
			missingPaths = append(missingPaths, path)
		}
	}

	if len(missingPaths) > 0 {
		return fmt.Errorf("cgroup %q has some missing paths: %v", name, strings.Join(missingPaths, ", "))
	}

	return nil // valid V1 cgroup
}

// Exists checks if all subsystem cgroups already exist
// 判断cgroup子系统是否存在
func (m *cgroupManagerImpl) Exists(name CgroupName) bool {
	return m.Validate(name) == nil
}

// Destroy destroys the specified cgroup
func (m *cgroupManagerImpl) Destroy(cgroupConfig *CgroupConfig) error {
	start := time.Now()
	defer func() {
		metrics.CgroupManagerDuration.WithLabelValues("destroy").Observe(metrics.SinceInSeconds(start))
	}()

	libcontainerCgroupConfig := m.libctCgroupConfig(cgroupConfig, false)
	manager, err := manager.New(libcontainerCgroupConfig)
	if err != nil {
		return err
	}

	// Delete cgroups using libcontainers Managers Destroy() method
	if err = manager.Destroy(); err != nil {
		return fmt.Errorf("unable to destroy cgroup paths for cgroup %v : %v", cgroupConfig.Name, err)
	}

	return nil
}

// getCPUWeight converts from the range [2, 262144] to [1, 10000]
func getCPUWeight(cpuShares *uint64) uint64 {
	if cpuShares == nil {
		return 0
	}
	if *cpuShares >= 262144 {
		return 10000
	}
	return 1 + ((*cpuShares-2)*9999)/262142
}

// readUnifiedControllers reads the controllers available at the specified cgroup
// 返回当前支持哪些资源的子系统控制，譬如返回值为：cpuset cpu io memory hugetlb pids rdma misc
func readUnifiedControllers(path string) (sets.String, error) {
	// 文件路径为：/sys/fs/cgroup/cgroup.controllers
	// 目前，从我自己的搭建的1.27.2版本的虚拟机读取出来的数据为：cpuset cpu io memory hugetlb pids rdma misc
	controllersFileContent, err := os.ReadFile(filepath.Join(path, "cgroup.controllers"))
	if err != nil {
		return nil, err
	}
	controllers := strings.Fields(string(controllersFileContent))
	return sets.NewString(controllers...), nil
}

var (
	availableRootControllersOnce sync.Once
	availableRootControllers     sets.String
)

// getSupportedUnifiedControllers returns a set of supported controllers when running on cgroup v2
// 获取支持的资源子系统，一般来说对于cgroup v2版本来说，cpu, cpuset, memeory, hugetlb, pids都支持
func getSupportedUnifiedControllers() sets.String {
	// This is the set of controllers used by the Kubelet
	supportedControllers := sets.NewString("cpu", "cpuset", "memory", "hugetlb", "pids")
	// Memoize the set of controllers that are present in the root cgroup
	availableRootControllersOnce.Do(func() {
		var err error
		// 返回当前支持哪些资源的子系统控制，譬如返回值为：cpuset cpu io memory hugetlb pids rdma misc
		availableRootControllers, err = readUnifiedControllers(cmutil.CgroupRoot)
		if err != nil {
			panic(fmt.Errorf("cannot read cgroup controllers at %s", cmutil.CgroupRoot))
		}
	})
	// Return the set of controllers that are supported both by the Kubelet and by the kernel
	return supportedControllers.Intersection(availableRootControllers)
}

func (m *cgroupManagerImpl) toResources(resourceConfig *ResourceConfig) *libcontainerconfigs.Resources {
	resources := &libcontainerconfigs.Resources{
		SkipDevices:     true,
		SkipFreezeOnSet: true,
	}
	if resourceConfig == nil {
		return resources
	}
	if resourceConfig.Memory != nil {
		resources.Memory = *resourceConfig.Memory
	}
	if resourceConfig.CPUShares != nil {
		if libcontainercgroups.IsCgroup2UnifiedMode() {
			resources.CpuWeight = getCPUWeight(resourceConfig.CPUShares)
		} else {
			resources.CpuShares = *resourceConfig.CPUShares
		}
	}
	if resourceConfig.CPUQuota != nil {
		resources.CpuQuota = *resourceConfig.CPUQuota
	}
	if resourceConfig.CPUPeriod != nil {
		resources.CpuPeriod = *resourceConfig.CPUPeriod
	}
	if resourceConfig.PidsLimit != nil {
		resources.PidsLimit = *resourceConfig.PidsLimit
	}

	m.maybeSetHugetlb(resourceConfig, resources)

	// Ideally unified is used for all the resources when running on cgroup v2.
	// It doesn't make difference for the memory.max limit, but for e.g. the cpu controller
	// you can specify the correct setting without relying on the conversions performed by the OCI runtime.
	if resourceConfig.Unified != nil && libcontainercgroups.IsCgroup2UnifiedMode() {
		resources.Unified = make(map[string]string)
		for k, v := range resourceConfig.Unified {
			resources.Unified[k] = v
		}
	}
	return resources
}

func (m *cgroupManagerImpl) maybeSetHugetlb(resourceConfig *ResourceConfig, resources *libcontainerconfigs.Resources) {
	// Check if hugetlb is supported.
	if libcontainercgroups.IsCgroup2UnifiedMode() {
		if !getSupportedUnifiedControllers().Has("hugetlb") {
			klog.V(6).InfoS("Optional subsystem not supported: hugetlb")
			return
		}
	} else if _, ok := m.subsystems.MountPoints["hugetlb"]; !ok {
		klog.V(6).InfoS("Optional subsystem not supported: hugetlb")
		return
	}

	// For each page size enumerated, set that value.
	pageSizes := sets.NewString()
	for pageSize, limit := range resourceConfig.HugePageLimit {
		sizeString, err := v1helper.HugePageUnitSizeFromByteSize(pageSize)
		if err != nil {
			klog.InfoS("Invalid pageSize", "err", err)
			continue
		}
		resources.HugetlbLimit = append(resources.HugetlbLimit, &libcontainerconfigs.HugepageLimit{
			Pagesize: sizeString,
			Limit:    uint64(limit),
		})
		pageSizes.Insert(sizeString)
	}
	// for each page size omitted, limit to 0
	for _, pageSize := range libcontainercgroups.HugePageSizes() {
		if pageSizes.Has(pageSize) {
			continue
		}
		resources.HugetlbLimit = append(resources.HugetlbLimit, &libcontainerconfigs.HugepageLimit{
			Pagesize: pageSize,
			Limit:    uint64(0),
		})
	}
}

// Update updates the cgroup with the specified Cgroup Configuration
func (m *cgroupManagerImpl) Update(cgroupConfig *CgroupConfig) error {
	start := time.Now()
	defer func() {
		metrics.CgroupManagerDuration.WithLabelValues("update").Observe(metrics.SinceInSeconds(start))
	}()

	libcontainerCgroupConfig := m.libctCgroupConfig(cgroupConfig, true)
	manager, err := manager.New(libcontainerCgroupConfig)
	if err != nil {
		return fmt.Errorf("failed to create cgroup manager: %v", err)
	}
	return manager.Set(libcontainerCgroupConfig.Resources)
}

// Create creates the specified cgroup
func (m *cgroupManagerImpl) Create(cgroupConfig *CgroupConfig) error {
	start := time.Now()
	defer func() {
		metrics.CgroupManagerDuration.WithLabelValues("create").Observe(metrics.SinceInSeconds(start))
	}()

	libcontainerCgroupConfig := m.libctCgroupConfig(cgroupConfig, true)
	manager, err := manager.New(libcontainerCgroupConfig)
	if err != nil {
		return err
	}

	// Apply(-1) is a hack to create the cgroup directories for each resource
	// subsystem. The function [cgroups.Manager.apply()] applies cgroup
	// configuration to the process with the specified pid.
	// It creates cgroup files for each subsystems and writes the pid
	// in the tasks file. We use the function to create all the required
	// cgroup files but not attach any "real" pid to the cgroup.
	if err := manager.Apply(-1); err != nil {
		return err
	}

	// it may confuse why we call set after we do apply, but the issue is that runc
	// follows a similar pattern.  it's needed to ensure cpu quota is set properly.
	if err := manager.Set(libcontainerCgroupConfig.Resources); err != nil {
		utilruntime.HandleError(fmt.Errorf("cgroup manager.Set failed: %w", err))
	}

	return nil
}

// Scans through all subsystems to find pids associated with specified cgroup.
func (m *cgroupManagerImpl) Pids(name CgroupName) []int {
	// we need the driver specific name
	cgroupFsName := m.Name(name)

	// Get a list of processes that we need to kill
	pidsToKill := sets.NewInt()
	var pids []int
	for _, val := range m.subsystems.MountPoints {
		dir := path.Join(val, cgroupFsName)
		_, err := os.Stat(dir)
		if os.IsNotExist(err) {
			// The subsystem pod cgroup is already deleted
			// do nothing, continue
			continue
		}
		// Get a list of pids that are still charged to the pod's cgroup
		pids, err = getCgroupProcs(dir)
		if err != nil {
			continue
		}
		pidsToKill.Insert(pids...)

		// WalkFunc which is called for each file and directory in the pod cgroup dir
		visitor := func(path string, info os.FileInfo, err error) error {
			if err != nil {
				klog.V(4).InfoS("Cgroup manager encountered error scanning cgroup path", "path", path, "err", err)
				return filepath.SkipDir
			}
			if !info.IsDir() {
				return nil
			}
			pids, err = getCgroupProcs(path)
			if err != nil {
				klog.V(4).InfoS("Cgroup manager encountered error getting procs for cgroup path", "path", path, "err", err)
				return filepath.SkipDir
			}
			pidsToKill.Insert(pids...)
			return nil
		}
		// Walk through the pod cgroup directory to check if
		// container cgroups haven't been GCed yet. Get attached processes to
		// all such unwanted containers under the pod cgroup
		if err = filepath.Walk(dir, visitor); err != nil {
			klog.V(4).InfoS("Cgroup manager encountered error scanning pids for directory", "path", dir, "err", err)
		}
	}
	return pidsToKill.List()
}

// ReduceCPULimits reduces the cgroup's cpu shares to the lowest possible value
func (m *cgroupManagerImpl) ReduceCPULimits(cgroupName CgroupName) error {
	// Set lowest possible CpuShares value for the cgroup
	minimumCPUShares := uint64(MinShares)
	resources := &ResourceConfig{
		CPUShares: &minimumCPUShares,
	}
	containerConfig := &CgroupConfig{
		Name:               cgroupName,
		ResourceParameters: resources,
	}
	return m.Update(containerConfig)
}

// MemoryUsage returns the current memory usage of the specified cgroup,
// as read from cgroupfs.
func (m *cgroupManagerImpl) MemoryUsage(name CgroupName) (int64, error) {
	var path, file string
	if libcontainercgroups.IsCgroup2UnifiedMode() {
		path = m.buildCgroupUnifiedPath(name)
		file = "memory.current"
	} else {
		mp, ok := m.subsystems.MountPoints["memory"]
		if !ok { // should not happen
			return -1, errors.New("no cgroup v1 mountpoint for memory controller found")
		}
		path = mp + "/" + m.Name(name)
		file = "memory.usage_in_bytes"
	}
	val, err := fscommon.GetCgroupParamUint(path, file)
	return int64(val), err
}

// Convert cgroup v1 cpu.shares value to cgroup v2 cpu.weight
// https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/2254-cgroup-v2#phase-1-convert-from-cgroups-v1-settings-to-v2
func CpuSharesToCpuWeight(cpuShares uint64) uint64 {
	return uint64((((cpuShares - 2) * 9999) / 262142) + 1)
}

// Convert cgroup v2 cpu.weight value to cgroup v1 cpu.shares
// https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/2254-cgroup-v2#phase-1-convert-from-cgroups-v1-settings-to-v2
func CpuWeightToCpuShares(cpuWeight uint64) uint64 {
	return uint64((((cpuWeight - 1) * 262142) / 9999) + 2)
}

func getCgroupv1CpuConfig(cgroupPath string) (*ResourceConfig, error) {
	cpuQuotaStr, errQ := fscommon.GetCgroupParamString(cgroupPath, "cpu.cfs_quota_us")
	if errQ != nil {
		return nil, fmt.Errorf("failed to read CPU quota for cgroup %v: %v", cgroupPath, errQ)
	}
	cpuQuota, errInt := strconv.ParseInt(cpuQuotaStr, 10, 64)
	if errInt != nil {
		return nil, fmt.Errorf("failed to convert CPU quota as integer for cgroup %v: %v", cgroupPath, errInt)
	}
	cpuPeriod, errP := fscommon.GetCgroupParamUint(cgroupPath, "cpu.cfs_period_us")
	if errP != nil {
		return nil, fmt.Errorf("failed to read CPU period for cgroup %v: %v", cgroupPath, errP)
	}
	cpuShares, errS := fscommon.GetCgroupParamUint(cgroupPath, "cpu.shares")
	if errS != nil {
		return nil, fmt.Errorf("failed to read CPU shares for cgroup %v: %v", cgroupPath, errS)
	}
	return &ResourceConfig{CPUShares: &cpuShares, CPUQuota: &cpuQuota, CPUPeriod: &cpuPeriod}, nil
}

func getCgroupv2CpuConfig(cgroupPath string) (*ResourceConfig, error) {
	var cpuLimitStr, cpuPeriodStr string
	cpuLimitAndPeriod, err := fscommon.GetCgroupParamString(cgroupPath, "cpu.max")
	if err != nil {
		return nil, fmt.Errorf("failed to read cpu.max file for cgroup %v: %v", cgroupPath, err)
	}
	numItems, errScan := fmt.Sscanf(cpuLimitAndPeriod, "%s %s", &cpuLimitStr, &cpuPeriodStr)
	if errScan != nil || numItems != 2 {
		return nil, fmt.Errorf("failed to correctly parse content of cpu.max file ('%s') for cgroup %v: %v",
			cpuLimitAndPeriod, cgroupPath, errScan)
	}
	cpuLimit := int64(-1)
	if cpuLimitStr != Cgroup2MaxCpuLimit {
		cpuLimit, err = strconv.ParseInt(cpuLimitStr, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("failed to convert CPU limit as integer for cgroup %v: %v", cgroupPath, err)
		}
	}
	cpuPeriod, errPeriod := strconv.ParseUint(cpuPeriodStr, 10, 64)
	if errPeriod != nil {
		return nil, fmt.Errorf("failed to convert CPU period as integer for cgroup %v: %v", cgroupPath, errPeriod)
	}
	cpuWeight, errWeight := fscommon.GetCgroupParamUint(cgroupPath, "cpu.weight")
	if errWeight != nil {
		return nil, fmt.Errorf("failed to read CPU weight for cgroup %v: %v", cgroupPath, errWeight)
	}
	cpuShares := CpuWeightToCpuShares(cpuWeight)
	return &ResourceConfig{CPUShares: &cpuShares, CPUQuota: &cpuLimit, CPUPeriod: &cpuPeriod}, nil
}

func getCgroupCpuConfig(cgroupPath string) (*ResourceConfig, error) {
	if libcontainercgroups.IsCgroup2UnifiedMode() {
		return getCgroupv2CpuConfig(cgroupPath)
	} else {
		return getCgroupv1CpuConfig(cgroupPath)
	}
}

func getCgroupMemoryConfig(cgroupPath string) (*ResourceConfig, error) {
	memLimitFile := "memory.limit_in_bytes"
	if libcontainercgroups.IsCgroup2UnifiedMode() {
		memLimitFile = "memory.max"
	}
	memLimit, err := fscommon.GetCgroupParamUint(cgroupPath, memLimitFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read %s for cgroup %v: %v", memLimitFile, cgroupPath, err)
	}
	mLim := int64(memLimit)
	//TODO(vinaykul,InPlacePodVerticalScaling): Add memory request support
	return &ResourceConfig{Memory: &mLim}, nil

}

// GetCgroupConfig Get the resource config values applied to the cgroup for specified resource type
func (m *cgroupManagerImpl) GetCgroupConfig(name CgroupName, resource v1.ResourceName) (*ResourceConfig, error) {
	cgroupPaths := m.buildCgroupPaths(name)
	cgroupResourcePath, found := cgroupPaths[string(resource)]
	if !found {
		return nil, fmt.Errorf("failed to build %v cgroup fs path for cgroup %v", resource, name)
	}
	switch resource {
	case v1.ResourceCPU:
		return getCgroupCpuConfig(cgroupResourcePath)
	case v1.ResourceMemory:
		return getCgroupMemoryConfig(cgroupResourcePath)
	}
	return nil, fmt.Errorf("unsupported resource %v for cgroup %v", resource, name)
}

func setCgroupv1CpuConfig(cgroupPath string, resourceConfig *ResourceConfig) error {
	var cpuQuotaStr, cpuPeriodStr, cpuSharesStr string
	if resourceConfig.CPUQuota != nil {
		cpuQuotaStr = strconv.FormatInt(*resourceConfig.CPUQuota, 10)
		if err := os.WriteFile(filepath.Join(cgroupPath, "cpu.cfs_quota_us"), []byte(cpuQuotaStr), 0700); err != nil {
			return fmt.Errorf("failed to write %v to %v: %v", cpuQuotaStr, cgroupPath, err)
		}
	}
	if resourceConfig.CPUPeriod != nil {
		cpuPeriodStr = strconv.FormatUint(*resourceConfig.CPUPeriod, 10)
		if err := os.WriteFile(filepath.Join(cgroupPath, "cpu.cfs_period_us"), []byte(cpuPeriodStr), 0700); err != nil {
			return fmt.Errorf("failed to write %v to %v: %v", cpuPeriodStr, cgroupPath, err)
		}
	}
	if resourceConfig.CPUShares != nil {
		cpuSharesStr = strconv.FormatUint(*resourceConfig.CPUShares, 10)
		if err := os.WriteFile(filepath.Join(cgroupPath, "cpu.shares"), []byte(cpuSharesStr), 0700); err != nil {
			return fmt.Errorf("failed to write %v to %v: %v", cpuSharesStr, cgroupPath, err)
		}
	}
	return nil
}

func setCgroupv2CpuConfig(cgroupPath string, resourceConfig *ResourceConfig) error {
	if resourceConfig.CPUQuota != nil {
		if resourceConfig.CPUPeriod == nil {
			return fmt.Errorf("CpuPeriod must be specified in order to set CpuLimit")
		}
		cpuLimitStr := Cgroup2MaxCpuLimit
		if *resourceConfig.CPUQuota > -1 {
			cpuLimitStr = strconv.FormatInt(*resourceConfig.CPUQuota, 10)
		}
		cpuPeriodStr := strconv.FormatUint(*resourceConfig.CPUPeriod, 10)
		cpuMaxStr := fmt.Sprintf("%s %s", cpuLimitStr, cpuPeriodStr)
		if err := os.WriteFile(filepath.Join(cgroupPath, "cpu.max"), []byte(cpuMaxStr), 0700); err != nil {
			return fmt.Errorf("failed to write %v to %v: %v", cpuMaxStr, cgroupPath, err)
		}
	}
	if resourceConfig.CPUShares != nil {
		cpuWeight := CpuSharesToCpuWeight(*resourceConfig.CPUShares)
		cpuWeightStr := strconv.FormatUint(cpuWeight, 10)
		if err := os.WriteFile(filepath.Join(cgroupPath, "cpu.weight"), []byte(cpuWeightStr), 0700); err != nil {
			return fmt.Errorf("failed to write %v to %v: %v", cpuWeightStr, cgroupPath, err)
		}
	}
	return nil
}

func setCgroupCpuConfig(cgroupPath string, resourceConfig *ResourceConfig) error {
	if libcontainercgroups.IsCgroup2UnifiedMode() {
		return setCgroupv2CpuConfig(cgroupPath, resourceConfig)
	} else {
		return setCgroupv1CpuConfig(cgroupPath, resourceConfig)
	}
}

func setCgroupMemoryConfig(cgroupPath string, resourceConfig *ResourceConfig) error {
	memLimitFile := "memory.limit_in_bytes"
	if libcontainercgroups.IsCgroup2UnifiedMode() {
		memLimitFile = "memory.max"
	}
	memLimit := strconv.FormatInt(*resourceConfig.Memory, 10)
	if err := os.WriteFile(filepath.Join(cgroupPath, memLimitFile), []byte(memLimit), 0700); err != nil {
		return fmt.Errorf("failed to write %v to %v/%v: %v", memLimit, cgroupPath, memLimitFile, err)
	}
	//TODO(vinaykul,InPlacePodVerticalScaling): Add memory request support
	return nil
}

// SetCgroupConfig Set resource config for the specified resource type on the cgroup
func (m *cgroupManagerImpl) SetCgroupConfig(name CgroupName, resource v1.ResourceName, resourceConfig *ResourceConfig) error {
	cgroupPaths := m.buildCgroupPaths(name)
	cgroupResourcePath, found := cgroupPaths[string(resource)]
	if !found {
		return fmt.Errorf("failed to build %v cgroup fs path for cgroup %v", resource, name)
	}
	switch resource {
	case v1.ResourceCPU:
		return setCgroupCpuConfig(cgroupResourcePath, resourceConfig)
	case v1.ResourceMemory:
		return setCgroupMemoryConfig(cgroupResourcePath, resourceConfig)
	}
	return nil
}
