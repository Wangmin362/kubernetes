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

	"github.com/emicklei/go-restful/v3"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/endpoints/handlers/negotiation"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
)

// APIGroupHandler creates a webservice serving the supported versions, preferred version, and name
// of a group. E.g., such a web service will be registered at /apis/extensions.
// TODO 暴露关于一个group的路由，使得用户可以查询一个group相关的信息
type APIGroupHandler struct {
	// 根据Group, Version协商出序列化、反序列化器
	serializer runtime.NegotiatedSerializer
	// TODO 标识一个group下包含了哪些
	group metav1.APIGroup
}

func NewAPIGroupHandler(serializer runtime.NegotiatedSerializer, group metav1.APIGroup) *APIGroupHandler {
	if keepUnversioned(group.Name) {
		// Because in release 1.1, /apis/extensions returns response with empty
		// APIVersion, we use stripVersionNegotiatedSerializer to keep the
		// response backwards compatible.
		serializer = stripVersionNegotiatedSerializer{serializer}
	}

	return &APIGroupHandler{
		serializer: serializer,
		group:      group,
	}
}

func (s *APIGroupHandler) WebService() *restful.WebService {
	mediaTypes, _ := negotiation.MediaTypesForSerializer(s.serializer)
	ws := new(restful.WebService)
	ws.Path(APIGroupPrefix + "/" + s.group.Name)
	ws.Doc("get information of a group")
	ws.Route(ws.GET("/").To(s.handle).
		Doc("get information of a group").
		Operation("getAPIGroup").
		Produces(mediaTypes...).
		Consumes(mediaTypes...).
		Writes(metav1.APIGroup{}))
	return ws
}

// handle returns a handler which will return the api.GroupAndVersion of the group.
func (s *APIGroupHandler) handle(req *restful.Request, resp *restful.Response) {
	s.ServeHTTP(resp.ResponseWriter, req.Request)
}

// 把一个组下的所有version，全部序列化并返回
func (s *APIGroupHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	responsewriters.WriteObjectNegotiated(s.serializer, negotiation.DefaultEndpointRestrictions,
		schema.GroupVersion{}, w, req, http.StatusOK, &s.group)
}
