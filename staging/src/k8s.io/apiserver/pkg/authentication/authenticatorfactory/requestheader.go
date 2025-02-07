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

package authenticatorfactory

import (
	"k8s.io/apiserver/pkg/authentication/request/headerrequest"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
)

type RequestHeaderConfig struct {
	// UsernameHeaders are the headers to check (in order, case-insensitively) for an identity. The first header with a value wins.
	UsernameHeaders headerrequest.StringSliceProvider
	// GroupHeaders are the headers to check (case-insensitively) for a group names.  All values will be used.
	GroupHeaders headerrequest.StringSliceProvider
	// ExtraHeaderPrefixes are the head prefixes to check (case-insentively) for filling in
	// the user.Info.Extra.  All values of all matching headers will be added.
	ExtraHeaderPrefixes headerrequest.StringSliceProvider
	// CAContentProvider the options for verifying incoming connections using mTLS.  Generally this points to CA bundle file which is used verify the identity of the front proxy.
	//	It may produce different options at will.
	// 1、CAContentProvider是用于提供证书用的
	// 2、由于APIServer开启了双向认证，因此代理是需要配置证书的，所以我们需要给代理配置证书
	// 3、代理认证的流程为：client -> proxyAuth(代理认证服务器) -> APIServer，代理认证完成之后，必须要把用户相关的认证信息放到请求头当中
	// 4、CAContentProvider用于持续监听证书，一旦发现此证书发生变化，就会通知关心这个证书的所有组件
	CAContentProvider dynamiccertificates.CAContentProvider
	// AllowedClientNames is a list of common names that may be presented by the authenticating front proxy.  Empty means: accept any.
	AllowedClientNames headerrequest.StringSliceProvider
}
