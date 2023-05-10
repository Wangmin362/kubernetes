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

package storagebackend

import (
	"time"

	oteltrace "go.opentelemetry.io/otel/trace"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/server/egressselector"
	"k8s.io/apiserver/pkg/storage/etcd3"
	"k8s.io/apiserver/pkg/storage/value"
	flowcontrolrequest "k8s.io/apiserver/pkg/util/flowcontrol/request"
)

const (
	// 默认就是ETCD3存储类型
	StorageTypeUnset = ""
	StorageTypeETCD2 = "etcd2"
	StorageTypeETCD3 = "etcd3"

	DefaultCompactInterval      = 5 * time.Minute
	DefaultDBMetricPollInterval = 30 * time.Second
	DefaultHealthcheckTimeout   = 2 * time.Second
	DefaultReadinessTimeout     = 2 * time.Second
)

// TransportConfig holds all connection related info,  i.e. equal TransportConfig means equal servers we talk to.
type TransportConfig struct {
	// ServerList is the list of storage servers to connect with.
	ServerList []string
	// TLS credentials
	KeyFile       string
	CertFile      string
	TrustedCAFile string
	// function to determine the egress dialer. (i.e. konnectivity server dialer)
	EgressLookup egressselector.Lookup
	// The TracerProvider can add tracing the connection
	TracerProvider oteltrace.TracerProvider
}

// Config is configuration for creating a storage backend.
// 该配置为K8S资源的后端存储配置，每个资源都应该有一个这样的配置，因为每个资源的存储配置极有可能是不同的
type Config struct {
	// Type defines the type of storage backend. Default ("") is "etcd3".
	// K8S的后端存储类型，空默认为ETCD3存储  TODO 这个类型有啥作用
	Type string
	// Prefix is the prefix to all keys passed to storage.Interface methods.
	// K8S存储ETCD的KEY的前缀，原因是K8S用户搭建的ETCD集群可能不单单是给K8S使用，可能还会有其它应用
	Prefix string
	// Transport holds all connection related info, i.e. equal TransportConfig means equal servers we talk to.
	// 传输配置，譬如CA证书、后端存储地址
	Transport TransportConfig
	// Paging indicates whether the server implementation should allow paging (if it is
	// supported). This is generally configured by feature gating, or by a specific
	// resource type not wishing to allow paging, and is not intended for end users to
	// set.
	// TODO 似乎是和分页相关
	Paging bool

	// 该资源的编解码器，实际上就是一个序列化/反序列化器
	Codec runtime.Codec
	// EncodeVersioner is the same groupVersioner used to build the
	// storage encoder. Given a list of kinds the input object might belong
	// to, the EncodeVersioner outputs the gvk the object will be
	// converted to before persisted in etcd.
	// TODO 如何理解这个属性？
	EncodeVersioner runtime.GroupVersioner
	// Transformer allows the value to be transformed prior to persisting into etcd.
	// 1、在把K8S资源对象序列化为二进制数据存储ETCD之前可能需要转换，譬如数据加密
	// 2、自然而然，Transformer接口内部有两个方法，其一是需要把数据通过某种方式转换为另外的数据，譬如加密
	// 其二是需要把加密的数据机型解密，正好是其一的逆过程
	Transformer value.Transformer

	// CompactionInterval is an interval of requesting compaction from apiserver.
	// If the value is 0, no compaction will be issued.
	CompactionInterval time.Duration
	// CountMetricPollPeriod specifies how often should count metric be updated
	CountMetricPollPeriod time.Duration
	// DBMetricPollInterval specifies how often should storage backend metric be updated.
	DBMetricPollInterval time.Duration
	// HealthcheckTimeout specifies the timeout used when checking health
	HealthcheckTimeout time.Duration
	// ReadycheckTimeout specifies the timeout used when checking readiness
	ReadycheckTimeout time.Duration

	// TODO ETCD的租约
	LeaseManagerConfig etcd3.LeaseManagerConfig

	// StorageObjectCountTracker is used to keep track of the total
	// number of objects in the storage per resource.
	// 用于统计跟踪一个对象的存储情况 TODO 详细分析
	StorageObjectCountTracker flowcontrolrequest.StorageObjectCountTracker
}

// ConfigForResource is a Config specialized to a particular `schema.GroupResource`
type ConfigForResource struct {
	// Config is the resource-independent configuration
	// K8S资源的存储后端配置，即当K8S需要存储一个资源的时候，这个资源到底该怎么存储？前缀是啥？如何序列化？这个资源的存储后端是谁？
	Config

	// GroupResource is the relevant one
	// TODO 用于标识当前配置是哪种资源配置  为什么不需要Version?  难道是K8S通过这一特性强制规定了相同的资源，不同的版本其存储配置必须一样？
	GroupResource schema.GroupResource
}

// ForResource specializes to the given resource
func (config *Config) ForResource(resource schema.GroupResource) *ConfigForResource {
	return &ConfigForResource{
		Config:        *config,
		GroupResource: resource,
	}
}

func NewDefaultConfig(prefix string, codec runtime.Codec) *Config {
	return &Config{
		Paging:               true,
		Prefix:               prefix,
		Codec:                codec,
		CompactionInterval:   DefaultCompactInterval,
		DBMetricPollInterval: DefaultDBMetricPollInterval,
		HealthcheckTimeout:   DefaultHealthcheckTimeout,
		ReadycheckTimeout:    DefaultReadinessTimeout,
		LeaseManagerConfig:   etcd3.NewDefaultLeaseManagerConfig(),
		Transport:            TransportConfig{TracerProvider: oteltrace.NewNoopTracerProvider()},
	}
}
