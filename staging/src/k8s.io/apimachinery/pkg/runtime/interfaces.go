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

package runtime

import (
	"io"
	"net/url"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// APIVersionInternal may be used if you are registering a type that should not
	// be considered stable or serialized - it is a convention only and has no
	// special behavior in this package.
	// TODO 如何理解K8S的internal版本
	APIVersionInternal = "__internal"
)

// GroupVersioner refines a set of possible conversion targets into a single option.
// TODO 如何理解这个接口的设计？
type GroupVersioner interface {
	// KindForGroupVersionKinds returns a desired target group version kind for the given input, or returns ok false if no
	// target is known. In general, if the return target is not in the input list, the caller is expected to invoke
	// Scheme.New(target) and then perform a conversion between the current Go type and the destination Go type.
	// Sophisticated implementations may use additional information about the input kinds to pick a destination kind.
	KindForGroupVersionKinds(kinds []schema.GroupVersionKind) (target schema.GroupVersionKind, ok bool)
	// Identifier returns string representation of the object.
	// Identifiers of two different encoders should be equal only if for every input
	// kinds they return the same result.
	Identifier() string
}

// Identifier represents an identifier.
// Identitier of two different objects should be equal if and only if for every
// input the output they produce is exactly the same.
type Identifier string

// Encoder writes objects to a serialized form
// TODO K8S资源的编解码
type Encoder interface {
	// Encode writes an object to a stream. Implementations may return errors if the versions are
	// incompatible, or if no conversion is defined.
	// 把obj对象序列化，然后把数据写入到w当中
	// TODO 需要注意的是obj对象是K8S内部对象（Internal）,K8S资源都需要先转为内部对象，然后才能序列化保存到ETCD当中
	Encode(obj Object, w io.Writer) error
	// Identifier returns an identifier of the encoder.
	// Identifiers of two different encoders should be equal if and only if for every input
	// object it will be encoded to the same representation by both of them.
	//
	// Identifier is intended for use with CacheableObject#CacheEncode method. In order to
	// correctly handle CacheableObject, Encode() method should look similar to below, where
	// doEncode() is the encoding logic of implemented encoder:
	//   func (e *MyEncoder) Encode(obj Object, w io.Writer) error {
	//     if co, ok := obj.(CacheableObject); ok {
	//       return co.CacheEncode(e.Identifier(), e.doEncode, w)
	//     }
	//     return e.doEncode(obj, w)
	//   }
	// TODO 为什么序列化需要 Identifier 这个东西？ 这玩意主要是用来干么的
	// Encode的结果是可以缓存的，TODO 暂时还是没有看懂序列化的缓存机制
	Identifier() Identifier
}

// MemoryAllocator is responsible for allocating memory.
// By encapsulating memory allocation into its own interface, we can reuse the memory
// across many operations in places we know it can significantly improve the performance.
// TODO 如何理解这个接口的设计？ 什么场合下需要这个接口？
type MemoryAllocator interface {
	// Allocate reserves memory for n bytes.
	// Note that implementations of this method are not required to zero the returned array.
	// It is the caller's responsibility to clean the memory if needed.
	Allocate(n uint64) []byte
}

// EncoderWithAllocator  serializes objects in a way that allows callers to manage any additional memory allocations.
type EncoderWithAllocator interface {
	Encoder
	// EncodeWithAllocator writes an object to a stream as Encode does.
	// In addition, it allows for providing a memory allocator for efficient memory usage during object serialization
	EncodeWithAllocator(obj Object, w io.Writer, memAlloc MemoryAllocator) error
}

