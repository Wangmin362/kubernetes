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

package controlplane

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	admissionregistrationv1alpha1 "k8s.io/api/admissionregistration/v1alpha1"
	apiserverinternalv1alpha1 "k8s.io/api/apiserverinternal/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authenticationv1alpha1 "k8s.io/api/authentication/v1alpha1"
	authenticationv1beta1 "k8s.io/api/authentication/v1beta1"
	authorizationapiv1 "k8s.io/api/authorization/v1"
	autoscalingapiv1 "k8s.io/api/autoscaling/v1"
	autoscalingapiv2 "k8s.io/api/autoscaling/v2"
	autoscalingapiv2beta1 "k8s.io/api/autoscaling/v2beta1"
	autoscalingapiv2beta2 "k8s.io/api/autoscaling/v2beta2"
	batchapiv1 "k8s.io/api/batch/v1"
	batchapiv1beta1 "k8s.io/api/batch/v1beta1"
	certificatesapiv1 "k8s.io/api/certificates/v1"
	certificatesv1alpha1 "k8s.io/api/certificates/v1alpha1"
	coordinationapiv1 "k8s.io/api/coordination/v1"
	apiv1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	discoveryv1beta1 "k8s.io/api/discovery/v1beta1"
	eventsv1 "k8s.io/api/events/v1"
	eventsv1beta1 "k8s.io/api/events/v1beta1"
	flowcontrolv1alpha1 "k8s.io/api/flowcontrol/v1alpha1"
	networkingapiv1 "k8s.io/api/networking/v1"
	networkingapiv1alpha1 "k8s.io/api/networking/v1alpha1"
	nodev1 "k8s.io/api/node/v1"
	nodev1beta1 "k8s.io/api/node/v1beta1"
	policyapiv1 "k8s.io/api/policy/v1"
	policyapiv1beta1 "k8s.io/api/policy/v1beta1"
	rbacv1 "k8s.io/api/rbac/v1"
	resourcev1alpha2 "k8s.io/api/resource/v1alpha2"
	schedulingapiv1 "k8s.io/api/scheduling/v1"
	storageapiv1 "k8s.io/api/storage/v1"
	storageapiv1alpha1 "k8s.io/api/storage/v1alpha1"
	storageapiv1beta1 "k8s.io/api/storage/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/endpoints/discovery"
	apiserverfeatures "k8s.io/apiserver/pkg/features"
	"k8s.io/apiserver/pkg/registry/generic"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	serverstorage "k8s.io/apiserver/pkg/server/storage"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	discoveryclient "k8s.io/client-go/kubernetes/typed/discovery/v1"
	"k8s.io/component-helpers/apimachinery/lease"
	"k8s.io/klog/v2"
	api "k8s.io/kubernetes/pkg/apis/core"
	flowcontrolv1beta1 "k8s.io/kubernetes/pkg/apis/flowcontrol/v1beta1"
	flowcontrolv1beta2 "k8s.io/kubernetes/pkg/apis/flowcontrol/v1beta2"
	flowcontrolv1beta3 "k8s.io/kubernetes/pkg/apis/flowcontrol/v1beta3"
	"k8s.io/kubernetes/pkg/controlplane/controller/apiserverleasegc"
	"k8s.io/kubernetes/pkg/controlplane/controller/clusterauthenticationtrust"
	"k8s.io/kubernetes/pkg/controlplane/controller/legacytokentracking"
	"k8s.io/kubernetes/pkg/controlplane/controller/systemnamespaces"
	"k8s.io/kubernetes/pkg/controlplane/reconcilers"
	kubeoptions "k8s.io/kubernetes/pkg/kubeapiserver/options"
	kubeletclient "k8s.io/kubernetes/pkg/kubelet/client"
	"k8s.io/kubernetes/pkg/routes"
	"k8s.io/kubernetes/pkg/serviceaccount"
	"k8s.io/utils/clock"

	// RESTStorage installers
	admissionregistrationrest "k8s.io/kubernetes/pkg/registry/admissionregistration/rest"
	apiserverinternalrest "k8s.io/kubernetes/pkg/registry/apiserverinternal/rest"
	appsrest "k8s.io/kubernetes/pkg/registry/apps/rest"
	authenticationrest "k8s.io/kubernetes/pkg/registry/authentication/rest"
	authorizationrest "k8s.io/kubernetes/pkg/registry/authorization/rest"
	autoscalingrest "k8s.io/kubernetes/pkg/registry/autoscaling/rest"
	batchrest "k8s.io/kubernetes/pkg/registry/batch/rest"
	certificatesrest "k8s.io/kubernetes/pkg/registry/certificates/rest"
	coordinationrest "k8s.io/kubernetes/pkg/registry/coordination/rest"
	corerest "k8s.io/kubernetes/pkg/registry/core/rest"
	discoveryrest "k8s.io/kubernetes/pkg/registry/discovery/rest"
	eventsrest "k8s.io/kubernetes/pkg/registry/events/rest"
	flowcontrolrest "k8s.io/kubernetes/pkg/registry/flowcontrol/rest"
	networkingrest "k8s.io/kubernetes/pkg/registry/networking/rest"
	noderest "k8s.io/kubernetes/pkg/registry/node/rest"
	policyrest "k8s.io/kubernetes/pkg/registry/policy/rest"
	rbacrest "k8s.io/kubernetes/pkg/registry/rbac/rest"
	resourcerest "k8s.io/kubernetes/pkg/registry/resource/rest"
	schedulingrest "k8s.io/kubernetes/pkg/registry/scheduling/rest"
	storagerest "k8s.io/kubernetes/pkg/registry/storage/rest"
)

