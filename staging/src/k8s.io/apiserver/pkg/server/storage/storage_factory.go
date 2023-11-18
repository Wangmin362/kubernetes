/*
Copyright 2016 The Kubernetes Authors.

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
	"crypto/tls"
	"crypto/x509"
	"io/ioutil"
	"strings"

	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
)

// Backend describes the storage servers, the information here should be enough
// for health validations.
// 1、K8S存储的后端配置非常简单，就是Server地址配置，加上证书配置
type Backend struct {
	// the url of storage backend like: https://etcd.domain:2379
	Server string
	// the required tls config
	TLSConfig *tls.Config
}

// StorageFactory is the interface to locate the storage for a given GroupResource
// 所谓存储工厂，其实就是能够根据不同的GR返回其对应的存储配置，其中包含非常重要的编解码器
type StorageFactory interface {
	// NewConfig New finds the storage destination for the given group and resource. It will
	// return an error if the group has no storage destination configured.
	// 返回某个GR资源的具体存储配置
	NewConfig(groupResource schema.GroupResource) (*storagebackend.ConfigForResource, error)

	// ResourcePrefix returns the overridden resource prefix for the GroupResource
	// This allows for cohabitation of resources with different native types and provides
	// centralized control over the shape of etcd directories
	// 1、资源前缀
	// TODO 这个资源前缀用在哪里？
	ResourcePrefix(groupResource schema.GroupResource) string

	// Backends gets all backends for all registered storage destinations.
	// Used for getting all instances for health validations.
	// 存储工厂的后端存储
	// 为什么是多个后端存储？ 具体在存储资源的时候，如何确定使用哪个存储？
	Backends() []Backend
}

// DefaultStorageFactory takes a GroupResource and returns back its storage interface.  This result includes:
// 1. Merged etcd config, including: auth, server locations, prefixes
// 2. Resource encodings for storage: group,version,kind to store as
// 3. Cohabitating default: some resources like hpa are exposed through multiple APIs.  They must agree on 1 and 2
// TODO 如何理解存储工厂？
type DefaultStorageFactory struct {
	// StorageConfig describes how to create a storage backend in general.
	// Its authentication information will be used for every storage.Interface returned.
	// 1、描述了如何创建一个存储后端
	// 2、每个存储资源的持久化都使用相同的认证
	StorageConfig storagebackend.Config

	// 1、用于资源覆盖默认配置，我们是可以覆盖某个资源的存储配置的
	// 2、可以理解为针对某个GR专门的配置，这个配置将来会覆盖 storagebackend.Config 配置
	// 3、当key.Reosurce=*时，表示这个组下的所有资源都使用这个配置
	Overrides map[schema.GroupResource]groupResourceOverrides

	// 默认资源的前缀
	DefaultResourcePrefixes map[schema.GroupResource]string

	// DefaultMediaType is the media type used to store resources. If it is not set, "application/json" is used.
	// 默认的媒体类型
	DefaultMediaType string

	// DefaultSerializer is used to create encoders and decoders for the storage.Interface.
	// 默认的序列化器
	DefaultSerializer runtime.StorageSerializer

	// ResourceEncodingConfig describes how to encode a particular GroupVersionResource
	// 外部资源、内部资源的映射
	ResourceEncodingConfig ResourceEncodingConfig

	// APIResourceConfigSource indicates whether the *storage* is enabled, NOT the API
	// This is discrete from resource enablement because those are separate concerns.  How this source is configured
	// is left to the caller.
	// 用于判断某个GVR，或者是某个组是否被启用
	APIResourceConfigSource APIResourceConfigSource

	// newStorageCodecFn exists to be overwritten for unit testing.
	// 可以实例化每个资源需要的编解码器
	newStorageCodecFn func(opts StorageCodecConfig) (codec runtime.Codec, encodeVersioner runtime.GroupVersioner, err error)
}

// 用于资源覆盖默认配置
type groupResourceOverrides struct {
	// etcdLocation contains the list of "special" locations that are used for particular GroupResources
	// These are merged on top of the StorageConfig when requesting the storage.Interface for a given GroupResource
	// ETCD的地址，一般来说都是使用的ETCD集群，所以这里需要把ETCD所有的地址都配置上
	etcdLocation []string
	// etcdPrefix is the base location for a GroupResource.
	// 资源的ETCD前缀
	etcdPrefix string
	// etcdResourcePrefix is the location to use to store a particular type under the `etcdPrefix` location
	// If empty, the default mapping is used.  If the default mapping doesn't contain an entry, it will use
	// the ToLowered name of the resource, not including the group.
	// TODO 这玩意干嘛的？
	etcdResourcePrefix string
	// mediaType is the desired serializer to choose. If empty, the default is chosen.
	// 媒体类型决定了当前资源使用哪一种序列化器进行编解码，当前K8S支持JSON, YAML, Protobuf三种格式的编解码
	mediaType string
	// serializer contains the list of "special" serializers for a GroupResource.  Resource=* means for the entire group
	serializer runtime.StorageSerializer
	// cohabitatingResources keeps track of which resources must be stored together.  This happens when we have multiple ways
	// of exposing one set of concepts.  autoscaling.HPA and extensions.HPA as a for instance
	// The order of the slice matters!  It is the priority order of lookup for finding a storage location
	// 所谓的cohabitating资源指的是一个资源在不同的组下，但是他们是相同的资源，只是因为历史原因在不同的组下。
	cohabitatingResources []schema.GroupResource
	// encoderDecoratorFn is optional and may wrap the provided encoder prior to being serialized.
	// 设计的人真TM的牛逼，扩展点是真的多
	encoderDecoratorFn func(runtime.Encoder) runtime.Encoder
	// decoderDecoratorFn is optional and may wrap the provided decoders (can add new decoders). The order of
	// returned decoders will be priority for attempt to decode.
	decoderDecoratorFn func([]runtime.Decoder) []runtime.Decoder
	// disablePaging will prevent paging on the provided resource.
	disablePaging bool
}

// Apply overrides the provided config and options if the override has a value in that position
func (o groupResourceOverrides) Apply(config *storagebackend.Config, options *StorageCodecConfig) {
	if len(o.etcdLocation) > 0 {
		config.Transport.ServerList = o.etcdLocation
	}
	if len(o.etcdPrefix) > 0 {
		config.Prefix = o.etcdPrefix
	}

	if len(o.mediaType) > 0 {
		options.StorageMediaType = o.mediaType
	}
	if o.serializer != nil {
		options.StorageSerializer = o.serializer
	}
	if o.encoderDecoratorFn != nil {
		options.EncoderDecoratorFn = o.encoderDecoratorFn
	}
	if o.decoderDecoratorFn != nil {
		options.DecoderDecoratorFn = o.decoderDecoratorFn
	}
	if o.disablePaging {
		config.Paging = false
	}
}

var _ StorageFactory = &DefaultStorageFactory{}

const AllResources = "*"

func NewDefaultStorageFactory(
	config storagebackend.Config, // 后端存储配置，这是一个通用配置，所有资源都是这个配置，当然资源可以进行定制化的配置
	defaultMediaType string, // 默认媒体类型
	defaultSerializer runtime.StorageSerializer, // 序列化器，用于编码、解码
	resourceEncodingConfig ResourceEncodingConfig, // 用于获取资源的存储版本以及__internal版本
	resourceConfig APIResourceConfigSource, // 用于获取哪些资源是启用的，哪些是禁用的
	specialDefaultResourcePrefixes map[schema.GroupResource]string, // 某些资源的前缀配置
) *DefaultStorageFactory {
	config.Paging = utilfeature.DefaultFeatureGate.Enabled(features.APIListChunking)
	// 如果没有指定默认的媒体类型，那么默认就是使用JSON格式存储
	if len(defaultMediaType) == 0 {
		defaultMediaType = runtime.ContentTypeJSON
	}
	return &DefaultStorageFactory{
		StorageConfig:           config,
		Overrides:               map[schema.GroupResource]groupResourceOverrides{},
		DefaultMediaType:        defaultMediaType,
		DefaultSerializer:       defaultSerializer,
		ResourceEncodingConfig:  resourceEncodingConfig,
		APIResourceConfigSource: resourceConfig,
		DefaultResourcePrefixes: specialDefaultResourcePrefixes,

		newStorageCodecFn: NewStorageCodec,
	}
}

// SetEtcdLocation 设置资源的存储位置
func (s *DefaultStorageFactory) SetEtcdLocation(groupResource schema.GroupResource, location []string) {
	overrides := s.Overrides[groupResource]
	overrides.etcdLocation = location
	s.Overrides[groupResource] = overrides
}

// SetEtcdPrefix 设置资源的存储前缀
func (s *DefaultStorageFactory) SetEtcdPrefix(groupResource schema.GroupResource, prefix string) {
	overrides := s.Overrides[groupResource]
	overrides.etcdPrefix = prefix
	s.Overrides[groupResource] = overrides
}

// SetDisableAPIListChunking allows a specific resource to disable paging at the storage layer, to prevent
// exposure of key names in continuations. This may be overridden by feature gates.
func (s *DefaultStorageFactory) SetDisableAPIListChunking(groupResource schema.GroupResource) {
	overrides := s.Overrides[groupResource]
	overrides.disablePaging = true
	s.Overrides[groupResource] = overrides
}

// SetResourceEtcdPrefix sets the prefix for a resource, but not the base-dir.  You'll end up in `etcdPrefix/resourceEtcdPrefix`.
func (s *DefaultStorageFactory) SetResourceEtcdPrefix(groupResource schema.GroupResource, prefix string) {
	overrides := s.Overrides[groupResource]
	overrides.etcdResourcePrefix = prefix
	s.Overrides[groupResource] = overrides
}

func (s *DefaultStorageFactory) SetSerializer(groupResource schema.GroupResource, mediaType string, serializer runtime.StorageSerializer) {
	overrides := s.Overrides[groupResource]
	overrides.mediaType = mediaType
	overrides.serializer = serializer
	s.Overrides[groupResource] = overrides
}

// AddCohabitatingResources links resources together the order of the slice matters!  its the priority order of lookup for finding a storage location
func (s *DefaultStorageFactory) AddCohabitatingResources(groupResources ...schema.GroupResource) {
	for _, groupResource := range groupResources {
		overrides := s.Overrides[groupResource]
		overrides.cohabitatingResources = groupResources
		s.Overrides[groupResource] = overrides
	}
}

func (s *DefaultStorageFactory) AddSerializationChains(encoderDecoratorFn func(runtime.Encoder) runtime.Encoder, decoderDecoratorFn func([]runtime.Decoder) []runtime.Decoder, groupResources ...schema.GroupResource) {
	for _, groupResource := range groupResources {
		overrides := s.Overrides[groupResource]
		overrides.encoderDecoratorFn = encoderDecoratorFn
		overrides.decoderDecoratorFn = decoderDecoratorFn
		s.Overrides[groupResource] = overrides
	}
}

func getAllResourcesAlias(resource schema.GroupResource) schema.GroupResource {
	return schema.GroupResource{Group: resource.Group, Resource: AllResources}
}

func (s *DefaultStorageFactory) getStorageGroupResource(groupResource schema.GroupResource) schema.GroupResource {
	for _, potentialStorageResource := range s.Overrides[groupResource].cohabitatingResources {
		// TODO deads2k or liggitt determine if have ever stored any of our cohabitating resources in a different location on new clusters
		if s.APIResourceConfigSource.AnyResourceForGroupEnabled(potentialStorageResource.Group) {
			return potentialStorageResource
		}
	}

	return groupResource
}

// NewConfig New finds the storage destination for the given group and resource. It will
// return an error if the group has no storage destination configured.
// 1、返回某个资源的存储配置
func (s *DefaultStorageFactory) NewConfig(groupResource schema.GroupResource) (*storagebackend.ConfigForResource, error) {
	// 1、获取当前资源的持久化GR，对于绝大多数的资源来说，都是它本身。
	// 2、但是对于networkpolicies, deployments, daemonsets, replicasets, events, replicationcontrollers, podsecuritypolicies, ingresses
	// 资源来说，由于这些资源同时在两个组下，因此需要选择其中的一个组来进行存储，这个时候就可能从一个GR变换为另外一个GR
	chosenStorageResource := s.getStorageGroupResource(groupResource)

	// 拷贝默认的存储配置，这个存储配置是默认的存储配置，有些资源做了特殊的配置，后面会进行对应的初始化
	storageConfig := s.StorageConfig
	codecConfig := StorageCodecConfig{
		StorageMediaType:  s.DefaultMediaType,
		StorageSerializer: s.DefaultSerializer,
	}

	// 如果对于某个组进行了通用配置（即resource=*），那么优先应用通用配置
	if override, ok := s.Overrides[getAllResourcesAlias(chosenStorageResource)]; ok {
		override.Apply(&storageConfig, &codecConfig)
	}
	// 1、如果对于当前的资源进行了单独的配置，那么还需要应用这个资源的配置
	// 2、从这里可以看出，即使我们通过resource=*的方式对于某个组进行了通用配置，但是我们还是可以指定这个组下的具体资源再次进行配置
	if override, ok := s.Overrides[chosenStorageResource]; ok {
		override.Apply(&storageConfig, &codecConfig)
	}

	var err error
	// 返回这个资源优先选择的持久化GV
	codecConfig.StorageVersion, err = s.ResourceEncodingConfig.StorageEncodingFor(chosenStorageResource)
	if err != nil {
		return nil, err
	}
	// 返回这个资源优先选择的__internal GV
	codecConfig.MemoryVersion, err = s.ResourceEncodingConfig.InMemoryEncodingFor(groupResource)
	if err != nil {
		return nil, err
	}
	codecConfig.Config = storageConfig

	// 获取当前资源的编解码器
	storageConfig.Codec, storageConfig.EncodeVersioner, err = s.newStorageCodecFn(codecConfig)
	if err != nil {
		return nil, err
	}
	klog.V(3).Infof("storing %v in %v, reading as %v from %#v", groupResource,
		codecConfig.StorageVersion, codecConfig.MemoryVersion, codecConfig.Config)

	return storageConfig.ForResource(groupResource), nil
}

// Backends returns all backends for all registered storage destinations.
// Used for getting all instances for health validations.
func (s *DefaultStorageFactory) Backends() []Backend {
	return backends(s.StorageConfig, s.Overrides)
}

// Backends returns all backends for all registered storage destinations.
// Used for getting all instances for health validations.
func Backends(storageConfig storagebackend.Config) []Backend {
	return backends(storageConfig, nil)
}

func backends(storageConfig storagebackend.Config, grOverrides map[schema.GroupResource]groupResourceOverrides) []Backend {
	servers := sets.NewString(storageConfig.Transport.ServerList...)

	for _, overrides := range grOverrides {
		servers.Insert(overrides.etcdLocation...)
	}

	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
	}
	if len(storageConfig.Transport.CertFile) > 0 && len(storageConfig.Transport.KeyFile) > 0 {
		cert, err := tls.LoadX509KeyPair(storageConfig.Transport.CertFile, storageConfig.Transport.KeyFile)
		if err != nil {
			klog.Errorf("failed to load key pair while getting backends: %s", err)
		} else {
			tlsConfig.Certificates = []tls.Certificate{cert}
		}
	}
	if len(storageConfig.Transport.TrustedCAFile) > 0 {
		if caCert, err := ioutil.ReadFile(storageConfig.Transport.TrustedCAFile); err != nil {
			klog.Errorf("failed to read ca file while getting backends: %s", err)
		} else {
			caPool := x509.NewCertPool()
			caPool.AppendCertsFromPEM(caCert)
			tlsConfig.RootCAs = caPool
			tlsConfig.InsecureSkipVerify = false
		}
	}

	backends := []Backend{}
	for server := range servers {
		backends = append(backends, Backend{
			Server: server,
			// We can't share TLSConfig across different backends to avoid races.
			// For more details see: https://pr.k8s.io/59338
			TLSConfig: tlsConfig.Clone(),
		})
	}
	return backends
}

func (s *DefaultStorageFactory) ResourcePrefix(groupResource schema.GroupResource) string {
	chosenStorageResource := s.getStorageGroupResource(groupResource)
	groupOverride := s.Overrides[getAllResourcesAlias(chosenStorageResource)]
	exactResourceOverride := s.Overrides[chosenStorageResource]

	etcdResourcePrefix := s.DefaultResourcePrefixes[chosenStorageResource]
	if len(groupOverride.etcdResourcePrefix) > 0 {
		etcdResourcePrefix = groupOverride.etcdResourcePrefix
	}
	if len(exactResourceOverride.etcdResourcePrefix) > 0 {
		etcdResourcePrefix = exactResourceOverride.etcdResourcePrefix
	}
	if len(etcdResourcePrefix) == 0 {
		etcdResourcePrefix = strings.ToLower(chosenStorageResource.Resource)
	}

	return etcdResourcePrefix
}
