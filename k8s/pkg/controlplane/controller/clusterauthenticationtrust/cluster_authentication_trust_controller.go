/*
Copyright 2019 The Kubernetes Authors.

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

package clusterauthenticationtrust

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/authentication/request/headerrequest"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	corev1informers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/cert"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const (
	// TODO 为什么只关心kube-system这个名称空间
	configMapNamespace = "kube-system"
	// 这个ConfigMap应该需要包含 ClusterAuthenticationInfo 相关信息
	configMapName = "extension-apiserver-authentication"
)

// TODO 正确格式的 extension-apiserver-authentication configmap的结构如下
/*
apiVersion: v1
kind: ConfigMap
metadata:
  name: extension-apiserver-authentication
  namespace: kube-system
data:
  client-ca-file: |
    -----BEGIN CERTIFICATE-----
    MIIC6TCCAdGgAwIBAgIBADANBgkqhkiG9w0BAQsFADAVMRMwEQYDVQQDEwprdWJl
    cm5ldGVzMCAXDTIyMTExMjAyNTU0MVoYDzIwNTIxMTA0MDI1NTQxWjAVMRMwEQYD
    VQQDEwprdWJlcm5ldGVzMIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA
    0N01OXM5sLzX2DIKL01NhHHmV0Qxb2fbW5UgY/B/VBNTBDsLTMjaNm77Icfu8QQP
    dygUS/MoDW8QrLOjE3J8PJWQYi6MjOfO5+yownZeqMSzwry/TxjcimiHCkdxC7tV
    3g9swXQ27dUOmTUKHjMtp+sBosXx0DxEzomSdGaZikMmZZy2gNuuQMroT7IeKlah
    KQnURkIihmf6dqQ3e9mQ91a/iE2Col93lnbyW5k46aUB3EKNQJLOA8pLOtZx2a1Q
    LJa1nbIMiOxyerlFpX/QyDal3I4R2UKpk0gS0jDPrA/E4dMJ9gqdCGr9fjWO58dg
    v81rGc6t8RBXpm+VBus87QIDAQABo0IwQDAOBgNVHQ8BAf8EBAMCAqQwDwYDVR0T
    AQH/BAUwAwEB/zAdBgNVHQ4EFgQUPtL0lWN4kzK6G7VW2SSt7FK4VSMwDQYJKoZI
    hvcNAQELBQADggEBAK7P+jngUWqFN8625gfIsCH2+bPtXx55/e2Dvt7Zw8lTgPdc
    mr7GnGTLjBsumzq54Wp3dd7cfBO66tCkKHsAzuxX4CQVJ+oDZMRLPfCGTR4pONz/
    DPCtWtZVXdl+ZNOiKCJu50yq47rnFCH8D/S+nADmAybYe7WJxGs69Mgd1qhlDEPU
    9AnUq6EtlaG3GNxWnixiRgqQ/lg/WluqzVodB0n+IHzjIg804v1EVpIwi2ZbXA+B
    KF2wo0KCmCSO7cbbDouhIQ6xiMVpin4fNgMGVe5LB5hQ46vQDFaRfRwG5MvcOjRQ
    /10gow5ZF5MBnQK+4CN5Rus6QmWsc2Xm+6cCLZM=
    -----END CERTIFICATE-----
  requestheader-allowed-names: '["front-proxy-client"]'
  requestheader-client-ca-file: |
    -----BEGIN CERTIFICATE-----
    MIIC8TCCAdmgAwIBAgIBADANBgkqhkiG9w0BAQsFADAZMRcwFQYDVQQDEw5mcm9u
    dC1wcm94eS1jYTAgFw0yMjExMTIwMjU1NDJaGA8yMDUyMTEwNDAyNTU0MlowGTEX
    MBUGA1UEAxMOZnJvbnQtcHJveHktY2EwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAw
    ggEKAoIBAQDmdPgeQBl2pe9IsQbgjPiNZqY+XrNuPqfxKw01eByngQAmCxH7lF0u
    esQIodHlsY8pEtVh+p/wsCriG8FwBUnwwHNauYoD/Zd5pLXFn9kPY2HuJV1SDaht
    Yw0saC2OWnECZjUa4uX9PUr74kZlW2JfmV2mR7MsfqquJU2RMA0+PoUJK4kXVSKe
    dLk8MvoHWdhJ/WIuHO7R/z0Tulx5aHUbJEUSgwazWrtWaid0tb1HujueMPaQ5At9
    DU8nTPfDFtW20KJ7w/7ff+aOPWKvbfnSZo/YEepr7P2p+Ihx8fQqh4GhplSF2Tv0
    aLmgpMN/UzGRJTfliC3cNU7XLjvPK1ULAgMBAAGjQjBAMA4GA1UdDwEB/wQEAwIC
    pDAPBgNVHRMBAf8EBTADAQH/MB0GA1UdDgQWBBRbYNFZT9LyhWMLJjuw3EBOXaXT
    hzANBgkqhkiG9w0BAQsFAAOCAQEAjTSGphKU9K4gdgC/VAu5p19PV3+cFRL3k/Mi
    vtl8j65nvGXhkYqfgL4VnnPd+v2Wbi+7UPv11wzKy5Qag1djYmN0/BYFy83bgawr
    dBj3iV+IJXe+1PDfrmQxeQFh2ythaCTnymVL1vTdCytMnZdO1vfvT5ixSzeS+H8g
    CFNQTw8QuOPrJWnA6nFst9T1Fa2qQYvJx+Muzh69oloM2nxNqLxoLa1X/9VeI3xu
    RFK+tS2NuWTRKCCPsOnVvziymPUigVm2gfUeqoLyvFtd9LI7vKyLOY5oHo/4MA0U
    aE3/sFki0hwCF7Oc4oxFqp/l78OkaiEvnDgS9uuKmZQGT5EQmQ==
    -----END CERTIFICATE-----
  requestheader-extra-headers-prefix: '["X-Remote-Extra-"]'
  requestheader-group-headers: '["X-Remote-Group"]'
  requestheader-username-headers: '["X-Remote-User"]'
*/

