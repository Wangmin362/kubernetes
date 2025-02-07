
# 1. 概念

- [ ] Owner, OwnerReference
- [x] APIAudiences
    - Audience实际上是JWT中的概念，JWT的Playload中有两个保留字段，一个是iss，也就是Token签发人；另外一个则是aud，也就是Token的接收人，也就是说JTW可以明确当前的Token是颁发给谁的。而这里的APIAudience就是这个作用。
- [ ] Event
- [ ] Condition
- [ ] Labels
- [ ] Annotaion

# 2. 组件原理
## 2.1. APIServer
- [ ] APIServer认证
  - [ ] APIServer有几种认证方式？每种认证方式的原理是啥？
      - [ ] Anonymous
          - [ ] --anonymous-auth
      - [x] TokenFile 其实就是StaticToken认证
          - [x] --token-auth-file 指定CSV Token文件，格式为：token,user,uid,"group1,group2,group3"
      - [x] ServiceAccount，这是K8S Pod认证原理; 所谓的SA认证，其实就是JWT认证。
          - [x] --service-account-key-file  JWT的密钥
          - [x] --service-account-lookup 是否开启校验JWT对应的Secret/ServiceAccount
          - [x] --service-account-issuer JWT的签发人
          - [ ] --service-account-jwks-uri
          - [ ] --service-account-max-token-expiration
          - [ ] --service-account-extend-token-expiration
      - [x] BootstrapToken
          - [x] 认证原理
          - [ ] 为啥需要？解决了什么问题？
          - [x] 参数配置
              - [x] --enable-bootstrap-token-auth 是否开启BootstrapToken认证方式
      - [x] WebhookToken认证
          - [x] 认证原理？
          - [ ] 为啥需要？解决了什么问题？
          - [ ] TokenReview, TokenRequest
          - [x] 参数配置
              - [x] --authentication-token-webhook-config-file 用于指定K8S如何访问远端webhook服务器
              - [x] --authentication-token-webhook-version 用于设置使用TokenReview的v1beta1版本还是v1版本，K8S为了保持版本兼容，默认使用的是v1beta1版本
              - [x] --authentication-token-webhook-cache-ttl 认证结果的缓存有效时间
      - [x] ClientCert
          - [s] --client-ca-file 用于指定Client端的证书
      - [ ] RequestHeader，K8S 代理认证
          - [ ] 认证原理？
          - [ ] 如何理解这种认证方式？为啥需要这种认证方式？
          - [ ] 参数
              - [ ] --requestheader-username-headers
              - [ ] --requestheader-group-headers
              - [ ] --requestheader-extra-headers-prefix
              - [ ] --requestheader-client-ca-file
              - [ ] --requestheader-allowed-names
      - [ ] OIDC
          - [ ] --oidc-issuer-url
          - [ ] --oidc-client-id
          - [ ] --oidc-ca-file
          - [ ] --oidc-username-claim
          - [ ] --oidc-username-prefix
          - [ ] --oidc-groups-claim
          - [ ] --oidc-groups-prefix
          - [ ] --oidc-signing-algs
          - [ ] --oidc-required-claim
  - [ ] APIServer如何自定义扩展认证方式？ 答：采用Webhook
  - [ ] 影响APIServer的认证参数有哪些？
      - [x] --api-audiences 设置JWT的合法audiance
- [ ] APIServer鉴权
  - [ ] APIServer有几种鉴权方式？每种鉴权方式的原理是啥？
      - [ ] AlwaysAllow
      - [ ] AlwaysDeny
      - [ ] ABAC
      - [ ] Webhook
      - [ ] RBAC
      - [ ] Node
  - [ ] APIServer如何自定义扩展鉴权方式？
  - [ ] 影响APIServer的鉴权参数有哪些？
