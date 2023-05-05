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
// ServerRunOptions的所有属性是通过启动apiserver时传入的参数初始化的，由于大部分属性都是指针，所以在实例化ServerRunOptions的时候
// 需要对ServerRunOptions的指针属性初始化，这样后续通过反序列化命令行参数到ServerRunOptions属性的时候就可以正常工作
type ServerRunOptions struct {
	GenericServerRunOptions *genericoptions.ServerRunOptions                 // genericServer配置
	Etcd                    *genericoptions.EtcdOptions                      // ETCD的配置
	SecureServing           *genericoptions.SecureServingOptionsWithLoopback // TLS相关配置
	// TODO K8S是如何实现审计的？
	Audit    *genericoptions.AuditOptions   // 审计相关配置
	Features *genericoptions.FeatureOptions // K8S的特性开关
	// TODO K8S是如何实现准入控制的？ 动态注入控制又是如何实现的？
	Admission      *kubeoptions.AdmissionOptions             // 准入控制
	Authentication *kubeoptions.BuiltInAuthenticationOptions // 认证相关
	Authorization  *kubeoptions.BuiltInAuthorizationOptions  // 授权相关
	// TODO cloudProvider是啥？
	CloudProvider *kubeoptions.CloudProviderOptions
	// 什么叫做APIEnablement? 答：所谓的APIEnablement，实际上就是启用哪些资源，禁用那些资源
	// 用于可以通过runtime-config配置参数来配置
	APIEnablement *genericoptions.APIEnablementOptions
	// TODO K8S有master, worker两种节点，他们之间的通信需要安全加密 而Konnectivity就是用于解决这个问题的
	// TODO egressorselector就是在配置Konnectivity
	// 参考：https://kubernetes.io/zh-cn/docs/tasks/extend-kubernetes/setup-konnectivity/
	// https://www.jianshu.com/p/d1528a9d3cfa
	EgressSelector *genericoptions.EgressSelectorOptions
	// TODO 指标参数，通过此参数可以配置开启哪些资源的指标，或者禁用哪些资源的指标
	Metrics *metrics.Options
	Logs    *logs.Options
	Traces  *genericoptions.TracingOptions

	// TODO 这个参数有啥用？
	AllowPrivileged bool
	// TODO logHandler的开启和关闭有啥影响？
	EnableLogsHandler bool
	EventTTL          time.Duration
	// TODO 为什么apiserver需要kubelet的配置
	KubeletConfig kubeletclient.KubeletClientConfig
	// TODO 这个是apiserver的6553端口么
	KubernetesServiceNodePort int
	MaxConnectionBytesPerSec  int64
	// ServiceClusterIPRange is mapped to input provided by user
	ServiceClusterIPRanges string
	// PrimaryServiceClusterIPRange and SecondaryServiceClusterIPRange are the results
	// of parsing ServiceClusterIPRange into actual values
	// TODO 这俩破玩意是干啥的？
	PrimaryServiceClusterIPRange   net.IPNet
	SecondaryServiceClusterIPRange net.IPNet
	// APIServerServiceIP is the first valid IP from PrimaryServiceClusterIPRange
	// 用于广播给集群的所有成员自己的IP地址，不指定的话就使用"--bind-address"的IP地址
	APIServerServiceIP net.IP

	// nodeport端口范围 todo 为什么apiserver需要关心这个参数，这不是kubelet的参数么？
	ServiceNodePortRange utilnet.PortRange

	// TODO proxy是啥意思？ 用来干嘛的
	ProxyClientCertFile string
	ProxyClientKeyFile  string

	// TODO 这个应该和aggregate apiserver有关
	EnableAggregatorRouting             bool
	AggregatorRejectForwardingRedirects bool

	// master的数量
	MasterCount            int
	EndpointReconcilerType string

	IdentityLeaseDurationSeconds      int
	IdentityLeaseRenewIntervalSeconds int

	// TODO 和BootstrapToken相关？
	ServiceAccountSigningKeyFile     string
	ServiceAccountIssuer             serviceaccount.TokenGenerator
	ServiceAccountTokenMaxExpiration time.Duration

	ShowHiddenMetricsForVersion string
}