// Controller holds the running state for the controller
type Controller struct {
	// 这些信息是在apiserver启动的时候配置的
	requiredAuthenticationData ClusterAuthenticationInfo

	configMapLister corev1listers.ConfigMapLister
	configMapClient corev1client.ConfigMapsGetter
	namespaceClient corev1client.NamespacesGetter

	// queue is where incoming work is placed to de-dup and to allow "easy" rate limited requeues on errors.
	// we only ever place one entry in here, but it is keyed as usual: namespace/name
	// TODO 这个queue中保存的是啥？
	queue workqueue.RateLimitingInterface

	// kubeSystemConfigMapInformer is tracked so that we can start these on Run
	// ConfigMap缓存
	kubeSystemConfigMapInformer cache.SharedIndexInformer

	// preRunCaches are the caches to sync before starting the work of this control loop
	preRunCaches []cache.InformerSynced
}

// ClusterAuthenticationInfo holds the information that will included in public configmap.
// TODO 认证信息这里相当重要，是理解apiserver认证的重要依据
// TODO 这里的认证信息应该适合K8S的认证相关
type ClusterAuthenticationInfo struct {
	// ClientCA is the CA that can be used to verify the identity of normal clients
	ClientCA dynamiccertificates.CAContentProvider

	// RequestHeaderUsernameHeaders are the headers used by this kube-apiserver to determine username
	RequestHeaderUsernameHeaders headerrequest.StringSliceProvider
	// RequestHeaderGroupHeaders are the headers used by this kube-apiserver to determine groups
	RequestHeaderGroupHeaders headerrequest.StringSliceProvider
	// RequestHeaderExtraHeaderPrefixes are the headers used by this kube-apiserver to determine user.extra
	RequestHeaderExtraHeaderPrefixes headerrequest.StringSliceProvider
	// RequestHeaderAllowedNames are the sujbects allowed to act as a front proxy
	RequestHeaderAllowedNames headerrequest.StringSliceProvider
	// RequestHeaderCA is the CA that can be used to verify the front proxy
	RequestHeaderCA dynamiccertificates.CAContentProvider
}

