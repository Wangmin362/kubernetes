/*
Copyright 2022 The Kubernetes Authors.

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

package aggregated

import (
	"net/http"
	"reflect"
	"sort"
	"sync"

	apidiscoveryv2beta1 "k8s.io/api/apidiscovery/v2beta1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
	"k8s.io/apiserver/pkg/endpoints/metrics"

	"sync/atomic"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
)

// Source 1、K8S中的资源主要有三类，其一是来源于Source, 其二是来源于内建资源，也就是所谓的核心资源与APIServer中的所有资源
// 其三是用户添加的CRD资源
type Source uint

// The GroupVersion from the lowest Source takes precedence
const (
	AggregatorSource Source = 0
	BuiltinSource    Source = 100
	CRDSource        Source = 200
)

// ResourceManager This handler serves the /apis endpoint for an aggregated list of
// api resources indexed by their group version.
// TODO 资源管理器是如何管理资源的？它提供了什么功能？
// 1、资源管理器本质上也是一个http.Handler，用于动态发现路由
type ResourceManager interface {
	// AddGroupVersion Adds knowledge of the given groupversion to the discovery document
	// If it was already being tracked, updates the stored APIVersionDiscovery
	// Thread-safe
	// APIVersionDiscovery本质上就是当前某个版本下的所有资源
	// TODO 第二个参数为什么不是APIGroupDiscovery
	AddGroupVersion(groupName string, value apidiscoveryv2beta1.APIVersionDiscovery)

	// SetGroupVersionPriority Sets a priority to be used while sorting a specific group and
	// group-version. If two versions report different priorities for
	// the group, the higher one will be used. If the group is not
	// known, the priority is ignored. The priority for this version
	// is forgotten once the group-version is forgotten
	SetGroupVersionPriority(gv metav1.GroupVersion, grouppriority, versionpriority int)

	// RemoveGroup Removes all group versions for a given group
	// Thread-safe
	RemoveGroup(groupName string)

	// RemoveGroupVersion Removes a specific groupversion. If all versions of a group have been
	// removed, then the entire group is unlisted.
	// Thread-safe
	RemoveGroupVersion(gv metav1.GroupVersion)

	// SetGroups Resets the manager's known list of group-versions and replaces them
	// with the given groups
	// Thread-Safe
	SetGroups([]apidiscoveryv2beta1.APIGroupDiscovery)

	// WithSource Returns the same resource manager using a different source
	// The source is used to decide how to de-duplicate groups.
	// The group from the least-numbered source is used
	// 不同的源有不同的资源管理器
	WithSource(source Source) ResourceManager

	// TODO 看到没，这里人家就是用的组合的方式
	http.Handler
}

// 源的资源管理器
type resourceManager struct {
	source Source
	*resourceDiscoveryManager
}

func (rm resourceManager) AddGroupVersion(groupName string, value apidiscoveryv2beta1.APIVersionDiscovery) {
	rm.resourceDiscoveryManager.AddGroupVersion(rm.source, groupName, value)
}
func (rm resourceManager) SetGroupVersionPriority(gv metav1.GroupVersion, grouppriority, versionpriority int) {
	rm.resourceDiscoveryManager.SetGroupVersionPriority(rm.source, gv, grouppriority, versionpriority)
}
func (rm resourceManager) RemoveGroup(groupName string) {
	rm.resourceDiscoveryManager.RemoveGroup(rm.source, groupName)
}
func (rm resourceManager) RemoveGroupVersion(gv metav1.GroupVersion) {
	rm.resourceDiscoveryManager.RemoveGroupVersion(rm.source, gv)
}
func (rm resourceManager) SetGroups(groups []apidiscoveryv2beta1.APIGroupDiscovery) {
	rm.resourceDiscoveryManager.SetGroups(rm.source, groups)
}

func (rm resourceManager) WithSource(source Source) ResourceManager {
	return resourceManager{
		source:                   source,
		resourceDiscoveryManager: rm.resourceDiscoveryManager,
	}
}

type groupKey struct {
	name string

	// Source identifies where this group came from and dictates which group
	// among duplicates is chosen to be used for discovery.
	// 资源的来源
	source Source
}

type groupVersionKey struct {
	metav1.GroupVersion
	source Source
}

type resourceDiscoveryManager struct {
	serializer runtime.NegotiatedSerializer // 编解码器
	// cache is an atomic pointer to avoid the use of locks
	// TODO 缓存的啥？
	cache atomic.Pointer[cachedGroupList]

	serveHTTPFunc http.HandlerFunc

	// Writes protected by the lock.
	// List of all apigroups & resources indexed by the resource manager
	lock sync.RWMutex
	// 保存每个组的所有资源
	apiGroups map[groupKey]*apidiscoveryv2beta1.APIGroupDiscovery
	// 保存每个GV的优先级
	versionPriorities map[groupVersionKey]priorityInfo
}

type priorityInfo struct {
	GroupPriorityMinimum int
	VersionPriority      int
}

func NewResourceManager(path string) ResourceManager {
	// 实例化Scheme
	scheme := runtime.NewScheme()
	// 通过scheme获取编解码器
	codecs := serializer.NewCodecFactory(scheme)
	// 向scheme中注册APIGroupDiscovery, APIGroupDiscoveryList
	utilruntime.Must(apidiscoveryv2beta1.AddToScheme(scheme))
	rdm := &resourceDiscoveryManager{
		serializer:        codecs,
		versionPriorities: make(map[groupVersionKey]priorityInfo),
	}
	// 使用metric指标包装请求，可以用于获取请求执行的时间
	rdm.serveHTTPFunc = metrics.InstrumentHandlerFunc("GET",
		/* group = */ "",
		/* version = */ "",
		/* resource = */ "",
		/* subresource = */ path,
		/* scope = */ "",
		/* component = */ metrics.APIServerComponent,
		/* deprecated */ false,
		/* removedRelease */ "",
		rdm.serveHTTP)
	return resourceManager{
		source:                   BuiltinSource,
		resourceDiscoveryManager: rdm,
	}
}