// NewServerRunOptions creates a new ServerRunOptions object with default parameters
func NewServerRunOptions() *ServerRunOptions {
	s := ServerRunOptions{
		GenericServerRunOptions: genericoptions.NewServerRunOptions(),
		Etcd:                    genericoptions.NewEtcdOptions(storagebackend.NewDefaultConfig(kubeoptions.DefaultEtcdPathPrefix, nil)),
		SecureServing:           kubeoptions.NewSecureServingOptions(),
		Audit:                   genericoptions.NewAuditOptions(),
		Features:                genericoptions.NewFeatureOptions(),
		Admission:               kubeoptions.NewAdmissionOptions(),                       // TODO 准入控制参数，注册了需要的注入控制器
		Authentication:          kubeoptions.NewBuiltInAuthenticationOptions().WithAll(), // TODO 构建默认的认证参数,主要是将内部的指针属性实例化出来，避免后续通过apiserver参数初始化的时候属性为nil
		Authorization:           kubeoptions.NewBuiltInAuthorizationOptions(),            // 构建默认的授权参数
		CloudProvider:           kubeoptions.NewCloudProviderOptions(),
		APIEnablement:           genericoptions.NewAPIEnablementOptions(),
		EgressSelector:          genericoptions.NewEgressSelectorOptions(),
		Metrics:                 metrics.NewOptions(),
		Logs:                    logs.NewOptions(),
		Traces:                  genericoptions.NewTracingOptions(),

		EnableLogsHandler: true,
		// TODO 意思是K8S中的时间只能保存一个小时？
		EventTTL:                          1 * time.Hour,
		MasterCount:                       1,
		EndpointReconcilerType:            string(reconcilers.LeaseEndpointReconcilerType),
		IdentityLeaseDurationSeconds:      3600,
		IdentityLeaseRenewIntervalSeconds: 10,
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
	// TODO ETCD的默认媒体类型为什么是 application/vnd.kubernetes.protobuf, 这种MIME有啥特性？
	s.Etcd.DefaultStorageMediaType = "application/vnd.kubernetes.protobuf"

	return &s
}

// Flags returns flags for a specific APIServer by section name
// TODO 根据命令行传入的参数初始化ServerRunOptions
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

	fs.StringVar(&s.EndpointReconcilerType, "endpoint-reconciler-type", string(s.EndpointReconcilerType),
		"Use an endpoint reconciler ("+strings.Join(reconcilers.AllTypes.Names(), ", ")+") master-count is deprecated, and will be removed in a future version.")

	fs.IntVar(&s.IdentityLeaseDurationSeconds, "identity-lease-duration-seconds", s.IdentityLeaseDurationSeconds,
		"The duration of kube-apiserver lease in seconds, must be a positive number. (In use when the APIServerIdentity feature gate is enabled.)")

	fs.IntVar(&s.IdentityLeaseRenewIntervalSeconds, "identity-lease-renew-interval-seconds", s.IdentityLeaseRenewIntervalSeconds,
		"The interval of kube-apiserver renewing its lease in seconds, must be a positive number. (In use when the APIServerIdentity feature gate is enabled.)")

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

	fs.StringVar(&s.KubeletConfig.CertFile, "kubelet-client-certificate", s.KubeletConfig.CertFile,
		"Path to a client cert file for TLS.")

	fs.StringVar(&s.KubeletConfig.KeyFile, "kubelet-client-key", s.KubeletConfig.KeyFile,
		"Path to a client key file for TLS.")

	fs.StringVar(&s.KubeletConfig.CAFile, "kubelet-certificate-authority", s.KubeletConfig.CAFile,
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
