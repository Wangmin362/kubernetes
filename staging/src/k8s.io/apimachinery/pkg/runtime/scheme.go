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
	"fmt"
	"reflect"
	"strings"

	"k8s.io/apimachinery/pkg/conversion"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/naming"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
)

// Scheme defines methods for serializing and deserializing API objects, a type
// registry for converting group, version, and kind information to and from Go
// schemas, and mappings between Go schemas of different versions. A scheme is the
// foundation for a versioned API and versioned configuration over time.
//
// In a Scheme, a Type is a particular Go struct, a Version is a point-in-time
// identifier for a particular representation of that Type (typically backwards
// compatible), a Kind is the unique name for that Type within the Version, and a
// Group identifies a set of Versions, Kinds, and Types that evolve over time. An
// Unversioned Type is one that is not yet formally bound to a type and is promised
// to be backwards compatible (effectively a "v1" of a Type that does not expect
// to break in the future).
//
// Schemes are not expected to change at runtime and are only threadsafe after
// registration is complete.
// TODO 仔细分析
// 1、Scheme用于记录有版本、无版本资源类型GVK到Go结构体之间的转换，以及Go结构体到GVK之间的映射
// 2、Scheme缓存了每个GKV资源的默认初始化函数
// 3、Scheme缓存了每个GVK资源的转换函数
// 4、Scheme缓存了每个组下的资源不同版本的优先级
type Scheme struct {
	// gvkToType allows one to figure out the go type of an object with
	// the given version and name.
	// 1、用于存储GVK到GoStruct之间的映射，这个映射是一对一的
	// 2、我们在请求进来时，需要对于请求的Body进行序列化时，就需要通过gvkToType确定当前的Body应该反序列化为哪一个结构体。
	gvkToType map[schema.GroupVersionKind]reflect.Type

	// typeToGVK allows one to find metadata for a given go object.
	// The reflect.Type we index by should *not* be a pointer.
	// 1、用于存储GoStruct到GVK之间的映射，这个映射是一对多的   TODO 为什么这里是一对多的？
	// 2、之所以是一对多，是因为存储一个资源可能在不同的组当中。还有可能功能不同的资源想用相同的结构体定义 TODO 举个例子嘞
	// TODO 什么时候需要用到这个字段
	typeToGVK map[reflect.Type][]schema.GroupVersionKind

	// unversionedTypes are transformed without conversion in ConvertToVersion.
	// 1、用于存在GoStruct到GVK的映射，由于无版本的资源类型是没有版本的，因此是一对一
	// 2、所谓的UnversionType其实就是指的没有版本概念的资源，譬如Status, WatchEvent, APIVersions, APIGroupList, APIGroup, APIResourceList
	// 在K8S当中还是有许多没有版本概念的资源存在
	unversionedTypes map[reflect.Type]schema.GroupVersionKind

	// unversionedKinds are the names of kinds that can be created in the context of any group
	// or version
	// TODO: resolve the status of unversioned types.
	// 1、用于存储GVK到GoStruct的映射, key为Kind，Value为GoStruct
	unversionedKinds map[string]reflect.Type

	// Map from version and resource to the corresponding func to convert
	// resource field labels in that version to internal version.
	// 1、用于设置资源的字段标签函数，此函数用于判断某个字段是否可以作为字段标签
	// 2、所谓的字段标签其实就是FieldSelector，也就是我们说的字段选择器；在K8S当中，字段选择器可以用来作为查询条件来查询该资源。譬如
	// kubectl get pods --field-selector status.phase=Running。在K8S当中，所有资源都支持设置metadata.name, metadata.namespace
	// 作为标签选择器。
	// 3、对于有些资源对象，有可能支持除了metadata.name, metadata.namespace字段以外的其它资源，因此我们需要有一个函数可以判断一个资源
	// 对象的哪些字段可以作为字段选择器。FieldLabelConversionFunc就是用于干这个事情的。
	// 4、一个组下的资源都是通过init()初始化函数进行初始化的，譬如：localSchemeBuilder.Register(addDefaultingFuncs, addConversionFuncs)
	fieldLabelConversionFuncs map[schema.GroupVersionKind]FieldLabelConversionFunc

	// defaulterFuncs is a map to funcs to be called with an object to provide defaulting
	// the provided object must be a pointer.
	// 1、用于设置对象的某些属性，用于设置默认值。 譬如某些资源对象空指针的初始化
	// 2、注册资源的默认赋值函数，当实例化某个资源时，会应用这些默认函数到刚实例化出来的资源上，为这些资源设置默认值
	// 3、每个组的资源都是通过init方法注册默认函数，譬如：localSchemeBuilder.Register(addDefaultingFuncs)
	// 4、为什么这里不设计为一个数组？ 一个资源为什么不能设置多个默认函数？ 一个资源实例化的时候，此时我们就能确定这个资源需要给哪些资源设置默认值，
	// 一个初始化函数和多个初始化函数没有区别，多个初始化函数也能合并为一个初始化函数，而且这玩意仅仅在实例化资源对象的时候被调用。
	defaulterFuncs map[reflect.Type]func(interface{})

	// converter stores all registered conversion functions. It also has
	// default converting behavior.
	// TODO 用于多个版本之间的相互转换
	converter *conversion.Converter

	// versionPriority is a map of groups to ordered lists of versions for those groups indicating the
	// default priorities of these versions as registered in the scheme
	// 1、每种资源会自己调用scheme.SetVersionPriority注册不同的版本，Key为Group，Value为不同的组，排在前面的版本优先级越高
	// 2、主要是记录每个组的不同版本优先级
	versionPriority map[string][]string

	// observedVersions keeps track of the order we've seen versions during type registration
	// 1、所谓的观察到的版本，其实就是当前K8S中所有资源的GV，scheme通过这个字段记录
	// 2、此字段不会记录__internal版本的资源
	// 3、各个资源再注册scheme的时候会自动注册到这里面
	observedVersions []schema.GroupVersion

	// schemeName is the name of this scheme.  If you don't specify a name, the stack of the NewScheme caller will be used.
	// This is useful for error reporting to indicate the origin of the scheme.
	// scheme的名字  TODO K8S会实例化几个Scheme?
	schemeName string
}