const (
	// DefaultEndpointReconcilerInterval is the default amount of time for how often the endpoints for
	// the kubernetes Service are reconciled.
	DefaultEndpointReconcilerInterval = 10 * time.Second
	// DefaultEndpointReconcilerTTL is the default TTL timeout for the storage layer
	DefaultEndpointReconcilerTTL = 15 * time.Second
	// IdentityLeaseComponentLabelKey is used to apply a component label to identity lease objects, indicating:
	//   1. the lease is an identity lease (different from leader election leases)
	//   2. which component owns this lease
	IdentityLeaseComponentLabelKey = "apiserver.kubernetes.io/identity"
	// KubeAPIServer defines variable used internally when referring to kube-apiserver component
	KubeAPIServer = "kube-apiserver"
	// DeprecatedKubeAPIServerIdentityLeaseLabelSelector selects kube-apiserver identity leases
	DeprecatedKubeAPIServerIdentityLeaseLabelSelector = "k8s.io/component=kube-apiserver"
	// KubeAPIServerIdentityLeaseLabelSelector selects kube-apiserver identity leases
	// apiserver.kubernetes.io/identity=kube-apiserver
	KubeAPIServerIdentityLeaseLabelSelector = IdentityLeaseComponentLabelKey + "=" + KubeAPIServer
	// repairLoopInterval defines the interval used to run the Services ClusterIP and NodePort repair loops
	repairLoopInterval = 3 * time.Minute
)

var (
	// IdentityLeaseGCPeriod is the interval which the lease GC controller checks for expired leases
	// IdentityLeaseGCPeriod is exposed so integration tests can tune this value.
	IdentityLeaseGCPeriod = 3600 * time.Second
	// IdentityLeaseDurationSeconds is the duration of kube-apiserver lease in seconds
	// IdentityLeaseDurationSeconds is exposed so integration tests can tune this value.
	IdentityLeaseDurationSeconds = 3600
	// IdentityLeaseRenewIntervalSeconds is the interval of kube-apiserver renewing its lease in seconds
	// IdentityLeaseRenewIntervalSeconds is exposed so integration tests can tune this value.
	IdentityLeaseRenewIntervalPeriod = 10 * time.Second
)

// ExtraConfig defines extra configuration for the master
type ExtraConfig struct {
	// APIServer集群认证信息，主要是包含证书认证以及代理认证
	ClusterAuthenticationInfo clusterauthenticationtrust.ClusterAuthenticationInfo

	// 用于判断某个GVR，或者是某个组是否被启用
	APIResourceConfigSource serverstorage.APIResourceConfigSource
	// 存储工厂，用于获取每个资源的存储配有，尤其是编解码器
	StorageFactory serverstorage.StorageFactory
	// TODO 这玩意干嘛的？
	EndpointReconcilerConfig EndpointReconcilerConfig
	EventTTL                 time.Duration
	KubeletClientConfig      kubeletclient.KubeletClientConfig

	EnableLogsSupport bool
	ProxyTransport    *http.Transport

	// Values to build the IP addresses used by discovery
	// The range of IPs to be assigned to services with type=ClusterIP or greater
	// K8S Service ClusterIP的地址范围
	ServiceIPRange net.IPNet
	// The IP address for the GenericAPIServer service (must be inside ServiceIPRange)
	// TODO 这玩意应该就是kubernetes.default.svc的地址范围
	APIServerServiceIP net.IP

	// dual stack services, the range represents an alternative IP range for service IP
	// must be of different family than primary (ServiceIPRange)
	SecondaryServiceIPRange net.IPNet
	// the secondary IP address the GenericAPIServer service (must be inside SecondaryServiceIPRange)
	SecondaryAPIServerServiceIP net.IP

	// Port for the apiserver service.
	// TODO 应该指的是kubernetes.default.svc的端口
	APIServerServicePort int

	// TODO, we can probably group service related items into a substruct to make it easier to configure
	// the API server items and `Extra*` fields likely fit nicely together.

	// The range of ports to be assigned to services with type=NodePort or greater
	ServiceNodePortRange utilnet.PortRange
	// If non-zero, the "kubernetes" services uses this port as NodePort.
	// 如果非空，那么kubernetes.default.svc将会使用NodePort类型
	KubernetesServiceNodePort int

	// Number of masters running; all masters must be started with the
	// same value for this field. (Numbers > 1 currently untested.)
	// APIServer节点数量
	MasterCount int

	// MasterEndpointReconcileTTL sets the time to live in seconds of an
	// endpoint record recorded by each master. The endpoints are checked at an
	// interval that is 2/3 of this value and this value defaults to 15s if
	// unset. In very large clusters, this value may be increased to reduce the
	// possibility that the master endpoint record expires (due to other load
	// on the etcd server) and causes masters to drop in and out of the
	// kubernetes service record. It is not recommended to set this value below
	// 15s.
	MasterEndpointReconcileTTL time.Duration

	// Selects which reconciler to use
	EndpointReconcilerType reconcilers.Type

	ServiceAccountIssuer        serviceaccount.TokenGenerator
	ServiceAccountMaxExpiration time.Duration
	ExtendExpiration            bool

	// ServiceAccountIssuerDiscovery
	ServiceAccountIssuerURL  string
	ServiceAccountJWKSURI    string
	ServiceAccountPublicKeys []interface{}

	// SharedInformer
	VersionedInformers informers.SharedInformerFactory

	// RepairServicesInterval interval used by the repair loops for
	// the Services NodePort and ClusterIP resources
	// TODO 这玩意估计用来控制多长时间查看一次kubernetes.default.svc，如果有毛病就修复
	RepairServicesInterval time.Duration
}