// NewClusterAuthenticationTrustController returns a controller that will maintain the kube-system configmap/extension-apiserver-authentication
// that holds information about how to aggregated apiservers are recommended (but not required) to configure themselves.
func NewClusterAuthenticationTrustController(requiredAuthenticationData ClusterAuthenticationInfo, kubeClient kubernetes.Interface) *Controller {
	// we construct our own informer because we need such a small subset of the information available.  Just one namespace.
	// TODO 为什么只关心kube-system这个名称空间？
	kubeSystemConfigMapInformer := corev1informers.NewConfigMapInformer(kubeClient, configMapNamespace, 12*time.Hour,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})

	c := &Controller{
		requiredAuthenticationData:  requiredAuthenticationData,
		configMapLister:             corev1listers.NewConfigMapLister(kubeSystemConfigMapInformer.GetIndexer()),
		configMapClient:             kubeClient.CoreV1(),
		namespaceClient:             kubeClient.CoreV1(),
		queue:                       workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "cluster_authentication_trust_controller"),
		preRunCaches:                []cache.InformerSynced{kubeSystemConfigMapInformer.HasSynced},
		kubeSystemConfigMapInformer: kubeSystemConfigMapInformer,
	}

	kubeSystemConfigMapInformer.AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			// TODO 为什么只关心extension-apiserver-authentication这个名字的configmap?
			if cast, ok := obj.(*corev1.ConfigMap); ok {
				return cast.Name == configMapName
			}
			if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				if cast, ok := tombstone.Obj.(*corev1.ConfigMap); ok {
					return cast.Name == configMapName
				}
			}
			// TODO 难道还有可能到达这里，应该不可能到达这里吧
			return true // always return true just in case.  The checks are fairly cheap
		},
		Handler: cache.ResourceEventHandlerFuncs{
			// we have a filter, so any time we're called, we may as well queue. We only ever check one configmap
			// so we don't have to be choosy about our key.
			AddFunc: func(obj interface{}) {
				c.queue.Add(keyFn())
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				c.queue.Add(keyFn())
			},
			DeleteFunc: func(obj interface{}) {
				c.queue.Add(keyFn())
			},
		},
	})

	return c
}

func (c *Controller) syncConfigMap() error {
	// 通过configmap缓存查询
	originalAuthConfigMap, err := c.configMapLister.ConfigMaps(configMapNamespace).Get(configMapName)
	if apierrors.IsNotFound(err) {
		originalAuthConfigMap = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: configMapNamespace, Name: configMapName},
		}
	} else if err != nil {
		return err
	}
	// keep the original to diff against later before updating
	authConfigMap := originalAuthConfigMap.DeepCopy()

	// 从configmap中解析出ClusterAuthenticationInfo数据
	existingAuthenticationInfo, err := getClusterAuthenticationInfoFor(originalAuthConfigMap.Data)
	if err != nil {
		return err
	}
	combinedInfo, err := combinedClusterAuthenticationInfo(existingAuthenticationInfo, c.requiredAuthenticationData)
	if err != nil {
		return err
	}
	authConfigMap.Data, err = getConfigMapDataFor(combinedInfo)
	if err != nil {
		return err
	}

	if equality.Semantic.DeepEqual(authConfigMap, originalAuthConfigMap) {
		klog.V(5).Info("no changes to configmap")
		return nil
	}
	klog.V(2).Infof("writing updated authentication info to  %s configmaps/%s", configMapNamespace, configMapName)

	if err := createNamespaceIfNeeded(c.namespaceClient, authConfigMap.Namespace); err != nil {
		return err
	}
	// 更新Configmap
	if err := writeConfigMap(c.configMapClient, authConfigMap); err != nil {
		return err
	}

	return nil
}