// FieldLabelConversionFunc converts a field selector to internal representation.
// 1、所谓的字段标签转换函数，其实就是用于判断一个资源的哪些字段可以作为字段选择器。
// 2、所谓的字段标签其实就是FieldSelector，也就是我们说的字段选择器；在K8S当中，字段选择器可以用来作为查询条件来查询该资源。譬如
// kubectl get pods --field-selector status.phase=Running。在K8S当中，所有资源都支持设置metadata.name, metadata.namespace
// 作为标签选择器。
// 3、对于有些资源对象，有可能支持除了metadata.name, metadata.namespace字段以外的其它资源，因此我们需要有一个函数可以判断一个资源
// 对象的哪些字段可以作为字段选择器。FieldLabelConversionFunc就是用于干这个事情的。
type FieldLabelConversionFunc func(label, value string) (internalLabel, internalValue string, err error)

// NewScheme creates a new Scheme. This scheme is pluggable by default.
func NewScheme() *Scheme {
	s := &Scheme{
		gvkToType:                 map[schema.GroupVersionKind]reflect.Type{},
		typeToGVK:                 map[reflect.Type][]schema.GroupVersionKind{},
		unversionedTypes:          map[reflect.Type]schema.GroupVersionKind{},
		unversionedKinds:          map[string]reflect.Type{},
		fieldLabelConversionFuncs: map[schema.GroupVersionKind]FieldLabelConversionFunc{},
		defaulterFuncs:            map[reflect.Type]func(interface{}){},
		versionPriority:           map[string][]string{},
		// TODO 为什么Scheme名字的生成这么复杂
		schemeName: naming.GetNameFromCallsite(internalPackages...),
	}
	// 实例化Converter
	s.converter = conversion.NewConverter(nil)

	// Enable couple default conversions by default.
	// 注册一些通用的转换函数
	utilruntime.Must(RegisterEmbeddedConversions(s))
	utilruntime.Must(RegisterStringConversions(s))
	return s
}

// Converter allows access to the converter for the scheme
func (s *Scheme) Converter() *conversion.Converter {
	return s.converter
}