- [ ] APIServer准入控制
  - [ ] APIServer有集中准入控制插件？每种准入控制插件的原理是啥？
      - [x] AlwaysAdmit 任何请求都允许准入
      - [ ] AlwaysPullImages
      - [ ] LimitPodHardAntiAffinityTopology
      - [ ] CertificateApproval
      - [ ] CertificateSigning
      - [ ] CertificateSubjectRestriction
      - [ ] DefaultTolerationSeconds
      - [x] AlwaysDeny 任何请求都不许允准入
      - [ ] EventRateLimit
      - [ ] ExtendedResourceToleration
      - [ ] OwnerReferencesPermissionEnforcement
      - [ ] ImagePolicyWebhook
      - [ ] LimitRanger
      - [ ] NamespaceAutoProvision
      - [ ] NamespaceExists
      - [ ] DefaultIngressClass
      - [ ] DenyServiceExternalIPs
      - [ ] NodeRestriction
      - [ ] TaintNodesByCondition
      - [ ] PodNodeSelector
      - [ ] PodTolerationRestriction
      - [ ] Priority
      - [ ] RuntimeClass
      - [ ] PodSecurity
      - [ ] SecurityContextDeny
      - [x] ServiceAccount
        - 1、如果Pod没有设置SA，那么设置default为该Pod的SA（MirrorPod除外） 2、为Pod挂载SA对应的Token
      - [ ] PersistentVolumeLabel
      - [ ] PersistentVolumeClaimResize
      - [ ] DefaultStorageClass
      - [ ] StorageObjectInUseProtection
  - [ ] MutatingWebhook和ValidationWebhook准入控制插件的原理？
  - [ ] K8S动态准入控制 (Dynamic Webhook)原理是啥？
  - [ ] 如何理解K8S准入控制架构设计？
  - [ ] 如何自定义准入控制插件？
  - [ ] 影响准入控制插件参数有哪些？
      - [ ] --admission-control
      - [ ] --enable-admission-plugins
      - [ ] --disable-admission-plugins
      - [ ] --admission-control-config-file
  - [ ] PreHook原理
- [ ] APIServer审计
  - [ ] K8S的审计架构？
  - [ ] 用户如何自定义审计后端(Audit Backend)？
  - [ ] 相关参数
      - [ ] --audit-policy-file
      - [ ] --audit-log-path
      - [ ] --audit-log-maxage
      - [ ] --audit-log-maxbackup
      - [ ] --audit-log-maxsize
      - [ ] --audit-log-format
      - [ ] --audit-log-compress
      - [ ] --audit-log-version
      - [ ] --audit-\<plugin-name>-truncate-enabled
      - [ ] --audit-\<plugin-name>-truncate-max-batch-size
      - [ ] --audit-\<plugin-name>-truncate-max-event-size
      - [ ] --audit-webhook-config-file
      - [ ] --audit-webhook-initial-backoff
      - [ ] --audit-webhook-batch-initial-backoff
      - [ ] --audit-webhook-batch-initial-backoff
      - [ ] --audit-webhook-version
      - [ ] --audit-\<plugin-name>-mode
      - [ ] --audit-\<plugin-name>-batch-buffer-size
      - [ ] --audit-\<plugin-name>-batch-max-size
      - [ ] --audit-\<plugin-name>-batch-max-wait
      - [ ] --audit-\<plugin-name>-batch-throttle-enable
      - [ ] --audit-\<plugin-name>-batch-throttle-qps
      - [ ] --audit-\<plugin-name>-batch-throttle-burst
- [ ] APIServer扩展点
    - [ ] API资源扩展
        - [ ] Annotation
        - [ ] Finalizer
        - [ ] CRD
        - [ ] Aggregation
    - [ ] API访问扩展
        - [ ] 认证Webhook
        - [ ] 授权Webhook
        - [ ] 准入控制Webhook
