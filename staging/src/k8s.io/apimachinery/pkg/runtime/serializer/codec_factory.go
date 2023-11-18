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

package serializer

import (
	"mime"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/apimachinery/pkg/runtime/serializer/recognizer"
	"k8s.io/apimachinery/pkg/runtime/serializer/versioning"
)

// serializerExtensions are for serializers that are conditionally compiled in
// 这玩意似乎没有地方在初始化
var serializerExtensions = []func(*runtime.Scheme) (serializerType, bool){}

// 用于
type serializerType struct {
	AcceptContentTypes []string
	ContentType        string   // DA
	FileExtensions     []string // 这里说的是文件扩展名，譬如.json, .yaml, .pb扩展民
	// EncodesAsText should be true if this content type can be represented safely in UTF-8
	// 用于缓存当前的媒体类型是否能够以UTF-8的方式进行编码。譬如JSON就可以支持UTF-8编码，但是Protobuf不支持UTF-8便阿门
	EncodesAsText bool

	Serializer       runtime.Serializer // 普通序列化器，即Pretty=false, Strict=false
	PrettySerializer runtime.Serializer // 在普通序列化器的基础上配置了Pretty=true，将会影响编码器的行为
	StrictSerializer runtime.Serializer // 在普通序列化器的基础上配置了Strict=true，将会影响解码器的行为

	AcceptStreamContentTypes []string
	StreamContentType        string

	// TODO 详细分析Framer的作用
	Framer           runtime.Framer
	StreamSerializer runtime.Serializer
}

// 初始化序列化器，包括JSON、YAML、ProtoBuf序列化器
func newSerializersForScheme(
	scheme *runtime.Scheme, // 主要是换粗了GVK到Go结构体之间的映射关系
	mf json.MetaFactory, // 用于从二进制信息当中解析出该资源对象的GVK信息
	options CodecFactoryOptions, // 是否启用Strict, 以及Pretty
) []serializerType {
	// 实例化JSON序列化器，用于把结构体序列化为二进制数据，或者反序列化二进制数据为结构体
	jsonSerializer := json.NewSerializerWithOptions(
		mf, scheme, scheme,
		json.SerializerOptions{Yaml: false, Pretty: false, Strict: options.Strict},
	)
	jsonSerializerType := serializerType{
		AcceptContentTypes: []string{runtime.ContentTypeJSON},
		ContentType:        runtime.ContentTypeJSON, // 只接受把Content-Type:application/json的数据进行序列化、反序列化
		FileExtensions:     []string{"json"},        // 文件扩展名为json
		EncodesAsText:      true,                    // JSON支持以UTF-8的方式进行编码
		Serializer:         jsonSerializer,

		Framer:           json.Framer,
		StreamSerializer: jsonSerializer,
	}
	if options.Pretty {
		// Pretty序列化
		jsonSerializerType.PrettySerializer = json.NewSerializerWithOptions(
			mf, scheme, scheme,
			json.SerializerOptions{Yaml: false, Pretty: true, Strict: options.Strict},
		)
	}

	// 严格序列化器
	strictJSONSerializer := json.NewSerializerWithOptions(
		mf, scheme, scheme,
		json.SerializerOptions{Yaml: false, Pretty: false, Strict: true},
	)
	jsonSerializerType.StrictSerializer = strictJSONSerializer

	// 实例化YAML序列化器，支持把结构体对象以YAML的格式序列化为二进制数据，也支持把二进制数据以YAML的方式发序列化为结构体
	yamlSerializer := json.NewSerializerWithOptions(
		mf, scheme, scheme,
		json.SerializerOptions{Yaml: true, Pretty: false, Strict: options.Strict},
	)
	// 由于YAML本身就是易读的，因此不需要初始化PrettySerializer
	strictYAMLSerializer := json.NewSerializerWithOptions(
		mf, scheme, scheme,
		json.SerializerOptions{Yaml: true, Pretty: false, Strict: true},
	)
	protoSerializer := protobuf.NewSerializer(scheme, scheme)
	protoRawSerializer := protobuf.NewRawSerializer(scheme, scheme)

	serializers := []serializerType{
		// json序列化器
		jsonSerializerType,
		// YAML序列化器
		{
			AcceptContentTypes: []string{runtime.ContentTypeYAML},
			ContentType:        runtime.ContentTypeYAML,
			FileExtensions:     []string{"yaml"},
			EncodesAsText:      true,
			Serializer:         yamlSerializer,
			StrictSerializer:   strictYAMLSerializer,
		},
		// protobuf序列化器
		{
			AcceptContentTypes: []string{runtime.ContentTypeProtobuf},
			ContentType:        runtime.ContentTypeProtobuf,
			FileExtensions:     []string{"pb"},
			Serializer:         protoSerializer,
			// note, strict decoding is unsupported for protobuf,
			// fall back to regular serializing
			StrictSerializer: protoSerializer,

			Framer:           protobuf.LengthDelimitedFramer,
			StreamSerializer: protoRawSerializer,
		},
	}

	for _, fn := range serializerExtensions {
		if serializer, ok := fn(scheme); ok {
			serializers = append(serializers, serializer)
		}
	}
	return serializers
}

