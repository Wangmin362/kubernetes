/*
Copyright 2020 The Kubernetes Authors.

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

package options

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apiserver/pkg/authentication/request/headerrequest"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	"k8s.io/client-go/kubernetes"
)

var _ dynamiccertificates.ControllerRunner = &DynamicRequestHeaderController{}
var _ dynamiccertificates.CAContentProvider = &DynamicRequestHeaderController{}

var _ headerrequest.RequestHeaderAuthRequestProvider = &DynamicRequestHeaderController{}

// DynamicRequestHeaderController combines DynamicCAFromConfigMapController and RequestHeaderAuthRequestController
// into one controller for dynamically filling RequestHeaderConfig struct
// 1、用于获取代理代理认证的五个配置信息，分别如下
// --requestheader-client-ca-file=xxx # 防止有人绕过认证代理服务
// --requestheader-allowed-names=xxx
// --requestheader-username-headers=X-Remote-User
// --requestheader-group-headers=X-Remote-Group
// --requestheader-extra-headers-prefix=X-Remote-Extra-
type DynamicRequestHeaderController struct {
	// 用于监听某个ConfigMap，获取证书的内容
	*dynamiccertificates.ConfigMapCAController
	*headerrequest.RequestHeaderAuthRequestController
}

// newDynamicRequestHeaderController creates a new controller that implements DynamicRequestHeaderController
func newDynamicRequestHeaderController(client kubernetes.Interface) (*DynamicRequestHeaderController, error) {
	requestHeaderCAController, err := dynamiccertificates.NewDynamicCAFromConfigMapController(
		"client-ca",
		authenticationConfigMapNamespace,
		authenticationConfigMapName,
		"requestheader-client-ca-file",
		client)
	if err != nil {
		return nil, fmt.Errorf("unable to create DynamicCAFromConfigMap controller: %v", err)
	}

	requestHeaderAuthRequestController := headerrequest.NewRequestHeaderAuthRequestController(
		authenticationConfigMapName,
		authenticationConfigMapNamespace,
		client,
		"requestheader-username-headers",
		"requestheader-group-headers",
		"requestheader-extra-headers-prefix",
		"requestheader-allowed-names",
	)
	return &DynamicRequestHeaderController{
		ConfigMapCAController:              requestHeaderCAController,
		RequestHeaderAuthRequestController: requestHeaderAuthRequestController,
	}, nil
}

func (c *DynamicRequestHeaderController) RunOnce(ctx context.Context) error {
	var errs []error
	errs = append(errs, c.ConfigMapCAController.RunOnce(ctx))
	errs = append(errs, c.RequestHeaderAuthRequestController.RunOnce(ctx))
	return errors.NewAggregate(errs)
}

func (c *DynamicRequestHeaderController) Run(ctx context.Context, workers int) {
	go c.ConfigMapCAController.Run(ctx, workers)
	go c.RequestHeaderAuthRequestController.Run(ctx, workers)
	<-ctx.Done()
}