func writeConfigMap(configMapClient corev1client.ConfigMapsGetter, required *corev1.ConfigMap) error {
	_, err := configMapClient.ConfigMaps(required.Namespace).Update(context.TODO(), required, metav1.UpdateOptions{})
	if apierrors.IsNotFound(err) {
		_, err := configMapClient.ConfigMaps(required.Namespace).Create(context.TODO(), required, metav1.CreateOptions{})
		return err
	}

	// If the configmap is too big, clear the entire thing and count on this controller (or another one) to add the correct data back.
	// We return the original error which causes the controller to re-queue.
	// Too big means
	//   1. request is so big the generic request catcher finds it
	//   2. the content is so large that that the server sends a validation error "Too long: must have at most 1048576 characters"
	if apierrors.IsRequestEntityTooLargeError(err) || (apierrors.IsInvalid(err) && strings.Contains(err.Error(), "Too long")) {
		if deleteErr := configMapClient.ConfigMaps(required.Namespace).Delete(context.TODO(), required.Name, metav1.DeleteOptions{}); deleteErr != nil {
			return deleteErr
		}
		return err
	}

	return err
}

func createNamespaceIfNeeded(nsClient corev1client.NamespacesGetter, ns string) error {
	if _, err := nsClient.Namespaces().Get(context.TODO(), ns, metav1.GetOptions{}); err == nil {
		// the namespace already exists
		return nil
	}
	newNs := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ns,
			Namespace: "",
		},
	}
	_, err := nsClient.Namespaces().Create(context.TODO(), newNs, metav1.CreateOptions{})
	if err != nil && apierrors.IsAlreadyExists(err) {
		err = nil
	}
	return err
}

// combinedClusterAuthenticationInfo combines two sets of authentication information into a new one
func combinedClusterAuthenticationInfo(lhs, rhs ClusterAuthenticationInfo) (ClusterAuthenticationInfo, error) {
	ret := ClusterAuthenticationInfo{
		RequestHeaderAllowedNames:        combineUniqueStringSlices(lhs.RequestHeaderAllowedNames, rhs.RequestHeaderAllowedNames),
		RequestHeaderExtraHeaderPrefixes: combineUniqueStringSlices(lhs.RequestHeaderExtraHeaderPrefixes, rhs.RequestHeaderExtraHeaderPrefixes),
		RequestHeaderGroupHeaders:        combineUniqueStringSlices(lhs.RequestHeaderGroupHeaders, rhs.RequestHeaderGroupHeaders),
		RequestHeaderUsernameHeaders:     combineUniqueStringSlices(lhs.RequestHeaderUsernameHeaders, rhs.RequestHeaderUsernameHeaders),
	}

	var err error
	ret.ClientCA, err = combineCertLists(lhs.ClientCA, rhs.ClientCA)
	if err != nil {
		return ClusterAuthenticationInfo{}, err
	}
	ret.RequestHeaderCA, err = combineCertLists(lhs.RequestHeaderCA, rhs.RequestHeaderCA)
	if err != nil {
		return ClusterAuthenticationInfo{}, err
	}

	return ret, nil
}

func getConfigMapDataFor(authenticationInfo ClusterAuthenticationInfo) (map[string]string, error) {
	data := map[string]string{}
	if authenticationInfo.ClientCA != nil {
		if caBytes := authenticationInfo.ClientCA.CurrentCABundleContent(); len(caBytes) > 0 {
			data["client-ca-file"] = string(caBytes)
		}
	}

	if authenticationInfo.RequestHeaderCA == nil {
		return data, nil
	}

	if caBytes := authenticationInfo.RequestHeaderCA.CurrentCABundleContent(); len(caBytes) > 0 {
		var err error

		// encoding errors aren't going to get better, so just fail on them.
		data["requestheader-username-headers"], err = jsonSerializeStringSlice(authenticationInfo.RequestHeaderUsernameHeaders.Value())
		if err != nil {
			return nil, err
		}
		data["requestheader-group-headers"], err = jsonSerializeStringSlice(authenticationInfo.RequestHeaderGroupHeaders.Value())
		if err != nil {
			return nil, err
		}
		data["requestheader-extra-headers-prefix"], err = jsonSerializeStringSlice(authenticationInfo.RequestHeaderExtraHeaderPrefixes.Value())
		if err != nil {
			return nil, err
		}

		data["requestheader-client-ca-file"] = string(caBytes)
		data["requestheader-allowed-names"], err = jsonSerializeStringSlice(authenticationInfo.RequestHeaderAllowedNames.Value())
		if err != nil {
			return nil, err
		}
	}

	return data, nil
}

