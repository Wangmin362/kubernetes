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

// Package app does all of the work necessary to create a Kubernetes
// APIServer by binding together the API, master and APIServer infrastructure.
// It can be configured and called directly or via the hyperkube framework.
package app

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	oteltrace "go.opentelemetry.io/otel/trace"

	extensionsapiserver "k8s.io/apiextensions-apiserver/pkg/apiserver"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/cel/openapi/resolver"
	"k8s.io/apiserver/pkg/endpoints/discovery/aggregated"
	genericapifilters "k8s.io/apiserver/pkg/endpoints/filters"
	openapinamer "k8s.io/apiserver/pkg/endpoints/openapi"
	genericfeatures "k8s.io/apiserver/pkg/features"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/egressselector"
	"k8s.io/apiserver/pkg/server/filters"
	serveroptions "k8s.io/apiserver/pkg/server/options"
	serverstorage "k8s.io/apiserver/pkg/server/storage"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	utilflowcontrol "k8s.io/apiserver/pkg/util/flowcontrol"
	"k8s.io/apiserver/pkg/util/notfoundhandler"
	"k8s.io/apiserver/pkg/util/openapi"
	"k8s.io/apiserver/pkg/util/webhook"
	clientgoinformers "k8s.io/client-go/informers"
	clientgoclientset "k8s.io/client-go/kubernetes"
	k8sscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/keyutil"
	cliflag "k8s.io/component-base/cli/flag"
	"k8s.io/component-base/cli/globalflag"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	_ "k8s.io/component-base/metrics/prometheus/workqueue" // for workqueue metric registration
	"k8s.io/component-base/term"
	"k8s.io/component-base/version"
	"k8s.io/component-base/version/verflag"
	"k8s.io/klog/v2"
	aggregatorapiserver "k8s.io/kube-aggregator/pkg/apiserver"
	aggregatorscheme "k8s.io/kube-aggregator/pkg/apiserver/scheme"
	netutils "k8s.io/utils/net"

	"k8s.io/kubernetes/cmd/kube-apiserver/app/options"
	"k8s.io/kubernetes/pkg/api/legacyscheme"
	"k8s.io/kubernetes/pkg/capabilities"
	"k8s.io/kubernetes/pkg/controlplane"
	"k8s.io/kubernetes/pkg/controlplane/reconcilers"
	generatedopenapi "k8s.io/kubernetes/pkg/generated/openapi"
	"k8s.io/kubernetes/pkg/kubeapiserver"
	kubeapiserveradmission "k8s.io/kubernetes/pkg/kubeapiserver/admission"
	kubeauthenticator "k8s.io/kubernetes/pkg/kubeapiserver/authenticator"
	"k8s.io/kubernetes/pkg/kubeapiserver/authorizer/modes"
	rbacrest "k8s.io/kubernetes/pkg/registry/rbac/rest"
	"k8s.io/kubernetes/pkg/serviceaccount"
)

func init() {
	utilruntime.Must(logsapi.AddFeatureGates(utilfeature.DefaultMutableFeatureGate))
}

// NewAPIServerCommand creates a *cobra.Command object with default parameters
func NewAPIServerCommand() *cobra.Command {
	// TODO 实例化APIServer需要的参数,
	s := options.NewServerRunOptions()
	cmd := &cobra.Command{
		Use: "kube-apiserver",
		Long: `The Kubernetes API server validates and configures data
for the api objects which include pods, services, replicationcontrollers, and
others. The API Server services REST operations and provides the frontend to the
cluster's shared state through which all other components interact.`,

		// stop printing usage when the command errors
		SilenceUsage: true,
		PersistentPreRunE: func(*cobra.Command, []string) error {
			// silence client-go warnings.
			// kube-apiserver loopback clients should not log self-issued warnings.
			rest.SetDefaultWarningHandler(rest.NoWarnings{})
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// 如果使用传入了-h、--help, -v都会打印当前参数，并退出，该操作会停止启动APIServer
			verflag.PrintAndExitIfRequested()
			// TODO 获取启动APIServer时传递的参数
			fs := cmd.Flags()

			// Activate logging as soon as possible, after that
			// show flags with the final logging configuration.
			// TODO
			if err := logsapi.ValidateAndApply(s.Logs, utilfeature.DefaultFeatureGate); err != nil {
				return err
			}
			// 在APIServer启动之前打印所有的参数
			cliflag.PrintFlags(fs)

			// set default options
			// 1、设置AdvertiseAddress
			// 2、根据用户设置的--service-cluster-ip-range参数计算出来kubernetes.default.svc的IP地址以及主从SVC地址范围
			// 3、校验用户启用的认证模式，如果启用了AlwaysAllow的认证器，并且允许匿名用户访问，则允许匿名用户访问这个设置是多余的，会被设置为false
			// 4、检查ServiceAccountSigningKeyFile配置以及Etcd.EnableWatchCache配置
			completedOptions, err := Complete(s)
			if err != nil {
				return err
			}

			// 验证参数
			if errs := completedOptions.Validate(); len(errs) != 0 {
				return utilerrors.NewAggregate(errs)
			}
			// add feature enablement metrics
			utilfeature.DefaultMutableFeatureGate.AddMetrics()
			return Run(completedOptions, genericapiserver.SetupSignalHandler())
		},
		Args: func(cmd *cobra.Command, args []string) error {
			for _, arg := range args {
				if len(arg) > 0 {
					return fmt.Errorf("%q does not take any arguments, got %q", cmd.CommandPath(), args)
				}
			}
			return nil
		},
	}

	fs := cmd.Flags()
	namedFlagSets := s.Flags()
	verflag.AddFlags(namedFlagSets.FlagSet("global"))
	globalflag.AddGlobalFlags(namedFlagSets.FlagSet("global"), cmd.Name(), logs.SkipLoggingConfigurationFlags())
	options.AddCustomGlobalFlags(namedFlagSets.FlagSet("generic"))
	for _, f := range namedFlagSets.FlagSets {
		fs.AddFlagSet(f)
	}

	cols, _, _ := term.TerminalSize(cmd.OutOrStdout())
	cliflag.SetUsageAndHelpFunc(cmd, namedFlagSets, cols)

	return cmd
}

