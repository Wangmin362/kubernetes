/*
Copyright 2020 The Kubernetes Authors.

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

package headerrequest

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	"k8s.io/client-go/kubernetes"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sync/atomic"
)

const (
	authenticationRoleName = "extension-apiserver-authentication-reader"
)

// RequestHeaderAuthRequestProvider a provider that knows how to dynamically fill parts of RequestHeaderConfig struct
/*
apiVersion: v1
kind: ConfigMap
metadata:
  name: extension-apiserver-authentication
  namespace: kube-system
data:
  client-ca-file: |
    -----BEGIN CERTIFICATE-----
    MIIDBTCCAe2gAwIBAgIIElEi2wpcXo8wDQYJKoZIhvcNAQELBQAwFTETMBEGA1UE
    AxMKa3ViZXJuZXRlczAeFw0yMzA4MjUwOTI5MThaFw0zMzA4MjIwOTI5MThaMBUx
    EzARBgNVBAMTCmt1YmVybmV0ZXMwggEiMA0GCSqGSIb3DQEBAQUAA4IBDwAwggEK
    AoIBAQCaaq0vBj5Bex9KR7wtMhxFSJkgk3QOBS4b8RfZxEgyOCtvy8vJvYuph1uX
    9lpqvkk+2Xl9/61VOwQO6L7+wG7OCt/8W1cvXaRuggGPMRescWh9OFGd5sprpOo+
    4R8ozAfMFOYm5DX+5ZYeL4S/ly7N/QxnbT2pGt1UyApuR4pFBZQcXETOY7Mj4UZo
    03W3ImX+IPnwj6vRcjIIxlFrN3bAfgVWnVq3l/ecfkVfP8proNHkIEStuqh4nsjN
    yCOfMkxxxbvmAUjO1eHDZfZnFqhcoVusuX1PTEmPctpPsxS6pP9SH1nLJEfWGfUn
    ofU/4XjgwkjZ8MuYqUB6d8cA27y1AgMBAAGjWTBXMA4GA1UdDwEB/wQEAwICpDAP
    BgNVHRMBAf8EBTADAQH/MB0GA1UdDgQWBBTiCmKJCLYwjaia4CS2pRoIdTX4EjAV
    BgNVHREEDjAMggprdWJlcm5ldGVzMA0GCSqGSIb3DQEBCwUAA4IBAQALuhb14Urp
    akdtGybb//cg95nvCuNjRWIYMZxTRDbHk+rX8kEcqz8yUEpySKNk8yPYw1u0x6rs
    +pQVRFNGlAeQ8tmR2cEV/zx79ycMRGTHhSxUjlCfPR0lNIeoZVWJGG6xk6t8fG92
    UUsItj5EMF95TeyL/22QMHtfabazdL+9Poaj/tsUkSAbpfEggBqp/0s0RYZ/2B4y
    N4BzZrk0f3iMG16yy6VXgxXp/7Oh+38hFAS8S50xb2gPnD6qyd+XY1whvoGJj6J1
    RoZqpSbjsMLb7SwEizsiBVVjaB8ZcLZIvKU9vxJ7TY1aNNAH3tOWR51FxP5lqP+k
    2h92OSiUN2y3
    -----END CERTIFICATE-----
  requestheader-allowed-names: '["front-proxy-client"]'
  requestheader-client-ca-file: |
    -----BEGIN CERTIFICATE-----
    MIIDETCCAfmgAwIBAgIIfFrMomj/o5IwDQYJKoZIhvcNAQELBQAwGTEXMBUGA1UE
    AxMOZnJvbnQtcHJveHktY2EwHhcNMjMwODI1MDkyOTE4WhcNMzMwODIyMDkyOTE4
    WjAZMRcwFQYDVQQDEw5mcm9udC1wcm94eS1jYTCCASIwDQYJKoZIhvcNAQEBBQAD
    ggEPADCCAQoCggEBAMuSozjmyTkhSLsZqmEXgUrqfJ/W1lYvmibEamqCerxfkVKF
    0LhQNRWA/GmHN6DJvstnlZrPuNKJI6/uwWBudlS5foyaFtrLPTltnAL1pV+4ykOs
    rKLiqBU90Ael3CIpWXpfEuqf1R22zyRhAq9MFFwKsCx2VaDlF8vawOA9yc6hTUzi
    jk8mdM6Rl6a5xTQ1lSNEXMMMqZk1PviXjB/0J6yAfvjOhoAcA87camUlMpH9l83l
    E5aLQrmmkXmPnXHowJAGpGc/qpicMcf9/4YppsQd6HvNhQ50tLTLHsnlnqJY6NAC
    wdaLA4rHvaaaUlClCaOLCswPxK7+5H3Xm25HlTUCAwEAAaNdMFswDgYDVR0PAQH/
    BAQDAgKkMA8GA1UdEwEB/wQFMAMBAf8wHQYDVR0OBBYEFKqCsu0Kf+y/eD/ptdaC
    CgrAhtbMMBkGA1UdEQQSMBCCDmZyb250LXByb3h5LWNhMA0GCSqGSIb3DQEBCwUA
    A4IBAQBmh0z7P9L0XASmW+hL5Mx7VF4HtwUTOiLngEeahKXdhH5EDLWVO6hYEHa2
    ccZWBZJGRLWhHFZrc5UO+gaGqTjKlsyGrLeYLnLHaQSH7CJFnLOiWOE4sPJeT2LB
    2zAV1z4LYdEzYawsSOzRVeLHG56+asH0kDY+XuZSZwFwhoxa8mtFZJhMo1mxVz6U
    lHtaBaODFwXg5eX1uEVcsS7ctdVAZwuvdfpioYS7S0Gc/m98VJwhV9iGnWTMJ4Tc
    A1ogHZa/sW7oV+XvF55oZcMxhOrmrg28jwp4KEkez4HxLm6t2sfiZw/HnJCLi/FH
    Wb+qXAw0P6znLTih7C9QbguQfgkv
    -----END CERTIFICATE-----
  requestheader-extra-headers-prefix: '["X-Remote-Extra-"]'
  requestheader-group-headers: '["X-Remote-Group"]'
  requestheader-username-headers: '["X-Remote-User"]'
*/
// 用于获取代理认证方式的各种请求头设置信息
type RequestHeaderAuthRequestProvider interface {
	UsernameHeaders() []string
	GroupHeaders() []string
	ExtraHeaderPrefixes() []string
	AllowedClientNames() []string
}