// CodecFactory provides methods for retrieving codecs and serializers for specific
// versions and content types.
// TODO 如何理解CodecFactory的设计，完成了什么功能？
// 1、编解码器工厂，其实就是对于scheme中所有注册资源，都可以支持编码、解码操作
type CodecFactory struct {
	scheme    *runtime.Scheme
	universal runtime.Decoder          // 所谓的通用编解码器，实际上就是万能编解码器，可能解码所有类型的数据
	accepts   []runtime.SerializerInfo // 不同的媒体类型所对应的序列化器，目前主要是针对JSON, YAML, ProtoBuf这三种媒体类型

	// 所谓的legacy序列化器，其实就是JSON序列化器
	legacySerializer runtime.Serializer
}

// CodecFactoryOptions holds the options for configuring CodecFactory behavior
type CodecFactoryOptions struct {
	// Strict configures all serializers in strict mode
	// Strict应用于Decode接口，表示严谨的。那什么是严谨的？笔者很难用语言表达，但是以下几种情况是不严谨的：
	// 1. 存在重复字段，比如{"value":1,"value":1};
	// 2. 不存在的字段，比如{"unknown": 1}，而目标API对象中不存在Unknown属性;
	// 3. 未打标签字段，比如{"Other":"test"}，虽然目标API对象中有Other字段，但是没有打`json:"Other"`标签
	// Strict选项可以理解为增加了很多校验，请注意，启用此选项的性能下降非常严重，因此不应在性能敏感的场景中使用。
	// 那什么场景需要用到Strict选项？比如Kubernetes各个服务的配置API，对性能要求不高，但需要严格的校验。
	Strict bool
	// Pretty includes a pretty serializer along with the non-pretty one
	// 此配置仅仅针对于编码，即序列化的时候
	// 这个设置仅仅针对于JSON序列化器有用，因为YAML格式的格式本身就是易读的。对于JSON数据来说，如果Pretty为false，那么序列化的数据可能就是这样：
	// {"name":"zhangsan", "age":27}, 而如果设置为true，那么这个数据就会是这样：
	/*
		{
			"name": "zhangsan",
			"age": 27

		}
	*/
	Pretty bool
}

// CodecFactoryOptionsMutator takes a pointer to an options struct and then modifies it.
// Functions implementing this type can be passed to the NewCodecFactory() constructor.
// CodecFactoryOptionsMutator就是用来修改CodecFactoryOptions参数
type CodecFactoryOptionsMutator func(*CodecFactoryOptions)

// EnablePretty enables including a pretty serializer along with the non-pretty one
func EnablePretty(options *CodecFactoryOptions) {
	options.Pretty = true
}

// DisablePretty disables including a pretty serializer along with the non-pretty one
func DisablePretty(options *CodecFactoryOptions) {
	options.Pretty = false
}

// EnableStrict enables configuring all serializers in strict mode
func EnableStrict(options *CodecFactoryOptions) {
	options.Strict = true
}

// DisableStrict disables configuring all serializers in strict mode
func DisableStrict(options *CodecFactoryOptions) {
	options.Strict = false
}

