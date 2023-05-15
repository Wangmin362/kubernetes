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

package serviceaccount

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	jose "gopkg.in/square/go-jose.v2"
	"gopkg.in/square/go-jose.v2/jwt"

	v1 "k8s.io/api/core/v1"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apiserver/pkg/audit"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	apiserverserviceaccount "k8s.io/apiserver/pkg/authentication/serviceaccount"
)

// ServiceAccountTokenGetter defines functions to retrieve a named service account and secret
type ServiceAccountTokenGetter interface {
	GetServiceAccount(namespace, name string) (*v1.ServiceAccount, error)
	GetPod(namespace, name string) (*v1.Pod, error)
	GetSecret(namespace, name string) (*v1.Secret, error)
}

type TokenGenerator interface {
	// GenerateToken generates a token which will identify the given
	// ServiceAccount. privateClaims is an interface that will be
	// serialized into the JWT payload JSON encoding at the root level of
	// the payload object. Public claims take precedent over private
	// claims i.e. if both claims and privateClaims have an "exp" field,
	// the value in claims will be used.
	GenerateToken(claims *jwt.Claims, privateClaims interface{}) (string, error)
}

// JWTTokenGenerator returns a TokenGenerator that generates signed JWT tokens, using the given privateKey.
// privateKey is a PEM-encoded byte array of a private RSA key.
func JWTTokenGenerator(iss string, privateKey interface{}) (TokenGenerator, error) {
	var signer jose.Signer
	var err error
	switch pk := privateKey.(type) {
	case *rsa.PrivateKey:
		signer, err = signerFromRSAPrivateKey(pk)
		if err != nil {
			return nil, fmt.Errorf("could not generate signer for RSA keypair: %v", err)
		}
	case *ecdsa.PrivateKey:
		signer, err = signerFromECDSAPrivateKey(pk)
		if err != nil {
			return nil, fmt.Errorf("could not generate signer for ECDSA keypair: %v", err)
		}
	case jose.OpaqueSigner:
		signer, err = signerFromOpaqueSigner(pk)
		if err != nil {
			return nil, fmt.Errorf("could not generate signer for OpaqueSigner: %v", err)
		}
	default:
		return nil, fmt.Errorf("unknown private key type %T, must be *rsa.PrivateKey, *ecdsa.PrivateKey, or jose.OpaqueSigner", privateKey)
	}

	return &jwtTokenGenerator{
		iss:    iss,
		signer: signer,
	}, nil
}

// keyIDFromPublicKey derives a key ID non-reversibly from a public key.
//
// The Key ID is field on a given on JWTs and JWKs that help relying parties
// pick the correct key for verification when the identity party advertises
// multiple keys.
//
// Making the derivation non-reversible makes it impossible for someone to
// accidentally obtain the real key from the key ID and use it for token
// validation.
func keyIDFromPublicKey(publicKey interface{}) (string, error) {
	publicKeyDERBytes, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		return "", fmt.Errorf("failed to serialize public key to DER format: %v", err)
	}

	hasher := crypto.SHA256.New()
	hasher.Write(publicKeyDERBytes)
	publicKeyDERHash := hasher.Sum(nil)

	keyID := base64.RawURLEncoding.EncodeToString(publicKeyDERHash)

	return keyID, nil
}

func signerFromRSAPrivateKey(keyPair *rsa.PrivateKey) (jose.Signer, error) {
	keyID, err := keyIDFromPublicKey(&keyPair.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to derive keyID: %v", err)
	}

	// IMPORTANT: If this function is updated to support additional key sizes,
	// algorithmForPublicKey in serviceaccount/openidmetadata.go must also be
	// updated to support the same key sizes. Today we only support RS256.

	// Wrap the RSA keypair in a JOSE JWK with the designated key ID.
	privateJWK := &jose.JSONWebKey{
		Algorithm: string(jose.RS256),
		Key:       keyPair,
		KeyID:     keyID,
		Use:       "sig",
	}

	signer, err := jose.NewSigner(
		jose.SigningKey{
			Algorithm: jose.RS256,
			Key:       privateJWK,
		},
		nil,
	)

	if err != nil {
		return nil, fmt.Errorf("failed to create signer: %v", err)
	}

	return signer, nil
}

