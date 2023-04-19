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
	"io"
	"strconv"

	kjson "sigs.k8s.io/json"
	"sigs.k8s.io/yaml"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer/recognizer"
	"k8s.io/apimachinery/pkg/util/framer"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/klog/v2"
)

// NewSerializer creates a JSON serializer that handles encoding versioned objects into the proper JSON form. If typer
// is not nil, the object has the group, version, and kind fields set.
// Deprecated: use NewSerializerWithOptions instead.
func NewSerializer(meta MetaFactory, creater runtime.ObjectCreater, typer runtime.ObjectTyper, pretty bool) *Serializer {
	return NewSerializerWithOptions(meta, creater, typer, SerializerOptions{false, pretty, false})
}

// NewYAMLSerializer creates a YAML serializer that handles encoding versioned objects into the proper YAML form. If typer
// is not nil, the object has the group, version, and kind fields set. This serializer supports only the subset of YAML that
// matches JSON, and will error if constructs are used that do not serialize to JSON.
// Deprecated: use NewSerializerWithOptions instead.
func NewYAMLSerializer(meta MetaFactory, creater runtime.ObjectCreater, typer runtime.ObjectTyper) *Serializer {
	return NewSerializerWithOptions(meta, creater, typer, SerializerOptions{true, false, false})
}

// NewSerializerWithOptions creates a JSON/YAML serializer that handles encoding versioned objects into the proper JSON/YAML
// form. If typer is not nil, the object has the group, version, and kind fields set. Options are copied into the Serializer
// and are immutable.
func NewSerializerWithOptions(meta MetaFactory, creater runtime.ObjectCreater, typer runtime.ObjectTyper, options SerializerOptions) *Serializer {
	return &Serializer{
		meta:       meta,
		creater:    creater,
		typer:      typer,
		options:    options,
		identifier: identifier(options),
	}
}

// identifier computes Identifier of Encoder based on the given options.
func identifier(options SerializerOptions) runtime.Identifier {
	result := map[string]string{
		"name":   "json",
		"yaml":   strconv.FormatBool(options.Yaml),
		"pretty": strconv.FormatBool(options.Pretty),
		"strict": strconv.FormatBool(options.Strict),
	}
	identifier, err := json.Marshal(result)
	if err != nil {
		klog.Fatalf("Failed marshaling identifier for json Serializer: %v", err)
	}
	return runtime.Identifier(identifier)
}

// SerializerOptions holds the options which are used to configure a JSON/YAML serializer.
// example:
// (1) To configure a JSON serializer, set `Yaml` to `false`.
// (2) To configure a YAML serializer, set `Yaml` to `true`.
// (3) To configure a strict serializer that can return strictDecodingError, set `Strict` to `true`.
type SerializerOptions struct {
	// Yaml: configures the Serializer to work with JSON(false) or YAML(true).
	// When `Yaml` is enabled, this serializer only supports the subset of YAML that
	// matches JSON, and will error if constructs are used that do not serialize to JSON.
	// json序列化器同时支持json以及yaml格式的序列化
	// TODO 有了这个参数，为什么还需要YamlSerializer 实际上，这个参数的意思是二进制数据的存储格式，二进制可能存储的是yaml格式，也可能存储
	// 的是json格式，这里做了通用的处理
	Yaml bool

	// Pretty: configures a JSON enabled Serializer(`Yaml: false`) to produce human-readable output.
	// This option is silently ignored when `Yaml` is `true`.
	// 这个参数应该是在decode当中使用的， 用于更好的展示给用户，其实就是方便人读
	Pretty bool

	// Strict: configures the Serializer to return strictDecodingError's when duplicate fields are present decoding JSON or YAML.
	// Note that enabling this option is not as performant as the non-strict variant, and should not be used in fast paths.
	// Strict应用于Decode接口，表示严谨的。那什么是严谨的？笔者很难用语言表达，但是以下几种情况是不严谨的：
	// 1. 存在重复字段，比如{"value":1,"value":1};
	// 2. 不存在的字段，比如{"unknown": 1}，而目标API对象中不存在Unknown属性;
	// 3. 未打标签字段，比如{"Other":"test"}，虽然目标API对象中有Other字段，但是没有打`json:"Other"`标签
	// Strict选项可以理解为增加了很多校验，请注意，启用此选项的性能下降非常严重，因此不应在性能敏感的场景中使用。
	// 那什么场景需要用到Strict选项？比如Kubernetes各个服务的配置API，对性能要求不高，但需要严格的校验。
	Strict bool
}