// NewCodecFactory provides methods for retrieving serializers for the supported wire formats
// and conversion wrappers to define preferred internal and external versions. In the future,
// as the internal version is used less, callers may instead use a defaulting serializer and
// only convert objects which are shared internally (Status, common API machinery).
//
// Mutators can be passed to change the CodecFactoryOptions before construction of the factory.
// It is recommended to explicitly pass mutators instead of relying on defaults.
// By default, Pretty is enabled -- this is conformant with previously supported behavior.
//
// TODO: allow other codecs to be compiled in?
// TODO: accept a scheme interface
func NewCodecFactory(scheme *runtime.Scheme, mutators ...CodecFactoryOptionsMutator) CodecFactory {
	options := CodecFactoryOptions{Pretty: true}
	for _, fn := range mutators {
		fn(&options)
	}

	// 初始化编解码器，包括JSON、YAML、ProtoBuf序列化器
	serializers := newSerializersForScheme(scheme, json.DefaultMetaFactory, options)
	// 实例化编解码器工厂，编解码工厂可以支持所有注册到scheme中资源的编码、解码
	return newCodecFactory(scheme, serializers)
}

// newCodecFactory is a helper for testing that allows a different metafactory to be specified.
func newCodecFactory(scheme *runtime.Scheme, serializers []serializerType) CodecFactory {
	decoders := make([]runtime.Decoder, 0, len(serializers))
	var accepts []runtime.SerializerInfo
	// 表示已经接受的媒体类型，所谓已经接受，其实就是这个媒体类型已经注册了对应的编解码器
	alreadyAccepted := make(map[string]struct{})

	var legacySerializer runtime.Serializer
	for _, d := range serializers {
		// Serializer本身既是一个Decoder，也是一个Encoder
		decoders = append(decoders, d.Serializer)
		for _, mediaType := range d.AcceptContentTypes {
			if _, ok := alreadyAccepted[mediaType]; ok {
				// 当前媒体类型已经注册编解码器，无需重新注册
				continue
			}
			alreadyAccepted[mediaType] = struct{}{}
			info := runtime.SerializerInfo{
				MediaType:        d.ContentType,
				EncodesAsText:    d.EncodesAsText,
				Serializer:       d.Serializer,
				PrettySerializer: d.PrettySerializer,
				StrictSerializer: d.StrictSerializer,
			}

			// 解析MediaType
			mediaType, _, err := mime.ParseMediaType(info.MediaType)
			if err != nil {
				panic(err)
			}
			parts := strings.SplitN(mediaType, "/", 2)
			info.MediaTypeType = parts[0]
			info.MediaTypeSubType = parts[1]

			if d.StreamSerializer != nil {
				info.StreamSerializer = &runtime.StreamSerializerInfo{
					Serializer:    d.StreamSerializer,
					EncodesAsText: d.EncodesAsText,
					Framer:        d.Framer,
				}
			}
			accepts = append(accepts, info)
			if mediaType == runtime.ContentTypeJSON {
				legacySerializer = d.Serializer
			}
		}
	}
	if legacySerializer == nil {
		legacySerializer = serializers[0].Serializer
	}

	return CodecFactory{
		scheme:    scheme,
		universal: recognizer.NewDecoder(decoders...),

		accepts: accepts,

		legacySerializer: legacySerializer,
	}
}

// WithoutConversion returns a NegotiatedSerializer that performs no conversion, even if the
// caller requests it.
func (f CodecFactory) WithoutConversion() runtime.NegotiatedSerializer {
	return WithoutConversionCodecFactory{f}
}

// SupportedMediaTypes returns the RFC2046 media types that this factory has serializers for.
// 返回支持的媒体类型
func (f CodecFactory) SupportedMediaTypes() []runtime.SerializerInfo {
	return f.accepts
}