func (rdm *resourceDiscoveryManager) SetGroupVersionPriority(source Source, gv metav1.GroupVersion, groupPriorityMinimum, versionPriority int) {
	rdm.lock.Lock()
	defer rdm.lock.Unlock()

	key := groupVersionKey{
		GroupVersion: gv,
		source:       source,
	}
	rdm.versionPriorities[key] = priorityInfo{
		GroupPriorityMinimum: groupPriorityMinimum,
		VersionPriority:      versionPriority,
	}
	// 清空缓存，其实就是让缓存失效，重新生成缓存
	rdm.cache.Store(nil)
}

func (rdm *resourceDiscoveryManager) SetGroups(source Source, groups []apidiscoveryv2beta1.APIGroupDiscovery) {
	rdm.lock.Lock()
	defer rdm.lock.Unlock()

	// 每次添加新的数据之前，清空老数据
	rdm.apiGroups = nil
	rdm.cache.Store(nil)

	// 1、注册每个group，以及每个group下的每个资源
	// 2、这里注册的每个GV的GroupPriorityMinimum默认为1000， VersionPriority默认为15
	for _, group := range groups {
		for _, version := range group.Versions {
			rdm.addGroupVersionLocked(source, group.Name, version)
		}
	}

	// Filter unused out priority entries
	// 删除无用的GV优先级，如果判断当前GV是无用的？ 只需要在apiGroups中查找这个GV，如果找不到直接删除
	for gv := range rdm.versionPriorities {
		key := groupKey{
			source: source,
			name:   gv.Group,
		}
		// 如果当前组已经不存在，那么删除这个组GV的优先级
		entry, exists := rdm.apiGroups[key]
		if !exists {
			delete(rdm.versionPriorities, gv)
			continue
		}

		containsVersion := false

		// 如果组存在，那么还需要看看这个组下的GV是存在
		for _, v := range entry.Versions {
			if v.Version == gv.Version {
				containsVersion = true
				break
			}
		}

		// 如果不存在，直接删除GV的优先级
		if !containsVersion {
			delete(rdm.versionPriorities, gv)
		}
	}
}

func (rdm *resourceDiscoveryManager) AddGroupVersion(source Source, groupName string, value apidiscoveryv2beta1.APIVersionDiscovery) {
	rdm.lock.Lock()
	defer rdm.lock.Unlock()

	rdm.addGroupVersionLocked(source, groupName, value)
}