func getClusterAuthenticationInfoFor(data map[string]string) (ClusterAuthenticationInfo, error) {
	ret := ClusterAuthenticationInfo{}

	var err error
	ret.RequestHeaderGroupHeaders, err = jsonDeserializeStringSlice(data["requestheader-group-headers"])
	if err != nil {
		return ClusterAuthenticationInfo{}, err
	}
	ret.RequestHeaderExtraHeaderPrefixes, err = jsonDeserializeStringSlice(data["requestheader-extra-headers-prefix"])
	if err != nil {
		return ClusterAuthenticationInfo{}, err
	}
	ret.RequestHeaderAllowedNames, err = jsonDeserializeStringSlice(data["requestheader-allowed-names"])
	if err != nil {
		return ClusterAuthenticationInfo{}, err
	}
	ret.RequestHeaderUsernameHeaders, err = jsonDeserializeStringSlice(data["requestheader-username-headers"])
	if err != nil {
		return ClusterAuthenticationInfo{}, err
	}

	if caBundle := data["requestheader-client-ca-file"]; len(caBundle) > 0 {
		ret.RequestHeaderCA, err = dynamiccertificates.NewStaticCAContent("existing", []byte(caBundle))
		if err != nil {
			return ClusterAuthenticationInfo{}, err
		}
	}

	if caBundle := data["client-ca-file"]; len(caBundle) > 0 {
		ret.ClientCA, err = dynamiccertificates.NewStaticCAContent("existing", []byte(caBundle))
		if err != nil {
			return ClusterAuthenticationInfo{}, err
		}
	}

	return ret, nil
}

func jsonSerializeStringSlice(in []string) (string, error) {
	out, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	return string(out), err
}

func jsonDeserializeStringSlice(in string) (headerrequest.StringSliceProvider, error) {
	if len(in) == 0 {
		return nil, nil
	}

	out := []string{}
	if err := json.Unmarshal([]byte(in), &out); err != nil {
		return nil, err
	}
	return headerrequest.StaticStringSlice(out), nil
}

func combineUniqueStringSlices(lhs, rhs headerrequest.StringSliceProvider) headerrequest.StringSliceProvider {
	ret := []string{}
	present := sets.String{}

	if lhs != nil {
		for _, curr := range lhs.Value() {
			if present.Has(curr) {
				continue
			}
			ret = append(ret, curr)
			present.Insert(curr)
		}
	}

	if rhs != nil {
		for _, curr := range rhs.Value() {
			if present.Has(curr) {
				continue
			}
			ret = append(ret, curr)
			present.Insert(curr)
		}
	}

	return headerrequest.StaticStringSlice(ret)
}

func combineCertLists(lhs, rhs dynamiccertificates.CAContentProvider) (dynamiccertificates.CAContentProvider, error) {
	certificates := []*x509.Certificate{}

	if lhs != nil {
		lhsCABytes := lhs.CurrentCABundleContent()
		lhsCAs, err := cert.ParseCertsPEM(lhsCABytes)
		if err != nil {
			return nil, err
		}
		certificates = append(certificates, lhsCAs...)
	}
	if rhs != nil {
		rhsCABytes := rhs.CurrentCABundleContent()
		rhsCAs, err := cert.ParseCertsPEM(rhsCABytes)
		if err != nil {
			return nil, err
		}
		certificates = append(certificates, rhsCAs...)
	}

	certificates = filterExpiredCerts(certificates...)

	finalCertificates := []*x509.Certificate{}
	// now check for duplicates. n^2, but super simple
	for i := range certificates {
		found := false
		for j := range finalCertificates {
			if reflect.DeepEqual(certificates[i].Raw, finalCertificates[j].Raw) {
				found = true
				break
			}
		}
		if !found {
			finalCertificates = append(finalCertificates, certificates[i])
		}
	}

	finalCABytes, err := encodeCertificates(finalCertificates...)
	if err != nil {
		return nil, err
	}

	if len(finalCABytes) == 0 {
		return nil, nil
	}
	// it makes sense for this list to be static because the combination of sources is only used just before writing and
	// is recalculated
	return dynamiccertificates.NewStaticCAContent("combined", finalCABytes)
}