// Serializer handles encoding versioned objects into the proper JSON form
type Serializer struct {
	// 实际上就是解析二进制数据，得到该数据的GVK，实际上MetaFactory放在这里并不合适，取名为MetaParser更加合适
	meta MetaFactory
	// 一些加单的参数，影响序列化、反序列化的过程
	options SerializerOptions
	// 根据名字不难理解，实际上就是为了创建一个go struct，然后通过序列化的方式进行字段填充
	creater runtime.ObjectCreater
	// 根据资源对象拿到对象的GVK
	typer runtime.ObjectTyper

	// TODO 暂时还不理解这个参数的含义
	identifier runtime.Identifier
}

// Serializer implements Serializer
var _ runtime.Serializer = &Serializer{}
var _ recognizer.RecognizingDecoder = &Serializer{}

// gvkWithDefaults returns group kind and version defaulting from provided default
func gvkWithDefaults(actual, defaultGVK schema.GroupVersionKind) schema.GroupVersionKind {
	// actual为真是反序列化得到的
	if len(actual.Kind) == 0 {
		actual.Kind = defaultGVK.Kind
	}
	if len(actual.Version) == 0 && len(actual.Group) == 0 {
		actual.Group = defaultGVK.Group
		actual.Version = defaultGVK.Version
	}
	if len(actual.Version) == 0 && actual.Group == defaultGVK.Group {
		actual.Version = defaultGVK.Version
	}
	return actual
}