// Run runs the specified APIServer.  This should never exit.
func Run(completeOptions completedServerRunOptions, stopCh <-chan struct{}) error {
	// To help debugging, immediately log version
	klog.Infof("Version: %+v", version.Get())

	klog.InfoS("Golang settings", "GOGC", os.Getenv("GOGC"), "GOMAXPROCS",
		os.Getenv("GOMAXPROCS"), "GOTRACEBACK", os.Getenv("GOTRACEBACK"))

	// 1、这里所说的ServerChain其实指的是AggregatorServer, APIServer, ExtensionServer，这三个Server
	server, err := CreateServerChain(completeOptions)
	if err != nil {
		return err
	}

	prepared, err := server.PrepareRun()
	if err != nil {
		return err
	}

	return prepared.Run(stopCh)
}

//                          Request
//                             |
//                             |
//			+------------------↓--------------------+
//          |          AggregatorServer		        |
//          |      +-----------↓-------------+      |
//			|      |    GenericAPIServer     |      |
//          |      +-----------|-------------+      |
//          +------------------|--------------------+
//                             |
//                             |
//			+------------------↓--------------------+
//          |              APIServer		        |
//          |      +-----------↓-------------+      |
//			|      |    GenericAPIServer     |      |
//          |      +-----------|-------------+      |
//          +------------------|--------------------+
//                             |
//                             |
//			+------------------↓--------------------+
//          |           ExtensionServer             |
//          |      +-----------↓-------------+      |
//			|      |    GenericAPIServer     |      |
//          |      +-------------------------+      |
//          +---------------------------------------+
//                             |
//                             |
//			+------------------↓--------------------+
//          |           NotFoundHandler             |
//          +---------------------------------------+

// CreateServerChain creates the apiservers connected via delegation.
func CreateServerChain(completedOptions completedServerRunOptions) (*aggregatorapiserver.APIAggregator, error) {
	// 1、初始化APIServer配置，APIServer配置是通过GenericConfig配置加上额外的配置构成的
	// 2、ServiceResolver用于把Service解析为一个合法的URL
	// 3、PluginInitializer中的Plugin指的是准入控制插件，这里的初始化器是为了向准入控制插件当中注入需要的依赖
	kubeAPIServerConfig, serviceResolver, pluginInitializer, err := CreateKubeAPIServerConfig(completedOptions)
	if err != nil {
		return nil, err
	}

	// If additional API servers are added, they should be gated.
	// 创建ExtensionServer配置，ExtensionServer允许需要这些参数进行配置
	// TODO 分析RESTOptionsGetter原理
	apiExtensionsConfig, err := createAPIExtensionsConfig(
		*kubeAPIServerConfig.GenericConfig,                 // GenericServer的通用配置
		kubeAPIServerConfig.ExtraConfig.VersionedInformers, // SharedInformerFactory，用于缓存各种APIServer，可以理解为一个缓存，用于以空间换时间
		pluginInitializer,                                  // 准入控制插件初始化器，用于再准入控制插件初始化的时候注入需要的依赖
		completedOptions.ServerRunOptions,                  // 用户启动KubeAPIServer时传递的参数
		completedOptions.MasterCount,                       // Master节点数量
		serviceResolver,                                    // 服务解析器，用于把Service解析为一个合法的URL
		webhook.NewDefaultAuthenticationInfoResolverWrapper( // TODO
			kubeAPIServerConfig.ExtraConfig.ProxyTransport,         // 用于HTTPS传输
			kubeAPIServerConfig.GenericConfig.EgressSelector,       // TODO 和APIServer的Konnectivity特性相关
			kubeAPIServerConfig.GenericConfig.LoopbackClientConfig, // TODO 用于请求本地的APIServer
			kubeAPIServerConfig.GenericConfig.TracerProvider,       // 用于链路追踪
		),
	)
	if err != nil {
		return nil, err
	}

	// 1、当AggregatorServer, APIServer, ExtensionServer都无法解析请求时，只能把请求委托给NotFoundHandler，返回给用户404错误信息
	// 2、本质上就是一个http.Handler
	// 3、请求到了NotFoundHandler并非一定是Server没有这个路由，也有可能是Server正在启动当中，路由还没有完全注册，所以NotFoundHandler
	// 发现当前Server还没有启动完成的时候，会告诉用户等一会儿再尝试。当然，如果Server已经完全启动，那么请求到了NotFoundHandler一定是
	// Server没有这个路由导致的，也就是说这个请求是一个非法请求，请求需要的资源Server并没有，所以只能返回404错误
	notFoundHandler := notfoundhandler.New(kubeAPIServerConfig.GenericConfig.Serializer, genericapifilters.NoMuxAndDiscoveryIncompleteKey)

	// 创建ExtensionServer，并把自己无法处理的请求委派给NotFoundHandler
	apiExtensionsServer, err := createAPIExtensionsServer(
		apiExtensionsConfig, // ExtensionServer配置，其中包含了GenericServer配置，以及ExtensionServer额外的配置
		genericapiserver.NewEmptyDelegateWithCustomHandler(notFoundHandler),
	)
	if err != nil {
		return nil, err
	}

	// 实例化APIServer
	kubeAPIServer, err := CreateKubeAPIServer(
		kubeAPIServerConfig,                  // APIServer的配置
		apiExtensionsServer.GenericAPIServer, // TODO
	)
	if err != nil {
		return nil, err
	}

	// 初始化AggregatorServer的配置
	aggregatorConfig, err := createAggregatorConfig(
		*kubeAPIServerConfig.GenericConfig,
		completedOptions.ServerRunOptions,
		kubeAPIServerConfig.ExtraConfig.VersionedInformers,
		serviceResolver,
		kubeAPIServerConfig.ExtraConfig.ProxyTransport,
		pluginInitializer,
	)
	if err != nil {
		return nil, err
	}

	// 实例化AggregatorServer
	aggregatorServer, err := createAggregatorServer(
		aggregatorConfig,
		kubeAPIServer.GenericAPIServer,
		apiExtensionsServer.Informers,
	)
	if err != nil {
		// we don't need special handling for innerStopCh because the aggregator server doesn't create any go routines
		return nil, err
	}

	return aggregatorServer, nil
}

