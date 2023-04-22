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

	restful "github.com/emicklei/go-restful/v3"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/endpoints/handlers/negotiation"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
)

// APIResourceLister TODO /apis/<group> 和 /apis/<group>/<version> 所列出的资源有何区别？
// 实际上这个接口定义并不难理解， /apis/<group>/<version>仅仅是
type APIResourceLister interface {
	ListAPIResources() []metav1.APIResource
}

// APIResourceListerFunc 是APIResourceLister的适配器
type APIResourceListerFunc func() []metav1.APIResource

func (f APIResourceListerFunc) ListAPIResources() []metav1.APIResource {
	return f()
}

// APIVersionHandler creates a webservice serving the supported resources for the version
// E.g., such a web service will be registered at /apis/extensions/v1beta1.
// TODO 暴露关于一个/apis/<group>/<version>的路由信息，使得用户可以查询关于一个version的所有资源信息
// 譬如当用户执行 /apis/skyguard.com.cn/v1beta1,那么说明用户需要返回skyguar.com.cn这个组下的v1beta1版本下的所有资源，譬如
// ucwi, dsg, ucsslite, tenantAuth CRD
type APIVersionHandler struct {
	// 序列化器
	serializer runtime.NegotiatedSerializer

	// 需要暴露的API的group, version信息，譬如暴露端点为：/apis/skyguard.com.cn/v1beta1
	groupVersion schema.GroupVersion
	// 列出 /apis/<group>/<version>下的所有资源，譬如ucwi, dsg, ucsslite, tenantAuth
	apiResourceLister APIResourceLister
}

func NewAPIVersionHandler(serializer runtime.NegotiatedSerializer, groupVersion schema.GroupVersion,
	apiResourceLister APIResourceLister) *APIVersionHandler {
	// 这里是为了能够前向兼容
	if keepUnversioned(groupVersion.Group) {
		// Because in release 1.1, /apis/extensions returns response with empty
		// APIVersion, we use stripVersionNegotiatedSerializer to keep the
		// response backwards compatible.
		serializer = stripVersionNegotiatedSerializer{serializer}
	}

	return &APIVersionHandler{
		serializer:        serializer,
		groupVersion:      groupVersion,
		apiResourceLister: apiResourceLister,
	}
}

// AddToWebService 增加路由
func (s *APIVersionHandler) AddToWebService(ws *restful.WebService) {
	mediaTypes, _ := negotiation.MediaTypesForSerializer(s.serializer)
	// TODO 这里之所以是根 "/", 是因为ws中已经包含了RootPath信息
	ws.Route(ws.GET("/").To(s.handle).
		Doc("get available resources").
		Operation("getAPIResources").
		Produces(mediaTypes...).
		Consumes(mediaTypes...).
		Writes(metav1.APIResourceList{}))
}

// handle returns a handler which will return the api.VersionAndVersion of the group.
func (s *APIVersionHandler) handle(req *restful.Request, resp *restful.Response) {
	s.ServeHTTP(resp.ResponseWriter, req.Request)
}

// ServeHTTP 把s.apiResourceLister.ListAPIResources()序列化之后写入到http.ResponseWriter当中
func (s *APIVersionHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	responsewriters.WriteObjectNegotiated(s.serializer, negotiation.DefaultEndpointRestrictions, schema.GroupVersion{}, w, req, http.StatusOK,
		&metav1.APIResourceList{GroupVersion: s.groupVersion.String(), APIResources: s.apiResourceLister.ListAPIResources()})
}