// Decoder attempts to load an object from data.
// TODO 如何理解这个解码接口的设计,怎么感觉前两个返回值是多余的
type Decoder interface {
	// Decode attempts to deserialize the provided data using either the innate typing of the scheme or the
	// default kind, group, and version provided. It returns a decoded object as well as the kind, group, and
	// version from the serialized data, or an error. If into is non-nil, it will be used as the target type
	// and implementations may choose to use it rather than reallocating an object. However, the object is not
	// guaranteed to be populated. The returned object is not guaranteed to match into. If defaults are
	// provided, they are applied to the data by default. If no defaults or partial defaults are provided, the
	// type of the into may be used to guide conversion decisions.
	// 把二进制data数据反序列化到Object当中
	// TODO 如果用户提供了 defaults gvk，这个gvk是如何影响反序列化的流程的呢？ defaults参数是为了能够提供一些默认值，主要原因是因为data二进制
	// TODO 数据当中可能没有提供kind , group ,group全部信息，而是提供了其中的一部分，这个时候就需要通过defaults参数补全默认
	// TODO into参数的作用是啥？
	// TODO 注意，返回的Object对象是内部版本对象，后续要转为需要的K8S资源对象
	Decode(data []byte, defaults *schema.GroupVersionKind, into Object) (Object, *schema.GroupVersionKind, error)
}

// Serializer is the core interface for transforming objects into a serialized format and back.
// Implementations may choose to perform conversion of the object, but no assumptions should be made.
// TODO 可以参考这两篇文章加深立即：https://mp.weixin.qq.com/s/fJf1mtCR49XO7BOUn2FRTg  https://cloud.tencent.com/developer/article/1519826
// TODO 为什么K8S需要抽象Serializer，不是就是对象的序列化反序列化么？
// 答：1、一般我们在项目当中只需要把数据序列化、反序列化为json, yaml, protobuf其中的一种，而K8S需要同时考虑三种格式的序列化和反序列化，
// 因此需要抽象出一个标准的接口。在K8S当中，ETCD中保存的最终是json格式的数据，而当返回给用户的时候需要yaml格式的数据，如果进行gRPC通信，
// 那么需要protobuf格式的数据，因此一个标准的序列化、发序列化接口是非常有必要的   2、万一K8S需要支持一个新的格式的序列化、反序列化，此时
// 定义为接口的好处就体现出来了
type Serializer interface {
	// Encoder 相当于json.Marshal(&obj)，也就是我们常说的序列化，主要是把http请求body序列化之后存储在etcd当中
	Encoder
	// Decoder 相当于json.Unmarshal(data, &obj)，即我们常说的反序列化，用于把二进制数据转换为语言相关的类型，放在K8S项目中就是转换为各个类型的资源对象的go struct
	Decoder
}

// Codec is a Serializer that deals with the details of versioning objects. It offers the same
// interface as Serializer, so this is a marker to consumers that care about the version of the objects
// they receive.
type Codec Serializer

// ParameterCodec defines methods for serializing and deserializing API objects to url.Values and
// performing any necessary conversion. Unlike the normal Codec, query parameters are not self describing
// and the desired version must be specified.
// TODO 什么叫做参数的编解码器？
type ParameterCodec interface {
	// DecodeParameters takes the given url.Values in the specified group version and decodes them
	// into the provided object, or returns an error.
	DecodeParameters(parameters url.Values, from schema.GroupVersion, into Object) error
	// EncodeParameters encodes the provided object as query parameters or returns an error.
	EncodeParameters(obj Object, to schema.GroupVersion) (url.Values, error)
}

// Framer is a factory for creating readers and writers that obey a particular framing pattern.
// TODO 如何理解这个接口的设计？
type Framer interface {
	NewFrameReader(r io.ReadCloser) io.ReadCloser
	// NewFrameWriter TODO 为什么这里不是io.WriterCloser
	NewFrameWriter(w io.Writer) io.Writer
}

// SerializerInfo contains information about a specific serialization format
type SerializerInfo struct {
	// MediaType is the value that represents this serializer over the wire.
	MediaType string
	// MediaTypeType is the first part of the MediaType ("application" in "application/json").
	MediaTypeType string
	// MediaTypeSubType is the second part of the MediaType ("json" in "application/json").
	MediaTypeSubType string
	// EncodesAsText indicates this serializer can be encoded to UTF-8 safely.
	EncodesAsText bool
	// Serializer is the individual object serializer for this media type.
	Serializer Serializer
	// PrettySerializer, if set, can serialize this object in a form biased towards
	// readability.
	PrettySerializer Serializer
	// StrictSerializer, if set, deserializes this object strictly,
	// erring on unknown fields.
	StrictSerializer Serializer
	// StreamSerializer, if set, describes the streaming serialization format
	// for this media type.
	// TODO 什么叫做流序列化器
	StreamSerializer *StreamSerializerInfo
}