// filterExpiredCerts checks are all certificates in the bundle valid, i.e. they have not expired.
// The function returns new bundle with only valid certificates or error if no valid certificate is found.
// We allow five minutes of slack for NotAfter comparisons
func filterExpiredCerts(certs ...*x509.Certificate) []*x509.Certificate {
	fiveMinutesAgo := time.Now().Add(-5 * time.Minute)

	var validCerts []*x509.Certificate
	for _, c := range certs {
		if c.NotAfter.After(fiveMinutesAgo) {
			validCerts = append(validCerts, c)
		}
	}

	return validCerts
}

// Enqueue a method to allow separate control loops to cause the controller to trigger and reconcile content.
func (c *Controller) Enqueue() {
	c.queue.Add(keyFn())
}

// Run the controller until stopped.
func (c *Controller) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	// make sure the work queue is shutdown which will trigger workers to end
	defer c.queue.ShutDown()

	klog.Infof("Starting cluster_authentication_trust_controller controller")
	defer klog.Infof("Shutting down cluster_authentication_trust_controller controller")

	// we have a personal informer that is narrowly scoped, start it.
	// 启动informer，同步apiserver中的数据
	go c.kubeSystemConfigMapInformer.Run(ctx.Done())

	// wait for your secondary caches to fill before starting your work
	// 等待configmap同步完成
	if !cache.WaitForNamedCacheSync("cluster_authentication_trust_controller", ctx.Done(), c.preRunCaches...) {
		return
	}

	// only run one worker
	// 消费workqueue
	go wait.Until(c.runWorker, time.Second, ctx.Done())

	// checks are cheap.  run once a minute just to be sure we stay in sync in case fsnotify fails again
	// start timer that rechecks every minute, just in case.  this also serves to prime the controller quickly.
	_ = wait.PollImmediateUntil(1*time.Minute, func() (bool, error) {
		c.queue.Add(keyFn())
		return false, nil
	}, ctx.Done())

	// wait until we're told to stop
	<-ctx.Done()
}

func (c *Controller) runWorker() {
	// hot loop until we're told to stop.  processNextWorkItem will automatically wait until there's work
	// available, so we don't worry about secondary waits
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem deals with one key off the queue.  It returns false when it's time to quit.
func (c *Controller) processNextWorkItem() bool {
	// pull the next work item from queue.  It should be a key we use to lookup something in a cache
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	// you always have to indicate to the queue that you've completed a piece of work
	defer c.queue.Done(key)

	// do your work on the key.  This method will contains your "do stuff" logic
	err := c.syncConfigMap()
	if err == nil {
		// if you had no error, tell the queue to stop tracking history for your key.  This will
		// reset things like failure counts for per-item rate limiting
		c.queue.Forget(key) // 看来workqueue的使用姿势，在没有出错的情况下，必须要执行Forget才行
		return true
	}

	// there was a failure so be sure to report it.  This method allows for pluggable error handling
	// which can be used for things like cluster-monitoring
	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", key, err))
	// since we failed, we should requeue the item to work on later.  This method will add a backoff
	// to avoid hotlooping on particular items (they're probably still not going to work right away)
	// and overall controller protection (everything I've done is broken, this controller needs to
	// calm down or it can starve other useful work) cases.
	c.queue.AddRateLimited(key)

	return true
}

func keyFn() string {
	// this format matches DeletionHandlingMetaNamespaceKeyFunc for our single key
	return configMapNamespace + "/" + configMapName
}

func encodeCertificates(certs ...*x509.Certificate) ([]byte, error) {
	b := bytes.Buffer{}
	for _, cert := range certs {
		if err := pem.Encode(&b, &pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw}); err != nil {
			return []byte{}, err
		}
	}
	return b.Bytes(), nil
}
