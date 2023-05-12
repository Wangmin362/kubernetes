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

package apiserver

import (
	"net/http"
	"strings"
	"sync"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/endpoints/discovery"
)

// TODO 如何理解VersionDiscoveryHandler结构体定义？
// 答：versionDiscoveryHandler顾名思义就是专门用来处理/apis/<group>/<version>的Handler，它需要返回当前版本下的所有资源
// 譬如当用户执行 /apis/skyguard.com.cn/v1beta1,那么说明用户需要返回skyguar.com.cn这个组下的v1beta1版本下的所有资源，譬如
// ucwi, dsg, ucsslite, tenantAuth CRD
type versionDiscoveryHandler struct {
	// TODO, writing is infrequent, optimize this
	// TODO 如果要优化这里，应该怎么优化？
	discoveryLock sync.RWMutex
	// 用于暴露某个组下某本版本的所有资源
	discovery map[schema.GroupVersion]*discovery.APIVersionHandler

	// 如果无法处理当前的/apis/<group>/<version>
	delegate http.Handler
}

func (r *versionDiscoveryHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	pathParts := splitPath(req.URL.Path)
	// only match /apis/<group>/<version>
	// TODO ExtensionServer的所有API一定是以 /apis开头的
	if len(pathParts) != 3 || pathParts[0] != "apis" {
		r.delegate.ServeHTTP(w, req)
		return
	}
	// 根据group, version信息，获取到APIVersionHandler
	discovery, ok := r.getDiscovery(schema.GroupVersion{Group: pathParts[1], Version: pathParts[2]})
	if !ok {
		r.delegate.ServeHTTP(w, req)
		return
	}

	// 利用得到的APIVersionHandler，返回其group, version下的所有资源的信息
	discovery.ServeHTTP(w, req)
}

func (r *versionDiscoveryHandler) getDiscovery(gv schema.GroupVersion) (*discovery.APIVersionHandler, bool) {
	r.discoveryLock.RLock()
	defer r.discoveryLock.RUnlock()

	ret, ok := r.discovery[gv]
	return ret, ok
}

func (r *versionDiscoveryHandler) setDiscovery(gv schema.GroupVersion, discovery *discovery.APIVersionHandler) {
	r.discoveryLock.Lock()
	defer r.discoveryLock.Unlock()

	r.discovery[gv] = discovery
}

func (r *versionDiscoveryHandler) unsetDiscovery(gv schema.GroupVersion) {
	r.discoveryLock.Lock()
	defer r.discoveryLock.Unlock()

	delete(r.discovery, gv)
}

// 如何理解groupDiscoveryHandler结构体定义？
// 答：groupDiscoveryHandler顾名思义就是专门用来处理/apis/<group>的Handler，它需要返回当前组下的所有资源
// 譬如当用户执行 /apis/skyguard.com.cn,那么说明用户需要返回skyguar.com.cn这个组下的所有版本的所有资源
type groupDiscoveryHandler struct {
	// TODO, writing is infrequent, optimize this
	// TODO 思考如何优化？
	discoveryLock sync.RWMutex
	// 用于返回某个组先的所有版本信息
	discovery map[string]*discovery.APIGroupHandler

	delegate http.Handler
}

func (r *groupDiscoveryHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	pathParts := splitPath(req.URL.Path)
	// only match /apis/<group>
	if len(pathParts) != 2 || pathParts[0] != "apis" {
		r.delegate.ServeHTTP(w, req)
		return
	}
	// 根据组名找到对应的Handler
	discovery, ok := r.getDiscovery(pathParts[1])
	if !ok {
		// 如果没有找到，就直接委派给delegator
		r.delegate.ServeHTTP(w, req)
		return
	}

	discovery.ServeHTTP(w, req)
}

func (r *groupDiscoveryHandler) getDiscovery(group string) (*discovery.APIGroupHandler, bool) {
	r.discoveryLock.RLock()
	defer r.discoveryLock.RUnlock()

	ret, ok := r.discovery[group]
	return ret, ok
}

func (r *groupDiscoveryHandler) setDiscovery(group string, discovery *discovery.APIGroupHandler) {
	r.discoveryLock.Lock()
	defer r.discoveryLock.Unlock()

	r.discovery[group] = discovery
}

func (r *groupDiscoveryHandler) unsetDiscovery(group string) {
	r.discoveryLock.Lock()
	defer r.discoveryLock.Unlock()

	delete(r.discovery, group)
}

// splitPath returns the segments for a URL path.
func splitPath(path string) []string {
	path = strings.Trim(path, "/")
	if path == "" {
		return []string{}
	}
	return strings.Split(path, "/")
}