// StreamSerializerInfo contains information about a specific stream serialization format
type StreamSerializerInfo struct {
	// EncodesAsText indicates this serializer can be encoded to UTF-8 safely.
	EncodesAsText bool
	// Serializer is the top level object serializer for this type when streaming
	Serializer
	// Framer is the factory for retrieving streams that separate objects on the wire
	Framer
}

// NegotiatedSerializer is an interface used for obtaining encoders, decoders, and serializers
// for multiple supported media types. This would commonly be accepted by a server component
// that performs HTTP content negotiation to accept multiple formats.
// TODO 什么叫做协商序列化器？ 猜测应该是根据HTTP的请求头类型自动选择合适的序列化器
type NegotiatedSerializer interface {
	// SupportedMediaTypes is the media types supported for reading and writing single objects.
	// TODO 列出当前序列化器支持的媒体类型，这里的媒体类型指的是HTTP MIME
	// 可以参考：https://developer.mozilla.org/en-US/docs/Web/HTTP/Basics_of_HTTP/MIME_types
	// 不同的媒体类型其序列化的方式可能不一样，因此需要不同的序列化器
	SupportedMediaTypes() []SerializerInfo

	// EncoderForVersion returns an encoder that ensures objects being written to the provided
	// serializer are in the provided group version.
	// TODO 为什么这里是这么设计的？ 传入一个Encoder，并且返回一个Encoder?
	EncoderForVersion(serializer Encoder, gv GroupVersioner) Encoder
	// DecoderToVersion returns a decoder that ensures objects being read by the provided
	// serializer are in the provided group version by default.
	DecoderToVersion(serializer Decoder, gv GroupVersioner) Decoder
}

// ClientNegotiator handles turning an HTTP content type into the appropriate encoder.
// Use NewClientNegotiator or NewVersionedClientNegotiator to create this interface from
// a NegotiatedSerializer.
// TODO 用于根据HTTP的MIME类型找到此类型合适的encoder以及decoder
type ClientNegotiator interface {
	// Encoder returns the appropriate encoder for the provided contentType (e.g. application/json)
	// and any optional mediaType parameters (e.g. pretty=1), or an error. If no serializer is found
	// a NegotiateError will be returned. The current client implementations consider params to be
	// optional modifiers to the contentType and will ignore unrecognized parameters.
	// TODO contentType就是MIME，譬如application/json
	Encoder(contentType string, params map[string]string) (Encoder, error)
	// Decoder returns the appropriate decoder for the provided contentType (e.g. application/json)
	// and any optional mediaType parameters (e.g. pretty=1), or an error. If no serializer is found
	// a NegotiateError will be returned. The current client implementations consider params to be
	// optional modifiers to the contentType and will ignore unrecognized parameters.
	Decoder(contentType string, params map[string]string) (Decoder, error)
	// StreamDecoder returns the appropriate stream decoder for the provided contentType (e.g.
	// application/json) and any optional mediaType parameters (e.g. pretty=1), or an error. If no
	// serializer is found a NegotiateError will be returned. The Serializer and Framer will always
	// be returned if a Decoder is returned. The current client implementations consider params to be
	// optional modifiers to the contentType and will ignore unrecognized parameters.
	StreamDecoder(contentType string, params map[string]string) (Decoder, Serializer, Framer, error)
}

