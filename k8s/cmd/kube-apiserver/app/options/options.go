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

// Package options contains flags and options for initializing an apiserver
package options

import (
	"net"
	"strings"
	"time"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	genericoptions "k8s.io/apiserver/pkg/server/options"
	"k8s.io/apiserver/pkg/storage/storagebackend"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/logs"
	"k8s.io/component-base/metrics"

	logsapi "k8s.io/component-base/logs/api/v1"
	api "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/cluster/ports"
	"k8s.io/kubernetes/pkg/controlplane/reconcilers"
	_ "k8s.io/kubernetes/pkg/features" // add the kubernetes feature gates
	kubeoptions "k8s.io/kubernetes/pkg/kubeapiserver/options"
	kubeletclient "k8s.io/kubernetes/pkg/kubelet/client"
	"k8s.io/kubernetes/pkg/serviceaccount"
)

// ServerRunOptions runs a kubernetes api server.
// 1、APIServer的运行参数，这些参数是通过命令行参数传递过来
type ServerRunOptions struct {
	GenericServerRunOptions *genericoptions.ServerRunOptions                 // Server运行的通用配置
	Etcd                    *genericoptions.EtcdOptions                      // ETCD相关配置
	SecureServing           *genericoptions.SecureServingOptionsWithLoopback // HTTPS相关配置
	Audit                   *genericoptions.AuditOptions                     // 审计相关配置
	Features                *genericoptions.FeatureOptions                   // 特性管相关配置，譬如pprof,debug
	Admission               *kubeoptions.AdmissionOptions                    // 准入控制配置
	Authentication          *kubeoptions.BuiltInAuthenticationOptions        // 认证配置，主要是配置支持几种认证模式，以及每种认证模式相关的配置
	Authorization           *kubeoptions.BuiltInAuthorizationOptions         // 及安全配置，主要是配置支持集中及安全模式以及每种鉴权模式相关的配置
	CloudProvider           *kubeoptions.CloudProviderOptions                // TODO CloudProvider相关配置
	APIEnablement           *genericoptions.APIEnablementOptions             // 配置启用/禁用哪些资源
	EgressSelector          *genericoptions.EgressSelectorOptions            // TODO Konnectivity配置
	Metrics                 *metrics.Options                                 // 指标配置
	Logs                    *logs.Options                                    // 日志配置
	Traces                  *genericoptions.TracingOptions                   // 链路追踪配置

	AllowPrivileged bool // 是否允许创建特权容器，通过Linux的Capability来实现
	// LogHandler有啥作用？什么时候应该开启？
	EnableLogsHandler bool
	EventTTL          time.Duration
	KubeletConfig     kubeletclient.KubeletClientConfig
	// 如果设置为0，表示kubernetes.default.svc为ClusterIP模式。如果指定了端口，那么就是NodePort端口
	KubernetesServiceNodePort int
	MaxConnectionBytesPerSec  int64
	// ServiceClusterIPRange is mapped to input provided by user
	ServiceClusterIPRanges string
	// PrimaryServiceClusterIPRange and SecondaryServiceClusterIPRange are the results
	// of parsing ServiceClusterIPRange into actual values
	// TODO 这两个参数的作用
	PrimaryServiceClusterIPRange   net.IPNet
	SecondaryServiceClusterIPRange net.IPNet
	// APIServerServiceIP is the first valid IP from PrimaryServiceClusterIPRange
	APIServerServiceIP net.IP

	ServiceNodePortRange utilnet.PortRange

	ProxyClientCertFile string
	ProxyClientKeyFile  string

	// TODO 有何作用？
	EnableAggregatorRouting             bool
	AggregatorRejectForwardingRedirects bool

	MasterCount            int
	EndpointReconcilerType string

	ServiceAccountSigningKeyFile     string
	ServiceAccountIssuer             serviceaccount.TokenGenerator
	ServiceAccountTokenMaxExpiration time.Duration

	ShowHiddenMetricsForVersion string
}

