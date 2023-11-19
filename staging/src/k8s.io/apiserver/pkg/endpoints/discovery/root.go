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

package discovery

import (
	"net/http"
	"sync"

	restful "github.com/emicklei/go-restful/v3"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apiserver/pkg/endpoints/handlers/negotiation"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
)

// GroupManager is an interface that allows dynamic mutation of the existing webservice to handle
// API groups being added or removed.
// 1、组管理器可以允许动态的修改已经存在的WebService，支持添加、删除组
// 2、组管理器的功能非常简单，就是用于维护当前GenericServer所管理的组。组管理器本质上是一个http.Handler，用户通过组管理器可以知道集群中
// 可以使用的组有哪些。  我们可以通过kubectl get --raw=/apis的方式查询非核心资源意外的所有组。
// 3、ExtensionServer、AIPServer、AggregatedServer在启动过程当中一定会对组管理器进行初始化，并且把组管理器返回的路由注册到GenericServer
// 当中，从而支持HTTP请求的查询
type GroupManager interface {
	// AddGroup 向组管理器添加组，实际上也能覆盖组，也就是拥有添加组的功能
	AddGroup(apiGroup metav1.APIGroup)
	// RemoveGroup 移除组管理器中的某个组
	RemoveGroup(groupName string)
	// ServeHTTP 本质上就是http.Handler  TODO 为什么这里不直接组合http.Handler?
	ServeHTTP(resp http.ResponseWriter, req *http.Request)
	// WebService 返回路由，调用方拿到路由之后可以把这个路由注册到GenericServer当中。用户就可以通过kubectl get --raw=/api的方式
	// 获取到当前集群支持的组
	WebService() *restful.WebService
}

// rootAPIsHandler creates a webservice serving api group discovery.
// The list of APIGroups may change while the server is running because additional resources
// are registered or removed.  It is not safe to cache the values.
type rootAPIsHandler struct {
	// addresses is used to build cluster IPs for discovery.
	addresses Addresses

	// 用于序列化响应数据
	serializer runtime.NegotiatedSerializer

	// Map storing information about all groups to be exposed in discovery response.
	// The map is from name to the group.
	lock sync.RWMutex
	// key为Group
	apiGroups map[string]metav1.APIGroup
	// apiGroupNames preserves insertion order
	// 这里又用到了数组 + Map的方式，既可以提供O(1)的查找速度，又可以知道元素的插入顺序
	apiGroupNames []string
}

func NewRootAPIsHandler(addresses Addresses, serializer runtime.NegotiatedSerializer) *rootAPIsHandler {
	// Because in release 1.1, /apis returns response with empty APIVersion, we
	// use stripVersionNegotiatedSerializer to keep the response backwards
	// compatible.
	serializer = stripVersionNegotiatedSerializer{serializer}

	return &rootAPIsHandler{
		addresses:  addresses,
		serializer: serializer,
		apiGroups:  map[string]metav1.APIGroup{},
	}
}

func (s *rootAPIsHandler) AddGroup(apiGroup metav1.APIGroup) {
	s.lock.Lock()
	defer s.lock.Unlock()

	_, alreadyExists := s.apiGroups[apiGroup.Name]

	s.apiGroups[apiGroup.Name] = apiGroup
	if !alreadyExists {
		s.apiGroupNames = append(s.apiGroupNames, apiGroup.Name)
	}
}

func (s *rootAPIsHandler) RemoveGroup(groupName string) {
	s.lock.Lock()
	defer s.lock.Unlock()

	delete(s.apiGroups, groupName)
	for i := range s.apiGroupNames {
		if s.apiGroupNames[i] == groupName {
			s.apiGroupNames = append(s.apiGroupNames[:i], s.apiGroupNames[i+1:]...)
			break
		}
	}
}

// ServeHTPP 用于把当前组管理中添加的组序列化
func (s *rootAPIsHandler) ServeHTTP(resp http.ResponseWriter, req *http.Request) {
	s.lock.RLock()
	defer s.lock.RUnlock()

	var orderedGroups []metav1.APIGroup
	// 按照添加的顺序响应
	for _, groupName := range s.apiGroupNames {
		orderedGroups = append(orderedGroups, s.apiGroups[groupName])
	}

	// 获取ClientIP
	clientIP := utilnet.GetClientIP(req)
	// 根据ClientIP，获取Server地址
	serverCIDR := s.addresses.ServerAddressByClientCIDRs(clientIP)
	groups := make([]metav1.APIGroup, len(orderedGroups))
	for i := range orderedGroups {
		groups[i] = orderedGroups[i]
		groups[i].ServerAddressByClientCIDRs = serverCIDR
	}

	responsewriters.WriteObjectNegotiated(s.serializer, negotiation.DefaultEndpointRestrictions,
		schema.GroupVersion{}, resp, req, http.StatusOK, &metav1.APIGroupList{Groups: groups}, false)
}

func (s *rootAPIsHandler) restfulHandle(req *restful.Request, resp *restful.Response) {
	s.ServeHTTP(resp.ResponseWriter, req.Request)
}

// WebService returns a webservice serving api group discovery.
// Note: during the server runtime apiGroups might change.
// 1、一点难度都没有，就是把当前组管理器所管理的组序列化后响应，目的是就是为了让别人知道当前有哪些组
func (s *rootAPIsHandler) WebService() *restful.WebService {
	mediaTypes, _ := negotiation.MediaTypesForSerializer(s.serializer)
	ws := new(restful.WebService)
	ws.Path(APIGroupPrefix)
	ws.Doc("get available API versions")
	ws.Route(ws.GET("/").To(s.restfulHandle).
		Doc("get available API versions").
		Operation("getAPIVersions").
		Produces(mediaTypes...).
		Consumes(mediaTypes...).
		Writes(metav1.APIGroupList{})) // 猜测这玩意应该是为了给OpenAPI用的，用于显示响应值的每个字段
	return ws
}
