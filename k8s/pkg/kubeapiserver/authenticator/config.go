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

package authenticator

import (
	"errors"
	"time"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/authenticatorfactory"
	"k8s.io/apiserver/pkg/authentication/group"
	"k8s.io/apiserver/pkg/authentication/request/anonymous"
	"k8s.io/apiserver/pkg/authentication/request/bearertoken"
	"k8s.io/apiserver/pkg/authentication/request/headerrequest"
	"k8s.io/apiserver/pkg/authentication/request/union"
	"k8s.io/apiserver/pkg/authentication/request/websocket"
	"k8s.io/apiserver/pkg/authentication/request/x509"
	tokencache "k8s.io/apiserver/pkg/authentication/token/cache"
	"k8s.io/apiserver/pkg/authentication/token/tokenfile"
	tokenunion "k8s.io/apiserver/pkg/authentication/token/union"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
	webhookutil "k8s.io/apiserver/pkg/util/webhook"
	"k8s.io/apiserver/plugin/pkg/authenticator/token/oidc"
	"k8s.io/apiserver/plugin/pkg/authenticator/token/webhook"
	"k8s.io/kube-openapi/pkg/validation/spec"

	// Initialize all known client auth plugins.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/client-go/util/keyutil"
	"k8s.io/kubernetes/pkg/serviceaccount"
)

// Config contains the data on how to authenticate a request to the Kube API Server
// K8S APIServer认证配置：用于配置如何认证一个请求
type Config struct {

	// 是否允许匿名访问
	Anonymous bool

	// 1、了解JWT的人应该就非常熟悉这个属性的作用了；JWT的Payload中有两个保留字段，一个是iss，也就是Token签发人；另外一个则是aud，也就是
	// Token的接收人，也就是说JTW可以明确当前的Token是颁发给谁的。而这里的APIAudience就是这个作用。
	// 2、对于Token认证的系统而言，我们可以通过Playload中的aud来做文章，那就是可以通过aud设置合法的用户。这里的APIAudience就是K8S留给
	// 用户用于指定合法用户的地方。由于JWT的特性，Playload中的内容不可能更改，因为一旦更改了，那么JWT认证将会失效。所以即便JWT认证通过，
	// 但是当前的用户并不是指定的合法用户，依然会拒绝该用户。
	APIAudiences authenticator.Audiences

	// BootstrapToken认证
	BootstrapToken              bool
	BootstrapTokenAuthenticator authenticator.Token

	// 1、StaticToken认证，通过--token-auth-file设置
	// 2、用于指定一个CSV文件，文件中每一行代表一个合法Token,格式为：token,user,uid,"group1,group2,group3"
	// 即token-auth-file文件第一列为toKen，第二列为用户名，第三列为UID，第四列为组（组可以有多个，但是需要用双引号括起来）
	TokenAuthFile string

	// ServiceAccount认证，用于校验ServiceAccountToken是否有效；这个参数可以理解为私钥，而Token则是公钥签名的
	ServiceAccountKeyFiles []string
	// 如果为true，SA Token认证的时候，即便JWT认证通过，SA Token认证器依然会判断两个事情：
	// 1、当前JWT所引用的Secret是否存在，是否被删除，是否和Secret中的Token相等
	// 2、当前JWT所引用的ServiceAccount是否存在，ID是否相等
	ServiceAccountLookup bool
	// 1、JWT Token签发者，这里应该填写HTTPS web地址
	// 2、这种方式是ServiceAccount较新的使用方式
	ServiceAccountIssuers []string
	// TODO, this is the only non-serializable part of the entire config.  Factor it out into a clientconfig
	ServiceAccountTokenGetter serviceaccount.ServiceAccountTokenGetter

	// Webhook认证
	WebhookTokenAuthnConfigFile string
	WebhookTokenAuthnVersion    string
	WebhookTokenAuthnCacheTTL   time.Duration
	// WebhookRetryBackoff specifies the backoff parameters for the authentication webhook retry logic.
	// This allows us to configure the sleep time at each iteration and the maximum number of retries allowed
	// before we fail the webhook call in order to limit the fan out that ensues when the system is degraded.
	WebhookRetryBackoff *wait.Backoff

	// OIDC认证
	OIDCIssuerURL      string
	OIDCClientID       string
	OIDCCAFile         string
	OIDCUsernameClaim  string
	OIDCUsernamePrefix string
	OIDCGroupsClaim    string
	OIDCGroupsPrefix   string
	OIDCSigningAlgs    []string
	OIDCRequiredClaims map[string]string

	// RequestHeader认证
	RequestHeaderConfig *authenticatorfactory.RequestHeaderConfig

	// ClientCAContentProvider are the options for verifying incoming connections using mTLS and directly assigning to users.
	// Generally this is the CA bundle file used to authenticate client certificates
	// If this value is nil, then mutual TLS is disabled.
	// 1、ClientCA认证
	// 2、监听client-ca-file参数所指向的证书，一旦证书发生变化，那么ClientCAContentProvider会通知所有对client-ca-file文件感兴趣的
	// controller；当然，前提是controller已经注册到ClientCAContentProvider当中
	ClientCAContentProvider dynamiccertificates.CAContentProvider

	TokenSuccessCacheTTL time.Duration
	TokenFailureCacheTTL time.Duration

	// Optional field, custom dial function used to connect to webhook
	CustomDial utilnet.DialFunc
}