func signerFromECDSAPrivateKey(keyPair *ecdsa.PrivateKey) (jose.Signer, error) {
	var alg jose.SignatureAlgorithm
	switch keyPair.Curve {
	case elliptic.P256():
		alg = jose.ES256
	case elliptic.P384():
		alg = jose.ES384
	case elliptic.P521():
		alg = jose.ES512
	default:
		return nil, fmt.Errorf("unknown private key curve, must be 256, 384, or 521")
	}

	keyID, err := keyIDFromPublicKey(&keyPair.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("failed to derive keyID: %v", err)
	}

	// Wrap the ECDSA keypair in a JOSE JWK with the designated key ID.
	privateJWK := &jose.JSONWebKey{
		Algorithm: string(alg),
		Key:       keyPair,
		KeyID:     keyID,
		Use:       "sig",
	}

	signer, err := jose.NewSigner(
		jose.SigningKey{
			Algorithm: alg,
			Key:       privateJWK,
		},
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create signer: %v", err)
	}

	return signer, nil
}

func signerFromOpaqueSigner(opaqueSigner jose.OpaqueSigner) (jose.Signer, error) {
	alg := jose.SignatureAlgorithm(opaqueSigner.Public().Algorithm)

	signer, err := jose.NewSigner(
		jose.SigningKey{
			Algorithm: alg,
			Key: &jose.JSONWebKey{
				Algorithm: string(alg),
				Key:       opaqueSigner,
				KeyID:     opaqueSigner.Public().KeyID,
				Use:       "sig",
			},
		},
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create signer: %v", err)
	}

	return signer, nil
}

type jwtTokenGenerator struct {
	iss    string
	signer jose.Signer
}

func (j *jwtTokenGenerator) GenerateToken(claims *jwt.Claims, privateClaims interface{}) (string, error) {
	// claims are applied in reverse precedence
	return jwt.Signed(j.signer).
		Claims(privateClaims).
		Claims(claims).
		Claims(&jwt.Claims{
			Issuer: j.iss,
		}).
		CompactSerialize()
}

// JWTTokenAuthenticator authenticates tokens as JWT tokens produced by JWTTokenGenerator
// Token signatures are verified using each of the given public keys until one works (allowing key rotation)
// If lookup is true, the service account and secret referenced as claims inside the token are retrieved and verified with the provided ServiceAccountTokenGetter
func JWTTokenAuthenticator(issuers []string, keys []interface{}, implicitAuds authenticator.Audiences, validator Validator) authenticator.Token {
	issuersMap := make(map[string]bool)
	for _, issuer := range issuers {
		issuersMap[issuer] = true
	}
	return &jwtTokenAuthenticator{
		issuers:      issuersMap,
		keys:         keys,
		implicitAuds: implicitAuds,
		validator:    validator,
	}
}