// StorageSerializer is an interface used for obtaining encoders, decoders, and serializers
// that can read and write data at rest. This would commonly be used by client tools that must
// read files, or server side storage interfaces that persist restful objects.
// TODO 这个接口似乎和 NegotiatedSerializer 接口非常类似，就多了一个UniversalDeserializer方法
type StorageSerializer interface {
	// SupportedMediaTypes are the media types supported for reading and writing objects.
	SupportedMediaTypes() []SerializerInfo

	// UniversalDeserializer returns a Serializer that can read objects in multiple supported formats
	// by introspecting the data at rest.
	// TODO 什么情况下会使用通用解码器？
	UniversalDeserializer() Decoder

	// EncoderForVersion returns an encoder that ensures objects being written to the provided
	// serializer are in the provided group version.
	EncoderForVersion(serializer Encoder, gv GroupVersioner) Encoder
	// DecoderForVersion returns a decoder that ensures objects being read by the provided
	// serializer are in the provided group version by default.
	DecoderToVersion(serializer Decoder, gv GroupVersioner) Decoder
}

// NestedObjectEncoder is an optional interface that objects may implement to be given
// an opportunity to encode any nested Objects / RawExtensions during serialization.
type NestedObjectEncoder interface {
	EncodeNestedObjects(e Encoder) error
}

// NestedObjectDecoder is an optional interface that objects may implement to be given
// an opportunity to decode any nested Objects / RawExtensions during serialization.
// It is possible for DecodeNestedObjects to return a non-nil error but for the decoding
// to have succeeded in the case of strict decoding errors (e.g. unknown/duplicate fields).
// As such it is important for callers of DecodeNestedObjects to check to confirm whether
// an error is a runtime.StrictDecodingError before short circuiting.
// Similarly, implementations of DecodeNestedObjects should ensure that a runtime.StrictDecodingError
// is only returned when the rest of decoding has succeeded.
type NestedObjectDecoder interface {
	DecodeNestedObjects(d Decoder) error
}

///////////////////////////////////////////////////////////////////////////////
// Non-codec interfaces

type ObjectDefaulter interface {
	// Default takes an object (must be a pointer) and applies any default values.
	// Defaulters may not error.
	Default(in Object)
}

type ObjectVersioner interface {
	ConvertToVersion(in Object, gv GroupVersioner) (out Object, err error)
}

// ObjectConvertor converts an object to a different version.
// TODO 用于K8S资源和内部资源之间的转换
type ObjectConvertor interface {
	// Convert attempts to convert one object into another, or returns an error. This
	// method does not mutate the in object, but the in and out object might share data structures,
	// i.e. the out object cannot be mutated without mutating the in object as well.
	// The context argument will be passed to all nested conversions.
	Convert(in, out, context interface{}) error
	// ConvertToVersion takes the provided object and converts it the provided version. This
	// method does not mutate the in object, but the in and out object might share data structures,
	// i.e. the out object cannot be mutated without mutating the in object as well.
	// This method is similar to Convert() but handles specific details of choosing the correct
	// output version.
	ConvertToVersion(in Object, gv GroupVersioner) (out Object, err error)
	ConvertFieldLabel(gvk schema.GroupVersionKind, label, value string) (string, string, error)
}

// ObjectTyper contains methods for extracting the APIVersion and Kind
// of objects.
// TODO ObjectTyper的一个实现就是: Scheme
// ObjectTyper的作用就是根据K8S资源对象，拿到这个对象的GVK
type ObjectTyper interface {
	// ObjectKinds returns the all possible group,version,kind of the provided object, true if
	// the object is unversioned, or an error if the object is not recognized
	// (IsNotRegisteredError will return true).
	// TODO K8S的一个资源对象的GVK为什么是复数
	ObjectKinds(Object) ([]schema.GroupVersionKind, bool, error)
	// Recognizes returns true if the scheme is able to handle the provided version and kind,
	// or more precisely that the provided version is a possible conversion or decoding
	// target.
	// TODO 能够被识别的GVK一定是通过Scheme提前进行注册了
	Recognizes(gvk schema.GroupVersionKind) bool
}

// ObjectCreater contains methods for instantiating an object by kind and version.
// TODO ObjectCreate比较简单，就是根据gvk创建K8S对象
type ObjectCreater interface {
	New(kind schema.GroupVersionKind) (out Object, err error)
}