// Config defines configuration for the master
type Config struct {
	GenericConfig *genericapiserver.Config //
	ExtraConfig   ExtraConfig
}

type completedConfig struct {
	GenericConfig genericapiserver.CompletedConfig
	ExtraConfig   *ExtraConfig
}

// CompletedConfig embeds a private pointer that cannot be instantiated outside of this package
type CompletedConfig struct {
	*completedConfig
}

// EndpointReconcilerConfig holds the endpoint reconciler and endpoint reconciliation interval to be
// used by the master.
// TODO 这玩意干嘛的？
type EndpointReconcilerConfig struct {
	Reconciler reconcilers.EndpointReconciler
	Interval   time.Duration
}

// Instance contains state for a Kubernetes cluster api server instance.
type Instance struct {
	GenericAPIServer *genericapiserver.GenericAPIServer

	ClusterAuthenticationInfo clusterauthenticationtrust.ClusterAuthenticationInfo
}

func (c *Config) createMasterCountReconciler() reconcilers.EndpointReconciler {
	endpointClient := corev1client.NewForConfigOrDie(c.GenericConfig.LoopbackClientConfig)
	endpointSliceClient := discoveryclient.NewForConfigOrDie(c.GenericConfig.LoopbackClientConfig)
	endpointsAdapter := reconcilers.NewEndpointsAdapter(endpointClient, endpointSliceClient)

	return reconcilers.NewMasterCountEndpointReconciler(c.ExtraConfig.MasterCount, endpointsAdapter)
}

func (c *Config) createNoneReconciler() reconcilers.EndpointReconciler {
	return reconcilers.NewNoneEndpointReconciler()
}

func (c *Config) createLeaseReconciler() reconcilers.EndpointReconciler {
	endpointClient := corev1client.NewForConfigOrDie(c.GenericConfig.LoopbackClientConfig)
	endpointSliceClient := discoveryclient.NewForConfigOrDie(c.GenericConfig.LoopbackClientConfig)
	endpointsAdapter := reconcilers.NewEndpointsAdapter(endpointClient, endpointSliceClient)

	ttl := c.ExtraConfig.MasterEndpointReconcileTTL
	config, err := c.ExtraConfig.StorageFactory.NewConfig(api.Resource("apiServerIPInfo"))
	if err != nil {
		klog.Fatalf("Error creating storage factory config: %v", err)
	}
	masterLeases, err := reconcilers.NewLeases(config, "/masterleases/", ttl)
	if err != nil {
		klog.Fatalf("Error creating leases: %v", err)
	}

	return reconcilers.NewLeaseEndpointReconciler(endpointsAdapter, masterLeases)
}

func (c *Config) createEndpointReconciler() reconcilers.EndpointReconciler {
	klog.Infof("Using reconciler: %v", c.ExtraConfig.EndpointReconcilerType)
	switch c.ExtraConfig.EndpointReconcilerType {
	// there are numerous test dependencies that depend on a default controller
	case reconcilers.MasterCountReconcilerType:
		return c.createMasterCountReconciler()
	case "", reconcilers.LeaseEndpointReconcilerType:
		return c.createLeaseReconciler()
	case reconcilers.NoneEndpointReconcilerType:
		return c.createNoneReconciler()
	default:
		klog.Fatalf("Reconciler not implemented: %v", c.ExtraConfig.EndpointReconcilerType)
	}
	return nil
}