/* ServiceAccount对应生成的Secret数据格式为：
apiVersion: v1
data:
  ca.crt: LS0tLS1CRUdJTiBDRVJUSUZJQ0FURS0tLS0tCk1JSUM2VENDQWRHZ0F3SUJBZ0lCQURBTkJna3Foa2lHOXcwQkFRc0ZBREFWTVJNd0VRWURWUVFERXdwcmRXSmwKY201bGRHVnpNQ0FYRFRJeU1URXhNREE0TXpReE1sb1lEekl3TlRJeE1UQXlNRGd6TkRFeVdqQVZNUk13RVFZRApWUVFERXdwcmRXSmxjbTVsZEdWek1JSUJJakFOQmdrcWhraUc5dzBCQVFFRkFBT0NBUThBTUlJQkNnS0NBUUVBCnR3Yk9tUnhMaVNldjlnbEhiWlN4ZUxHalZSeGRRTlc5VU1vYlhELzZpV0k0b3I0dlNjeEtoeFE3UUVycVhZQzAKTE9Yb1lWb3ZHdzhFL2NscFFSV1lqd2pEeUE3a1FqdTk5blI0eExucXJITDhSYWJERC83L1NJOEtYL1BKYTB1dApuWVltcTVlanloMWQxUEg1ZE9DL2gycW9seHVaM2hhSmJIK3BXUFpSSUJkWVRrL2JRQ3B1RnFhbytWV3hkbUg2Cm81QXJyWmlmajMzZktEZkl2YWNHZnpuUzA4TWJXSTdXa0d5Z2h6S3lJVUZVREtncWV5M0d3U2VZNm15cW11YVAKOFBpSUttOUY5dnBBb0JuaW5ONHRWOEhkaG1FanU4ejN0d1ozbnY4ekVIQ3hoZldOUVVWTWRrNVlSUHlHei84Nwo4c0RYZXpMVGVIMjMxc3p5Z3pxZmt3SURBUUFCbzBJd1FEQU9CZ05WSFE4QkFmOEVCQU1DQXFRd0R3WURWUjBUCkFRSC9CQVV3QXdFQi96QWRCZ05WSFE0RUZnUVVxSEtocys1clNNZS9ISGUxY0hkWDBTWmJ6b0V3RFFZSktvWkkKaHZjTkFRRUxCUUFEZ2dFQkFGSEZ3V0xPRFBZQ3ErcTl1NS9lVWgzc2NtVi95Yks2NUhYVjFwazlGMHE1UzhvVgp5ejUwKzBZWFBnSGdtWTlZVXJGdXRJNENod1g1TDBNUWtXSkFVaDhRTXBRanhhaVRZRFlmcHZBQ1RzcUtDZ3Q4CkZLaHlmZTc4d1V1RGtvdXNiaW5XTmpnMk9uV3p1TDNMWlJFYzNETm5wSUZSMlcrVzhPd3BLSUpPaVBVRStJUXMKL3BvYVQzS0U1UGZ1aitMQ1lOUU5FZW4wVXhJT3FBMG9tNUdCNXJJWWxHcW45OU5sbGd6TmhsVHlhWUR1elVsTgpJWHFBNFhubkFpZ1FUZ3N1MEVvN1UyTVdvUEkyRXdibjhhSThTZjl1SGloZTR0US9qSTQ4R3o1MmFyQXdVa3FwCjhyQ0tqNXhhdnhNaFRpVVVacGdTRFVLc0ZqVDNLdjNWTnFvK1FDQT0KLS0tLS1FTkQgQ0VSVElGSUNBVEUtLS0tLQo=
  namespace: ZGVmYXVsdA==
  token: ZXlKaGJHY2lPaUpTVXpJMU5pSXNJbXRwWkNJNkluWmZSbFJpVlVab2VFbFhSVUV0Wkc5WWEyMXZZemhGTUVKVU4yUXRVVkpXWjBGTFoxbExTMmgxYWxVaWZRLmV5SnBjM01pT2lKcmRXSmxjbTVsZEdWekwzTmxjblpwWTJWaFkyTnZkVzUwSWl3aWEzVmlaWEp1WlhSbGN5NXBieTl6WlhKMmFXTmxZV05qYjNWdWRDOXVZVzFsYzNCaFkyVWlPaUprWldaaGRXeDBJaXdpYTNWaVpYSnVaWFJsY3k1cGJ5OXpaWEoyYVdObFlXTmpiM1Z1ZEM5elpXTnlaWFF1Ym1GdFpTSTZJbVJsWm1GMWJIUXRkRzlyWlc0dFluaDBhamNpTENKcmRXSmxjbTVsZEdWekxtbHZMM05sY25acFkyVmhZMk52ZFc1MEwzTmxjblpwWTJVdFlXTmpiM1Z1ZEM1dVlXMWxJam9pWkdWbVlYVnNkQ0lzSW10MVltVnlibVYwWlhNdWFXOHZjMlZ5ZG1salpXRmpZMjkxYm5RdmMyVnlkbWxqWlMxaFkyTnZkVzUwTG5WcFpDSTZJbVEyT0dFek16RTJMVFUxTlRZdE5ERTBaQzFpWWpVMkxUVmxOamc1WXprek5tUTVaU0lzSW5OMVlpSTZJbk41YzNSbGJUcHpaWEoyYVdObFlXTmpiM1Z1ZERwa1pXWmhkV3gwT21SbFptRjFiSFFpZlEuZXBQbkc3bG9KckU0U1lPZjV4ekdOV25NRDlldi11dWJqQ3ZleWUwVlJ6dWk5S2ZqSk5BdExDOGRHVVJ0bWxxS2kxTzFmdVZhNUtUR0R2UWhsVGtrTlhMS2FFYnFKT1ZiTlFEUERnSlZfdDZSMWNtbHplRGd1S3NRbWtIMWowVFBiUHA0UmVkYWZkdEVwdFFPdExSbmdTMG9ocHRiR0JNY3B1M3lsN3JRcEdJQUlsQWlyWnZzNTJNWFZlUWVHQmlCakF3WFdXZVUza29ScHo0czdUck03amp1dllUbE5GODNibHhYQ1hSRFI0ei1zX0piWlBqN1F2MGZxdzc0bTl2eTZxWjZISXFnM2JHc0lRNFQ4LWdHRWtHTTNEcFZqQ2RUVnVTMUtjMXNBbkZzT0ozdHpPcEJLRFJHazBUTE83ZEllQ283MXRiQkhEMkllNm50ZEZSVU9B
kind: Secret
metadata:
  annotations:
    kubernetes.io/service-account.name: default
    kubernetes.io/service-account.uid: d68a3316-5556-414d-bb56-5e689c936d9e
  name: default-token-bxtj7
  namespace: default
type: kubernetes.io/service-account-token
*/
/* token解压缩之后
Header为：
{
    "alg": "RS256",
    "kid": "v_FTbUFhxIWEA-doXkmoc8E0BT7d-QRVgAKgYKKhujU"
}

Payload为：
{
    "iss": "kubernetes/serviceaccount",
    "kubernetes.io/serviceaccount/namespace": "default",
    "kubernetes.io/serviceaccount/secret.name": "default-token-bxtj7",
    "kubernetes.io/serviceaccount/service-account.name": "default",
    "kubernetes.io/serviceaccount/service-account.uid": "d68a3316-5556-414d-bb56-5e689c936d9e",
    "sub": "system:serviceaccount:default:default"
}

*/
// 实际上ServiceAccount认证，就是JWT认证