- [ ] [APIServer流控](https://kubernetes.io/zh-cn/docs/concepts/cluster-administration/flow-control/)
  - [ ] APIServer有几种限速策略？每种限速策略原理是啥？影响像素策略的参数是啥？
      - [ ] APF (API Priority-and-Fairness)原理？ 影响参数？
      - [ ] Max-in-Flight原理？影响参数？
  - [ ] 影响参数
    - [ ] --max-requests-inflight
    - [ ] --min-request-timeout
- [ ] APIServer的HST (Strict-Transport-Security)是啥？为啥需要？
- [ ] CA证书
  - [ ] K8S是如何设计证书的？
  - [ ] 加入集群的证书到期了，或者私钥被以外泄露，如何安全的更新证书？
  - [ ] APIServer的动态证书是怎么回事？
- [ ] Lease
  - [ ] APIServer是如何使用Lease的？
  - [ ] APIServer的Leader选举原理
  - [ ] 自定义的服务，倘若以集群的方式部署，如何解决Lease解决领导选举的问题？
  - [ ] 为什么Lease需要续期？租约续期是怎么回事？
- [ ] EgressSelector是啥？Konnectivity又是为了解决什么问题？
    - [ ] --egress-selector-config-file
- [ ] K8S是如何解决跨域问题的（CORS）？
- [ ] 如何理解K8S的资源与子资源？
  - [ ] K8S资源
  - [ ] K8S子资源
  - [ ] Schema
  - [ ] RestMapping
  - [ ] RestGetMapper
- [ ] K8S Misc
  - [ ] Event
  - [ ] Seccomp
  - [ ] Pod Security Standards
- [ ] APIServer架构
  - [ ] K8S是如何设计APIServer、ExtensionServer、AggregatorServer这三个Server的？从这个设计当中我们能学到什么？
- [ ] AggregatorServer
  - [ ] K8S Aggregator扩展原理？
  - [ ] 用户如何自定义Aggregator？ 什么时候需要自定义Aggregator?  Aggregator解决了CRD的哪些不足？ 自定义Aggregator最佳实践？
- [ ] ExtensionServer
  - [ ] 如何自定义CRD？CRD的最佳实践？什么情况下应该使用CRD，什么情况下应该使用Aggregator?
  - [ ] 自定义的资源什么时候需要Approval机制？为什么需要Approval机制？
  - [ ] Operator、Helm、Docker分别解决了什么问题？
  - [ ] 一个CRD提交后，APIServer都做了哪些操作？ CRD处于什么阶段之后，用户才可以提交CR？
- [ ] APIServer的后端存储
  - [ ] 如何理解K8S后端存储设计？从这个设计当中我们能学到什么？
  - [ ] 如何自定义K8S后端存储?
  - [ ] K8S的发行版K3S是如何解决存储问题的？
- [ ] K8S的日志
  - [ ] K8S的日志架构？
  - [ ] K8S日志原理？
  - [ ] 影响K8S日志参数有哪些？
- [ ] Informer机制
  - [ ] 如何理解K8S的Informer机制？从K8S Informer机制中我们能学到什么？
  - [ ] 如何自定义Informer? 很多前期的Operator都是自定义Informer
  - [ ] 影响Informer的参数有哪些？如何正确利用Informer? 错误使用Informer的姿势有哪些？
- [ ] ListWatcher机制
- [ ] Finalizer机制
- [ ] CloudProvider
- [ ] APIServer的分布式链路追踪是如何设计的？
- [ ] APIServer的启动参数功能分析
    - [ ] 通用参数
      - [ ] --watch-cache
      - [ ] --watch-cache-sizes
      - [ ] --goaway-chance 有何作用？为啥需要？原理是啥？从中可以学到什么？
      - [ ] --service-cluster-ip-range
      - [ ] --service-node-port-range
  - [ ] CloudProvider参数
      - [ ] --cloud-provider
      - [ ] --cloud-config
  - [ ] API启用禁用
      - [ ] --egress-selector-config-file
- [ ] FeatureGate
  - [ ] 特性开关有哪些？每个开关有何影响？
- [ ] 指标
  - [ ] APIServer暴露了哪些指标，每种指标代表了什么含义？

## 2.2. ControllerManager

- [ ] Ingress原理? K8S Ingress架构？
- [ ] SessionAffinity原理
- [ ] CSI插件
- [ ] Plugin插件原理？

## 2.3. Kubelet

- [ ] Kubelet是如何调用CRI的？
- [ ] Kubelet有几种可用的CRI？每种CRI有何区别？如何指定Kubelet使用不同的CRI?
- [ ] 抽象结构
    - [ ] CloudProvider
    - [ ] PodStorage
    - [ ] Kubelet认证 & 鉴权
    - [ ] CGroup
- [ ] 模块
    - [ ] KubeletServerCertificateManager
    - [ ] CheckpointManager
    - [ ] CloudResourceSyncManager
    - [ ] CGroupManager
    - [ ] ContainerManager
    - [ ] NodeContainerManager
    - [ ] PodContainerManager
    - [ ] QosContainerManager
    - [ ] CpuManager
    - [ ] MemoryManager
    - [ ] DRAManager
    - [ ] DeviceManager
    - [ ] ConfigMapManager
    - [ ] Runtime
    - [ ] RemoteImageService
    - [ ] EvictionManager
    - [ ] ImageGCManger
    - [ ] PLEG
    - [ ] ContainerLogManager
    - [ ] NodeShutdownManager
    - [ ] OomWatcher
    - [ ] PluginManager
    - [ ] PodManager
    - [ ] ProbeManager
    - [ ] RuntimeClassManager
    - [ ] SecretManager
    - [ ] StatusManager
    - [ ] TokenManager
    - [ ] VolumeManager
    - [ ] VolumePluginManager
    - [ ] ResourceAnalyzer
    - [ ] CertificateManager
    - [ ] PodKiller
    - [ ] NodeLeaseController
    - [ ] PodAdmitHandlers
    - [ ] LinessManager
    - [ ] ImageManager
    - [ ] StartupManager

## 2.4. KubeScheduler

- [ ] K8S调度架构
- [ ] `Scheduler`扩展点
- [ ] 如何自定义调度器？
- [ ] 参数
- [ ] Extender机制是啥？优缺点？
- [ ] 如何理解`Framework`接口和`Handler`接口抽象
- [ ] 如何理解`PluginFactory`
- [ ] 什么是`AssumedPod`，如果`AssumedPod`后续在`BindingCycle`真的失败了会怎么样？
- [ ] 什么是`NominatedPod`？有啥作用？ 如何理解`PodNominator`接口抽象？
- [ ] `Pod`的`Priority`有啥作用？是如何影响`Pod`调度的？最佳实践？
    - [ ] 如果`Pod`调度失败，`KubeScheduler`会如何处理这个`Pod`？
- [ ] 内部调度原理
    - [ ] SchedulerQueue -> PriorityQueue
    - [ ] Cache -> cacheImpl
    - [ ] Framework -> frameworkImpl
    - [ ] Nominator
    - [ ] Exender
    - [ ] Snapshot
    - [ ] Scheduler

## 2.5. KubeProxy

- [ ] K8S支持几种代理模式？每种代理模式的原理是啥？
  - [ ] ipvs代理模式
  - [ ] iptable代理模式
  - [ ] userspace代理模式
- [ ] KubeProxy和CNI插件的关系？
- [ ] K8S单协议栈和双协议栈有何区别？
- [ ] 扩展点？


## 2.6. Kubectl

## 2.7. CodeGenerator

- [ ] K8S有哪些标签？每种标签的作用？


## 2.8. ContainerD

- [ ] RMI接口？


# 3. FeatureGate

- [ ] AnyVolumeDataSource
- [ ] AppArmor
- [ ] CPUCFSQuotaPeriod
- [ ] CPUManager
- [ ] CPUManagerPolicyAlphaOptions
- [ ] CPUManagerPolicyBetaOptions
- [ ] CPUManagerPolicyOptions
- [ ] CSIInlineVolume
- [ ] CSIMigration
- [ ] CSIMigrationAWS
- [ ] CSIMigrationAzureDisk
- [ ] CSIMigrationAzureFile
- [ ] CSIMigrationGCE
- [ ] CSIMigrationOpenStack
- [ ] CSIMigrationPortworx
- [ ] CSIMigrationRBD
- [ ] CSIMigrationvSphere
- [ ] CSINodeExpandSecret
- [ ] CSIStorageCapacity
- [ ] CSIVolumeHealth
- [ ] CSRDuration
- [ ] ContainerCheckpoint
- [ ] ControllerManagerLeaderMigration
- [ ] CronJobTimeZone
- [ ] DaemonSetUpdateSurge
- [ ] DefaultPodTopologySpread
- [ ] DelegateFSGroupToCSIDriver
- [ ] DevicePlugins
- [ ] DisableAcceleratorUsageMetrics
- [ ] DisableCloudProviders
- [ ] DisableKubeletCloudCredentialProviders
- [ ] DownwardAPIHugePages
- [ ] DynamicKubeletConfig
- [ ] EndpointSliceTerminatingCondition
- [ ] EphemeralContainers
- [ ] ExecProbeTimeout
- [ ] ExpandCSIVolumes
- [ ] ExpandInUsePersistentVolumes
- [ ] ExpandPersistentVolumes
- [ ] ExpandedDNSConfig
- [ ] ExperimentalHostUserNamespaceDefaultingGate
- [ ] GRPCContainerProbe
- [ ] GracefulNodeShutdown
- [ ] GracefulNodeShutdownBasedOnPodPriority
- [ ] HPAContainerMetrics
- [ ] HPAScaleToZero
- [ ] HonorPVReclaimPolicy
- [ ] IdentifyPodOS
- [ ] InTreePluginAWSUnregister
- [ ] InTreePluginAzureDiskUnregister
- [ ] InTreePluginAzureFileUnregister
- [ ] InTreePluginGCEUnregister
- [ ] InTreePluginOpenStackUnregister
- [ ] InTreePluginPortworxUnregister
- [ ] InTreePluginRBDUnregister
- [ ] InTreePluginvSphereUnregister
- [ ] IndexedJob
- [ ] IPTablesOwnershipCleanup
- [ ] JobPodFailurePolicy
- [ ] JobMutableNodeSchedulingDirectives
- [ ] JobReadyPods
- [ ] JobTrackingWithFinalizers
- [ ] KubeletCredentialProviders
- [ ] KubeletInUserNamespace
- [ ] KubeletPodResources
- [ ] KubeletPodResourcesGetAllocatable
- [ ] KubeletTracing  分布式链路追踪特性开关
- [ ] LegacyServiceAccountTokenNoAutoGeneration
- [ ] LocalStorageCapacityIsolation
- [ ] LocalStorageCapacityIsolationFSQuotaMonitoring
- [ ] LogarithmicScaleDown
- [ ] MatchLabelKeysInPodTopologySpread
- [ ] MaxUnavailableStatefulSet
- [ ] MemoryManager
- [ ] MemoryQoS
- [ ] MinDomainsInPodTopologySpread
- [ ] MixedProtocolLBService
- [ ] MultiCIDRRangeAllocator
- [ ] NetworkPolicyEndPort
- [ ] NetworkPolicyStatus
- [ ] NodeOutOfServiceVolumeDetach
- [ ] NodeSwap
- [ ] NonPreemptingPriority
- [ ] PodAffinityNamespaceSelector
- [ ] PodAndContainerStatsFromCRI
- [ ] PodDeletionCost
- [ ] PodDisruptionConditions
- [ ] PodHasNetworkCondition
- [ ] PodOverhead
- [ ] PodSecurity
- [ ] PreferNominatedNode
- [ ] ProbeTerminationGracePeriod
- [ ] ProcMountType
- [ ] ProxyTerminatingEndpoints
- [ ] QOSReserved
- [ ] ReadWriteOncePod
- [ ] RecoverVolumeExpansionFailure
- [ ] RetroactiveDefaultStorageClass
- [ ] RotateKubeletServerCertificate
- [ ] SeccompDefault
- [ ] ServiceInternalTrafficPolicy
- [ ] ServiceIPStaticSubrange
- [ ] ServiceLBNodePortControl
- [ ] ServiceLoadBalancerClass
- [ ] SizeMemoryBackedVolumes
- [ ] StatefulSetAutoDeletePVC
- [ ] StatefulSetMinReadySeconds
- [ ] SuspendJob
- [ ] TopologyAwareHints
- [ ] TopologyManager
- [ ] UserNamespacesStatelessPodsSupport
- [ ] VolumeCapacityPriority
- [ ] WinDSR
- [ ] WinOverlay
- [ ] WindowsHostProcessContainers
- [ ] NodeInclusionPolicyInPodTopologySpread
- [ ] SELinuxMountReadWriteOncePod


# 4. QA
## 4.1. Ingress原理

## 4.2. NetworkPolicy原理
## 4.3. PodSecurityPolicy原理
## 4.4. PodSecurityAdmin原理
## 4.5. SessionAffinity原理
## 4.6. DNS原理？CoreDNS?
## 4.7. CSI
## 4.8. Pod Disruption Budget
## 4.9. 学习K8S对于分布式链路追踪的使用 OpenTelemetry
## 4.10. CFS Quota
## 4.11. CGroup