// New returns an authenticator.Request or an error that supports the standard
// Kubernetes authentication mechanisms.
func (config Config) New() (authenticator.Request, *spec.SecurityDefinitions, error) {
	// 通过解析HTTP请求得到用户身份信息，方便后续认证
	var authenticators []authenticator.Request
	// 通过解析HTTP请求的Token信息，方便后续认证
	var tokenAuthenticators []authenticator.Token
	// TODO openapi相关的东西，可以暂时不用关心
	securityDefinitions := spec.SecurityDefinitions{}

	// front-proxy, BasicAuth methods, local first, then remote
	// Add the front proxy authenticator if requested
	// TODO RequestHeader认证 详细分析
	if config.RequestHeaderConfig != nil {
		requestHeaderAuthenticator := headerrequest.NewDynamicVerifyOptionsSecure(
			config.RequestHeaderConfig.CAContentProvider.VerifyOptions,
			config.RequestHeaderConfig.AllowedClientNames,
			config.RequestHeaderConfig.UsernameHeaders,
			config.RequestHeaderConfig.GroupHeaders,
			config.RequestHeaderConfig.ExtraHeaderPrefixes,
		)
		authenticators = append(authenticators, authenticator.WrapAudienceAgnosticRequest(config.APIAudiences, requestHeaderAuthenticator))
	}

	// X509 methods
	// TODO CA认证 仔细分析
	if config.ClientCAContentProvider != nil {
		certAuth := x509.NewDynamic(config.ClientCAContentProvider.VerifyOptions, x509.CommonNameUserConversion)
		authenticators = append(authenticators, certAuth)
	}

	// Bearer token methods, local first, then remote
	// 1、通过设置token-auth-file参数，为StaticToken认证器指定合法的认证Token
	// 2、如果请求上下文的token是token-auth-file文件中指定的token就是合法token，否则认为是非法token
	if len(config.TokenAuthFile) > 0 {
		tokenAuth, err := newAuthenticatorFromTokenFile(config.TokenAuthFile)
		if err != nil {
			return nil, nil, err
		}
		tokenAuthenticators = append(tokenAuthenticators, authenticator.WrapAudienceAgnosticToken(config.APIAudiences, tokenAuth))
	}
	// 1、这种方式是LegacyServiceAccount认证，认证过程大致如下：
	// 2、首先判断JWT的签发人是否合法，对于LegacyServiceAccount而言，签发人必须是：kubernetes/serviceaccount
	// 3、根据私钥解析JWT Token，如果无法正确解析JWT，就说明此JWT为非法Token
	// 4、判断JWT Token的Audience是否是合法的Audience，如果当前的Audience为非法用户，那么本次认证失败
	// 5、如果APIServer指定service-account-lookup参数为true,那么需要检查当前JWT所对应的Secret， ServiceAccount是否合法，是否存在
	// 是否被删除， token是否和当前JWT是否相等
	// 6、如果上述检测都通过，那么当前的token认证成功；但凡有一点检查失败，当前Token就认证失败
	if len(config.ServiceAccountKeyFiles) > 0 {
		// 所谓的LegacyServiceAccount,其实是因为ServiceAccount对应的token是有K8S生成，签发者为:kubernetes/serviceaccount
		serviceAccountAuth, err := newLegacyServiceAccountAuthenticator(config.ServiceAccountKeyFiles, config.ServiceAccountLookup,
			config.APIAudiences, config.ServiceAccountTokenGetter)
		if err != nil {
			return nil, nil, err
		}
		tokenAuthenticators = append(tokenAuthenticators, serviceAccountAuth)
	}

	// 如果用户指定了service-account-issuer参数，那么再实例化一个ServiceToken认证器。该认证且和LegacyServiceToken基本上是一摸一样的
	// 仅仅在检测JWT的签发者有细微差别，LegacyServiceToken的签发者只有kubernetes/serviceaccount，而这里刚实例化的认证器则是通过
	// service-account-issuer参数指定的
	if len(config.ServiceAccountIssuers) > 0 {
		serviceAccountAuth, err := newServiceAccountAuthenticator(config.ServiceAccountIssuers, config.ServiceAccountKeyFiles,
			config.APIAudiences, config.ServiceAccountTokenGetter)
		if err != nil {
			return nil, nil, err
		}
		tokenAuthenticators = append(tokenAuthenticators, serviceAccountAuth)
	}

	// BootstrapToken认证，认证过程比较简单，步骤如下：
	// 1、解析Token，Token格式为：token-id.token-secret   token-id为6位，token-secret为16位
	// 2、通过BootstrapToken的规则，通过解析到的TokenID找到K8S中对应的Secret，如果不存在，认证失败
	// 3、如果Secret的类型不是bootstrap.kubernetes.io/token，那么认证失败
	// 4、对比Secret的token-id、token-secret，如果不相同，认证失败
	// 5、如果Secret的usage-bootstrap-authentication数据不等于true，认证失败
	// 6、否则，认证成功
	if config.BootstrapToken {
		if config.BootstrapTokenAuthenticator != nil {
			// TODO: This can sometimes be nil because of
			tokenAuthenticators = append(tokenAuthenticators, authenticator.WrapAudienceAgnosticToken(config.APIAudiences, config.BootstrapTokenAuthenticator))
		}
	}
	// NOTE(ericchiang): Keep the OpenID Connect after Service Accounts.
	//
	// Because both plugins verify JWTs whichever comes first in the union experiences
	// cache misses for all requests using the other. While the service account plugin
	// simply returns an error, the OpenID Connect plugin may query the provider to
	// update the keys, causing performance hits.
	// TODO OIDC认证 仔细分析
	if len(config.OIDCIssuerURL) > 0 && len(config.OIDCClientID) > 0 {
		// TODO(enj): wire up the Notifier and ControllerRunner bits when OIDC supports CA reload
		var oidcCAContent oidc.CAContentProvider
		if len(config.OIDCCAFile) != 0 {
			var oidcCAErr error
			oidcCAContent, oidcCAErr = dynamiccertificates.NewDynamicCAContentFromFile("oidc-authenticator", config.OIDCCAFile)
			if oidcCAErr != nil {
				return nil, nil, oidcCAErr
			}
		}

		oidcAuth, err := newAuthenticatorFromOIDCIssuerURL(oidc.Options{
			IssuerURL:            config.OIDCIssuerURL,
			ClientID:             config.OIDCClientID,
			CAContentProvider:    oidcCAContent,
			UsernameClaim:        config.OIDCUsernameClaim,
			UsernamePrefix:       config.OIDCUsernamePrefix,
			GroupsClaim:          config.OIDCGroupsClaim,
			GroupsPrefix:         config.OIDCGroupsPrefix,
			SupportedSigningAlgs: config.OIDCSigningAlgs,
			RequiredClaims:       config.OIDCRequiredClaims,
		})
		if err != nil {
			return nil, nil, err
		}
		tokenAuthenticators = append(tokenAuthenticators, authenticator.WrapAudienceAgnosticToken(config.APIAudiences, oidcAuth))
	}
	// TODO webhook认证 仔细分析
	if len(config.WebhookTokenAuthnConfigFile) > 0 {
		webhookTokenAuth, err := newWebhookTokenAuthenticator(config)
		if err != nil {
			return nil, nil, err
		}

		tokenAuthenticators = append(tokenAuthenticators, webhookTokenAuth)
	}

	if len(tokenAuthenticators) > 0 {
		// Union the token authenticators
		tokenAuth := tokenunion.New(tokenAuthenticators...)
		// Optionally cache authentication results
		if config.TokenSuccessCacheTTL > 0 || config.TokenFailureCacheTTL > 0 {
			tokenAuth = tokencache.New(tokenAuth, true, config.TokenSuccessCacheTTL, config.TokenFailureCacheTTL)
		}
		authenticators = append(authenticators, bearertoken.New(tokenAuth), websocket.NewProtocolAuthenticator(tokenAuth))
		securityDefinitions["BearerToken"] = &spec.SecurityScheme{
			SecuritySchemeProps: spec.SecuritySchemeProps{
				Type:        "apiKey",
				Name:        "authorization",
				In:          "header",
				Description: "Bearer Token authentication",
			},
		}
	}

	if len(authenticators) == 0 {
		if config.Anonymous {
			return anonymous.NewAuthenticator(), &securityDefinitions, nil
		}
		return nil, &securityDefinitions, nil
	}

	// 认证器数组
	authenticator := union.New(authenticators...)

	// 实际上就是一个认证器Wrapper，为认证器添加Group相关信息
	authenticator = group.NewAuthenticatedGroupAdder(authenticator)

	// TODO 匿名认证  仔细分析
	if config.Anonymous {
		// If the authenticator chain returns an error, return an error (don't consider a bad bearer token
		// or invalid username/password combination anonymous).
		authenticator = union.NewFailOnError(authenticator, anonymous.NewAuthenticator())
	}

	return authenticator, &securityDefinitions, nil
}