type jwtTokenAuthenticator struct {
	// Token签发人
	issuers   map[string]bool
	keys      []interface{}
	validator Validator
	// Token的接受者
	implicitAuds authenticator.Audiences
}

// Validator is called by the JWT token authenticator to apply domain specific
// validation to a token and extract user information.
type Validator interface {
	// Validate validates a token and returns user information or an error.
	// Validator can assume that the issuer and signature of a token are already
	// verified when this function is called.
	Validate(ctx context.Context, tokenData string, public *jwt.Claims, private interface{}) (*apiserverserviceaccount.ServiceAccountInfo, error)
	// NewPrivateClaims returns a struct that the authenticator should
	// deserialize the JWT payload into. The authenticator may then pass this
	// struct back to the Validator as the 'private' argument to a Validate()
	// call. This struct should contain fields for any private claims that the
	// Validator requires to validate the JWT.
	NewPrivateClaims() interface{}
}

func (j *jwtTokenAuthenticator) AuthenticateToken(ctx context.Context, tokenData string) (*authenticator.Response, bool, error) {
	// 如果签发人不是合法的签发人，那么直接认为当前Token校验没有通过
	if !j.hasCorrectIssuer(tokenData) {
		return nil, false, nil
	}

	tok, err := jwt.ParseSigned(tokenData)
	if err != nil {
		return nil, false, nil
	}

	public := &jwt.Claims{}
	// 私有声明
	private := j.validator.NewPrivateClaims()

	// TODO: Pick the key that has the same key ID as `tok`, if one exists.
	var (
		found   bool
		errlist []error
	)
	for _, key := range j.keys {
		// 使用私钥校验JWT
		if err := tok.Claims(key, public, private); err != nil {
			errlist = append(errlist, err)
			continue
		}
		// 如果没有错误，说明当前的Token是合法的Token
		found = true
		break
	}

	if !found {
		return nil, false, utilerrors.NewAggregate(errlist)
	}

	// 拿到JWT中的Audience
	tokenAudiences := authenticator.Audiences(public.Audience)
	if len(tokenAudiences) == 0 {
		// only apiserver audiences are allowed for legacy tokens
		audit.AddAuditAnnotation(ctx, "authentication.k8s.io/legacy-token", public.Subject)
		legacyTokensTotal.WithContext(ctx).Inc()
		tokenAudiences = j.implicitAuds
	}

	// 获取当前请求的Audience
	requestedAudiences, ok := authenticator.AudiencesFrom(ctx)
	if !ok {
		// default to apiserver audiences
		requestedAudiences = j.implicitAuds
	}

	// 获取tokenAudiences和requestedAudiences的交集
	auds := authenticator.Audiences(tokenAudiences).Intersect(requestedAudiences)
	// 说明当前请求的Audience不正确
	if len(auds) == 0 && len(j.implicitAuds) != 0 {
		return nil, false, fmt.Errorf("token audiences %q is invalid for the target audiences %q", tokenAudiences, requestedAudiences)
	}

	// If we get here, we have a token with a recognized signature and issuer string.
	// JWT认真通过之后，还需要检验JWT中所指向的Secret，ServiceAccount是否合法
	sa, err := j.validator.Validate(ctx, tokenData, public, private)
	if err != nil {
		return nil, false, err
	}

	return &authenticator.Response{
		User:      sa.UserInfo(),
		Audiences: auds,
	}, true, nil
}

// hasCorrectIssuer returns true if tokenData is a valid JWT in compact
// serialization format and the "iss" claim matches the iss field of this token
// authenticator, and otherwise returns false.
//
// Note: go-jose currently does not allow access to unverified JWS payloads.
// See https://github.com/square/go-jose/issues/169
func (j *jwtTokenAuthenticator) hasCorrectIssuer(tokenData string) bool {
	parts := strings.Split(tokenData, ".")
	// 如果是一个有效的JWT，那么肯定是三段
	if len(parts) != 3 {
		return false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	claims := struct {
		// WARNING: this JWT is not verified. Do not trust these claims.
		Issuer string `json:"iss"`
	}{}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return false
	}
	return j.issuers[claims.Issuer]
}