// CreateKubeAPIServer creates and wires a workable kube-apiserver
func CreateKubeAPIServer(
	kubeAPIServerConfig *controlplane.Config,
	delegateAPIServer genericapiserver.DelegationTarget,
) (*controlplane.Instance, error) {
	// Complete用于补全APIServer的参数，设置一些默认值
	// New实例化APIServer
	return kubeAPIServerConfig.Complete().New(delegateAPIServer)
}

// CreateProxyTransport creates the dialer infrastructure to connect to the nodes.
func CreateProxyTransport() *http.Transport {
	var proxyDialerFn utilnet.DialFunc
	// Proxying to pods and services is IP-based... don't expect to be able to verify the hostname
	proxyTLSClientConfig := &tls.Config{InsecureSkipVerify: true}
	proxyTransport := utilnet.SetTransportDefaults(&http.Transport{
		DialContext:     proxyDialerFn,
		TLSClientConfig: proxyTLSClientConfig,
	})
	return proxyTransport
}

//                          Request
//                             |
//                             |
//			+------------------↓--------------------+
//          |          AggregatorServer		        |
//          |      +-----------↓-------------+      |
//			|      |    GenericAPIServer     |      |
//          |      +-----------|-------------+      |
//          +------------------|--------------------+
//                             |
//                             |
//			+------------------↓--------------------+
//          |              APIServer		        |
//          |      +-----------↓-------------+      |
//			|      |    GenericAPIServer     |      |
//          |      +-----------|-------------+      |
//          +------------------|--------------------+
//                             |
//                             |
//			+------------------↓--------------------+
//          |           ExtensionServer             |
//          |      +-----------↓-------------+      |
//			|      |    GenericAPIServer     |      |
//          |      +-------------------------+      |
//          +---------------------------------------+
//                             |
//                             |
//			+------------------↓--------------------+
//          |           NotFoundHandler             |
//          +---------------------------------------+