// AddUnversionedTypes registers the provided types as "unversioned", which means that they follow special rules.
// Whenever an object of this type is serialized, it is serialized with the provided group version and is not
// converted. Thus unversioned objects are expected to remain backwards compatible forever, as if they were in an
// API group and version that would never be updated.
//
// TODO: there is discussion about removing unversioned and replacing it with objects that are manifest into
// every version with particular schemas. Resolve this method at that point.
// 1、向scheme中注册没有版本概念的资源，譬如Status, WatchEvent, APIVersions, APIGroupList, APIGroup, APIResourceList
// 2、scheme.observedVersion中添加GV， scheme.gvkToType注册每个GVK到GoStruct的映射， scheme.typeToGVK注册GoStruct到GVK的映射
// scheme.unversionedTypes记录GoStruct到GVK的映射，scheme.unversionedKinds记录资源Kind类型到GoStruct的映射
func (s *Scheme) AddUnversionedTypes(version schema.GroupVersion, types ...Object) {
	// 如果当前GV不是__internal，那么把GV记录下来
	s.addObservedVersion(version)
	s.AddKnownTypes(version, types...)
	for _, obj := range types {
		t := reflect.TypeOf(obj).Elem()
		gvk := version.WithKind(t.Name())
		s.unversionedTypes[t] = gvk
		if old, ok := s.unversionedKinds[gvk.Kind]; ok && t != old {
			panic(fmt.Sprintf("%v.%v has already been registered as unversioned kind %q - kind name must be unique in scheme %q",
				old.PkgPath(), old.Name(), gvk, s.schemeName))
		}
		s.unversionedKinds[gvk.Kind] = t
	}
}

// AddKnownTypes registers all types passed in 'types' as being members of version 'version'.
// All objects passed to types should be pointers to structs. The name that go reports for
// the struct becomes the "kind" field when encoding. Version may not be empty - use the
// APIVersionInternal constant if you have a type that does not have a formal version.
// 1、所谓的KnownType，其实指的就是有版本的资源类型。在K8S当中，存在一些没有版本的资源类型，被称之为UnVersionedType，譬如APIGroup,
// APIGroupList,APIResource, APIResourceList等资源，不过K8S中的无版本资源这个概念现在已经逐渐弱化，更多则是KnownType，也就是有版本的
// 资源类型
// 2、所谓的添加已知类型，其实就是K8S已经内置的资源类型
// 3、调用这个方法的时候，一般是添加一个组先的所有资源，所以只需要指定Group, Version，而无需指定具体的Kind，因为我们可以通过反射从具体的对象
// 当中读取出来。实际上最后还是通过调用的AddKnownTypeWithName添加的一个具体类型
func (s *Scheme) AddKnownTypes(gv schema.GroupVersion, types ...Object) {
	s.addObservedVersion(gv)
	for _, obj := range types {
		t := reflect.TypeOf(obj)
		if t.Kind() != reflect.Pointer {
			panic("All types must be pointers to structs.")
		}
		t = t.Elem()
		// 通过反射可以获取到当前资源的名字，也即是我们说的Kind值
		s.AddKnownTypeWithName(gv.WithKind(t.Name()), obj)
	}
}

// AddKnownTypeWithName is like AddKnownTypes, but it lets you specify what this type should
// be encoded as. Useful for testing when you don't want to make multiple packages to define
// your structs. Version may not be empty - use the APIVersionInternal constant if you have a
// type that does not have a formal version.
// 注册一个GVK到Go结构体的映射。
func (s *Scheme) AddKnownTypeWithName(gvk schema.GroupVersionKind, obj Object) {
	// 1、Version为空，或者Version为__internal,都不会执改变scheme的任何属性
	s.addObservedVersion(gvk.GroupVersion())
	t := reflect.TypeOf(obj)
	if len(gvk.Version) == 0 {
		panic(fmt.Sprintf("version is required on all types: %s %v", gvk, t))
	}
	if t.Kind() != reflect.Pointer {
		panic("All types must be pointers to structs.")
	}
	t = t.Elem()
	if t.Kind() != reflect.Struct {
		panic("All types must be pointers to structs.")
	}

	// 相同的类型不能注册多次
	if oldT, found := s.gvkToType[gvk]; found && oldT != t {
		panic(fmt.Sprintf("Double registration of different types for %v: old=%v.%v, new=%v.%v in scheme %q",
			gvk, oldT.PkgPath(), oldT.Name(), t.PkgPath(), t.Name(), s.schemeName))
	}

	// 显然，如果一个GVK之前注册了另外一个类型，此时就会覆盖之前的类型
	s.gvkToType[gvk] = t

	for _, existingGvk := range s.typeToGVK[t] {
		// 如果已经存在，则直接返回
		if existingGvk == gvk {
			return
		}
	}
	s.typeToGVK[t] = append(s.typeToGVK[t], gvk)

	// if the type implements DeepCopyInto(<obj>), register a self-conversion
	// 如果一个资源对象实现了DeepCopyInto(into)方法，并且输入参数只有一个，并且输出参数，并且没有输出参数，并且输出参数的类型就是自身，那么
	// 就像scheme注册转换函数
	// TODO 一个资源什么时候应该实现DeepCopyInfo方法？
	// TODO 转换函数有啥用？
	if m := reflect.ValueOf(obj).MethodByName("DeepCopyInto"); m.IsValid() && m.Type().NumIn() == 1 &&
		m.Type().NumOut() == 0 && m.Type().In(0) == reflect.TypeOf(obj) {
		// TODO 注册一个转换函数
		if err := s.AddGeneratedConversionFunc(obj, obj, func(a, b interface{}, scope conversion.Scope) error {
			// copy a to b
			reflect.ValueOf(a).MethodByName("DeepCopyInto").Call([]reflect.Value{reflect.ValueOf(b)})
			// clear TypeMeta to match legacy reflective conversion
			b.(Object).GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{})
			return nil
		}); err != nil {
			panic(err)
		}
	}
}

