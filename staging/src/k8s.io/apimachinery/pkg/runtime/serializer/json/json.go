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
func NewSerializerWithOptions(
	meta MetaFactory, // 用于从二进制信息中获取GVK信息
	creater runtime.ObjectCreater, // scheme实现了此接口，用于根据GVK信息实例化对应的Go结构体
	typer runtime.ObjectTyper, // scheme实现了接口，用于获取Go结构体的GVK，以及判断某个GVK是否是合法的GVK
	options SerializerOptions, // JSON序列化参数
) *Serializer {
	return &Serializer{
		meta:       meta,
		creater:    creater,
		typer:      typer,
		options:    options,
		identifier: identifier(options), // 根据指定的序列化参数信息，获取序列化器的名字
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
	// JSON解码器既可以对JSON数据进行编码解码，也可以对YAML数据进行解码，所以如果需要一个JSON编解码器，这里应该设置为false。如果需要一个
	// YAML类型的编解码器，这里应该设置为true
	Yaml bool

	// Pretty: configures a JSON enabled Serializer(`Yaml: false`) to produce human-readable output.
	// This option is silently ignored when `Yaml` is `true`.
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
// JSON序列化器，实现了加密、编码的功能
type Serializer struct {
	meta    MetaFactory           // 用于从二进制信息中获取GVK信息
	options SerializerOptions     // 配置参数
	creater runtime.ObjectCreater // 用于根据GVK信息实例化对应的Go结构体。这个映射关系保存在scheme当中
	typer   runtime.ObjectTyper   // 用于根据Go结构体信息获取GVK，以及判断一个GKV是否合法

	// 序列化器的标识，K8S中可以缓存一个对象的序列化结果，但是K8S中支持YAML, JSON, Protobuf多种格式的序列化，因此需要能够区分当前缓存的
	// 结果是哪个序列化器的结果。
	identifier runtime.Identifier
}

// Serializer implements Serializer
var _ runtime.Serializer = &Serializer{}
var _ recognizer.RecognizingDecoder = &Serializer{}

// gvkWithDefaults returns group kind and version defaulting from provided default
// 根据默认的GVK初始化实际的GVK，主要还是以实际的GVK为主，默认的GVK只是辅助作用，只要实际的GVK是残缺的，默认GVK才有用，否则默认GVK没啥用
func gvkWithDefaults(actual, defaultGVK schema.GroupVersionKind) schema.GroupVersionKind {
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
// If into is not registered with the typer, then the object will be straight decoded using normal JSON/YAML unmarshalling.
// If into is provided and the original data is not fully qualified with kind/version/group, the type of the into will be used to alter the returned gvk.
// If into is nil or data's gvk different from into's gvk, it will generate a new Object with ObjectCreater.New(gvk)
// On success or most errors, the method will return the calculated schema kind.
// The gvk calculate priority will be originalData > default gvk > into
// 用于把二进制信息反序列化为一个Go结构体资源对象
func (s *Serializer) Decode(
	originalData []byte, // 原始数据，这个数据一般是从ETCD中查出来的二进制信息
	gvk *schema.GroupVersionKind, // 指定默认的GVK，即如果从二进制信息提取不到GVK信息，或者提取到的GVK信息是残缺的，那么就使用这个默认的GVK信息填充
	into runtime.Object, // 用于告诉当前序列化的Go结构体的类型，也即是说需要按照into对象的类型进行序列化
) (runtime.Object, *schema.GroupVersionKind, error) {
	data := originalData
	// 如果originalData是通过YAML方式序列化之后得到的信息，那么治理需要转为JSON
	if s.options.Yaml {
		altered, err := yaml.YAMLToJSON(data)
		if err != nil {
			return nil, nil, err
		}
		data = altered
	}

	// 根据二进制信息获取资源对象的GVK信息
	actual, err := s.meta.Interpret(data)
	if err != nil {
		return nil, nil, err
	}

	if gvk != nil {
		// 如果从二进制信息提取不到GVK信息，或者提取到的GVK信息是不完整的，那么使用默认的GVK信息进行补全
		*actual = gvkWithDefaults(*actual, *gvk)
	}

	// 如果调用方想要一个Unknown对象，那么直接返回即可，无需解码
	if unk, ok := into.(*runtime.Unknown); ok && unk != nil {
		// 如果当前数据为YAML数据，这里是不是应该使用data数据，而不是originData数据。
		// 这里这么使用没有出问题的原因是因为再K8S当中，Unknown对象都是内部使用的，而K8S默认都是用JSON数据，所以没有毛病
		unk.Raw = originalData
		unk.ContentType = runtime.ContentTypeJSON
		unk.GetObjectKind().SetGroupVersionKind(*actual)
		return unk, actual, nil
	}

	if into != nil {
		_, isUnstructured := into.(runtime.Unstructured)
		// 根据Go结构体获取对象的GVK信息
		types, _, err := s.typer.ObjectKinds(into)
		switch {
		case runtime.IsNotRegisteredError(err), isUnstructured:
			// 1、如果当前指定的into类型没有注册过，直接尝试硬解码
			// 2、如果当前指定的into类型就是非结构化对象，也即是map[string]interface{}类型，直接尝试反序列化即可
			strictErrs, err := s.unmarshal(into, data, originalData)
			if err != nil {
				return nil, actual, err
			}

			// when decoding directly into a provided unstructured object,
			// extract the actual gvk decoded from the provided data,
			// and ensure it is non-empty.
			// 如果是非结构化数据，还需要设置GVK
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
			// 说明into对象是一个结构化资源对象
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
	// 用于判断当前指定的into资源对象的GKV信息就是actual，如果是那么我们可以直接把二进制信息发序列化到into对象当中，否则只能根据actual信息
	// 实例化一个资源对象
	obj, err := runtime.UseOrCreateObject(s.typer, s.creater, *actual, into)
	if err != nil {
		return nil, actual, err
	}

	// 把data二进制信息发序列化到obj当中
	strictErrs, err := s.unmarshal(obj, data, originalData)
	if err != nil {
		return nil, actual, err
	} else if len(strictErrs) > 0 {
		return obj, actual, runtime.NewStrictDecodingError(strictErrs)
	}
	return obj, actual, nil
}

// Encode serializes the provided object to the given writer.
// 1、把当前对象序列化到w当中
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

func (s *Serializer) unmarshal(
	into runtime.Object, // 反序列化后用Into保存数据
	data,
	originalData []byte, // 用来做严格序列化检查的数据
) (strictErrs []error, err error) {
	// If the deserializer is non-strict, return here.
	if !s.options.Strict {
		// 把data数据反序列化到into当中
		if err := kjson.UnmarshalCaseSensitivePreserveInts(data, into); err != nil {
			return nil, err
		}
		return nil, nil
	}

	// 如果是需要反序列化YAML格式的数据
	if s.options.Yaml {
		// In strict mode pass the original data through the YAMLToJSONStrict converter.
		// This is done to catch duplicate fields in YAML that would have been dropped in the original YAMLToJSON conversion.
		// TODO: rework YAMLToJSONStrict to return warnings about duplicate fields without terminating so we don't have to do this twice.
		// TODO 严格模式的检查因该就是在这里实现的
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
// 1、用于判断二进制数据data,是否是通过JSON的方式序列化得到的
// 2、判断当前data数据是否是
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