// IsValidServiceAccountKeyFile returns true if a valid public RSA key can be read from the given file
func IsValidServiceAccountKeyFile(file string) bool {
	_, err := keyutil.PublicKeysFromFile(file)
	return err == nil
}

// newAuthenticatorFromTokenFile returns an authenticator.Token or an error
func newAuthenticatorFromTokenFile(tokenAuthFile string) (authenticator.Token, error) {
	tokenAuthenticator, err := tokenfile.NewCSV(tokenAuthFile)
	if err != nil {
		return nil, err
	}

	return tokenAuthenticator, nil
}

// newAuthenticatorFromOIDCIssuerURL returns an authenticator.Token or an error.
func newAuthenticatorFromOIDCIssuerURL(opts oidc.Options) (authenticator.Token, error) {
	const noUsernamePrefix = "-"

	if opts.UsernamePrefix == "" && opts.UsernameClaim != "email" {
		// Old behavior. If a usernamePrefix isn't provided, prefix all claims other than "email"
		// with the issuerURL.
		//
		// See https://github.com/kubernetes/kubernetes/issues/31380
		opts.UsernamePrefix = opts.IssuerURL + "#"
	}

	if opts.UsernamePrefix == noUsernamePrefix {
		// Special value indicating usernames shouldn't be prefixed.
		opts.UsernamePrefix = ""
	}

	tokenAuthenticator, err := oidc.New(opts)
	if err != nil {
		return nil, err
	}

	return tokenAuthenticator, nil
}