func (rdm *resourceDiscoveryManager) addGroupVersionLocked(source Source, groupName string, value apidiscoveryv2beta1.APIVersionDiscovery) {
	klog.Infof("Adding GroupVersion %s %s to ResourceManager", groupName, value.Version)

	if rdm.apiGroups == nil {
		rdm.apiGroups = make(map[groupKey]*apidiscoveryv2beta1.APIGroupDiscovery)
	}

	key := groupKey{
		source: source,
		name:   groupName,
	}

	// 获取当前组
	if existing, groupExists := rdm.apiGroups[key]; groupExists {
		// If this version already exists, replace it
		versionExists := false

		// Not very efficient, but in practice there are generally not many versions
		for i := range existing.Versions {
			if existing.Versions[i].Version == value.Version {
				// The new gv is the exact same as what is already in
				// the map. This is a noop and cache should not be
				// invalidated.
				// 如果相等，也就没有替换的必要了，直接退出
				if reflect.DeepEqual(existing.Versions[i], value) {
					return
				}

				// 如果不相等，直接替换
				existing.Versions[i] = value
				versionExists = true
				break
			}
		}

		// 如果当前group中还没有保存这个版本，直接添加进去
		if !versionExists {
			existing.Versions = append(existing.Versions, value)
		}

	} else {
		// 如果当前还没有保存这个组下的任何资源，直接新建一个组
		group := &apidiscoveryv2beta1.APIGroupDiscovery{
			ObjectMeta: metav1.ObjectMeta{
				Name: groupName,
			},
			Versions: []apidiscoveryv2beta1.APIVersionDiscovery{value},
		}
		rdm.apiGroups[key] = group
	}

	gv := metav1.GroupVersion{Group: groupName, Version: value.Version}
	gvKey := groupVersionKey{
		GroupVersion: gv,
		source:       source,
	}
	if _, ok := rdm.versionPriorities[gvKey]; !ok {
		rdm.versionPriorities[gvKey] = priorityInfo{
			GroupPriorityMinimum: 1000, // TODO 为什么这里的版本号是固定的
			VersionPriority:      15,
		}
	}

	// Reset response document so it is recreated lazily
	// 由于当前保存的资源已经发生了变化，因此需要清空缓存，重新生成
	rdm.cache.Store(nil)
}

func (rdm *resourceDiscoveryManager) RemoveGroupVersion(source Source, apiGroup metav1.GroupVersion) {
	rdm.lock.Lock()
	defer rdm.lock.Unlock()

	key := groupKey{
		source: source,
		name:   apiGroup.Group,
	}

	// 如果当前group不存在，直接退出
	group, exists := rdm.apiGroups[key]
	if !exists {
		return
	}

	modified := false
	for i := range group.Versions {
		// 删除GV
		if group.Versions[i].Version == apiGroup.Version {
			group.Versions = append(group.Versions[:i], group.Versions[i+1:]...)
			modified = true
			break
		}
	}
	// If no modification was done, cache does not need to be cleared
	// 如果根本就不存在这个GV，直接退出
	if !modified {
		return
	}

	// 如果删除了这个GV，那么还需要清理优先级
	gvKey := groupVersionKey{
		GroupVersion: apiGroup,
		source:       source,
	}

	delete(rdm.versionPriorities, gvKey)
	// 如果当前组删除这个GV之后，没有任何版本了，直接删除这个组
	if len(group.Versions) == 0 {
		delete(rdm.apiGroups, key)
	}

	// Reset response document so it is recreated lazily
	// 清空缓存，其实就是让缓存失效，重新生成缓存
	rdm.cache.Store(nil)
}

func (rdm *resourceDiscoveryManager) RemoveGroup(source Source, groupName string) {
	rdm.lock.Lock()
	defer rdm.lock.Unlock()

	key := groupKey{
		source: source,
		name:   groupName,
	}

	// 直接移除组
	delete(rdm.apiGroups, key)

	// 同时需要移除当前组的每个GV的优先级
	for k := range rdm.versionPriorities {
		if k.Group == groupName && k.source == source {
			delete(rdm.versionPriorities, k)
		}
	}

	// Reset response document so it is recreated lazily
	// 清空缓存，其实就是让缓存失效，重新生成缓存
	rdm.cache.Store(nil)
}