// KnownTypes returns the types known for the given version.
// 1、获取指定GV下的所有GKV
func (s *Scheme) KnownTypes(gv schema.GroupVersion) map[string]reflect.Type {
	types := make(map[string]reflect.Type)
	for gvk, t := range s.gvkToType {
		if gv != gvk.GroupVersion() {
			continue
		}

		types[gvk.Kind] = t
	}
	return types
}

// VersionsForGroupKind returns the versions that a particular GroupKind can be converted to within the given group.
// A GroupKind might be converted to a different group. That information is available in EquivalentResourceMapper.
// 找到一个GK的所有版本，并且按照优先级排序
func (s *Scheme) VersionsForGroupKind(gk schema.GroupKind) []schema.GroupVersion {
	// 获取指定GK的所有GV，其实就是取出一个资源的所有版本
	var availableVersions []schema.GroupVersion
	for gvk := range s.gvkToType {
		if gk != gvk.GroupKind() {
			continue
		}

		availableVersions = append(availableVersions, gvk.GroupVersion())
	}

	// order the return for stability
	var ret []schema.GroupVersion
	// 获取一个组的不同版本优先级
	for _, version := range s.PrioritizedVersionsForGroup(gk.Group) {
		for _, availableVersion := range availableVersions {
			if version != availableVersion {
				continue
			}
			ret = append(ret, availableVersion)
		}
	}

	return ret
}

// AllKnownTypes returns the all known types.
func (s *Scheme) AllKnownTypes() map[schema.GroupVersionKind]reflect.Type {
	return s.gvkToType
}

// ObjectKinds returns all possible group,version,kind of the go object, true if the
// object is considered unversioned, or an error if it's not a pointer or is unregistered.
// 通过反射获取当前对象的GVK，并返回当前资源是否是UnversionedType
func (s *Scheme) ObjectKinds(obj Object) ([]schema.GroupVersionKind, bool, error) {
	// Unstructured objects are always considered to have their declared GVK
	if _, ok := obj.(Unstructured); ok {
		// we require that the GVK be populated in order to recognize the object
		gvk := obj.GetObjectKind().GroupVersionKind()
		if len(gvk.Kind) == 0 {
			return nil, false, NewMissingKindErr("unstructured object has no kind")
		}
		if len(gvk.Version) == 0 {
			return nil, false, NewMissingVersionErr("unstructured object has no version")
		}
		return []schema.GroupVersionKind{gvk}, false, nil
	}

	v, err := conversion.EnforcePtr(obj)
	if err != nil {
		return nil, false, err
	}
	t := v.Type()

	gvks, ok := s.typeToGVK[t]
	if !ok {
		return nil, false, NewNotRegisteredErrForType(s.schemeName, t)
	}
	_, unversionedType := s.unversionedTypes[t]

	return gvks, unversionedType, nil
}

// Recognizes returns true if the scheme is able to handle the provided group,version,kind
// of an object.
// 判断当前GVK是否是已经注册过的GVK
func (s *Scheme) Recognizes(gvk schema.GroupVersionKind) bool {
	_, exists := s.gvkToType[gvk]
	return exists
}