// EquivalentResourceMapper provides information about resources that address the same underlying data as a specified resource
// TODO 什么叫等效资源映射器
type EquivalentResourceMapper interface {
	// EquivalentResourcesFor returns a list of resources that address the same underlying data as resource.
	// If subresource is specified, only equivalent resources which also have the same subresource are included.
	// The specified resource can be included in the returned list.
	EquivalentResourcesFor(resource schema.GroupVersionResource, subresource string) []schema.GroupVersionResource
	// KindFor returns the kind expected by the specified resource[/subresource].
	// A zero value is returned if the kind is unknown.
	KindFor(resource schema.GroupVersionResource, subresource string) schema.GroupVersionKind
}

// EquivalentResourceRegistry provides an EquivalentResourceMapper interface,
// and allows registering known resource[/subresource] -> kind
type EquivalentResourceRegistry interface {
	EquivalentResourceMapper
	// RegisterKindFor registers the existence of the specified resource[/subresource] along with its expected kind.
	RegisterKindFor(resource schema.GroupVersionResource, subresource string, kind schema.GroupVersionKind)
}

// ResourceVersioner provides methods for setting and retrieving
// the resource version from an API object.
type ResourceVersioner interface {
	SetResourceVersion(obj Object, version string) error
	ResourceVersion(obj Object) (string, error)
}

// Namer provides methods for retrieving name and namespace of an API object.
type Namer interface {
	// Name returns the name of a given object.
	Name(obj Object) (string, error)
	// Namespace returns the name of a given object.
	Namespace(obj Object) (string, error)
}

// Object interface must be supported by all API types registered with Scheme. Since objects in a scheme are
// expected to be serialized to the wire, the interface an Object must provide to the Scheme allows
// serializers to set the kind, version, and group the object is represented as. An Object may choose
// to return a no-op ObjectKindAccessor in cases where it is not expected to be serialized.
// TODO 非常重要的接口,K8S中所有的资源对象都实现了这个接口
type Object interface {
	GetObjectKind() schema.ObjectKind
	DeepCopyObject() Object
}

// CacheableObject allows an object to cache its different serializations
// to avoid performing the same serialization multiple times.
// TODO CacheableObject接口主要是用在什么场景下？
type CacheableObject interface {
	// CacheEncode writes an object to a stream. The <encode> function will
	// be used in case of cache miss. The <encode> function takes ownership
	// of the object.
	// If CacheableObject is a wrapper, then deep-copy of the wrapped object
	// should be passed to <encode> function.
	// CacheEncode assumes that for two different calls with the same <id>,
	// <encode> function will also be the same.
	CacheEncode(id Identifier, encode func(Object, io.Writer) error, w io.Writer) error
	// GetObject returns a deep-copy of an object to be encoded - the caller of
	// GetObject() is the owner of returned object. The reason for making a copy
	// is to avoid bugs, where caller modifies the object and forgets to copy it,
	// thus modifying the object for everyone.
	// The object returned by GetObject should be the same as the one that is supposed
	// to be passed to <encode> function in CacheEncode method.
	// If CacheableObject is a wrapper, the copy of wrapped object should be returned.
	GetObject() Object
}

// Unstructured objects store values as map[string]interface{}, with only values that can be serialized
// to JSON allowed.
type Unstructured interface {
	Object
	// NewEmptyInstance returns a new instance of the concrete type containing only kind/apiVersion and no other data.
	// This should be called instead of reflect.New() for unstructured types because the go type alone does not preserve kind/apiVersion info.
	NewEmptyInstance() Unstructured
	// UnstructuredContent returns a non-nil map with this object's contents. Values may be
	// []interface{}, map[string]interface{}, or any primitive type. Contents are typically serialized to
	// and from JSON. SetUnstructuredContent should be used to mutate the contents.
	UnstructuredContent() map[string]interface{}
	// SetUnstructuredContent updates the object content to match the provided map.
	SetUnstructuredContent(map[string]interface{})
	// IsList returns true if this type is a list or matches the list convention - has an array called "items".
	IsList() bool
	// EachListItem should pass a single item out of the list as an Object to the provided function. Any
	// error should terminate the iteration. If IsList() returns false, this method should return an error
	// instead of calling the provided function.
	EachListItem(func(Object) error) error
}