// Complete fills in any fields not set that are required to have valid data. It's mutating the receiver.
func (c *Config) Complete() CompletedConfig {
	cfg := completedConfig{
		c.GenericConfig.Complete(c.ExtraConfig.VersionedInformers), // 补全GenericServer配置
		&c.ExtraConfig,
	}

	// 解析Service的IP范围，以及kubernetes.default.svc的地址
	serviceIPRange, apiServerServiceIP, err := ServiceIPRange(cfg.ExtraConfig.ServiceIPRange)
	if err != nil {
		klog.Fatalf("Error determining service IP ranges: %v", err)
	}
	if cfg.ExtraConfig.ServiceIPRange.IP == nil {
		cfg.ExtraConfig.ServiceIPRange = serviceIPRange
	}
	if cfg.ExtraConfig.APIServerServiceIP == nil {
		cfg.ExtraConfig.APIServerServiceIP = apiServerServiceIP
	}

	// 1、用于返回给客户端合适的ServerAddress TODO 还需要分析的再详细一些
	// 2、可以通过kubectl get --raw=/api获取此数据
	/*
		root@k8s-master1:~# kubectl get --raw=/api | jq
		{
		  "kind": "APIVersions",
		  "versions": [
		    "v1"
		  ],
		  "serverAddressByClientCIDRs": [
		    {
		      "clientCIDR": "0.0.0.0/0",
		      "serverAddress": "192.168.11.71:6443"
		    }
		  ]
		}
		root@k8s-master1:~#
	*/
	discoveryAddresses := discovery.DefaultAddresses{DefaultAddress: cfg.GenericConfig.ExternalAddress}
	discoveryAddresses.CIDRRules = append(discoveryAddresses.CIDRRules,
		discovery.CIDRRule{
			IPRange: cfg.ExtraConfig.ServiceIPRange,
			Address: net.JoinHostPort(cfg.ExtraConfig.APIServerServiceIP.String(), strconv.Itoa(cfg.ExtraConfig.APIServerServicePort)),
		})
	cfg.GenericConfig.DiscoveryAddresses = discoveryAddresses

	// 设置默认的NodePort端口范围
	if cfg.ExtraConfig.ServiceNodePortRange.Size == 0 {
		// TODO: Currently no way to specify an empty range (do we need to allow this?)
		// We should probably allow this for clouds that don't require NodePort to do load-balancing (GCE)
		// but then that breaks the strict nestedness of ServiceType.
		// Review post-v1
		cfg.ExtraConfig.ServiceNodePortRange = kubeoptions.DefaultServiceNodePortRange
		klog.Infof("Node port range unspecified. Defaulting to %v.", cfg.ExtraConfig.ServiceNodePortRange)
	}

	if cfg.ExtraConfig.EndpointReconcilerConfig.Interval == 0 {
		cfg.ExtraConfig.EndpointReconcilerConfig.Interval = DefaultEndpointReconcilerInterval
	}

	if cfg.ExtraConfig.MasterEndpointReconcileTTL == 0 {
		cfg.ExtraConfig.MasterEndpointReconcileTTL = DefaultEndpointReconcilerTTL
	}

	if cfg.ExtraConfig.EndpointReconcilerConfig.Reconciler == nil {
		cfg.ExtraConfig.EndpointReconcilerConfig.Reconciler = c.createEndpointReconciler()
	}

	if cfg.ExtraConfig.RepairServicesInterval == 0 {
		cfg.ExtraConfig.RepairServicesInterval = repairLoopInterval
	}

	return CompletedConfig{&cfg}
}