// TODO 第二个返回值代表什么含义？
func (s *Scheme) IsUnversioned(obj Object) (bool, bool) {
	v, err := conversion.EnforcePtr(obj)
	if err != nil {
		return false, false
	}
	t := v.Type()

	if _, ok := s.typeToGVK[t]; !ok {
		return false, false
	}
	_, ok := s.unversionedTypes[t]
	return ok, true
}

// New returns a new API object of the given version and name, or an error if it hasn't
// been registered. The version and kind fields must be specified.
// 1、根据GVK实例化一个资源对象，该资源对象可能是有版本的，也可能是没有版本的。如果此资源没有注册过，那么返回错误
func (s *Scheme) New(kind schema.GroupVersionKind) (Object, error) {
	// 根据GVK获取这个资源的结构体类型
	if t, exists := s.gvkToType[kind]; exists {
		// 如果存在的化，直接通过反射实例化资源对象
		return reflect.New(t).Interface().(Object), nil
	}

	if t, exists := s.unversionedKinds[kind.Kind]; exists {
		return reflect.New(t).Interface().(Object), nil
	}
	return nil, NewNotRegisteredErrForKind(s.schemeName, kind)
}

// AddIgnoredConversionType identifies a pair of types that should be skipped by
// conversion (because the data inside them is explicitly dropped during
// conversion).
// 1、用于添加忽略类型转换函数，也就是说从from转为to类型时会被忽略
// 2、在K8S当中，被忽略的类型就一个：从TypeMeta转为TypeMeta
func (s *Scheme) AddIgnoredConversionType(from, to interface{}) error {
	return s.converter.RegisterIgnoredConversion(from, to)
}

// AddConversionFunc registers a function that converts between a and b by passing objects of those
// types to the provided function. The function *must* accept objects of a and b - this machinery will not enforce
// any other guarantee.
// 用于注册a -> b的转换函数
func (s *Scheme) AddConversionFunc(a, b interface{}, fn conversion.ConversionFunc) error {
	return s.converter.RegisterUntypedConversionFunc(a, b, fn)
}

// AddGeneratedConversionFunc registers a function that converts between a and b by passing objects of those
// types to the provided function. The function *must* accept objects of a and b - this machinery will not enforce
// any other guarantee.
func (s *Scheme) AddGeneratedConversionFunc(a, b interface{}, fn conversion.ConversionFunc) error {
	return s.converter.RegisterGeneratedUntypedConversionFunc(a, b, fn)
}

// AddFieldLabelConversionFunc adds a conversion function to convert field selectors
// of the given kind from the given version to internal version representation.
// 添加资源的字段选择器函数，用于判断一个资源的哪些字段可以作为选择器
func (s *Scheme) AddFieldLabelConversionFunc(gvk schema.GroupVersionKind, conversionFunc FieldLabelConversionFunc) error {
	s.fieldLabelConversionFuncs[gvk] = conversionFunc
	return nil
}

// AddTypeDefaultingFunc registers a function that is passed a pointer to an
// object and can default fields on the object. These functions will be invoked
// when Default() is called. The function will never be called unless the
// defaulted object matches srcType. If this function is invoked twice with the
// same srcType, the fn passed to the later call will be used instead.
// 1、注册资源的默认赋值函数，当实例化某个资源时，会应用这些默认函数到刚实例化出来的资源上，为这些资源设置默认值
func (s *Scheme) AddTypeDefaultingFunc(srcType Object, fn func(interface{})) {
	s.defaulterFuncs[reflect.TypeOf(srcType)] = fn
}

// Default sets defaults on the provided Object.
// 通过默认的方法为资源对象设置默认值
func (s *Scheme) Default(src Object) {
	if fn, ok := s.defaulterFuncs[reflect.TypeOf(src)]; ok {
		fn(src)
	}
}