// Prepares the api group list for serving by converting them from map into
// list and sorting them according to insertion order
func (rdm *resourceDiscoveryManager) calculateAPIGroupsLocked() []apidiscoveryv2beta1.APIGroupDiscovery {
	// 指标加一
	regenerationCounter.Inc()
	// Re-order the apiGroups by their priority.
	var groups []apidiscoveryv2beta1.APIGroupDiscovery

	groupsToUse := map[string]apidiscoveryv2beta1.APIGroupDiscovery{}
	// 记录每个GV的源
	sourcesUsed := map[metav1.GroupVersion]Source{}

	// 遍历所有的分组
	for key, group := range rdm.apiGroups {
		// 如果当前已经记录过这个组
		if existing, ok := groupsToUse[key.name]; ok {
			// 遍历每个版本
			for _, v := range group.Versions {
				gv := metav1.GroupVersion{Group: key.name, Version: v.Version}

				// Skip groupversions we've already seen before. Only DefaultSource
				// takes precedence
				// TODO 这里在干嘛？
				if usedSource, seen := sourcesUsed[gv]; seen && key.source >= usedSource {
					continue
				} else if seen {
					// Find the index of the duplicate version and replace
					for i := 0; i < len(existing.Versions); i++ {
						if existing.Versions[i].Version == v.Version {
							existing.Versions[i] = v
							break
						}
					}

				} else {
					// New group-version, just append
					existing.Versions = append(existing.Versions, v)
				}

				sourcesUsed[gv] = key.source
				groupsToUse[key.name] = existing
			}
			// Check to see if we have overlapping versions. If we do, take the one
			// with highest source precedence
		} else {
			groupsToUse[key.name] = *group.DeepCopy()
			for _, v := range group.Versions {
				gv := metav1.GroupVersion{Group: key.name, Version: v.Version}
				sourcesUsed[gv] = key.source
			}
		}
	}

	// 给每个组不同的GV排序
	for _, group := range groupsToUse {

		// Re-order versions based on their priority. Use kube-aware string
		// comparison as a tie breaker
		sort.SliceStable(group.Versions, func(i, j int) bool {
			iVersion := group.Versions[i].Version
			jVersion := group.Versions[j].Version

			iGV := metav1.GroupVersion{Group: group.Name, Version: iVersion}
			jGV := metav1.GroupVersion{Group: group.Name, Version: jVersion}

			iSource := sourcesUsed[iGV]
			jSource := sourcesUsed[jGV]

			iPriority := rdm.versionPriorities[groupVersionKey{iGV, iSource}].VersionPriority
			jPriority := rdm.versionPriorities[groupVersionKey{jGV, jSource}].VersionPriority

			// Sort by version string comparator if priority is equal
			if iPriority == jPriority {
				return version.CompareKubeAwareVersionStrings(iVersion, jVersion) > 0
			}

			// i sorts before j if it has a higher priority
			return iPriority > jPriority
		})

		groups = append(groups, group)
	}

	// For each group, determine the highest minimum group priority and use that
	priorities := map[string]int{}
	for gv, info := range rdm.versionPriorities {
		if source := sourcesUsed[gv.GroupVersion]; source != gv.source {
			continue
		}

		if existing, exists := priorities[gv.Group]; exists {
			if existing < info.GroupPriorityMinimum {
				priorities[gv.Group] = info.GroupPriorityMinimum
			}
		} else {
			priorities[gv.Group] = info.GroupPriorityMinimum
		}
	}

	sort.SliceStable(groups, func(i, j int) bool {
		iName := groups[i].Name
		jName := groups[j].Name

		// Default to 0 priority by default
		iPriority := priorities[iName]
		jPriority := priorities[jName]

		// Sort discovery based on apiservice priority.
		// Duplicated from staging/src/k8s.io/kube-aggregator/pkg/apis/apiregistration/v1/helpers.go
		if iPriority == jPriority {
			// Equal priority uses name to break ties
			return iName < jName
		}

		// i sorts before j if it has a higher priority
		return iPriority > jPriority
	})

	return groups
}

// Fetches from cache if it exists. If cache is empty, create it.
func (rdm *resourceDiscoveryManager) fetchFromCache() *cachedGroupList {
	rdm.lock.RLock()
	defer rdm.lock.RUnlock()

	cacheLoad := rdm.cache.Load()
	if cacheLoad != nil {
		return cacheLoad
	}
	response := apidiscoveryv2beta1.APIGroupDiscoveryList{
		Items: rdm.calculateAPIGroupsLocked(),
	}
	etag, err := calculateETag(response)
	if err != nil {
		klog.Errorf("failed to calculate etag for discovery document: %s", etag)
		etag = ""
	}
	cached := &cachedGroupList{
		cachedResponse:     response,
		cachedResponseETag: etag,
	}
	rdm.cache.Store(cached)
	return cached
}

type cachedGroupList struct {
	cachedResponse     apidiscoveryv2beta1.APIGroupDiscoveryList
	cachedResponseETag string
}

func (rdm *resourceDiscoveryManager) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	rdm.serveHTTPFunc(resp, req)
}

func (rdm *resourceDiscoveryManager) serveHTTP(resp http.ResponseWriter, req *http.Request) {
	cache := rdm.fetchFromCache()
	response := cache.cachedResponse
	etag := cache.cachedResponseETag

	if len(etag) > 0 {
		// Use proper e-tag headers if one is available
		ServeHTTPWithETag(
			&response,
			etag,
			rdm.serializer,
			resp,
			req,
		)
	} else {
		// Default to normal response in rare case etag is
		// not cached with the object for some reason.
		responsewriters.WriteObjectNegotiated(
			rdm.serializer,
			DiscoveryEndpointRestrictions,
			AggregatedDiscoveryGV,
			resp,
			req,
			http.StatusOK,
			&response,
			true,
		)
	}
}