var _ RequestHeaderAuthRequestProvider = &RequestHeaderAuthRequestController{}

type requestHeaderBundle struct {
	UsernameHeaders     []string
	GroupHeaders        []string
	ExtraHeaderPrefixes []string
	AllowedClientNames  []string
}

// RequestHeaderAuthRequestController a controller that exposes a set of methods for dynamically filling parts of RequestHeaderConfig struct.
// The methods are sourced from the config map which is being monitored by this controller.
// The controller is primed from the server at the construction time for components that don't want to dynamically react to changes
// in the config map.
type RequestHeaderAuthRequestController struct {
	name string

	configmapName      string
	configmapNamespace string

	client                  kubernetes.Interface
	configmapLister         corev1listers.ConfigMapNamespaceLister
	configmapInformer       cache.SharedIndexInformer
	configmapInformerSynced cache.InformerSynced

	queue workqueue.RateLimitingInterface

	// exportedRequestHeaderBundle is a requestHeaderBundle that contains the last read, non-zero length content of the configmap
	exportedRequestHeaderBundle atomic.Value

	usernameHeadersKey     string
	groupHeadersKey        string
	extraHeaderPrefixesKey string
	allowedClientNamesKey  string
}

// NewRequestHeaderAuthRequestController creates a new controller that implements RequestHeaderAuthRequestController
func NewRequestHeaderAuthRequestController(
	cmName string, // configmap的名字
	cmNamespace string,
	client kubernetes.Interface,
	usernameHeadersKey,
	groupHeadersKey,
	extraHeaderPrefixesKey,
	allowedClientNamesKey string,
) *RequestHeaderAuthRequestController {
	c := &RequestHeaderAuthRequestController{
		name: "RequestHeaderAuthRequestController",

		client: client,

		configmapName:      cmName,
		configmapNamespace: cmNamespace,

		usernameHeadersKey:     usernameHeadersKey,
		groupHeadersKey:        groupHeadersKey,
		extraHeaderPrefixesKey: extraHeaderPrefixesKey,
		allowedClientNamesKey:  allowedClientNamesKey,

		queue: workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "RequestHeaderAuthRequestController"),
	}

	// we construct our own informer because we need such a small subset of the information available.  Just one namespace.
	// 监听某个configmap
	c.configmapInformer = coreinformers.NewFilteredConfigMapInformer(client, c.configmapNamespace, 12*time.Hour,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
		func(listOptions *metav1.ListOptions) {
			listOptions.FieldSelector = fields.OneTermEqualSelector("metadata.name", c.configmapName).String()
		})

	c.configmapInformer.AddEventHandler(cache.FilteringResourceEventHandler{
		FilterFunc: func(obj interface{}) bool {
			if cast, ok := obj.(*corev1.ConfigMap); ok {
				return cast.Name == c.configmapName && cast.Namespace == c.configmapNamespace
			}
			if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				if cast, ok := tombstone.Obj.(*corev1.ConfigMap); ok {
					return cast.Name == c.configmapName && cast.Namespace == c.configmapNamespace
				}
			}
			return true // always return true just in case.  The checks are fairly cheap
		},
		Handler: cache.ResourceEventHandlerFuncs{
			// we have a filter, so any time we're called, we may as well queue. We only ever check one configmap
			// so we don't have to be choosy about our key.
			AddFunc: func(obj interface{}) {
				c.queue.Add(c.keyFn())
			},
			UpdateFunc: func(oldObj, newObj interface{}) {
				c.queue.Add(c.keyFn())
			},
			DeleteFunc: func(obj interface{}) {
				c.queue.Add(c.keyFn())
			},
		},
	})

	c.configmapLister = corev1listers.NewConfigMapLister(c.configmapInformer.GetIndexer()).ConfigMaps(c.configmapNamespace)
	c.configmapInformerSynced = c.configmapInformer.HasSynced

	return c
}

