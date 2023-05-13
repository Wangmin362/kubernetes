# QA
## APIServer
- [ ] APIServer认证
  - [ ] APIServer有几种认证方式？每种认证方式的原理是啥？
  - [ ] APIServer如何自定义扩展认证方式？
  - [ ] 影响APIServer的认证参数有哪些？
  - [ ] Pod通过ServiceAccount认证原理？
  - [ ] APIServer的Token认证原理？
  - [ ] TLS Bootstrap Token认证原理？ 为什么需要TLS Bootstrap Token认证? 这种认证方式解决了什么问题？
  - [ ] RequestHeader认证原理？
- [ ] APIServer鉴权
  - [ ] APIServer有几种鉴权方式？每种鉴权方式的原理是啥？
  - [ ] APIServer如何自定义扩展鉴权方式？
  - [ ] 影响APIServer的鉴权参数有哪些？
- [ ] APIServer准入控制
  - [ ] APIServer有集中准入控制插件？每种准入控制插件的原理是啥？
  - [ ] MutatingWebhook和ValidationWebhook准入控制插件的原理？
  - [ ] K8S动态准入控制 (Dynamic Webhook)原理是啥？
  - [ ] 如何理解K8S准入控制架构设计？
  - [ ] 如何自定义准入控制插件？
  - [ ] 影响准入控制插件参数有哪些？
  - [ ] PreHook原理
- [ ] APIServer审计
  - [ ] K8S的审计架构？
  - [ ] 用户如何自定义审计后端(Audit Backend)？
- [ ] APIServer扩展点
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
- [ ] K8S是如何解决跨域问题的（CORS）？
- [ ] 如何理解K8S的资源与子资源？
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
  - [ ] --watch-cache
  - [ ] --watch-cache-sizes
  - [ ] --goaway-chance 有何作用？为啥需要？原理是啥？从中可以学到什么？
  - [ ] --service-cluster-ip-range
  - [ ] --service-node-port-range
- [ ] FeatureGate
  - [ ] 特性开关有哪些？每个开关有何影响？
- [ ] 指标
  - [ ] APIServer暴露了哪些指标，每种指标代表了什么含义？

## ControllerManager

- [ ] Ingress原理? K8S Ingress架构？
- [ ] SessionAffinity原理
- [ ] CSI插件
- [ ] Plugin插件原理？

## Kubelet

- [ ] PLEG原理？PLEG有无性能问题？
- [ ] Kubelet是如何调用CRI的？
- [ ] CPU Typology?

## KubeScheduler

- [ ] K8S调度架构
- [ ] Scheduler扩展点
- [ ] 如何自定义调度器？

## KubeProxy

- [ ] K8S支持几种代理模式？每种代理模式的原理是啥？
  - [ ] ipvs代理模式
  - [ ] iptable代理模式
  - [ ] userspace代理模式
- [ ] KubeProxy和CNI插件的关系？
- [ ] K8S单协议栈和双协议栈有何区别？
- [ ] 扩展点？

## CodeGenerator

- [ ] K8S有哪些标签？每种标签的作用？


## ContainerD

- [ ] RMI接口？