// New returns a new instance of Master from the given config.
// Certain config fields will be set to a default value if unset.
// Certain config fields must be specified, including:
// KubeletClientConfig
// 实例化APIServer，本质上就是GenericServer
func (c completedConfig) New(delegationTarget genericapiserver.DelegationTarget) (*Instance, error) {
	if reflect.DeepEqual(c.ExtraConfig.KubeletClientConfig, kubeletclient.KubeletClientConfig{}) {
		return nil, fmt.Errorf("Master.New() called with empty config.KubeletClientConfig")
	}

	// 实例化GenericServer
	s, err := c.GenericConfig.New("kube-apiserver", delegationTarget)
	if err != nil {
		return nil, err
	}

	// 注册/logs路由
	if c.ExtraConfig.EnableLogsSupport {
		routes.Logs{}.Install(s.Handler.GoRestfulContainer)
	}

	// Metadata and keys are expected to only change across restarts at present,
	// so we just marshal immediately and serve the cached JSON bytes.
	md, err := serviceaccount.NewOpenIDMetadata(
		c.ExtraConfig.ServiceAccountIssuerURL,
		c.ExtraConfig.ServiceAccountJWKSURI,
		c.GenericConfig.ExternalAddress,
		c.ExtraConfig.ServiceAccountPublicKeys,
	)
	if err != nil {
		// If there was an error, skip installing the endpoints and log the
		// error, but continue on. We don't return the error because the
		// metadata responses require additional, backwards incompatible
		// validation of command-line options.
		msg := fmt.Sprintf("Could not construct pre-rendered responses for"+
			" ServiceAccountIssuerDiscovery endpoints. Endpoints will not be"+
			" enabled. Error: %v", err)
		if c.ExtraConfig.ServiceAccountIssuerURL != "" {
			// The user likely expects this feature to be enabled if issuer URL is
			// set and the feature gate is enabled. In the future, if there is no
			// longer a feature gate and issuer URL is not set, the user may not
			// expect this feature to be enabled. We log the former case as an Error
			// and the latter case as an Info.
			klog.Error(msg)
		} else {
			klog.Info(msg)
		}
	} else {
		// 注册/.well-known/openid-configuration, /openid/v1/jwks路由
		routes.NewOpenIDMetadataServer(md.ConfigJSON, md.PublicKeysetJSON).
			Install(s.Handler.GoRestfulContainer)
	}

	m := &Instance{
		GenericAPIServer:          s,
		ClusterAuthenticationInfo: c.ExtraConfig.ClusterAuthenticationInfo,
	}

	// install legacy rest storage

	// 向GenericServer中注册核心资源路由
	if err := m.InstallLegacyAPI(&c, c.GenericConfig.RESTOptionsGetter); err != nil {
		return nil, err
	}

	// 实例化APIServer客户端工具
	clientset, err := kubernetes.NewForConfig(c.GenericConfig.LoopbackClientConfig)
	if err != nil {
		return nil, err
	}

	// TODO: update to a version that caches success but will recheck on failure, unlike memcache discovery
	discoveryClientForAdmissionRegistration := clientset.Discovery()

	// The order here is preserved in discovery.
	// If resources with identical names exist in more than one of these groups (e.g. "deployments.apps"" and "deployments.extensions"),
	// the order of this list determines which group an unqualified resource name (e.g. "deployments") should prefer.
	// This priority order is used for local discovery, but it ends up aggregated in `k8s.io/kubernetes/cmd/kube-apiserver/app/aggregator.go
	// with specific priorities.
	// TODO: describe the priority all the way down in the RESTStorageProviders and plumb it back through the various discovery
	// handlers that we have.
	restStorageProviders := []RESTStorageProvider{
		apiserverinternalrest.StorageProvider{},
		authenticationrest.RESTStorageProvider{Authenticator: c.GenericConfig.Authentication.Authenticator, APIAudiences: c.GenericConfig.Authentication.APIAudiences},
		authorizationrest.RESTStorageProvider{Authorizer: c.GenericConfig.Authorization.Authorizer, RuleResolver: c.GenericConfig.RuleResolver},
		autoscalingrest.RESTStorageProvider{},
		batchrest.RESTStorageProvider{},
		certificatesrest.RESTStorageProvider{},
		coordinationrest.RESTStorageProvider{},
		discoveryrest.StorageProvider{},
		networkingrest.RESTStorageProvider{},
		noderest.RESTStorageProvider{},
		policyrest.RESTStorageProvider{},
		rbacrest.RESTStorageProvider{Authorizer: c.GenericConfig.Authorization.Authorizer},
		schedulingrest.RESTStorageProvider{},
		storagerest.RESTStorageProvider{},
		flowcontrolrest.RESTStorageProvider{InformerFactory: c.GenericConfig.SharedInformerFactory},
		// keep apps after extensions so legacy clients resolve the extensions versions of shared resource names.
		// See https://github.com/kubernetes/kubernetes/issues/42392
		appsrest.StorageProvider{},
		admissionregistrationrest.RESTStorageProvider{Authorizer: c.GenericConfig.Authorization.Authorizer, DiscoveryClient: discoveryClientForAdmissionRegistration},
		eventsrest.RESTStorageProvider{TTL: c.ExtraConfig.EventTTL},
		resourcerest.RESTStorageProvider{},
	}
	// 注册除核心资源以外的其它路由
	if err := m.InstallAPIs(c.ExtraConfig.APIResourceConfigSource, c.GenericConfig.RESTOptionsGetter, restStorageProviders...); err != nil {
		return nil, err
	}

	// 监听ClientCA,代理证书的变化，并更新（如果没有就创建）kube-system名称空间下名为extension-apiserver-authentication的ConfigMap
	m.GenericAPIServer.AddPostStartHookOrDie("start-cluster-authentication-info-controller", func(hookContext genericapiserver.PostStartHookContext) error {
		kubeClient, err := kubernetes.NewForConfig(hookContext.LoopbackClientConfig)
		if err != nil {
			return err
		}
		controller := clusterauthenticationtrust.NewClusterAuthenticationTrustController(m.ClusterAuthenticationInfo, kubeClient)

		// generate a context  from stopCh. This is to avoid modifying files which are relying on apiserver
		// TODO: See if we can pass ctx to the current method
		ctx := wait.ContextForChannel(hookContext.StopCh)

		// prime values and start listeners
		if m.ClusterAuthenticationInfo.ClientCA != nil {
			// ClusterAuthenticationTrustController也关心ClientCA的变化
			m.ClusterAuthenticationInfo.ClientCA.AddListener(controller)
			if controller, ok := m.ClusterAuthenticationInfo.ClientCA.(dynamiccertificates.ControllerRunner); ok {
				// runonce to be sure that we have a value.
				// 初始化CAContentProvider，这是第一次读取ClientCA
				if err := controller.RunOnce(ctx); err != nil {
					runtime.HandleError(err)
				}
				// 启动CAContentProvider，ClientCAProvider启动之后将会持续监听ClientCA的变化
				go controller.Run(ctx, 1)
			}
		}
		if m.ClusterAuthenticationInfo.RequestHeaderCA != nil {
			// ClusterAuthenticationTrustController关心代理证书的变化
			m.ClusterAuthenticationInfo.RequestHeaderCA.AddListener(controller)
			if controller, ok := m.ClusterAuthenticationInfo.RequestHeaderCA.(dynamiccertificates.ControllerRunner); ok {
				// runonce to be sure that we have a value.
				// 初始化CAContentProvider，这是第一次读取ClientCA
				if err := controller.RunOnce(ctx); err != nil {
					runtime.HandleError(err)
				}
				// 启动CAContentProvider，ClientCAProvider启动之后将会持续监听代理证书的变化
				go controller.Run(ctx, 1)
			}
		}

		// 启动Controller，一旦发现ClientCA,或者是代理证书发生变化，就更新kube-system名称空间下名为extension-apiserver-authentication的ConfigMap
		go controller.Run(ctx, 1)
		return nil
	})

	// TODO 这玩意用于给每个APIServer的Lease续期  每个APIServer的Lease对象用来干啥的？ 判断APIServer是否存活？
	if utilfeature.DefaultFeatureGate.Enabled(apiserverfeatures.APIServerIdentity) {
		m.GenericAPIServer.AddPostStartHookOrDie("start-kube-apiserver-identity-lease-controller", func(hookContext genericapiserver.PostStartHookContext) error {
			// 实例化客户端
			kubeClient, err := kubernetes.NewForConfig(hookContext.LoopbackClientConfig)
			if err != nil {
				return err
			}

			// generate a context  from stopCh. This is to avoid modifying files which are relying on apiserver
			// TODO: See if we can pass ctx to the current method
			ctx := wait.ContextForChannel(hookContext.StopCh)

			leaseName := m.GenericAPIServer.APIServerID
			holderIdentity := m.GenericAPIServer.APIServerID + "_" + string(uuid.NewUUID())

			controller := lease.NewController(
				clock.RealClock{},                   // 时钟
				kubeClient,                          // APIServer Client客户端
				holderIdentity,                      // APIServer的标识符
				int32(IdentityLeaseDurationSeconds), // 1小时
				nil,
				IdentityLeaseRenewIntervalPeriod, // 10秒钟
				leaseName,                        // APIServer的ID
				metav1.NamespaceSystem,
				// TODO: receive identity label value as a parameter when post start hook is moved to generic apiserver.
				labelAPIServerHeartbeatFunc(KubeAPIServer))
			go controller.Run(ctx)
			return nil
		})
		// Labels for apiserver idenitiy leases switched from k8s.io/component=kube-apiserver to apiserver.kubernetes.io/identity=kube-apiserver.
		// For compatibility, garbage collect leases with both labels for at least 1 release
		// TODO: remove in Kubernetes 1.28
		// 用于清理kube-system名称空间中的过期Lease资源(标签为k8s.io/component=kube-apiserver)
		m.GenericAPIServer.AddPostStartHookOrDie("start-deprecated-kube-apiserver-identity-lease-garbage-collector", func(hookContext genericapiserver.PostStartHookContext) error {
			kubeClient, err := kubernetes.NewForConfig(hookContext.LoopbackClientConfig)
			if err != nil {
				return err
			}
			go apiserverleasegc.NewAPIServerLeaseGC(
				kubeClient,
				IdentityLeaseGCPeriod, // 1个小时
				metav1.NamespaceSystem,
				DeprecatedKubeAPIServerIdentityLeaseLabelSelector, // k8s.io/component=kube-apiserver
			).Run(hookContext.StopCh)
			return nil
		})
		// TODO: move this into generic apiserver and make the lease identity value configurable
		// 用于清理kube-system名称空间中的过期Lease资源(标签为apiserver.kubernetes.io/identity=kube-apiserver)
		m.GenericAPIServer.AddPostStartHookOrDie("start-kube-apiserver-identity-lease-garbage-collector", func(hookContext genericapiserver.PostStartHookContext) error {
			kubeClient, err := kubernetes.NewForConfig(hookContext.LoopbackClientConfig)
			if err != nil {
				return err
			}
			go apiserverleasegc.NewAPIServerLeaseGC(
				kubeClient,
				IdentityLeaseGCPeriod,                   // 3600秒
				metav1.NamespaceSystem,                  // kube-system
				KubeAPIServerIdentityLeaseLabelSelector, // apiserver.kubernetes.io/identity=kube-apiserver
			).Run(hookContext.StopCh)
			return nil
		})
	}

	// 监听并更新kube-system名称空间中名为kube-apiserver-legacy-service-account-token-tracking的ConfigMap
	m.GenericAPIServer.AddPostStartHookOrDie("start-legacy-token-tracking-controller", func(hookContext genericapiserver.PostStartHookContext) error {
		kubeClient, err := kubernetes.NewForConfig(hookContext.LoopbackClientConfig)
		if err != nil {
			return err
		}
		go legacytokentracking.NewController(kubeClient).Run(hookContext.StopCh)
		return nil
	})

	return m, nil
}

