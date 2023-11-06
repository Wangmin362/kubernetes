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

package json

import (
	"encoding/json"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// MetaFactory is used to store and retrieve the version and kind
// information for JSON objects in a serializer.
// 1、用于从二进制信息当中获取GVK，显然这个二进制信息必须是资源对象序列化之后的二进制，否则转换肯定是失败的。
// 2、实现原理还是比较简单的，就是定义一个结构体，这个结构体只含有APIVersion以及Kind字段，然后根据这个结构体进行反序列换，如果能够反序列化
// 成功，就可以获取到这个资源对象的GVK
type MetaFactory interface {
	// Interpret should return the version and kind of the wire-format of
	// the object.
	Interpret(data []byte) (*schema.GroupVersionKind, error)
}

// DefaultMetaFactory is a default factory for versioning objects in JSON. The object
// in memory and in the default JSON serialization will use the "kind" and "apiVersion"
// fields.
var DefaultMetaFactory = SimpleMetaFactory{}

// SimpleMetaFactory provides default methods for retrieving the type and version of objects
// that are identified with an "apiVersion" and "kind" fields in their JSON
// serialization. It may be parameterized with the names of the fields in memory, or an
// optional list of base structs to search for those fields in memory.
type SimpleMetaFactory struct {
}

// Interpret will return the APIVersion and Kind of the JSON wire-format
// encoding of an object, or an error.
// 解析二进制对象的GVK信息
func (SimpleMetaFactory) Interpret(data []byte) (*schema.GroupVersionKind, error) {
	findKind := struct {
		// +optional
		APIVersion string `json:"apiVersion,omitempty"`
		// +optional
		Kind string `json:"kind,omitempty"`
	}{}
	if err := json.Unmarshal(data, &findKind); err != nil {
		return nil, fmt.Errorf("couldn't get version/kind; json parse error: %v", err)
	}
	// 解析APIVersion字段为GV
	gv, err := schema.ParseGroupVersion(findKind.APIVersion)
	if err != nil {
		return nil, err
	}
	return &schema.GroupVersionKind{Group: gv.Group, Version: gv.Version, Kind: findKind.Kind}, nil
}