// CreateKubeAPIServerConfig creates all the resources for running the API server, but runs none of them
// 1、初始化APIServer配置，APIServer配置是有GenericServer配置加上额外的参数构成的
// 2、
func CreateKubeAPIServerConfig(s completedServerRunOptions) (
	*controlplane.Config,
	aggregatorapiserver.ServiceResolver,
	[]admission.PluginInitializer,
	error,
) {
	// TODO 理解golang HTTP设计  ProxyTransport是什么时候使用的？
	// Transport用于APIServer去连接Node节点，这是一个HTTPS客户端配置
	proxyTransport := CreateProxyTransport()

	// 1、ServerRunOptions当中包含了APIServer运行所需要的全部参数，而GenericConfig则是用于给GenericServer使用的
	// 2、在KubeAPIServer当中，APIServer, ExtensionServer, AggregatedServer都是GenericServer
	// 3、versionedInformers：本质上就是SharedInformerFactory，用于缓存K8S各种资源
	// 4、serviceResolver：用于把Service转为一个可用的URL，其实就是用于把Service转为一个可用的URL
	// 5、pluginInitializers：这里所说的插件其实指的是准入插件，K8S中有许多内置的准入插件，用户也可以定制Webhook准入插件。这里的插件
	// 初始化器是为了向插件注入需要的依赖。 TODO 好好再分析下
	// 6、admissionPostStartHook：准入插件后置启动回调 TODO 什么时候被调用？生命周期？
	// 7、storageFactory：TODO APIServer的存储是如何设计的？
	genericConfig, versionedInformers, serviceResolver, pluginInitializers,
		admissionPostStartHook, storageFactory, err := buildGenericConfig(s.ServerRunOptions, proxyTransport)
	if err != nil {
		return nil, nil, nil, err
	}

	// TODO 通过Linux的Capability设置是否允许创建特权容器，以及每个连接每秒钟传输的最大字节数
	capabilities.Setup(s.AllowPrivileged, s.MaxConnectionBytesPerSec)

	// TODO 这里在干嘛
	s.Metrics.Apply()
	// 注册serviceAccount相关指标
	serviceaccount.RegisterMetrics()
	// APIServer配置，其中包含了GenericServer使用的通用配置以及APIServer需要的额外配置
	config := &controlplane.Config{
		// 1、GenericConfig配置定然是给GenericServer使用的，AggregatorServer, APIServer, ExtensionServer使用的配置肯定都是一样的
		// 2、如果说AggregatorServer, APIServer, ExtensionServer只有通用配置，没有自己业务相关的配置，那这三个Server也就没有什么差别了
		// 所以AggregatorServer, APIServer, ExtensionServer还有额外的配置，这些配置和这三个Server不同业务有关
		GenericConfig: genericConfig,
		// 3、APIServer特定的配置  TODO 似乎再K8S当中controlPlane代表着APIServer
		ExtraConfig: controlplane.ExtraConfig{
			APIResourceConfigSource: storageFactory.APIResourceConfigSource,
			StorageFactory:          storageFactory,
			EventTTL:                s.EventTTL,
			KubeletClientConfig:     s.KubeletConfig,
			EnableLogsSupport:       s.EnableLogsHandler,
			ProxyTransport:          proxyTransport,

			// ServiceIP主范围
			ServiceIPRange:     s.PrimaryServiceClusterIPRange,
			APIServerServiceIP: s.APIServerServiceIP,
			// ServiceIP从范围
			SecondaryServiceIPRange: s.SecondaryServiceClusterIPRange,

			// kubernetes.default.svc的端口
			APIServerServicePort: 443,

			// NodePort端口范围
			ServiceNodePortRange: s.ServiceNodePortRange,
			// kubernetes.default.svc的NodePort端口，默认kubernetes.default.svc是ClusterIP模式，依次这个端口是0
			KubernetesServiceNodePort: s.KubernetesServiceNodePort,

			// TODO 这玩意干嘛的
			EndpointReconcilerType: reconcilers.Type(s.EndpointReconcilerType),
			// Master的数量
			MasterCount: s.MasterCount,

			// TODO ServiceAccountToken认证机制
			ServiceAccountIssuer:        s.ServiceAccountIssuer,
			ServiceAccountMaxExpiration: s.ServiceAccountTokenMaxExpiration,
			ExtendExpiration:            s.Authentication.ServiceAccounts.ExtendExpiration,

			// SharedInformerFactory
			VersionedInformers: versionedInformers,
		},
	}

	// TODO 用于获取CA实际内容
	clientCAProvider, err := s.Authentication.ClientCert.GetClientCAContentProvider()
	if err != nil {
		return nil, nil, nil, err
	}
	config.ExtraConfig.ClusterAuthenticationInfo.ClientCA = clientCAProvider

	// 代理认证配置
	requestHeaderConfig, err := s.Authentication.RequestHeader.ToAuthenticationRequestHeaderConfig()
	if err != nil {
		return nil, nil, nil, err
	}
	if requestHeaderConfig != nil {
		config.ExtraConfig.ClusterAuthenticationInfo.RequestHeaderCA = requestHeaderConfig.CAContentProvider
		config.ExtraConfig.ClusterAuthenticationInfo.RequestHeaderAllowedNames = requestHeaderConfig.AllowedClientNames
		config.ExtraConfig.ClusterAuthenticationInfo.RequestHeaderExtraHeaderPrefixes = requestHeaderConfig.ExtraHeaderPrefixes
		config.ExtraConfig.ClusterAuthenticationInfo.RequestHeaderGroupHeaders = requestHeaderConfig.GroupHeaders
		config.ExtraConfig.ClusterAuthenticationInfo.RequestHeaderUsernameHeaders = requestHeaderConfig.UsernameHeaders
	}

	// 添加回调
	if err := config.GenericConfig.AddPostStartHook("start-kube-apiserver-admission-initializer", admissionPostStartHook); err != nil {
		return nil, nil, nil, err
	}

	// TODO 分析Konnectivity原理
	if config.GenericConfig.EgressSelector != nil {
		// Use the config.GenericConfig.EgressSelector lookup to find the dialer to connect to the kubelet
		config.ExtraConfig.KubeletClientConfig.Lookup = config.GenericConfig.EgressSelector.Lookup

		// Use the config.GenericConfig.EgressSelector lookup as the transport used by the "proxy" subresources.
		networkContext := egressselector.Cluster.AsNetworkContext()
		dialer, err := config.GenericConfig.EgressSelector.Lookup(networkContext)
		if err != nil {
			return nil, nil, nil, err
		}
		c := proxyTransport.Clone()
		c.DialContext = dialer
		config.ExtraConfig.ProxyTransport = c
	}

	// Load the public keys.
	// TODO 分析
	var pubKeys []interface{}
	for _, f := range s.Authentication.ServiceAccounts.KeyFiles {
		keys, err := keyutil.PublicKeysFromFile(f)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to parse key file %q: %v", f, err)
		}
		pubKeys = append(pubKeys, keys...)
	}
	// Plumb the required metadata through ExtraConfig.
	config.ExtraConfig.ServiceAccountIssuerURL = s.Authentication.ServiceAccounts.Issuers[0]
	config.ExtraConfig.ServiceAccountJWKSURI = s.Authentication.ServiceAccounts.JWKSURI
	config.ExtraConfig.ServiceAccountPublicKeys = pubKeys

	return config, serviceResolver, pluginInitializers, nil
}