// LegacyCodec encodes output to a given API versions, and decodes output into the internal form from
// any recognized source. The returned codec will always encode output to JSON. If a type is not
// found in the list of versions an error will be returned.
//
// This method is deprecated - clients and servers should negotiate a serializer by mime-type and
// invoke CodecForVersions. Callers that need only to read data should use UniversalDecoder().
//
// TODO: make this call exist only in pkg/api, and initialize it with the set of default versions.
// All other callers will be forced to request a Codec directly.
func (f CodecFactory) LegacyCodec(version ...schema.GroupVersion) runtime.Codec {
	return versioning.NewDefaultingCodecForScheme(f.scheme, f.legacySerializer, f.universal, schema.GroupVersions(version), runtime.InternalGroupVersioner)
}

// UniversalDeserializer can convert any stored data recognized by this factory into a Go object that satisfies
// runtime.Object. It does not perform conversion. It does not perform defaulting.
func (f CodecFactory) UniversalDeserializer() runtime.Decoder {
	return f.universal
}

// UniversalDecoder returns a runtime.Decoder capable of decoding all known API objects in all known formats. Used
// by clients that do not need to encode objects but want to deserialize API objects stored on disk. Only decodes
// objects in groups registered with the scheme. The GroupVersions passed may be used to select alternate
// versions of objects to return - by default, runtime.APIVersionInternal is used. If any versions are specified,
// unrecognized groups will be returned in the version they are encoded as (no conversion). This decoder performs
// defaulting.
//
// TODO: the decoder will eventually be removed in favor of dealing with objects in their versioned form
// TODO: only accept a group versioner
func (f CodecFactory) UniversalDecoder(versions ...schema.GroupVersion) runtime.Decoder {
	var versioner runtime.GroupVersioner
	if len(versions) == 0 {
		versioner = runtime.InternalGroupVersioner
	} else {
		versioner = schema.GroupVersions(versions)
	}
	return f.CodecForVersions(nil, f.universal, nil, versioner)
}

// CodecForVersions creates a codec with the provided serializer. If an object is decoded and its group is not in the list,
// it will default to runtime.APIVersionInternal. If encode is not specified for an object's group, the object is not
// converted. If encode or decode are nil, no conversion is performed.
func (f CodecFactory) CodecForVersions(
	encoder runtime.Encoder, // 编码器
	decoder runtime.Decoder, // 解码器
	encode runtime.GroupVersioner,
	decode runtime.GroupVersioner,
) runtime.Codec {
	// TODO: these are for backcompat, remove them in the future
	if encode == nil {
		encode = runtime.DisabledGroupVersioner
	}
	if decode == nil {
		// 默认解码为__internal版本
		decode = runtime.InternalGroupVersioner
	}
	return versioning.NewDefaultingCodecForScheme(f.scheme, encoder, decoder, encode, decode)
}

// DecoderToVersion returns a decoder that targets the provided group version.
func (f CodecFactory) DecoderToVersion(decoder runtime.Decoder, gv runtime.GroupVersioner) runtime.Decoder {
	return f.CodecForVersions(nil, decoder, nil, gv)
}

// EncoderForVersion returns an encoder that targets the provided group version.
func (f CodecFactory) EncoderForVersion(encoder runtime.Encoder, gv runtime.GroupVersioner) runtime.Encoder {
	return f.CodecForVersions(encoder, nil, gv, nil)
}

// WithoutConversionCodecFactory is a CodecFactory that will explicitly ignore requests to perform conversion.
// This wrapper is used while code migrates away from using conversion (such as external clients) and in the future
// will be unnecessary when we change the signature of NegotiatedSerializer.
type WithoutConversionCodecFactory struct {
	CodecFactory
}

// EncoderForVersion returns an encoder that does not do conversion, but does set the group version kind of the object
// when serialized.
func (f WithoutConversionCodecFactory) EncoderForVersion(serializer runtime.Encoder, version runtime.GroupVersioner) runtime.Encoder {
	return runtime.WithVersionEncoder{
		Version:     version,
		Encoder:     serializer,
		ObjectTyper: f.CodecFactory.scheme,
	}
}

// DecoderToVersion returns an decoder that does not do conversion.
func (f WithoutConversionCodecFactory) DecoderToVersion(serializer runtime.Decoder, _ runtime.GroupVersioner) runtime.Decoder {
	return runtime.WithoutVersionDecoder{
		Decoder: serializer,
	}
}