// NewServerRunOptions creates a new ServerRunOptions object with default parameters
func NewServerRunOptions() *ServerRunOptions {
	s := ServerRunOptions{
		// 通用配置，其中的值是通过实例化了一个默认的GenericAPIServer配置，然后拷贝其默认值到此配置当中
		GenericServerRunOptions: genericoptions.NewServerRunOptions(),
		// 1、ETCD相关配置，譬如APIServer访问ETCD使用的证书，以及ETCD的服务器根证书
		// 2、默认K8S存入ETCD的所有资源的前缀都是/registry
		// TODO 猜测这里的存储后端是一个抽象接口，让我们可以选择把数据存入其它数据库当中
		Etcd: genericoptions.NewEtcdOptions(storagebackend.NewDefaultConfig(kubeoptions.DefaultEtcdPathPrefix, nil)),
		// 安全服务配置，用于设置APIServer监听的地址、端口以及提供HTTPS所使用的证书
		SecureServing: kubeoptions.NewSecureServingOptions(),
		// 审计配置，审计功能解决了APIServer持久化审计事件，方便K8S用户溯源
		Audit: genericoptions.NewAuditOptions(),
		// 特性配置，主要用户Debug K8S, 查看K8S的性能
		Features: genericoptions.NewFeatureOptions(),
		// 准入控制配置
		Admission: kubeoptions.NewAdmissionOptions(),
		// 认证参数
		Authentication: kubeoptions.NewBuiltInAuthenticationOptions().WithAll(),
		// 鉴权参数
		Authorization: kubeoptions.NewBuiltInAuthorizationOptions(),
		// TODO CloudProvider配置
		CloudProvider: kubeoptions.NewCloudProviderOptions(),
		// 用于启用或者禁用K8S的API
		APIEnablement: genericoptions.NewAPIEnablementOptions(),
		// TODO Konnectivity配置
		EgressSelector: genericoptions.NewEgressSelectorOptions(),
		// APIServer Metric指标
		Metrics: metrics.NewOptions(),
		// APIServer的日志配置
		Logs: logs.NewOptions(),
		// TODO 链路追踪
		Traces: genericoptions.NewTracingOptions(),

		EnableLogsHandler:      true,
		EventTTL:               1 * time.Hour,
		MasterCount:            1,
		EndpointReconcilerType: string(reconcilers.LeaseEndpointReconcilerType),
		KubeletConfig: kubeletclient.KubeletClientConfig{
			Port:         ports.KubeletPort,
			ReadOnlyPort: ports.KubeletReadOnlyPort,
			PreferredAddressTypes: []string{
				// --override-hostname
				string(api.NodeHostName),

				// internal, preferring DNS if reported
				string(api.NodeInternalDNS),
				string(api.NodeInternalIP),

				// external, preferring DNS if reported
				string(api.NodeExternalDNS),
				string(api.NodeExternalIP),
			},
			HTTPTimeout: time.Duration(5) * time.Second,
		},
		ServiceNodePortRange:                kubeoptions.DefaultServiceNodePortRange,
		AggregatorRejectForwardingRedirects: true,
	}

	// Overwrite the default for storage data format.
	s.Etcd.DefaultStorageMediaType = "application/vnd.kubernetes.protobuf"

	return &s
}