func labelAPIServerHeartbeatFunc(identity string) lease.ProcessLeaseFunc {
	return func(lease *coordinationapiv1.Lease) error {
		if lease.Labels == nil {
			lease.Labels = map[string]string{}
		}

		// This label indiciates the identity of the lease object.
		lease.Labels[IdentityLeaseComponentLabelKey] = identity

		hostname, err := os.Hostname()
		if err != nil {
			return err
		}

		// convenience label to easily map a lease object to a specific apiserver
		lease.Labels[apiv1.LabelHostname] = hostname
		return nil
	}
}

// InstallLegacyAPI will install the legacy APIs for the restStorageProviders if they are enabled.
func (m *Instance) InstallLegacyAPI(
	c *completedConfig, // APIServer配置
	restOptionsGetter generic.RESTOptionsGetter, // 可以理解为资源的存储工厂，用于获取资源的存储配置，特别是编解码器
) error {
	legacyRESTStorageProvider := corerest.LegacyRESTStorageProvider{
		StorageFactory:              c.ExtraConfig.StorageFactory,
		ProxyTransport:              c.ExtraConfig.ProxyTransport,
		KubeletClientConfig:         c.ExtraConfig.KubeletClientConfig,
		EventTTL:                    c.ExtraConfig.EventTTL,
		ServiceIPRange:              c.ExtraConfig.ServiceIPRange,
		SecondaryServiceIPRange:     c.ExtraConfig.SecondaryServiceIPRange,
		ServiceNodePortRange:        c.ExtraConfig.ServiceNodePortRange,
		LoopbackClientConfig:        c.GenericConfig.LoopbackClientConfig,
		ServiceAccountIssuer:        c.ExtraConfig.ServiceAccountIssuer,
		ExtendExpiration:            c.ExtraConfig.ExtendExpiration,
		ServiceAccountMaxExpiration: c.ExtraConfig.ServiceAccountMaxExpiration,
		APIAudiences:                c.GenericConfig.Authentication.APIAudiences,
		Informers:                   c.ExtraConfig.VersionedInformers,
	}
	// TODO 仔细分析
	legacyRESTStorage, apiGroupInfo, err := legacyRESTStorageProvider.NewLegacyRESTStorage(c.ExtraConfig.APIResourceConfigSource, restOptionsGetter)
	if err != nil {
		return fmt.Errorf("error building core storage: %v", err)
	}
	if len(apiGroupInfo.VersionedResourcesStorageMap) == 0 { // if all core storage is disabled, return.
		return nil
	}

	controllerName := "bootstrap-controller"
	// 实例化APIServer客户端
	client := kubernetes.NewForConfigOrDie(c.GenericConfig.LoopbackClientConfig)
	// 用于创建kube-system, kube-node-lease, kube-public, default名称空间，如果不存在的话
	m.GenericAPIServer.AddPostStartHookOrDie("start-system-namespaces-controller", func(hookContext genericapiserver.PostStartHookContext) error {
		go systemnamespaces.NewController(client, c.ExtraConfig.VersionedInformers.Core().V1().Namespaces()).Run(hookContext.StopCh)
		return nil
	})

	// TODO 详细分析BootstrapController作用
	bootstrapController, err := c.NewBootstrapController(legacyRESTStorage, client)
	if err != nil {
		return fmt.Errorf("error creating bootstrap controller: %v", err)
	}
	m.GenericAPIServer.AddPostStartHookOrDie(controllerName, bootstrapController.PostStartHook)
	m.GenericAPIServer.AddPreShutdownHookOrDie(controllerName, bootstrapController.PreShutdownHook)

	// 注册路由
	if err := m.GenericAPIServer.InstallLegacyAPIGroup(genericapiserver.DefaultLegacyAPIPrefix, &apiGroupInfo); err != nil {
		return fmt.Errorf("error in registering group versions: %v", err)
	}
	return nil
}

