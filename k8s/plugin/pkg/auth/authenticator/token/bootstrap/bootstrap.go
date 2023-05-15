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

/*
Package bootstrap provides a token authenticator for TLS bootstrap secrets.
*/
package bootstrap

import (
	"context"
	"crypto/subtle"
	"fmt"
	"time"

	"k8s.io/klog/v2"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	corev1listers "k8s.io/client-go/listers/core/v1"
	bootstrapapi "k8s.io/cluster-bootstrap/token/api"
	bootstrapsecretutil "k8s.io/cluster-bootstrap/util/secrets"
	bootstraptokenutil "k8s.io/cluster-bootstrap/util/tokens"
)

// TODO: A few methods in this package is copied from other sources. Either
// because the existing functionality isn't exported or because it is in a
// package that shouldn't be directly imported by this packages.

// NewTokenAuthenticator initializes a bootstrap token authenticator.
//
// Lister is expected to be for the "kube-system" namespace.
func NewTokenAuthenticator(lister corev1listers.SecretNamespaceLister) *TokenAuthenticator {
	return &TokenAuthenticator{lister}
}

// TokenAuthenticator authenticates bootstrap tokens from secrets in the API server.
type TokenAuthenticator struct {
	lister corev1listers.SecretNamespaceLister
}

// tokenErrorf prints a error message for a secret that has matched a bearer
// token but fails to meet some other criteria.
//
//	tokenErrorf(secret, "has invalid value for key %s", key)
func tokenErrorf(s *corev1.Secret, format string, i ...interface{}) {
	format = fmt.Sprintf("Bootstrap secret %s/%s matching bearer token ", s.Namespace, s.Name) + format
	klog.V(3).Infof(format, i...)
}

// AuthenticateToken tries to match the provided token to a bootstrap token secret
// in a given namespace. If found, it authenticates the token in the
// "system:bootstrappers" group and with the "system:bootstrap:(token-id)" username.
//
// All secrets must be of type "bootstrap.kubernetes.io/token". An example secret:
//
//	apiVersion: v1
//	kind: Secret
//	metadata:
//	  # Name MUST be of form "bootstrap-token-( token id )".
//	  name: bootstrap-token-( token id )
//	  namespace: kube-system
//	# Only secrets of this type will be evaluated.
//	type: bootstrap.kubernetes.io/token
//	data:
//	  token-secret: ( private part of token )
//	  token-id: ( token id )
//	  # Required key usage.
//	  usage-bootstrap-authentication: true
//	  auth-extra-groups: "system:bootstrappers:custom-group1,system:bootstrappers:custom-group2"
//	  # May also contain an expiry.
//
// Tokens are expected to be of the form:
//
//	TODO token的格式为：( token-id ).( token-secret )
//
/* bootstrap token的格式一般长这样
apiVersion: v1
kind: Secret
metadata:
  name: bootstrap-token-09phqo
  namespace: kube-system
type: bootstrap.kubernetes.io/token
data:
  auth-extra-groups: c3lzdGVtOmJvb3RzdHJhcHBlcnM6a3ViZWFkbTpkZWZhdWx0LW5vZGUtdG9rZW4= (解码后为：system:bootstrappers:kubeadm:default-node-token)
  description: a3ViZXNwcmF5IGt1YmVhZG0gYm9vdHN0cmFwIHRva2Vu (解码后为：kubespray kubeadm bootstrap token)
  token-id: MDlwaHFv (解码后为：09phqo)
  token-secret: cG54ZzI1MzZudW1xem9wOA== (解码后为：pnxg2536numqzop8)
  usage-bootstrap-authentication: dHJ1ZQ== (解码后为：true)
  usage-bootstrap-signing: dHJ1ZQ== (解码后为：true)
*/
func (t *TokenAuthenticator) AuthenticateToken(ctx context.Context, token string) (*authenticator.Response, bool, error) {
	// 解析Token，Token格式为：token-id.token-secret   token-id为6位，token-secret为16位
	tokenID, tokenSecret, err := bootstraptokenutil.ParseToken(token)
	if err != nil {
		// Token isn't of the correct form, ignore it.
		return nil, false, nil
	}

	// bootstrap-token-<token id>
	// 通过tokenId找到对应的Secret  TODO 这个Secret是由谁生成的？ 似乎是由Kubelet创建的
	secretName := bootstrapapi.BootstrapTokenSecretPrefix + tokenID
	secret, err := t.lister.Get(secretName)
	if err != nil {
		if errors.IsNotFound(err) {
			klog.V(3).Infof("No secret of name %s to match bootstrap bearer token", secretName)
			return nil, false, nil
		}
		return nil, false, err
	}

	// 如果Secret已经被删除了，那么认为token认证失败
	if secret.DeletionTimestamp != nil {
		tokenErrorf(secret, "is deleted and awaiting removal")
		return nil, false, nil
	}

	// secret必须是bootstrap.kubernetes.io/token类型
	// 如果Token对应的Secret类型不对，认证失败
	if string(secret.Type) != string(bootstrapapi.SecretTypeBootstrapToken) || secret.Data == nil {
		tokenErrorf(secret, "has invalid type, expected %s.", bootstrapapi.SecretTypeBootstrapToken)
		return nil, false, nil
	}

	// 对比当前Token与从Secret中获取到的TokenSecret是否一致，如果不一致，认证失败
	ts := bootstrapsecretutil.GetData(secret, bootstrapapi.BootstrapTokenSecretKey)
	if subtle.ConstantTimeCompare([]byte(ts), []byte(tokenSecret)) != 1 {
		tokenErrorf(secret, "has invalid value for key %s, expected %s.", bootstrapapi.BootstrapTokenSecretKey, tokenSecret)
		return nil, false, nil
	}

	// 对比当前Token与从Secret中获取到的TokenID是否一致，如果不一致，认证失败
	id := bootstrapsecretutil.GetData(secret, bootstrapapi.BootstrapTokenIDKey)
	if id != tokenID {
		tokenErrorf(secret, "has invalid value for key %s, expected %s.", bootstrapapi.BootstrapTokenIDKey, tokenID)
		return nil, false, nil
	}

	// 判断token是否过期  token的过期时间通过expiration来设置，如果没有设置，那么认为token永不过期
	// 如果Secret已经过期，认证失败
	if bootstrapsecretutil.HasExpired(secret, time.Now()) {
		// logging done in isSecretExpired method.
		return nil, false, nil
	}

	// 如果从Secret中获取到的usage-bootstrap-authentication不等于true，认证失败
	if bootstrapsecretutil.GetData(secret, bootstrapapi.BootstrapTokenUsageAuthentication) != "true" {
		tokenErrorf(secret, "not marked %s=true.", bootstrapapi.BootstrapTokenUsageAuthentication)
		return nil, false, nil
	}

	// 获取Secret的授权组信息
	groups, err := bootstrapsecretutil.GetGroups(secret)
	if err != nil {
		tokenErrorf(secret, "has invalid value for key %s: %v.", bootstrapapi.BootstrapTokenExtraGroupsKey, err)
		return nil, false, nil
	}

	return &authenticator.Response{
		User: &user.DefaultInfo{
			Name:   bootstrapapi.BootstrapUserPrefix + string(id),
			Groups: groups,
		},
	}, true, nil
}