// BuildGenericConfig takes the master server options and produces the genericapiserver.Config associated with it
func buildGenericConfig(
	s *options.ServerRunOptions, // 此参数为用户启动KubeAPIServer传入的参数
	proxyTransport *http.Transport, // 此参数用于完成HTTP协议的解析
) (
	genericConfig *genericapiserver.Config, // GenericServer配置
	versionedInformers clientgoinformers.SharedInformerFactory, // SharedInformerFactory，用于缓存K8S所有资源
	serviceResolver aggregatorapiserver.ServiceResolver, // 服务解析器，用于把Server解析为URL
	pluginInitializers []admission.PluginInitializer, // 插件初始化器  用于向webhook插件注入依赖
	admissionPostStartHook genericapiserver.PostStartHookFunc, // GenericServer的PostStartHook，应该是给准入控制插件使用的
	storageFactory *serverstorage.DefaultStorageFactory, // 存储工厂，用于构建K8S的后端存储
	lastErr error,
) {
	// 1、生成GenericServer配置，此配置当中包含HTTPS服务设置（监听地址、端口、证书）、认证器、鉴权器、回环网卡客户端配置、准入控制、
	// 流控（也就是限速）、审计、链路追踪配置
	// 2、这里生成的配置仅仅是初始化配置，仅仅会设置一些默认参数，用户配置的参数还没有赋值
	// 3、TODO 这里比较重要的就是其中配置了请求处理链
	genericConfig = genericapiserver.NewConfig(legacyscheme.Codecs)

	// 后续开始给初始化GenericServer配置

	// 1、MergedResourceConfig用于表示GV的启用/禁用，或者是GVR的启用/禁用
	// 2、通过MergedResourceConfig，我们可以知道某个资源是否被启用，还是被禁用
	// 3、这里在设置默认的启用或禁用的GVR，启用所有处于稳定版本的GRV，禁用所有处于Alpha/Beta版本的资源（但是启用流控相关的GVR）
	genericConfig.MergedResourceConfig = controlplane.DefaultAPIResourceConfigSource()

	// 利用GenericServerRunOptions参数初始化GenericServerConfig某些参数
	if lastErr = s.GenericServerRunOptions.ApplyTo(genericConfig); lastErr != nil {
		return
	}

	// 利用用户设置的SecureServing参数初始化GenericServerConfig配置的HTTPS服务启动参数、回环客户端参数
	if lastErr = s.SecureServing.ApplyTo(&genericConfig.SecureServing, &genericConfig.LoopbackClientConfig); lastErr != nil {
		return
	}
	// 给GenericServerConfig配置一些特性相关的参数
	if lastErr = s.Features.ApplyTo(genericConfig); lastErr != nil {
		return
	}
	// 设置启用/禁用的资源
	if lastErr = s.APIEnablement.ApplyTo(genericConfig, controlplane.DefaultAPIResourceConfigSource(), legacyscheme.Scheme); lastErr != nil {
		return
	}
	// TODO 和Konnectity相关
	if lastErr = s.EgressSelector.ApplyTo(genericConfig); lastErr != nil {
		return
	}

	// 如果启用的APIServer链路追踪的功能，就初始化链路追踪配置 TODO OTLP
	if utilfeature.DefaultFeatureGate.Enabled(genericfeatures.APIServerTracing) {
		if lastErr = s.Traces.ApplyTo(genericConfig.EgressSelector, genericConfig); lastErr != nil {
			return
		}
	}
	// wrap the definitions to revert any changes from disabled features
	// TODO Swagger是不是和这里相关
	getOpenAPIDefinitions := openapi.GetOpenAPIDefinitionsWithoutDisabledFeatures(generatedopenapi.GetOpenAPIDefinitions)
	genericConfig.OpenAPIConfig = genericapiserver.DefaultOpenAPIConfig(getOpenAPIDefinitions, openapinamer.NewDefinitionNamer(legacyscheme.Scheme, extensionsapiserver.Scheme, aggregatorscheme.Scheme))
	genericConfig.OpenAPIConfig.Info.Title = "Kubernetes"
	genericConfig.OpenAPIV3Config = genericapiserver.DefaultOpenAPIV3Config(getOpenAPIDefinitions, openapinamer.NewDefinitionNamer(legacyscheme.Scheme, extensionsapiserver.Scheme, aggregatorscheme.Scheme))
	genericConfig.OpenAPIV3Config.Info.Title = "Kubernetes"

	// TODO 这里的长时间运行的请求为什么要单独处理，K8S又是怎样处理的？
	genericConfig.LongRunningFunc = filters.BasicLongRunningRequestCheck(
		sets.NewString("watch", "proxy"),
		sets.NewString("attach", "exec", "proxy", "log", "portforward"),
	)

	kubeVersion := version.Get()
	genericConfig.Version = &kubeVersion

	// TODO Konnectity为什么和ETCD还有关系
	if genericConfig.EgressSelector != nil {
		s.Etcd.StorageConfig.Transport.EgressLookup = genericConfig.EgressSelector.Lookup
	}

	// 启用了链路追踪，还需要跟踪ETCD处理请求
	if utilfeature.DefaultFeatureGate.Enabled(genericfeatures.APIServerTracing) {
		s.Etcd.StorageConfig.Transport.TracerProvider = genericConfig.TracerProvider
	} else {
		s.Etcd.StorageConfig.Transport.TracerProvider = oteltrace.NewNoopTracerProvider()
	}

	// ETCD的初始化  主要是像GenericServer中添加了一个名为start-encryption-provider-config-automatic-reload的PostStartHook
	if lastErr = s.Etcd.Complete(genericConfig.StorageObjectCountTracker, genericConfig.DrainedNotify(), genericConfig.AddPostStartHook); lastErr != nil {
		return
	}

	// TODO APIServer的存储系统这一节必须得好好分析分析
	storageFactoryConfig := kubeapiserver.NewStorageFactoryConfig()
	storageFactoryConfig.APIResourceConfig = genericConfig.MergedResourceConfig
	// 实例化存储工厂，通过存储工厂，我们可以知道如何编解码每个资源
	storageFactory, lastErr = storageFactoryConfig.Complete(s.Etcd).New()
	if lastErr != nil {
		return
	}
	// 最最重要的是实例化了GenericServer的RESTOptionsGetter
	if lastErr = s.Etcd.ApplyWithStorageFactoryTo(storageFactory, genericConfig); lastErr != nil {
		return
	}

	// Use protobufs for self-communication.
	// Since not every generic apiserver has to support protobufs, we
	// cannot default to it in generic apiserver and need to explicitly
	// set it in kube-apiserver.
	// TODO content-type为什么是这个？有啥用？
	genericConfig.LoopbackClientConfig.ContentConfig.ContentType = "application/vnd.kubernetes.protobuf"
	// Disable compression for self-communication, since we are going to be
	// on a fast local network
	genericConfig.LoopbackClientConfig.DisableCompression = true

	kubeClientConfig := genericConfig.LoopbackClientConfig
	// TODO 为什么叫做ExternalClient? 这是为了和LoopbackClient做区分么？
	clientgoExternalClient, err := clientgoclientset.NewForConfig(kubeClientConfig)
	if err != nil {
		lastErr = fmt.Errorf("failed to create real external clientset: %v", err)
		return
	}
	// TODO 分析ShardInformer
	versionedInformers = clientgoinformers.NewSharedInformerFactory(clientgoExternalClient, 10*time.Minute)

	// Authentication.ApplyTo requires already applied OpenAPIConfig and EgressSelector if present
	// 1、这里主要还是在初始化认证参数，仅仅实例化了BootstrapToken认证器，其余的认证器并没有实例化
	// 2、所有的认证器初始化完成之后，会实例化一个UnionAuthenticator，用于包装所有的认证器。
	// TODO 那么其它认证模式的认证器在什么地方初始化的？
	if lastErr = s.Authentication.ApplyTo(&genericConfig.Authentication, genericConfig.SecureServing, genericConfig.EgressSelector,
		genericConfig.OpenAPIConfig, genericConfig.OpenAPIV3Config, clientgoExternalClient, versionedInformers); lastErr != nil {
		return
	}

	// 实例化各个模式的鉴权器，除此之外每个模式还实例化了一个RuleResolver，并且还实例化了一个特权鉴权器
	genericConfig.Authorization.Authorizer, genericConfig.RuleResolver, err = BuildAuthorizer(s, genericConfig.EgressSelector, versionedInformers)
	if err != nil {
		lastErr = fmt.Errorf("invalid authorization config: %v", err)
		return
	}
	// 如果没有启用RBAC模式，那么需要禁用名为rbac/bootstrap-roles的PostStartHook
	if !sets.NewString(s.Authorization.Modes...).Has(modes.ModeRBAC) {
		genericConfig.DisabledPostStartHooks.Insert(rbacrest.PostStartHookName)
	}

	// TODO 审计配置
	lastErr = s.Audit.ApplyTo(genericConfig)
	if lastErr != nil {
		return
	}

	admissionConfig := &kubeapiserveradmission.Config{
		ExternalInformers:    versionedInformers,
		LoopbackClientConfig: genericConfig.LoopbackClientConfig,
		CloudConfigFile:      s.CloudProvider.CloudConfigFile,
	}
	// 所谓的服务解析器实际上就是把一个Service
	serviceResolver = buildServiceResolver(s.EnableAggregatorRouting, genericConfig.LoopbackClientConfig.Host, versionedInformers)
	// TODO 这玩意是干嘛的
	schemaResolver := resolver.NewDefinitionsSchemaResolver(k8sscheme.Scheme, genericConfig.OpenAPIConfig.GetDefinitions)
	pluginInitializers, admissionPostStartHook, err = admissionConfig.New(proxyTransport, genericConfig.EgressSelector, serviceResolver, genericConfig.TracerProvider, schemaResolver)
	if err != nil {
		lastErr = fmt.Errorf("failed to create admission plugin initializer: %v", err)
		return
	}

	// TODO 准入控制插件的初始化
	err = s.Admission.ApplyTo(
		genericConfig,
		versionedInformers,
		kubeClientConfig,
		utilfeature.DefaultFeatureGate,
		pluginInitializers...)
	if err != nil {
		lastErr = fmt.Errorf("failed to initialize admission: %v", err)
		return
	}

	// TODO 留空初始化
	if utilfeature.DefaultFeatureGate.Enabled(genericfeatures.APIPriorityAndFairness) && s.GenericServerRunOptions.EnablePriorityAndFairness {
		genericConfig.FlowControl, lastErr = BuildPriorityAndFairness(s, clientgoExternalClient, versionedInformers)
	}
	// TODO 资源管理器有啥用？
	if utilfeature.DefaultFeatureGate.Enabled(genericfeatures.AggregatedDiscoveryEndpoint) {
		genericConfig.AggregatedDiscoveryGroupManager = aggregated.NewResourceManager("apis")
	}

	return
}

