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
	APIVersionInternal = "__internal"
)

// GroupVersioner refines a set of possible conversion targets into a single option.
// TODO 这玩意干嘛的？
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
// 1、K8S中的编码器用于把结构体资源对象序列化
// 2、与之不同的，K8S还设计了一个Identifier接口，此接口用于标识不同的编码器。需要注意的是，相同的编码器，其返回的标识一定是相同的。它们
// 对于一个对象进行序列化的结果也一定是一样的。在K8S当中，编码器的标识其实就是一个字符串，并没有特别稀奇。
// 3、之所以设计Identifier接口，主要是想缓存一个对象的序列化结果，而在K8S当汇总，由于支持YAML, JSON, Protobuf类型的数据，因此就有不同
// 的编码器，而且由于YAML, JSON的pretty, strict设置，还可以衍生出更多的编码器，因此要缓存对象的编码结果，就必须知道当前缓存的数据是哪个
// 编码器的结果，因此如何区分编码器就显得非常关键了，K8S就是通过Identifier接口来区分不同的编码器，只要名称相同，对同一个对象进行编码其结果
// 一定是相同的
// 4、在K8S当中，编码相比于解码简单许多，譬如对于JSON编码器，只需要执行json.Marshal类似的动作即可，对于YAML, Protobuf也是类似的，不需要
// 做额外的判断
type Encoder interface {
	// Encode writes an object to a stream. Implementations may return errors if the versions are
	// incompatible, or if no conversion is defined.
	// 把资源对象序列化到w当中
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
	Identifier() Identifier
}

// MemoryAllocator is responsible for allocating memory.
// By encapsulating memory allocation into its own interface, we can reuse the memory
// across many operations in places we know it can significantly improve the performance.
// 之所以能够提升性能是因为内存的复用
type MemoryAllocator interface {
	// Allocate reserves memory for n bytes.
	// Note that implementations of this method are not required to zero the returned array.
	// It is the caller's responsibility to clean the memory if needed.
	Allocate(n uint64) []byte
}

// EncoderWithAllocator  serializes objects in a way that allows callers to manage any additional memory allocations.
// TODO 序列化消耗了什么内存？ 消耗了多大的内存？ 为什么K8S开发者会想着优化内存使用以提升性能？
type EncoderWithAllocator interface {
	Encoder
	// EncodeWithAllocator writes an object to a stream as Encode does.
	// In addition, it allows for providing a memory allocator for efficient memory usage during object serialization
	EncodeWithAllocator(obj Object, w io.Writer, memAlloc MemoryAllocator) error
}

// Decoder attempts to load an object from data.
// 1、在K8S当中解码是一个非常复杂的过程，因为K8S的资源非常多，而一个二进制数据一般只对应于一种结构。因此在解码复杂的原因就是需要根据二进制数据
// 找到此数据对应的结构体，此外，我们还需要根据数据的特征判断应当使用JSON、YAML、Protobuf解码器的哪一种。这也是解码的复杂所在。
type Decoder interface {
	// Decode attempts to deserialize the provided data using either the innate typing of the scheme or the
	// default kind, group, and version provided. It returns a decoded object as well as the kind, group, and
	// version from the serialized data, or an error. If into is non-nil, it will be used as the target type
	// and implementations may choose to use it rather than reallocating an object. However, the object is not
	// guaranteed to be populated. The returned object is not guaranteed to match into. If defaults are
	// provided, they are applied to the data by default. If no defaults or partial defaults are provided, the
	// type of the into may be used to guide conversion decisions.
	// 1、把data反序列化到into对象当中，至于序列化到哪个结构体当中是通过defaults、into、data决定的，如果指定了defaults，那么就是用
	// defaults所对应的GVK结构体进行序列化。如果defaults为空，但是into不为空，那么根据into对象中提取到的GVK信息进行反序列化。如果
	// defaults没有指定，并且into也为空，那么只能指望data，如果能从data中提取到GVK，那么也能自己实例化一个资源对象，然后作为返回值传出。
	// 如果data对象中也不能提取出任何GVK相关的信息，那么这次序列化只能宣告失败
	// 2、反序列化最终的Go结构体主要受到三个因素的影响，正好就是输入的三个参数。data为K8S的资源对象序列化的二进制信息，这个信息一般是从
	// ETCD当中查询出来的。其二就是指定的默认的GVK defaults参数。其三就是希望反序列化的Go类型。在序列化时，首先会从data数据当中获取
	// 到GVK信息，如果指定了默认的GVK，那么根据默认的GVK补全GVK信息。如果into指定的类型为非结构化对象，那么进行非结构化的反序列化。
	// 如果指定的into对象是结构化的，并且如果通过data计算出来的GVK信息就是into的GKV，那么就直接反序列化到Into对象当中，否则，就自己实例化
	// 一个新的资源对象，然后进行反序列化。
	Decode(data []byte, defaults *schema.GroupVersionKind, into Object) (out Object, gvk *schema.GroupVersionKind, err error)
}