// Decode attempts to convert the provided data into YAML or JSON, extract the stored schema kind, apply the provided default gvk, and then
// load that data into an object matching the desired schema kind or the provided into.
// If into is *runtime.Unknown, the raw data will be extracted and no decoding will be performed.
// 如果into对象是 *runtime.Unknown 类型，那么originalData数据将不会被解码
// If into is not registered with the typer, then the object will be straight decoded using normal JSON/YAML unmarshalling.
// 如果into对象没有被注册，那么直接把originalData数据按照into的结构进行反序列化
// If into is provided and the original data is not fully qualified with kind/version/group, the type of the into will be used to alter the returned gvk.
// 如果originalData中没有完整的GVK数据，并且into对象不为空，那么会使用Into的GVK设置originalData的GVK
// If into is nil or data's gvk different from into's gvk, it will generate a new Object with ObjectCreater.New(gvk)
// 如果into是空的，但是提供了默认的gvk，将会使用提供的gvk补全originalData的gvk
// On success or most errors, the method will return the calculated schema kind.
// The gvk calculate priority will be originalData > default gvk > into
func (s *Serializer) Decode(originalData []byte, gvk *schema.GroupVersionKind, into runtime.Object) (runtime.Object, *schema.GroupVersionKind, error) {
	// TODO 这里应该是data的一份拷贝
	data := originalData
	if s.options.Yaml { // 如果原始数据是通过yaml格式存储的，那么先把数据转为json格式的二进制数据
		// TODO 既然数据是通过YAML格式存储的，而接口调用方仅仅是为了得到go struct，那为什么这里不直接调用 YAML Serializer的Decode方法？
		altered, err := yaml.YAMLToJSON(data)
		if err != nil {
			return nil, nil, err
		}
		data = altered
	}

	// 获取到资源对象的GVK
	actual, err := s.meta.Interpret(data)
	if err != nil {
		return nil, nil, err
	}

	if gvk != nil {
		// 设置GVK的默认值 TODO 什么情况下GVK会缺失呢,除了核心资源会确实Group,其余资源提交的时候都必须提交GVK吧
		// 解析类型元数据大部分情况是正确的，除非不是json或者apiVersion格式不对。
		// 但是GVK三元组可能有所缺失，比如只有Kind，Group/Version，其他字段就用默认的GVK补全。
		// 这也体现出了原始数据中的GVK的优先级最高，其次是默认的GVK。gvkWithDefaults()函数下面有注释。
		// 实际上通常情况下defaults参数并不起作用，因为data二进制数据提供了完整的group, version, kind信息
		*actual = gvkWithDefaults(*actual, *gvk)
	}

	// TODO 啥时候一个资源对象会是UnKnown的?
	// 如果into对象是UnKnown类型，那么直接取出其中的GVK，其余信息还是二进制的。
	if unk, ok := into.(*runtime.Unknown); ok && unk != nil {
		unk.Raw = originalData
		unk.ContentType = runtime.ContentTypeJSON
		unk.GetObjectKind().SetGroupVersionKind(*actual)
		return unk, actual, nil
	}

	// TODO 这里就体现出来了into对象的意义之所在
	if into != nil {
		_, isUnstructured := into.(runtime.Unstructured)
		types, _, err := s.typer.ObjectKinds(into)
		switch {
		// TODO 如何理解这个case语句的条件
		// TODO 什么情况下into对象会是一个没有注册的对象
		case runtime.IsNotRegisteredError(err), isUnstructured:
			strictErrs, err := s.unmarshal(into, data, originalData)
			if err != nil {
				return nil, actual, err
			}

			// when decoding directly into a provided unstructured object,
			// extract the actual gvk decoded from the provided data,
			// and ensure it is non-empty.
			if isUnstructured {
				*actual = into.GetObjectKind().GroupVersionKind()
				if len(actual.Kind) == 0 {
					return nil, actual, runtime.NewMissingKindErr(string(originalData))
				}
				// TODO(109023): require apiVersion here as well once unstructuredJSONScheme#Decode does
			}

			if len(strictErrs) > 0 {
				return into, actual, runtime.NewStrictDecodingError(strictErrs)
			}
			return into, actual, nil
		case err != nil:
			return nil, actual, err
		default:
			*actual = gvkWithDefaults(*actual, types[0])
		}
	}

	if len(actual.Kind) == 0 {
		return nil, actual, runtime.NewMissingKindErr(string(originalData))
	}
	if len(actual.Version) == 0 {
		return nil, actual, runtime.NewMissingVersionErr(string(originalData))
	}

	// use the target if necessary
	// 创建资源对象，如果into对象的GVK和通过字节数组解析出来的一样，可以直接使用into对象
	obj, err := runtime.UseOrCreateObject(s.typer, s.creater, *actual, into)
	if err != nil {
		return nil, actual, err
	}

	// 反序列化 originalData有可能是yaml格式，此时data就是转换为json格式的数据。
	strictErrs, err := s.unmarshal(obj, data, originalData)
	if err != nil {
		return nil, actual, err
	} else if len(strictErrs) > 0 {
		return obj, actual, runtime.NewStrictDecodingError(strictErrs)
	}
	return obj, actual, nil
}

// Encode serializes the provided object to the given writer.
// 把obj对象json序列化，然后把序列化数据写入到w当中
func (s *Serializer) Encode(obj runtime.Object, w io.Writer) error {
	if co, ok := obj.(runtime.CacheableObject); ok {
		return co.CacheEncode(s.Identifier(), s.doEncode, w)
	}
	return s.doEncode(obj, w)
}

