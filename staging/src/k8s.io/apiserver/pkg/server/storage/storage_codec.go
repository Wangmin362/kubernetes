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

package storage

import (
	"fmt"
	"mime"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/recognizer"
	"k8s.io/apiserver/pkg/storage/storagebackend"
)

// StorageCodecConfig are the arguments passed to newStorageCodecFn
type StorageCodecConfig struct {
	StorageMediaType  string                    // 存储资源时，资源序列化的格式，目前K8S支持JSON, YAML, Protobuf三种格式
	StorageSerializer runtime.StorageSerializer // 所谓的存储序列化器，其实就是支持任意类型的媒体类型、任意资源的编解码
	StorageVersion    schema.GroupVersion       // TODO 什么叫做存储版本？
	MemoryVersion     schema.GroupVersion       // TODO 什么叫做内存版本？，所谓的MemoryVersion,似乎就是__internal version
	Config            storagebackend.Config     // 存储后端配置

	EncoderDecoratorFn func(runtime.Encoder) runtime.Encoder     // 编码装饰器
	DecoderDecoratorFn func([]runtime.Decoder) []runtime.Decoder // 解码装饰器
}

// NewStorageCodec assembles a storage codec for the provided storage media type, the provided serializer, and the requested
// storage and memory versions.
func NewStorageCodec(opts StorageCodecConfig) (runtime.Codec, runtime.GroupVersioner, error) {
	// 解析媒体类型
	mediaType, _, err := mime.ParseMediaType(opts.StorageMediaType)
	if err != nil {
		return nil, nil, fmt.Errorf("%q is not a valid mime-type", opts.StorageMediaType)
	}

	// 获取当前存储序列化器所支持的媒体类型
	supportedMediaTypes := opts.StorageSerializer.SupportedMediaTypes()
	// 根据需要的媒体类型，获取该媒体类型所对应的序列化器
	serializer, ok := runtime.SerializerInfoForMediaType(supportedMediaTypes, mediaType)
	if !ok {
		// 如果没有找到，返回提示信息
		supportedMediaTypeList := make([]string, len(supportedMediaTypes))
		for i, mediaType := range supportedMediaTypes {
			supportedMediaTypeList[i] = mediaType.MediaType
		}
		return nil, nil, fmt.Errorf("unable to find serializer for %q, supported media types: %v", mediaType, supportedMediaTypeList)
	}

	s := serializer.Serializer

	// Give callers the opportunity to wrap encoders and decoders.  For decoders, each returned decoder will
	// be passed to the recognizer so that multiple decoders are available.
	var encoder runtime.Encoder = s
	if opts.EncoderDecoratorFn != nil {
		// 把编码器进行包装一次
		encoder = opts.EncoderDecoratorFn(encoder)
	}
	decoders := []runtime.Decoder{
		// selected decoder as the primary
		// 根据媒体类型获取到的序列化器本身就是解码器
		s,
		// universal deserializer as a fallback
		// 获取存储序列化器的万能解码器
		opts.StorageSerializer.UniversalDeserializer(),
		// base64-wrapped universal deserializer as a last resort.
		// this allows reading base64-encoded protobuf, which should only exist if etcd2+protobuf was used at some point.
		// data written that way could exist in etcd2, or could have been migrated to etcd3.
		// TODO: flag this type of data if we encounter it, require migration (read to decode, write to persist using a supported encoder), and remove in 1.8
		// 利用base64先解码，然后再用解码器
		runtime.NewBase64Serializer(nil, opts.StorageSerializer.UniversalDeserializer()),
	}
	if opts.DecoderDecoratorFn != nil {
		// 应该是针对每个解码器都进行了一次包装
		decoders = opts.DecoderDecoratorFn(decoders)
	}

	encodeVersioner := runtime.NewMultiGroupVersioner(
		opts.StorageVersion,
		schema.GroupKind{Group: opts.StorageVersion.Group},
		schema.GroupKind{Group: opts.MemoryVersion.Group},
	)

	// Ensure the storage receives the correct version.
	// TODO 获取编码器
	encoder = opts.StorageSerializer.EncoderForVersion(
		encoder,
		encodeVersioner,
	)
	// TODO 获取解码器
	decoder := opts.StorageSerializer.DecoderToVersion(
		recognizer.NewDecoder(decoders...),
		// decode需要优先转为__internal版本
		runtime.NewCoercingMultiGroupVersioner(
			opts.MemoryVersion,
			schema.GroupKind{Group: opts.MemoryVersion.Group},
			schema.GroupKind{Group: opts.StorageVersion.Group},
		),
	)

	// 实例化编解码器
	return runtime.NewCodec(encoder, decoder), encodeVersioner, nil
}