// Convert will attempt to convert in into out. Both must be pointers. For easy
// testing of conversion functions. Returns an error if the conversion isn't
// possible. You can call this with types that haven't been registered (for example,
// a to test conversion of types that are nested within registered types). The
// context interface is passed to the convertor. Convert also supports Unstructured
// types and will convert them intelligently.
func (s *Scheme) Convert(in, out interface{}, context interface{}) error {
	unstructuredIn, okIn := in.(Unstructured)
	unstructuredOut, okOut := out.(Unstructured)
	switch {
	case okIn && okOut:
		// 如果两个都是非结构化类型，那么直接设置就是，无需转换
		// converting unstructured input to an unstructured output is a straight copy - unstructured
		// is a "smart holder" and the contents are passed by reference between the two objects
		unstructuredOut.SetUnstructuredContent(unstructuredIn.UnstructuredContent())
		return nil

	case okOut: // 如果输出对象为非结构体对象。in对象肯定不是非结构体对象，肯定是一个结构体对象。
		// if the output is an unstructured object, use the standard Go type to unstructured
		// conversion. The object must not be internal.
		obj, ok := in.(Object)
		if !ok {
			return fmt.Errorf("unable to convert object type %T to Unstructured, must be a runtime.Object", in)
		}
		gvks, unversioned, err := s.ObjectKinds(obj)
		if err != nil {
			return err
		}
		// TODO 为什么这里可以直接取第一个元素
		gvk := gvks[0]

		// if no conversion is necessary, convert immediately
		if unversioned || gvk.Version != APIVersionInternal {
			// 把in结构体对象转为非结构体对象
			content, err := DefaultUnstructuredConverter.ToUnstructured(in)
			if err != nil {
				return err
			}
			// 直接设置其内容即可
			unstructuredOut.SetUnstructuredContent(content)
			unstructuredOut.GetObjectKind().SetGroupVersionKind(gvk)
			return nil
		}

		// attempt to convert the object to an external version first.
		target, ok := context.(GroupVersioner)
		if !ok {
			return fmt.Errorf("unable to convert the internal object type %T to Unstructured without providing a preferred version to convert to", in)
		}
		// Convert is implicitly unsafe, so we don't need to perform a safe conversion
		versioned, err := s.UnsafeConvertToVersion(obj, target)
		if err != nil {
			return err
		}
		content, err := DefaultUnstructuredConverter.ToUnstructured(versioned)
		if err != nil {
			return err
		}
		unstructuredOut.SetUnstructuredContent(content)
		return nil

	case okIn:
		// converting an unstructured object to any type is modeled by first converting
		// the input to a versioned type, then running standard conversions
		typed, err := s.unstructuredToTyped(unstructuredIn)
		if err != nil {
			return err
		}
		in = typed
	}

	meta := s.generateConvertMeta(in)
	meta.Context = context
	return s.converter.Convert(in, out, meta)
}

// ConvertFieldLabel alters the given field label and value for an kind field selector from
// versioned representation to an unversioned one or returns an error.
// 判断当前GVK资源是否支持label作为字段选择器
func (s *Scheme) ConvertFieldLabel(gvk schema.GroupVersionKind, label, value string) (string, string, error) {
	conversionFunc, ok := s.fieldLabelConversionFuncs[gvk]
	if !ok {
		// 默认只有metadata.name, metadata.namespace作为字段标签
		return DefaultMetaV1FieldSelectorConversion(label, value)
	}
	return conversionFunc(label, value)
}

// ConvertToVersion attempts to convert an input object to its matching Kind in another
// version within this scheme. Will return an error if the provided version does not
// contain the inKind (or a mapping by name defined with AddKnownTypeWithName). Will also
// return an error if the conversion does not result in a valid Object being
// returned. Passes target down to the conversion methods as the Context on the scope.
func (s *Scheme) ConvertToVersion(in Object, target GroupVersioner) (Object, error) {
	return s.convertToVersion(true, in, target)
}

// UnsafeConvertToVersion will convert in to the provided target if such a conversion is possible,
// but does not guarantee the output object does not share fields with the input object. It attempts to be as
// efficient as possible when doing conversion.
func (s *Scheme) UnsafeConvertToVersion(in Object, target GroupVersioner) (Object, error) {
	return s.convertToVersion(false, in, target)
}