// Serializer is the core interface for transforming objects into a serialized format and back.
// Implementations may choose to perform conversion of the object, but no assumptions should be made.
// 1、所谓的序列化器，其实就是结合了编码、解码两个动作。
// 2、在K8S当中，主要有两个地方需要用到解码器，一个是在解析用户请求,k8s需要把收到的YAML数据解码为合适的结构体对象，另外一个地方则是从ETCD当中
// 获取到了查询的数据，由于ETCD中保存的是序列化之后的数据，因此我们也需要把这个序列化之后的数据进行反序列化，我们把这个过程称之为反序列化，
// 也可以叫做解码
// 3、在K8S当中，主要有两个两个地方需要用的编码器，一个实在响应用户请求，K8S需要把结构体序列化之后交给请求。另外一个则是在需要把数据存储在ETCD
// 中时，也需要使用编码器把结构体对象编码为字节数据。我们把这个过程称之为序列化，也可以称之为编码。
type Serializer interface {
	Encoder
	Decoder
}

// Codec is a Serializer that deals with the details of versioning objects. It offers the same
// interface as Serializer, so this is a marker to consumers that care about the version of the objects
// they receive.
type Codec Serializer

// ParameterCodec defines methods for serializing and deserializing API objects to url.Values and
// performing any necessary conversion. Unlike the normal Codec, query parameters are not self describing
// and the desired version must be specified.
type ParameterCodec interface {
	// DecodeParameters takes the given url.Values in the specified group version and decodes them
	// into the provided object, or returns an error.
	DecodeParameters(parameters url.Values, from schema.GroupVersion, into Object) error
	// EncodeParameters encodes the provided object as query parameters or returns an error.
	EncodeParameters(obj Object, to schema.GroupVersion) (url.Values, error)
}

// Framer is a factory for creating readers and writers that obey a particular framing pattern.
type Framer interface {
	NewFrameReader(r io.ReadCloser) io.ReadCloser
	NewFrameWriter(w io.Writer) io.Writer
}

// SerializerInfo contains information about a specific serialization format
// 所谓的序列化信息，就是根据MediaType, MediaTypeType, MediaTypeSubType决定到底使用哪个序列化器
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
// TODO 如何理解这玩意的抽象？
type NegotiatedSerializer interface {
	// SupportedMediaTypes is the media types supported for reading and writing single objects.
	// 当前序列化工厂支持哪些媒体类型的序列化、反序列化
	SupportedMediaTypes() []SerializerInfo

	// EncoderForVersion returns an encoder that ensures objects being written to the provided
	// serializer are in the provided group version.
	EncoderForVersion(serializer Encoder, gv GroupVersioner) Encoder
	// DecoderToVersion returns a decoder that ensures objects being read by the provided
	// serializer are in the provided group version by default.
	DecoderToVersion(serializer Decoder, gv GroupVersioner) Decoder
}