// BuildAuthorizer constructs the authorizer
// 实例化各个模式的鉴权器，除此之外每个模式还实例化了一个RuleResolver，并且还实例化了一个特权鉴权器
func BuildAuthorizer(s *options.ServerRunOptions, EgressSelector *egressselector.EgressSelector, versionedInformers clientgoinformers.SharedInformerFactory) (authorizer.Authorizer, authorizer.RuleResolver, error) {
	// 生成认证配置
	authorizationConfig := s.Authorization.ToAuthorizationConfig(versionedInformers)

	if EgressSelector != nil {
		egressDialer, err := EgressSelector.Lookup(egressselector.ControlPlane.AsNetworkContext())
		if err != nil {
			return nil, nil, err
		}
		authorizationConfig.CustomDial = egressDialer
	}

	// 实例化各个模式的鉴权器，除此之外每个模式还实例化了一个RuleResolver，并且还实例化了一个特权鉴权器
	return authorizationConfig.New()
}

// BuildPriorityAndFairness constructs the guts of the API Priority and Fairness filter
func BuildPriorityAndFairness(s *options.ServerRunOptions, extclient clientgoclientset.Interface, versionedInformer clientgoinformers.SharedInformerFactory) (utilflowcontrol.Interface, error) {
	if s.GenericServerRunOptions.MaxRequestsInFlight+s.GenericServerRunOptions.MaxMutatingRequestsInFlight <= 0 {
		return nil, fmt.Errorf("invalid configuration: MaxRequestsInFlight=%d and MaxMutatingRequestsInFlight=%d; they must add up to something positive", s.GenericServerRunOptions.MaxRequestsInFlight, s.GenericServerRunOptions.MaxMutatingRequestsInFlight)
	}
	return utilflowcontrol.New(
		versionedInformer,
		extclient.FlowcontrolV1beta3(),
		s.GenericServerRunOptions.MaxRequestsInFlight+s.GenericServerRunOptions.MaxMutatingRequestsInFlight,
		s.GenericServerRunOptions.RequestTimeout/4,
	), nil
}

