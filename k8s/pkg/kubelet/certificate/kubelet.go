/*
Copyright 2017 The Kubernetes Authors.

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

package certificate

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math"
	"net"
	"sort"
	"time"

	certificates "k8s.io/api/certificates/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/certificate"
	compbasemetrics "k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	kubeletconfig "k8s.io/kubernetes/pkg/kubelet/apis/config"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	netutils "k8s.io/utils/net"
)

// NewKubeletServerCertificateManager creates a certificate manager for the kubelet when retrieving a server certificate
// or returns an error.
func NewKubeletServerCertificateManager(
	kubeClient clientset.Interface, // APIServer客户端
	kubeCfg *kubeletconfig.KubeletConfiguration, // kubelet配置
	nodeName types.NodeName, // node名字
	getAddresses func() []v1.NodeAddress, // 用于获取node地址
	certDirectory string, // 证书目录,一般默认为 /var/lib/kubelet/pki
) (certificate.Manager, error) {
	var clientsetFn certificate.ClientsetFunc
	if kubeClient != nil {
		clientsetFn = func(current *tls.Certificate) (clientset.Interface, error) {
			return kubeClient, nil
		}
	}
	// 用于保存证书
	certificateStore, err := certificate.NewFileStore(
		"kubelet-server",
		certDirectory,             // 证书目录
		certDirectory,             // 私钥目录
		kubeCfg.TLSCertFile,       // 证书路径
		kubeCfg.TLSPrivateKeyFile, // 私钥路径
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize server certificate store: %v", err)
	}
	var certificateRenewFailure = compbasemetrics.NewCounter(
		&compbasemetrics.CounterOpts{
			Subsystem:      metrics.KubeletSubsystem,
			Name:           "server_expiration_renew_errors",
			Help:           "Counter of certificate renewal errors.",
			StabilityLevel: compbasemetrics.ALPHA,
		},
	)
	legacyregistry.MustRegister(certificateRenewFailure)

	certificateRotationAge := compbasemetrics.NewHistogram(
		&compbasemetrics.HistogramOpts{
			Subsystem: metrics.KubeletSubsystem,
			Name:      "certificate_manager_server_rotation_seconds",
			Help:      "Histogram of the number of seconds the previous certificate lived before being rotated.",
			Buckets: []float64{
				60,        // 1  minute
				3600,      // 1  hour
				14400,     // 4  hours
				86400,     // 1  day
				604800,    // 1  week
				2592000,   // 1  month
				7776000,   // 3  months
				15552000,  // 6  months
				31104000,  // 1  year
				124416000, // 4  years
			},
			StabilityLevel: compbasemetrics.ALPHA,
		},
	)
	legacyregistry.MustRegister(certificateRotationAge)

	// TODO 这玩意是证书签名请求么？
	getTemplate := func() *x509.CertificateRequest {
		hostnames, ips := addressesToHostnamesAndIPs(getAddresses())
		// don't return a template if we have no addresses to request for
		if len(hostnames) == 0 && len(ips) == 0 {
			return nil
		}
		return &x509.CertificateRequest{
			Subject: pkix.Name{
				CommonName:   fmt.Sprintf("system:node:%s", nodeName),
				Organization: []string{"system:nodes"},
			},
			DNSNames:    hostnames,
			IPAddresses: ips,
		}
	}

	m, err := certificate.NewManager(&certificate.Config{
		ClientsetFn:             clientsetFn,
		GetTemplate:             getTemplate,
		SignerName:              certificates.KubeletServingSignerName,
		GetUsages:               certificate.DefaultKubeletServingGetUsages,
		CertificateStore:        certificateStore,
		CertificateRotation:     certificateRotationAge,
		CertificateRenewFailure: certificateRenewFailure,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize server certificate manager: %v", err)
	}
	legacyregistry.RawMustRegister(compbasemetrics.NewGaugeFunc(
		&compbasemetrics.GaugeOpts{
			Subsystem: metrics.KubeletSubsystem,
			Name:      "certificate_manager_server_ttl_seconds",
			Help: "Gauge of the shortest TTL (time-to-live) of " +
				"the Kubelet's serving certificate. The value is in seconds " +
				"until certificate expiry (negative if already expired). If " +
				"serving certificate is invalid or unused, the value will " +
				"be +INF.",
			StabilityLevel: compbasemetrics.ALPHA,
		},
		func() float64 {
			if c := m.Current(); c != nil && c.Leaf != nil {
				return math.Trunc(time.Until(c.Leaf.NotAfter).Seconds())
			}
			return math.Inf(1)
		},
	))
	return m, nil
}

func addressesToHostnamesAndIPs(addresses []v1.NodeAddress) (dnsNames []string, ips []net.IP) {
	seenDNSNames := map[string]bool{}
	seenIPs := map[string]bool{}
	for _, address := range addresses {
		if len(address.Address) == 0 {
			continue
		}

		switch address.Type {
		case v1.NodeHostName:
			if ip := netutils.ParseIPSloppy(address.Address); ip != nil {
				seenIPs[address.Address] = true
			} else {
				seenDNSNames[address.Address] = true
			}
		case v1.NodeExternalIP, v1.NodeInternalIP:
			if ip := netutils.ParseIPSloppy(address.Address); ip != nil {
				seenIPs[address.Address] = true
			}
		case v1.NodeExternalDNS, v1.NodeInternalDNS:
			seenDNSNames[address.Address] = true
		}
	}

	for dnsName := range seenDNSNames {
		dnsNames = append(dnsNames, dnsName)
	}
	for ip := range seenIPs {
		ips = append(ips, netutils.ParseIPSloppy(ip))
	}

	// return in stable order
	sort.Strings(dnsNames)
	sort.Slice(ips, func(i, j int) bool { return ips[i].String() < ips[j].String() })

	return dnsNames, ips
}

// NewKubeletClientCertificateManager sets up a certificate manager without a
// client that can be used to sign new certificates (or rotate). If a CSR
// client is set later, it may begin rotating/renewing the client cert.
func NewKubeletClientCertificateManager(
	certDirectory string,
	nodeName types.NodeName,
	bootstrapCertData []byte,
	bootstrapKeyData []byte,
	certFile string,
	keyFile string,
	clientsetFn certificate.ClientsetFunc,
) (certificate.Manager, error) {

	// 用于存保存证书，并且可以获取证书
	certificateStore, err := certificate.NewFileStore(
		"kubelet-client",
		certDirectory, // /var/lib/kubelet/pki
		certDirectory, // /var/lib/kubelet/pki
		certFile,      // /var/lib/kubelet/pki/kubelet-client-current.pem
		keyFile,       // /var/lib/kubelet/pki/kubelet-client-current.pem
	)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize client certificate store: %v", err)
	}
	var certificateRenewFailure = compbasemetrics.NewCounter(
		&compbasemetrics.CounterOpts{
			Namespace:      metrics.KubeletSubsystem,
			Subsystem:      "certificate_manager",
			Name:           "client_expiration_renew_errors",
			Help:           "Counter of certificate renewal errors.",
			StabilityLevel: compbasemetrics.ALPHA,
		},
	)
	legacyregistry.Register(certificateRenewFailure)

	m, err := certificate.NewManager(&certificate.Config{
		ClientsetFn: clientsetFn,
		Template: &x509.CertificateRequest{
			Subject: pkix.Name{
				CommonName:   fmt.Sprintf("system:node:%s", nodeName),
				Organization: []string{"system:nodes"},
			},
		},
		SignerName: certificates.KubeAPIServerClientKubeletSignerName,
		GetUsages:  certificate.DefaultKubeletClientGetUsages,
		// For backwards compatibility, the kubelet supports the ability to
		// provide a higher privileged certificate as initial data that will
		// then be rotated immediately. This code path is used by kubeadm on
		// the masters.
		BootstrapCertificatePEM: bootstrapCertData,
		BootstrapKeyPEM:         bootstrapKeyData,

		CertificateStore:        certificateStore,
		CertificateRenewFailure: certificateRenewFailure,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to initialize client certificate manager: %v", err)
	}

	return m, nil
}