// ClientNegotiator handles turning an HTTP content type into the appropriate encoder.
// Use NewClientNegotiator or NewVersionedClientNegotiator to create this interface from
// a NegotiatedSerializer.
type ClientNegotiator interface {
	// Encoder returns the appropriate encoder for the provided contentType (e.g. application/json)
	// and any optional mediaType parameters (e.g. pretty=1), or an error. If no serializer is found
	// a NegotiateError will be returned. The current client implementations consider params to be
	// optional modifiers to the contentType and will ignore unrecognized parameters.
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
// TODO 如何理解这个存储序列化器抽象
type StorageSerializer interface {
	// SupportedMediaTypes are the media types supported for reading and writing objects.
	SupportedMediaTypes() []SerializerInfo

	// UniversalDeserializer returns a Serializer that can read objects in multiple supported formats
	// by introspecting the data at rest.
	UniversalDeserializer() Decoder

	// EncoderForVersion returns an encoder that ensures objects being written to the provided
	// serializer are in the provided group version.
	EncoderForVersion(serializer Encoder, gv GroupVersioner) Encoder
	// DecoderToVersion returns a decoder that ensures objects being read by the provided
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

// ObjectDefaulter 通过默认的方法为资源对象设置默认值
type ObjectDefaulter interface {
	// Default takes an object (must be a pointer) and applies any default values.
	// Defaulters may not error.
	Default(in Object)
}

type ObjectVersioner interface {
	ConvertToVersion(in Object, gv GroupVersioner) (out Object, err error)
}

// ObjectConvertor converts an object to a different version.
// TODO 这玩意干嘛的
type ObjectConvertor interface {
	// Convert attempts to convert one object into another, or returns an error. This
	// method does not mutate the in object, but the in and out object might share data structures,
	// i.e. the out object cannot be mutated without mutating the in object as well.
	// The context argument will be passed to all nested conversions.
	// 把in对象转换为out对象
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
// 1、用于获取一个资源对象的GVK
// 2、用于根据一个资源的GVK判断这个资源是否识别
// 3、runtime.Scheme实现了这个接口
type ObjectTyper interface {
	// ObjectKinds returns the all possible group,version,kind of the provided object, true if
	// the object is unversioned, or an error if the object is not recognized
	// (IsNotRegisteredError will return true).
	// 通过反射获取当前对象的GVK，并返回当前资源是否是UnversionedType
	ObjectKinds(Object) ([]schema.GroupVersionKind, bool, error)
	// Recognizes returns true if the scheme is able to handle the provided version and kind,
	// or more precisely that the provided version is a possible conversion or decoding
	// target.
	// 判断当前GVK是否是已经注册过的GVK
	Recognizes(gvk schema.GroupVersionKind) bool
}

// ObjectCreater contains methods for instantiating an object by kind and version.
// 根据GVK实例化一个资源对象，该资源对象可能是有版本的，也可能是没有版本的。如果此资源没有注册过，那么返回错误
type ObjectCreater interface {
	New(kind schema.GroupVersionKind) (out Object, err error)
}

// EquivalentResourceMapper provides information about resources that address the same underlying data as a specified resource
// TODO 什么叫做等效资源？ 它是如何抽象的？
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
// 1、用于缓存子资源到Kind的映射  TODO 为什么需要这种映射关系呢？
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
// 用于获取资源对象的名字或者名称空间
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
// 1、K8S当中的顶级接口，所有的资源都是runtime.Object
// 2、K8S专门设计了deepcopy-gen代码生成器为资源对象生成这两个接口，资源对象只需要打上// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object注释即可
type Object interface {
	GetObjectKind() schema.ObjectKind
	DeepCopyObject() Object
}

// CacheableObject allows an object to cache its different serializations
// to avoid performing the same serialization multiple times.
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