// Flags returns flags for a specific APIServer by section name
func (s *ServerRunOptions) Flags() (fss cliflag.NamedFlagSets) {
	// Add the generic flags.
	s.GenericServerRunOptions.AddUniversalFlags(fss.FlagSet("generic"))
	s.Etcd.AddFlags(fss.FlagSet("etcd"))
	s.SecureServing.AddFlags(fss.FlagSet("secure serving"))
	s.Audit.AddFlags(fss.FlagSet("auditing"))
	s.Features.AddFlags(fss.FlagSet("features"))
	s.Authentication.AddFlags(fss.FlagSet("authentication"))
	s.Authorization.AddFlags(fss.FlagSet("authorization"))
	s.CloudProvider.AddFlags(fss.FlagSet("cloud provider"))
	s.APIEnablement.AddFlags(fss.FlagSet("API enablement"))
	s.EgressSelector.AddFlags(fss.FlagSet("egress selector"))
	s.Admission.AddFlags(fss.FlagSet("admission"))
	s.Metrics.AddFlags(fss.FlagSet("metrics"))
	logsapi.AddFlags(s.Logs, fss.FlagSet("logs"))
	s.Traces.AddFlags(fss.FlagSet("traces"))

	// Note: the weird ""+ in below lines seems to be the only way to get gofmt to
	// arrange these text blocks sensibly. Grrr.
	fs := fss.FlagSet("misc")
	fs.DurationVar(&s.EventTTL, "event-ttl", s.EventTTL,
		"Amount of time to retain events.")

	fs.BoolVar(&s.AllowPrivileged, "allow-privileged", s.AllowPrivileged,
		"If true, allow privileged containers. [default=false]")

	fs.BoolVar(&s.EnableLogsHandler, "enable-logs-handler", s.EnableLogsHandler,
		"If true, install a /logs handler for the apiserver logs.")
	fs.MarkDeprecated("enable-logs-handler", "This flag will be removed in v1.19")

	fs.Int64Var(&s.MaxConnectionBytesPerSec, "max-connection-bytes-per-sec", s.MaxConnectionBytesPerSec, ""+
		"If non-zero, throttle each user connection to this number of bytes/sec. "+
		"Currently only applies to long-running requests.")

	fs.IntVar(&s.MasterCount, "apiserver-count", s.MasterCount,
		"The number of apiservers running in the cluster, must be a positive number. (In use when --endpoint-reconciler-type=master-count is enabled.)")
	fs.MarkDeprecated("apiserver-count", "apiserver-count is deprecated and will be removed in a future version.")

	fs.StringVar(&s.EndpointReconcilerType, "endpoint-reconciler-type", s.EndpointReconcilerType,
		"Use an endpoint reconciler ("+strings.Join(reconcilers.AllTypes.Names(), ", ")+") master-count is deprecated, and will be removed in a future version.")

	// See #14282 for details on how to test/try this option out.
	// TODO: remove this comment once this option is tested in CI.
	fs.IntVar(&s.KubernetesServiceNodePort, "kubernetes-service-node-port", s.KubernetesServiceNodePort, ""+
		"If non-zero, the Kubernetes master service (which apiserver creates/maintains) will be "+
		"of type NodePort, using this as the value of the port. If zero, the Kubernetes master "+
		"service will be of type ClusterIP.")

	fs.StringVar(&s.ServiceClusterIPRanges, "service-cluster-ip-range", s.ServiceClusterIPRanges, ""+
		"A CIDR notation IP range from which to assign service cluster IPs. This must not "+
		"overlap with any IP ranges assigned to nodes or pods. Max of two dual-stack CIDRs is allowed.")

	fs.Var(&s.ServiceNodePortRange, "service-node-port-range", ""+
		"A port range to reserve for services with NodePort visibility.  This must not overlap with the ephemeral port range on nodes.  "+
		"Example: '30000-32767'. Inclusive at both ends of the range.")

	// Kubelet related flags:
	fs.StringSliceVar(&s.KubeletConfig.PreferredAddressTypes, "kubelet-preferred-address-types", s.KubeletConfig.PreferredAddressTypes,
		"List of the preferred NodeAddressTypes to use for kubelet connections.")

	fs.UintVar(&s.KubeletConfig.Port, "kubelet-port", s.KubeletConfig.Port,
		"DEPRECATED: kubelet port.")
	fs.MarkDeprecated("kubelet-port", "kubelet-port is deprecated and will be removed.")

	fs.UintVar(&s.KubeletConfig.ReadOnlyPort, "kubelet-read-only-port", s.KubeletConfig.ReadOnlyPort,
		"DEPRECATED: kubelet read only port.")
	fs.MarkDeprecated("kubelet-read-only-port", "kubelet-read-only-port is deprecated and will be removed.")

	fs.DurationVar(&s.KubeletConfig.HTTPTimeout, "kubelet-timeout", s.KubeletConfig.HTTPTimeout,
		"Timeout for kubelet operations.")

	fs.StringVar(&s.KubeletConfig.TLSClientConfig.CertFile, "kubelet-client-certificate", s.KubeletConfig.TLSClientConfig.CertFile,
		"Path to a client cert file for TLS.")

	fs.StringVar(&s.KubeletConfig.TLSClientConfig.KeyFile, "kubelet-client-key", s.KubeletConfig.TLSClientConfig.KeyFile,
		"Path to a client key file for TLS.")

	fs.StringVar(&s.KubeletConfig.TLSClientConfig.CAFile, "kubelet-certificate-authority", s.KubeletConfig.TLSClientConfig.CAFile,
		"Path to a cert file for the certificate authority.")

	fs.StringVar(&s.ProxyClientCertFile, "proxy-client-cert-file", s.ProxyClientCertFile, ""+
		"Client certificate used to prove the identity of the aggregator or kube-apiserver "+
		"when it must call out during a request. This includes proxying requests to a user "+
		"api-server and calling out to webhook admission plugins. It is expected that this "+
		"cert includes a signature from the CA in the --requestheader-client-ca-file flag. "+
		"That CA is published in the 'extension-apiserver-authentication' configmap in "+
		"the kube-system namespace. Components receiving calls from kube-aggregator should "+
		"use that CA to perform their half of the mutual TLS verification.")
	fs.StringVar(&s.ProxyClientKeyFile, "proxy-client-key-file", s.ProxyClientKeyFile, ""+
		"Private key for the client certificate used to prove the identity of the aggregator or kube-apiserver "+
		"when it must call out during a request. This includes proxying requests to a user "+
		"api-server and calling out to webhook admission plugins.")

	fs.BoolVar(&s.EnableAggregatorRouting, "enable-aggregator-routing", s.EnableAggregatorRouting,
		"Turns on aggregator routing requests to endpoints IP rather than cluster IP.")

	fs.BoolVar(&s.AggregatorRejectForwardingRedirects, "aggregator-reject-forwarding-redirect", s.AggregatorRejectForwardingRedirects,
		"Aggregator reject forwarding redirect response back to client.")

	fs.StringVar(&s.ServiceAccountSigningKeyFile, "service-account-signing-key-file", s.ServiceAccountSigningKeyFile, ""+
		"Path to the file that contains the current private key of the service account token issuer. The issuer will sign issued ID tokens with this private key.")

	return fss
}
