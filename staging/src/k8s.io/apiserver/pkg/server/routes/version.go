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

package routes

import (
	"net/http"

	"github.com/emicklei/go-restful/v3"

	"k8s.io/apimachinery/pkg/version"
	"k8s.io/apiserver/pkg/endpoints/handlers/responsewriters"
)

// Version provides a webservice with version information.
type Version struct {
	Version *version.Info
}

// Install registers the APIServer's `/version` handler.
// 添加/version路由，返回K8S相关的版本信息
/*
root@k8s-master1:~# kubectl get --raw=/version
{
  "major": "1",
  "minor": "27",
  "gitVersion": "v1.27.2",
  "gitCommit": "7f6f68fdabc4df88cfea2dcf9a19b2b830f1e647",
  "gitTreeState": "clean",
  "buildDate": "2023-05-17T14:13:28Z",
  "goVersion": "go1.20.4",
  "compiler": "gc",
  "platform": "linux/amd64"
}
*/
func (v Version) Install(c *restful.Container) {
	if v.Version == nil {
		return
	}

	// Set up a service to return the git code version.
	versionWS := new(restful.WebService)
	versionWS.Path("/version")
	versionWS.Doc("git code version from which this is built")
	versionWS.Route(
		versionWS.GET("/").To(v.handleVersion).
			Doc("get the code version").
			Operation("getCodeVersion").
			Produces(restful.MIME_JSON).
			Consumes(restful.MIME_JSON).
			Writes(version.Info{}))

	c.Add(versionWS)
}

// handleVersion writes the server's version information.
func (v Version) handleVersion(req *restful.Request, resp *restful.Response) {
	responsewriters.WriteRawJSON(http.StatusOK, *v.Version, resp.ResponseWriter)
}