// convertToVersion handles conversion with an optional copy.
func (s *Scheme) convertToVersion(copy bool, in Object, target GroupVersioner) (Object, error) {
	var t reflect.Type

	if u, ok := in.(Unstructured); ok {
		// 把非结构体对象转为结构体对象
		typed, err := s.unstructuredToTyped(u)
		if err != nil {
			return nil, err
		}

		in = typed
		// unstructuredToTyped returns an Object, which must be a pointer to a struct.
		t = reflect.TypeOf(in).Elem()

	} else {
		// determine the incoming kinds with as few allocations as possible.
		t = reflect.TypeOf(in)
		if t.Kind() != reflect.Pointer {
			return nil, fmt.Errorf("only pointer types may be converted: %v", t)
		}
		t = t.Elem()
		if t.Kind() != reflect.Struct {
			return nil, fmt.Errorf("only pointers to struct types may be converted: %v", t)
		}
	}

	// 获取资源对象的GVK
	kinds, ok := s.typeToGVK[t]
	if !ok || len(kinds) == 0 {
		return nil, NewNotRegisteredErrForType(s.schemeName, t)
	}

	gvk, ok := target.KindForGroupVersionKinds(kinds)
	if !ok {
		// try to see if this type is listed as unversioned (for legacy support)
		// TODO: when we move to server API versions, we should completely remove the unversioned concept
		if unversionedKind, ok := s.unversionedTypes[t]; ok {
			if gvk, ok := target.KindForGroupVersionKinds([]schema.GroupVersionKind{unversionedKind}); ok {
				return copyAndSetTargetKind(copy, in, gvk)
			}
			return copyAndSetTargetKind(copy, in, unversionedKind)
		}
		return nil, NewNotRegisteredErrForTarget(s.schemeName, t, target)
	}

	// target wants to use the existing type, set kind and return (no conversion necessary)
	for _, kind := range kinds {
		if gvk == kind {
			return copyAndSetTargetKind(copy, in, gvk)
		}
	}

	// type is unversioned, no conversion necessary
	if unversionedKind, ok := s.unversionedTypes[t]; ok {
		if gvk, ok := target.KindForGroupVersionKinds([]schema.GroupVersionKind{unversionedKind}); ok {
			return copyAndSetTargetKind(copy, in, gvk)
		}
		return copyAndSetTargetKind(copy, in, unversionedKind)
	}

	out, err := s.New(gvk)
	if err != nil {
		return nil, err
	}

	if copy {
		in = in.DeepCopyObject()
	}

	meta := s.generateConvertMeta(in)
	meta.Context = target
	if err := s.converter.Convert(in, out, meta); err != nil {
		return nil, err
	}

	setTargetKind(out, gvk)
	return out, nil
}

// unstructuredToTyped attempts to transform an unstructured object to a typed
// object if possible. It will return an error if conversion is not possible, or the versioned
// Go form of the object. Note that this conversion will lose fields.
// 1、获取非结构化对象的对应类型的Go结构体，其原理就是先根据非结构体获取GVK，然后通过GVK获取Go结构体，然后进行实例化，最后进行属性赋值
// 2、其实就是把非结构体对象转为结构体对象
func (s *Scheme) unstructuredToTyped(in Unstructured) (Object, error) {
	// the type must be something we recognize
	gvks, _, err := s.ObjectKinds(in)
	if err != nil {
		return nil, err
	}
	typed, err := s.New(gvks[0])
	if err != nil {
		return nil, err
	}
	if err := DefaultUnstructuredConverter.FromUnstructured(in.UnstructuredContent(), typed); err != nil {
		return nil, fmt.Errorf("unable to convert unstructured object to %v: %v", gvks[0], err)
	}
	return typed, nil
}

// generateConvertMeta constructs the meta value we pass to Convert.
func (s *Scheme) generateConvertMeta(in interface{}) *conversion.Meta {
	return s.converter.DefaultMeta(reflect.TypeOf(in))
}

// copyAndSetTargetKind performs a conditional copy before returning the object, or an error if copy was not successful.
func copyAndSetTargetKind(copy bool, obj Object, kind schema.GroupVersionKind) (Object, error) {
	if copy {
		obj = obj.DeepCopyObject()
	}
	setTargetKind(obj, kind)
	return obj, nil
}

// setTargetKind sets the kind on an object, taking into account whether the target kind is the internal version.
func setTargetKind(obj Object, kind schema.GroupVersionKind) {
	if kind.Version == APIVersionInternal {
		// internal is a special case
		// TODO: look at removing the need to special case this
		obj.GetObjectKind().SetGroupVersionKind(schema.GroupVersionKind{})
		return
	}
	obj.GetObjectKind().SetGroupVersionKind(kind)
}