// completedServerRunOptions is a private wrapper that enforces a call of Complete() before Run can be invoked.
type completedServerRunOptions struct {
	*options.ServerRunOptions
}

// Complete set default ServerRunOptions.
// Should be called after kube-apiserver flags parsed.
// 1、设置AdvertiseAddress
// 2、根据用户设置的--service-cluster-ip-range参数计算出来kubernetes.default.svc的IP地址以及主从SVC地址范围
// 3、校验用户启用的认证模式，如果启用了AlwaysAllow的认证器，并且允许匿名用户访问，则允许匿名用户访问这个设置是多余的，会被设置为false
// 4、检查ServiceAccountSigningKeyFile配置以及Etcd.EnableWatchCache配置
func Complete(s *options.ServerRunOptions) (completedServerRunOptions, error) {
	var options completedServerRunOptions
	// set defaults
	// TODO 根据安全服务设置AdvertiseAddress参数设置通用服务运行参数的AdvertiseAddress地址
	if err := s.GenericServerRunOptions.DefaultAdvertiseAddress(s.SecureServing.SecureServingOptions); err != nil {
		return options, err
	}

	// process s.ServiceClusterIPRange from list to Primary and Secondary
	// we process secondary only if provided by user
	// 1、通过用户设置的Service IP地址范围解析出apiServerServiceIP, primaryServiceIPRange, secondaryServiceIPRange
	// 2、K8S是可以支持双栈的，也就是说我们可以同时启用IPv4协议栈和IPv6协议栈，只需要通过--service-cluster-ip-range参数同时配置
	// 两个协议栈的Service地址范围即可
	// 3、K8S会分配SVC地址范围的第一个地址给名字为kubernetes.default.svc
	apiServerServiceIP, primaryServiceIPRange, secondaryServiceIPRange, err := getServiceIPAndRanges(s.ServiceClusterIPRanges)
	if err != nil {
		return options, err
	}
	s.PrimaryServiceClusterIPRange = primaryServiceIPRange
	s.SecondaryServiceClusterIPRange = secondaryServiceIPRange
	s.APIServerServiceIP = apiServerServiceIP

	if err := s.SecureServing.MaybeDefaultWithSelfSignedCerts(s.GenericServerRunOptions.AdvertiseAddress.String(), []string{"kubernetes.default.svc", "kubernetes.default", "kubernetes"}, []net.IP{apiServerServiceIP}); err != nil {
		return options, fmt.Errorf("error creating self-signed certificates: %v", err)
	}

	if len(s.GenericServerRunOptions.ExternalHost) == 0 {
		if len(s.GenericServerRunOptions.AdvertiseAddress) > 0 {
			s.GenericServerRunOptions.ExternalHost = s.GenericServerRunOptions.AdvertiseAddress.String()
		} else {
			hostname, err := os.Hostname()
			if err != nil {
				return options, fmt.Errorf("error finding host name: %v", err)
			}
			s.GenericServerRunOptions.ExternalHost = hostname
		}
		klog.Infof("external host was not specified, using %v", s.GenericServerRunOptions.ExternalHost)
	}

	// 校验用户启用的认证模式，如果启用了AlwaysAllow的认证器，并且允许匿名用户访问，则允许匿名用户访问这个设置是多余的，会被设置为false
	s.Authentication.ApplyAuthorization(s.Authorization)

	// Use (ServiceAccountSigningKeyFile != "") as a proxy to the user enabling
	// TokenRequest functionality. This defaulting was convenient, but messed up
	// a lot of people when they rotated their serving cert with no idea it was
	// connected to their service account keys. We are taking this opportunity to
	// remove this problematic defaulting.
	if s.ServiceAccountSigningKeyFile == "" {
		// Default to the private server key for service account token signing
		if len(s.Authentication.ServiceAccounts.KeyFiles) == 0 && s.SecureServing.ServerCert.CertKey.KeyFile != "" {
			if kubeauthenticator.IsValidServiceAccountKeyFile(s.SecureServing.ServerCert.CertKey.KeyFile) {
				s.Authentication.ServiceAccounts.KeyFiles = []string{s.SecureServing.ServerCert.CertKey.KeyFile}
			} else {
				klog.Warning("No TLS key provided, service account token authentication disabled")
			}
		}
	}

	if s.ServiceAccountSigningKeyFile != "" && len(s.Authentication.ServiceAccounts.Issuers) != 0 && s.Authentication.ServiceAccounts.Issuers[0] != "" {
		sk, err := keyutil.PrivateKeyFromFile(s.ServiceAccountSigningKeyFile)
		if err != nil {
			return options, fmt.Errorf("failed to parse service-account-issuer-key-file: %v", err)
		}
		if s.Authentication.ServiceAccounts.MaxExpiration != 0 {
			lowBound := time.Hour
			upBound := time.Duration(1<<32) * time.Second
			if s.Authentication.ServiceAccounts.MaxExpiration < lowBound ||
				s.Authentication.ServiceAccounts.MaxExpiration > upBound {
				return options, fmt.Errorf("the service-account-max-token-expiration must be between 1 hour and 2^32 seconds")
			}
			if s.Authentication.ServiceAccounts.ExtendExpiration {
				if s.Authentication.ServiceAccounts.MaxExpiration < serviceaccount.WarnOnlyBoundTokenExpirationSeconds*time.Second {
					klog.Warningf("service-account-extend-token-expiration is true, in order to correctly trigger safe transition logic, service-account-max-token-expiration must be set longer than %d seconds (currently %s)", serviceaccount.WarnOnlyBoundTokenExpirationSeconds, s.Authentication.ServiceAccounts.MaxExpiration)
				}
				if s.Authentication.ServiceAccounts.MaxExpiration < serviceaccount.ExpirationExtensionSeconds*time.Second {
					klog.Warningf("service-account-extend-token-expiration is true, enabling tokens valid up to %d seconds, which is longer than service-account-max-token-expiration set to %s seconds", serviceaccount.ExpirationExtensionSeconds, s.Authentication.ServiceAccounts.MaxExpiration)
				}
			}
		}

		s.ServiceAccountIssuer, err = serviceaccount.JWTTokenGenerator(s.Authentication.ServiceAccounts.Issuers[0], sk)
		if err != nil {
			return options, fmt.Errorf("failed to build token generator: %v", err)
		}
		s.ServiceAccountTokenMaxExpiration = s.Authentication.ServiceAccounts.MaxExpiration
	}

	if s.Etcd.EnableWatchCache {
		sizes := kubeapiserver.DefaultWatchCacheSizes()
		// Ensure that overrides parse correctly.
		userSpecified, err := serveroptions.ParseWatchCacheSizes(s.Etcd.WatchCacheSizes)
		if err != nil {
			return options, err
		}
		for resource, size := range userSpecified {
			sizes[resource] = size
		}
		s.Etcd.WatchCacheSizes, err = serveroptions.WriteWatchCacheSizes(sizes)
		if err != nil {
			return options, err
		}
	}

	for key, value := range s.APIEnablement.RuntimeConfig {
		if key == "v1" || strings.HasPrefix(key, "v1/") ||
			key == "api/v1" || strings.HasPrefix(key, "api/v1/") {
			delete(s.APIEnablement.RuntimeConfig, key)
			s.APIEnablement.RuntimeConfig["/v1"] = value
		}
		if key == "api/legacy" {
			delete(s.APIEnablement.RuntimeConfig, key)
		}
	}

	options.ServerRunOptions = s
	return options, nil
}