// RESTStorageProvider is a factory type for REST storage.
type RESTStorageProvider interface {
	GroupName() string
	NewRESTStorage(apiResourceConfigSource serverstorage.APIResourceConfigSource, restOptionsGetter generic.RESTOptionsGetter) (genericapiserver.APIGroupInfo, error)
}

// InstallAPIs will install the APIs for the restStorageProviders if they are enabled.
func (m *Instance) InstallAPIs(
	apiResourceConfigSource serverstorage.APIResourceConfigSource, // // 用于判断某个GVR，或者是某个组是否被启用
	restOptionsGetter generic.RESTOptionsGetter, // 存储工厂，可以获取每个资源的存储配置，以及资源的编解码器
	restStorageProviders ...RESTStorageProvider, // 组下所有资源的存储增删改查handler
) error {
	var apiGroupsInfo []*genericapiserver.APIGroupInfo

	// used later in the loop to filter the served resource by those that have expired.
	// TODO 这玩意干嘛的？
	resourceExpirationEvaluator, err := genericapiserver.NewResourceExpirationEvaluator(*m.GenericAPIServer.Version)
	if err != nil {
		return err
	}

	// 遍历APIServer中除了核心资源以外的所有组，组装APIGroupInfo
	for _, restStorageBuilder := range restStorageProviders {
		groupName := restStorageBuilder.GroupName()
		// 实例化APIGroupInfo
		apiGroupInfo, err := restStorageBuilder.NewRESTStorage(apiResourceConfigSource, restOptionsGetter)
		if err != nil {
			return fmt.Errorf("problem initializing API group %q : %v", groupName, err)
		}
		// 如果当前组下没有资源，那么直接退出
		if len(apiGroupInfo.VersionedResourcesStorageMap) == 0 {
			// If we have no storage for any resource configured, this API group is effectively disabled.
			// This can happen when an entire API group, version, or development-stage (alpha, beta, GA) is disabled.
			klog.Infof("API group %q is not enabled, skipping.", groupName)
			continue
		}

		// Remove resources that serving kinds that are removed.
		// We do this here so that we don't accidentally serve versions without resources or openapi information that for kinds we don't serve.
		// This is a spot above the construction of individual storage handlers so that no sig accidentally forgets to check.
		// TODO 这里在干嘛？
		resourceExpirationEvaluator.RemoveDeletedKinds(groupName, apiGroupInfo.Scheme, apiGroupInfo.VersionedResourcesStorageMap)
		if len(apiGroupInfo.VersionedResourcesStorageMap) == 0 {
			klog.V(1).Infof("Removing API group %v because it is time to stop serving it because it has no versions per APILifecycle.", groupName)
			continue
		}

		klog.V(1).Infof("Enabling API group %q.", groupName)

		// TODO StorageFlowControl, StorageRBAC, StorageScheduling实现了这个接口，仔细分析这三个实现了什么功能
		if postHookProvider, ok := restStorageBuilder.(genericapiserver.PostStartHookProvider); ok {
			name, hook, err := postHookProvider.PostStartHook()
			if err != nil {
				klog.Fatalf("Error building PostStartHook: %v", err)
			}
			m.GenericAPIServer.AddPostStartHookOrDie(name, hook)
		}

		apiGroupsInfo = append(apiGroupsInfo, &apiGroupInfo)
	}

	if err := m.GenericAPIServer.InstallAPIGroups(apiGroupsInfo...); err != nil {
		return fmt.Errorf("error in registering group versions: %v", err)
	}
	return nil
}