// SetVersionPriority allows specifying a precise order of priority. All specified versions must be in the same group,
// and the specified order overwrites any previously specified order for this group
// 1、不允许注册内部支援
// 2、每次设置版本优先级，只能设置一个组下的版本优先级，不能同时设置多个组
// 3、此函数的功能为设置组的不同版本的优先级，传参越前面的版本优先级越高
func (s *Scheme) SetVersionPriority(versions ...schema.GroupVersion) error {
	groups := sets.String{}
	var order []string
	for _, version := range versions {
		if len(version.Version) == 0 || version.Version == APIVersionInternal {
			return fmt.Errorf("internal versions cannot be prioritized: %v", version)
		}

		groups.Insert(version.Group)
		order = append(order, version.Version)
	}
	if len(groups) != 1 {
		return fmt.Errorf("must register versions for exactly one group: %v", strings.Join(groups.List(), ", "))
	}

	s.versionPriority[groups.List()[0]] = order
	return nil
}

// PrioritizedVersionsForGroup returns versions for a single group in priority order
// 1、获取一个组的不同版本的优先级
func (s *Scheme) PrioritizedVersionsForGroup(group string) []schema.GroupVersion {
	var ret []schema.GroupVersion
	for _, version := range s.versionPriority[group] {
		ret = append(ret, schema.GroupVersion{Group: group, Version: version})
	}
	for _, observedVersion := range s.observedVersions {
		if observedVersion.Group != group {
			continue
		}
		found := false
		for _, existing := range ret {
			if existing == observedVersion {
				found = true
				break
			}
		}
		// 可能存在有些资源的某些版本没有指定优先级
		if !found {
			ret = append(ret, observedVersion)
		}
	}

	return ret
}

// PrioritizedVersionsAllGroups returns all known versions in their priority order.  Groups are random, but
// versions for a single group are prioritized
// 1、返回当前注册的所有资源的GV，并且对于一个组先的GV，其优先级顺序就是数组中的顺序
// TODO 2、PreferredVersionAllGroups和PrioritizedVersionsAllGroups有何不同？
func (s *Scheme) PrioritizedVersionsAllGroups() []schema.GroupVersion {
	var ret []schema.GroupVersion
	// 获取所有的GV
	for group, versions := range s.versionPriority {
		for _, version := range versions {
			ret = append(ret, schema.GroupVersion{Group: group, Version: version})
		}
	}
	for _, observedVersion := range s.observedVersions {
		found := false
		for _, existing := range ret {
			if existing == observedVersion {
				found = true
				break
			}
		}
		if !found {
			ret = append(ret, observedVersion)
		}
	}
	return ret
}

// PreferredVersionAllGroups returns the most preferred version for every group.
// group ordering is random.
func (s *Scheme) PreferredVersionAllGroups() []schema.GroupVersion {
	var ret []schema.GroupVersion
	for group, versions := range s.versionPriority {
		for _, version := range versions {
			ret = append(ret, schema.GroupVersion{Group: group, Version: version})
			break
		}
	}
	for _, observedVersion := range s.observedVersions {
		found := false
		for _, existing := range ret {
			if existing.Group == observedVersion.Group {
				found = true
				break
			}
		}
		if !found {
			ret = append(ret, observedVersion)
		}
	}

	return ret
}

// IsGroupRegistered returns true if types for the group have been registered with the scheme
// 返回当前的组是否已经注册
func (s *Scheme) IsGroupRegistered(group string) bool {
	for _, observedVersion := range s.observedVersions {
		if observedVersion.Group == group {
			return true
		}
	}
	return false
}

// IsVersionRegistered returns true if types for the version have been registered with the scheme
// 返回当前的GV是否已经注册
func (s *Scheme) IsVersionRegistered(version schema.GroupVersion) bool {
	for _, observedVersion := range s.observedVersions {
		if observedVersion == version {
			return true
		}
	}

	return false
}

// 注册观察到的GV，如果Version为空，或者为__internal，将会被忽略
func (s *Scheme) addObservedVersion(version schema.GroupVersion) {
	if len(version.Version) == 0 || version.Version == APIVersionInternal {
		return
	}
	for _, observedVersion := range s.observedVersions {
		if observedVersion == version {
			return
		}
	}

	s.observedVersions = append(s.observedVersions, version)
}

func (s *Scheme) Name() string {
	return s.schemeName
}

// internalPackages are packages that ignored when creating a default reflector name. These packages are in the common
// call chains to NewReflector, so they'd be low entropy names for reflectors
var internalPackages = []string{"k8s.io/apimachinery/pkg/runtime/scheme.go"}