var testServiceResolver webhook.ServiceResolver

// SetServiceResolverForTests allows the service resolver to be overridden during tests.
// Tests using this function must run serially as this function is not safe to call concurrently with server start.
func SetServiceResolverForTests(resolver webhook.ServiceResolver) func() {
	if testServiceResolver != nil {
		panic("test service resolver is set: tests are either running concurrently or clean up was skipped")
	}

	testServiceResolver = resolver

	return func() {
		testServiceResolver = nil
	}
}

func buildServiceResolver(enabledAggregatorRouting bool, hostname string, informer clientgoinformers.SharedInformerFactory) webhook.ServiceResolver {
	if testServiceResolver != nil {
		return testServiceResolver
	}

	var serviceResolver webhook.ServiceResolver
	if enabledAggregatorRouting {
		serviceResolver = aggregatorapiserver.NewEndpointServiceResolver(
			informer.Core().V1().Services().Lister(),
			informer.Core().V1().Endpoints().Lister(),
		)
	} else {
		serviceResolver = aggregatorapiserver.NewClusterIPServiceResolver(
			informer.Core().V1().Services().Lister(),
		)
	}
	// resolve kubernetes.default.svc locally
	if localHost, err := url.Parse(hostname); err == nil {
		serviceResolver = aggregatorapiserver.NewLoopbackServiceResolver(serviceResolver, localHost)
	}
	return serviceResolver
}

func getServiceIPAndRanges(serviceClusterIPRanges string) (net.IP, net.IPNet, net.IPNet, error) {
	serviceClusterIPRangeList := []string{}
	if serviceClusterIPRanges != "" {
		serviceClusterIPRangeList = strings.Split(serviceClusterIPRanges, ",")
	}

	var apiServerServiceIP net.IP
	var primaryServiceIPRange net.IPNet
	var secondaryServiceIPRange net.IPNet
	var err error
	// nothing provided by user, use default range (only applies to the Primary)
	if len(serviceClusterIPRangeList) == 0 {
		var primaryServiceClusterCIDR net.IPNet
		primaryServiceIPRange, apiServerServiceIP, err = controlplane.ServiceIPRange(primaryServiceClusterCIDR)
		if err != nil {
			return net.IP{}, net.IPNet{}, net.IPNet{}, fmt.Errorf("error determining service IP ranges: %v", err)
		}
		return apiServerServiceIP, primaryServiceIPRange, net.IPNet{}, nil
	}

	_, primaryServiceClusterCIDR, err := netutils.ParseCIDRSloppy(serviceClusterIPRangeList[0])
	if err != nil {
		return net.IP{}, net.IPNet{}, net.IPNet{}, fmt.Errorf("service-cluster-ip-range[0] is not a valid cidr")
	}

	primaryServiceIPRange, apiServerServiceIP, err = controlplane.ServiceIPRange(*primaryServiceClusterCIDR)
	if err != nil {
		return net.IP{}, net.IPNet{}, net.IPNet{}, fmt.Errorf("error determining service IP ranges for primary service cidr: %v", err)
	}

	// user provided at least two entries
	// note: validation asserts that the list is max of two dual stack entries
	if len(serviceClusterIPRangeList) > 1 {
		_, secondaryServiceClusterCIDR, err := netutils.ParseCIDRSloppy(serviceClusterIPRangeList[1])
		if err != nil {
			return net.IP{}, net.IPNet{}, net.IPNet{}, fmt.Errorf("service-cluster-ip-range[1] is not an ip net")
		}
		secondaryServiceIPRange = *secondaryServiceClusterCIDR
	}
	return apiServerServiceIP, primaryServiceIPRange, secondaryServiceIPRange, nil
}