var (
	// stableAPIGroupVersionsEnabledByDefault is a list of our stable versions.
	stableAPIGroupVersionsEnabledByDefault = []schema.GroupVersion{
		admissionregistrationv1.SchemeGroupVersion,
		apiv1.SchemeGroupVersion,
		appsv1.SchemeGroupVersion,
		authenticationv1.SchemeGroupVersion,
		authorizationapiv1.SchemeGroupVersion,
		autoscalingapiv1.SchemeGroupVersion,
		autoscalingapiv2.SchemeGroupVersion,
		batchapiv1.SchemeGroupVersion,
		certificatesapiv1.SchemeGroupVersion,
		coordinationapiv1.SchemeGroupVersion,
		discoveryv1.SchemeGroupVersion,
		eventsv1.SchemeGroupVersion,
		networkingapiv1.SchemeGroupVersion,
		nodev1.SchemeGroupVersion,
		policyapiv1.SchemeGroupVersion,
		rbacv1.SchemeGroupVersion,
		storageapiv1.SchemeGroupVersion,
		schedulingapiv1.SchemeGroupVersion,
	}

	// legacyBetaEnabledByDefaultResources is the list of beta resources we enable.  You may only add to this list
	// if your resource is already enabled by default in a beta level we still serve AND there is no stable API for it.
	// see https://github.com/kubernetes/enhancements/tree/master/keps/sig-architecture/3136-beta-apis-off-by-default
	// for more details.
	legacyBetaEnabledByDefaultResources = []schema.GroupVersionResource{
		flowcontrolv1beta2.SchemeGroupVersion.WithResource("flowschemas"),                 // remove in 1.29
		flowcontrolv1beta2.SchemeGroupVersion.WithResource("prioritylevelconfigurations"), // remove in 1.29
		flowcontrolv1beta3.SchemeGroupVersion.WithResource("flowschemas"),                 // deprecate in 1.29, remove in 1.32
		flowcontrolv1beta3.SchemeGroupVersion.WithResource("prioritylevelconfigurations"), // deprecate in 1.29, remove in 1.32
	}
	// betaAPIGroupVersionsDisabledByDefault is for all future beta groupVersions.
	betaAPIGroupVersionsDisabledByDefault = []schema.GroupVersion{
		authenticationv1beta1.SchemeGroupVersion,
		autoscalingapiv2beta1.SchemeGroupVersion,
		autoscalingapiv2beta2.SchemeGroupVersion,
		batchapiv1beta1.SchemeGroupVersion,
		discoveryv1beta1.SchemeGroupVersion,
		eventsv1beta1.SchemeGroupVersion,
		nodev1beta1.SchemeGroupVersion, // remove in 1.26
		policyapiv1beta1.SchemeGroupVersion,
		storageapiv1beta1.SchemeGroupVersion,
		flowcontrolv1beta1.SchemeGroupVersion,
		flowcontrolv1beta2.SchemeGroupVersion,
		flowcontrolv1beta3.SchemeGroupVersion,
	}

	// alphaAPIGroupVersionsDisabledByDefault holds the alpha APIs we have.  They are always disabled by default.
	alphaAPIGroupVersionsDisabledByDefault = []schema.GroupVersion{
		admissionregistrationv1alpha1.SchemeGroupVersion,
		apiserverinternalv1alpha1.SchemeGroupVersion,
		authenticationv1alpha1.SchemeGroupVersion,
		resourcev1alpha2.SchemeGroupVersion,
		certificatesv1alpha1.SchemeGroupVersion,
		networkingapiv1alpha1.SchemeGroupVersion,
		storageapiv1alpha1.SchemeGroupVersion,
		flowcontrolv1alpha1.SchemeGroupVersion,
	}
)

// DefaultAPIResourceConfigSource returns default configuration for an APIResource.
// 启用所有处于稳定版本的GRV，禁用所有处于Alpha/Beta版本的资源（但是启用流控相关的GVR）
func DefaultAPIResourceConfigSource() *serverstorage.ResourceConfig {
	ret := serverstorage.NewResourceConfig()
	// NOTE: GroupVersions listed here will be enabled by default. Don't put alpha or beta versions in the list.
	// 默认启用所有处于稳定版本的GRV
	ret.EnableVersions(stableAPIGroupVersionsEnabledByDefault...)

	// disable alpha and beta versions explicitly so we have a full list of what's possible to serve
	// 默认禁用所有处于beta版本的GRV
	ret.DisableVersions(betaAPIGroupVersionsDisabledByDefault...)
	// 默认禁用处于alpha的GRV
	ret.DisableVersions(alphaAPIGroupVersionsDisabledByDefault...)

	// enable the legacy beta resources that were present before stopped serving new beta APIs by default.
	// 启用流控相关的GRV
	ret.EnableResources(legacyBetaEnabledByDefaultResources...)

	return ret
}