func (c *RequestHeaderAuthRequestController) UsernameHeaders() []string {
	return c.loadRequestHeaderFor(c.usernameHeadersKey)
}

func (c *RequestHeaderAuthRequestController) GroupHeaders() []string {
	return c.loadRequestHeaderFor(c.groupHeadersKey)
}

func (c *RequestHeaderAuthRequestController) ExtraHeaderPrefixes() []string {
	return c.loadRequestHeaderFor(c.extraHeaderPrefixesKey)
}

func (c *RequestHeaderAuthRequestController) AllowedClientNames() []string {
	return c.loadRequestHeaderFor(c.allowedClientNamesKey)
}

// Run starts RequestHeaderAuthRequestController controller and blocks until stopCh is closed.
func (c *RequestHeaderAuthRequestController) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.Infof("Starting %s", c.name)
	defer klog.Infof("Shutting down %s", c.name)

	go c.configmapInformer.Run(ctx.Done())

	// wait for caches to fill before starting your work
	if !cache.WaitForNamedCacheSync(c.name, ctx.Done(), c.configmapInformerSynced) {
		return
	}

	// doesn't matter what workers say, only start one.
	go wait.Until(c.runWorker, time.Second, ctx.Done())

	<-ctx.Done()
}

// // RunOnce runs a single sync loop
func (c *RequestHeaderAuthRequestController) RunOnce(ctx context.Context) error {
	// 获取configmap
	configMap, err := c.client.CoreV1().ConfigMaps(c.configmapNamespace).Get(ctx, c.configmapName, metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
		// ignore, authConfigMap is nil now
		return nil
	case errors.IsForbidden(err):
		klog.Warningf("Unable to get configmap/%s in %s.  Usually fixed by "+
			"'kubectl create rolebinding -n %s ROLEBINDING_NAME --role=%s --serviceaccount=YOUR_NS:YOUR_SA'",
			c.configmapName, c.configmapNamespace, c.configmapNamespace, authenticationRoleName)
		return err
	case err != nil:
		return err
	}
	return c.syncConfigMap(configMap)
}

