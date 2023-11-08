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

package legacyscheme

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

var (
	// Scheme is the default instance of runtime.Scheme to which types in the Kubernetes API are already registered.
	// NOTE: If you are copying this file to start a new api group, STOP! Copy the
	// extensions group instead. This Scheme is special and should appear ONLY in
	// the api group, unless you really know what you're doing.
	// TODO(lavalamp): make the above error impossible.
	// 这里说的Legacy资源其实就是核心资源，这里注册的都是内部资源，也就是__internal资源
	// 2、有哪些资源对象注册进来了？ K8S所有已知的资源都会注册到这里
	Scheme = runtime.NewScheme()

	// Codecs provides access to encoding and decoding for the scheme
	// 所谓的编解码器，其实就是CodecFactory，可以用于获取每个GV的编解码器
	Codecs = serializer.NewCodecFactory(Scheme)

	// ParameterCodec handles versioning of objects that are converted to query parameters.
	// TODO 参数编解码器
	ParameterCodec = runtime.NewParameterCodec(Scheme)
)