func (s *Serializer) doEncode(obj runtime.Object, w io.Writer) error {
	if s.options.Yaml {
		json, err := json.Marshal(obj)
		if err != nil {
			return err
		}
		data, err := yaml.JSONToYAML(json)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}

	if s.options.Pretty {
		data, err := json.MarshalIndent(obj, "", "  ")
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	encoder := json.NewEncoder(w)
	return encoder.Encode(obj)
}

// IsStrict indicates whether the serializer
// uses strict decoding or not
func (s *Serializer) IsStrict() bool {
	return s.options.Strict
}

func (s *Serializer) unmarshal(into runtime.Object, data, originalData []byte) (strictErrs []error, err error) {
	// If the deserializer is non-strict, return here.
	if !s.options.Strict {
		if err := kjson.UnmarshalCaseSensitivePreserveInts(data, into); err != nil {
			return nil, err
		}
		return nil, nil
	}

	if s.options.Yaml {
		// In strict mode pass the original data through the YAMLToJSONStrict converter.
		// This is done to catch duplicate fields in YAML that would have been dropped in the original YAMLToJSON conversion.
		// TODO: rework YAMLToJSONStrict to return warnings about duplicate fields without terminating so we don't have to do this twice.
		_, err := yaml.YAMLToJSONStrict(originalData)
		if err != nil {
			strictErrs = append(strictErrs, err)
		}
	}

	var strictJSONErrs []error
	if u, isUnstructured := into.(runtime.Unstructured); isUnstructured {
		// Unstructured is a custom unmarshaler that gets delegated
		// to, so in order to detect strict JSON errors we need
		// to unmarshal directly into the object.
		m := map[string]interface{}{}
		strictJSONErrs, err = kjson.UnmarshalStrict(data, &m)
		u.SetUnstructuredContent(m)
	} else {
		strictJSONErrs, err = kjson.UnmarshalStrict(data, into)
	}
	if err != nil {
		// fatal decoding error, not due to strictness
		return nil, err
	}
	strictErrs = append(strictErrs, strictJSONErrs...)
	return strictErrs, nil
}

// Identifier implements runtime.Encoder interface.
func (s *Serializer) Identifier() runtime.Identifier {
	return s.identifier
}

// RecognizesData implements the RecognizingDecoder interface.
func (s *Serializer) RecognizesData(data []byte) (ok, unknown bool, err error) {
	if s.options.Yaml {
		// we could potentially look for '---'
		return false, true, nil
	}
	return utilyaml.IsJSONBuffer(data), false, nil
}

// Framer is the default JSON framing behavior, with newlines delimiting individual objects.
var Framer = jsonFramer{}

type jsonFramer struct{}

// NewFrameWriter implements stream framing for this serializer
func (jsonFramer) NewFrameWriter(w io.Writer) io.Writer {
	// we can write JSON objects directly to the writer, because they are self-framing
	return w
}

// NewFrameReader implements stream framing for this serializer
func (jsonFramer) NewFrameReader(r io.ReadCloser) io.ReadCloser {
	// we need to extract the JSON chunks of data to pass to Decode()
	return framer.NewJSONFramedReader(r)
}

// YAMLFramer is the default JSON framing behavior, with newlines delimiting individual objects.
var YAMLFramer = yamlFramer{}

type yamlFramer struct{}

// NewFrameWriter implements stream framing for this serializer
func (yamlFramer) NewFrameWriter(w io.Writer) io.Writer {
	return yamlFrameWriter{w}
}

// NewFrameReader implements stream framing for this serializer
func (yamlFramer) NewFrameReader(r io.ReadCloser) io.ReadCloser {
	// extract the YAML document chunks directly
	return utilyaml.NewDocumentDecoder(r)
}

type yamlFrameWriter struct {
	w io.Writer
}

// Write separates each document with the YAML document separator (`---` followed by line
// break). Writers must write well formed YAML documents (include a final line break).
func (w yamlFrameWriter) Write(data []byte) (n int, err error) {
	if _, err := w.w.Write([]byte("---\n")); err != nil {
		return 0, err
	}
	return w.w.Write(data)
}