func (c *RequestHeaderAuthRequestController) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *RequestHeaderAuthRequestController) processNextWorkItem() bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	err := c.sync()
	if err == nil {
		c.queue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.queue.AddRateLimited(dsKey)

	return true
}

// sync reads the config and propagates the changes to exportedRequestHeaderBundle
// which is exposed by the set of methods that are used to fill RequestHeaderConfig struct
func (c *RequestHeaderAuthRequestController) sync() error {
	configMap, err := c.configmapLister.Get(c.configmapName)
	if err != nil {
		return err
	}
	return c.syncConfigMap(configMap)
}

func (c *RequestHeaderAuthRequestController) syncConfigMap(configMap *corev1.ConfigMap) error {
	hasChanged, newRequestHeaderBundle, err := c.hasRequestHeaderBundleChanged(configMap)
	if err != nil {
		return err
	}
	if hasChanged {
		c.exportedRequestHeaderBundle.Store(newRequestHeaderBundle)
		klog.V(2).Infof("Loaded a new request header values for %v", c.name)
	}
	return nil
}

func (c *RequestHeaderAuthRequestController) hasRequestHeaderBundleChanged(cm *corev1.ConfigMap) (bool, *requestHeaderBundle, error) {
	currentHeadersBundle, err := c.getRequestHeaderBundleFromConfigMap(cm)
	if err != nil {
		return false, nil, err
	}

	rawHeaderBundle := c.exportedRequestHeaderBundle.Load()
	if rawHeaderBundle == nil {
		return true, currentHeadersBundle, nil
	}

	// check to see if we have a change. If the values are the same, do nothing.
	loadedHeadersBundle, ok := rawHeaderBundle.(*requestHeaderBundle)
	if !ok {
		return true, currentHeadersBundle, nil
	}

	if !equality.Semantic.DeepEqual(loadedHeadersBundle, currentHeadersBundle) {
		return true, currentHeadersBundle, nil
	}
	return false, nil, nil
}

func (c *RequestHeaderAuthRequestController) getRequestHeaderBundleFromConfigMap(cm *corev1.ConfigMap) (*requestHeaderBundle, error) {
	usernameHeaderCurrentValue, err := deserializeStrings(cm.Data[c.usernameHeadersKey])
	if err != nil {
		return nil, err
	}

	groupHeadersCurrentValue, err := deserializeStrings(cm.Data[c.groupHeadersKey])
	if err != nil {
		return nil, err
	}

	extraHeaderPrefixesCurrentValue, err := deserializeStrings(cm.Data[c.extraHeaderPrefixesKey])
	if err != nil {
		return nil, err

	}

	allowedClientNamesCurrentValue, err := deserializeStrings(cm.Data[c.allowedClientNamesKey])
	if err != nil {
		return nil, err
	}

	return &requestHeaderBundle{
		UsernameHeaders:     usernameHeaderCurrentValue,
		GroupHeaders:        groupHeadersCurrentValue,
		ExtraHeaderPrefixes: extraHeaderPrefixesCurrentValue,
		AllowedClientNames:  allowedClientNamesCurrentValue,
	}, nil
}

func (c *RequestHeaderAuthRequestController) loadRequestHeaderFor(key string) []string {
	rawHeaderBundle := c.exportedRequestHeaderBundle.Load()
	if rawHeaderBundle == nil {
		return nil // this can happen if we've been unable load data from the apiserver for some reason
	}
	headerBundle := rawHeaderBundle.(*requestHeaderBundle)

	switch key {
	case c.usernameHeadersKey:
		return headerBundle.UsernameHeaders
	case c.groupHeadersKey:
		return headerBundle.GroupHeaders
	case c.extraHeaderPrefixesKey:
		return headerBundle.ExtraHeaderPrefixes
	case c.allowedClientNamesKey:
		return headerBundle.AllowedClientNames
	default:
		return nil
	}
}

func (c *RequestHeaderAuthRequestController) keyFn() string {
	// this format matches DeletionHandlingMetaNamespaceKeyFunc for our single key
	return c.configmapNamespace + "/" + c.configmapName
}

func deserializeStrings(in string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	var ret []string
	if err := json.Unmarshal([]byte(in), &ret); err != nil {
		return nil, err
	}
	return ret, nil
}