// newLegacyServiceAccountAuthenticator returns an authenticator.Token or an error
func newLegacyServiceAccountAuthenticator(keyfiles []string, lookup bool, apiAudiences authenticator.Audiences,
	serviceAccountGetter serviceaccount.ServiceAccountTokenGetter) (authenticator.Token, error) {
	var allPublicKeys []interface{}
	for _, keyfile := range keyfiles {
		// 解析PEM私钥
		publicKeys, err := keyutil.PublicKeysFromFile(keyfile)
		if err != nil {
			return nil, err
		}
		allPublicKeys = append(allPublicKeys, publicKeys...)
	}

	tokenAuthenticator := serviceaccount.JWTTokenAuthenticator([]string{serviceaccount.LegacyIssuer}, allPublicKeys,
		apiAudiences, serviceaccount.NewLegacyValidator(lookup, serviceAccountGetter))
	return tokenAuthenticator, nil
}

// newServiceAccountAuthenticator returns an authenticator.Token or an error
func newServiceAccountAuthenticator(issuers []string, keyfiles []string, apiAudiences authenticator.Audiences,
	serviceAccountGetter serviceaccount.ServiceAccountTokenGetter) (authenticator.Token, error) {
	allPublicKeys := []interface{}{}
	for _, keyfile := range keyfiles {
		publicKeys, err := keyutil.PublicKeysFromFile(keyfile)
		if err != nil {
			return nil, err
		}
		allPublicKeys = append(allPublicKeys, publicKeys...)
	}

	tokenAuthenticator := serviceaccount.JWTTokenAuthenticator(issuers, allPublicKeys, apiAudiences, serviceaccount.NewValidator(serviceAccountGetter))
	return tokenAuthenticator, nil
}

func newWebhookTokenAuthenticator(config Config) (authenticator.Token, error) {
	if config.WebhookRetryBackoff == nil {
		return nil, errors.New("retry backoff parameters for authentication webhook has not been specified")
	}

	clientConfig, err := webhookutil.LoadKubeconfig(config.WebhookTokenAuthnConfigFile, config.CustomDial)
	if err != nil {
		return nil, err
	}
	webhookTokenAuthenticator, err := webhook.New(clientConfig, config.WebhookTokenAuthnVersion, config.APIAudiences, *config.WebhookRetryBackoff)
	if err != nil {
		return nil, err
	}

	return tokencache.New(webhookTokenAuthenticator, false, config.WebhookTokenAuthnCacheTTL, config.WebhookTokenAuthnCacheTTL), nil
}
