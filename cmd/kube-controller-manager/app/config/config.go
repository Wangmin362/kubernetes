/*
Copyright 2018 The Kubernetes Authors.

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

package config

import (
	apiserver "k8s.io/apiserver/pkg/server"
	clientset "k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	kubectrlmgrconfig "k8s.io/kubernetes/pkg/controller/apis/config"
)

// Config is the main context object for the controller manager.
type Config struct {
	// 各个组件的配置，每个组件可配置的行为就是通过这些配置决定的。这些配置就是通过controller-manager的配置传进来的
	ComponentConfig kubectrlmgrconfig.KubeControllerManagerConfiguration

	// 安全服务相关，譬如CA证书，加密套件等等
	SecureServing *apiserver.SecureServingInfo
	// LoopbackClientConfig is a config for a privileged loopback connection
	// todo loopback指的是什么？
	LoopbackClientConfig *restclient.Config

	// TODO: remove deprecated insecure serving
	// apiserver除了可以开启一个安全端口，还支持开启一个非安全的端口，也就是不需要经过认证、鉴权，不过默认是关闭的
	InsecureServing *apiserver.DeprecatedInsecureServingInfo
	// 认证信息
	Authentication apiserver.AuthenticationInfo
	// 授权信息
	Authorization apiserver.AuthorizationInfo

	// apiserver客户端
	Client *clientset.Clientset

	// kubeconfig文件
	Kubeconfig *restclient.Config

	EventBroadcaster record.EventBroadcaster
	EventRecorder    record.EventRecorder
}

type completedConfig struct {
	*Config
}

// CompletedConfig same as Config, just to swap private object.
type CompletedConfig struct {
	// Embed a private pointer that cannot be instantiated outside of this package.
	*completedConfig
}

// Complete fills in any fields not set that are required to have valid data. It's mutating the receiver.
func (c *Config) Complete() *CompletedConfig {
	cc := completedConfig{c}

	// todo 这一块得好好研究下
	apiserver.AuthorizeClientBearerToken(c.LoopbackClientConfig, &c.Authentication, &c.Authorization)

	return &CompletedConfig{&cc}
}
