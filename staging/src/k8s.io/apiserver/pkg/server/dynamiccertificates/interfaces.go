/*
Copyright 2021 The Kubernetes Authors.

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

package dynamiccertificates

import (
	"crypto/x509"
)

// Listener is an interface to use to notify interested parties of a change.
type Listener interface {
	// Enqueue should be called when an input may have changed
	Enqueue()
}

// Notifier is a way to add listeners
type Notifier interface {
	// AddListener is adds a listener to be notified of potential input changes.
	// This is a noop on static providers.
	AddListener(listener Listener)
}

// CAContentProvider provides ca bundle byte content
// 1、CAContentProvider用于获取CA证书的内容以及证书的X509校验参数
// 2、其原理就是CAContentProvider会一直持续不断的监视CA的变化（通过是通过Informer机制持续不断的监视），一旦发现CAContentProvider发现
// CA内容发生变化，就会循环依次遍历每个Listener，让每个Listener都知道证书发生变化了，此时Listener可以通过CAContentProvider的CurrentCABundleContent
// 获取证书的内容。同时也可以通过CAContentProvider的VerifyOptions获取证书校验参数。
/*
3、作为使用者，使用者一般是组合CAContentProvider，并在实例化的时候向CAContentProvider注册自己，也就是使用者就是所谓的Listener
type Component struct {
	caProvider CAContentProvider
	caBundle []byte
	name string
	xxx Int
	...
}

func NewComponent(name string, caProvider CAContentProvider) *Component{
	comp := &Component{
		caProvider: caProvider
		name: name
	}
	// 向CAContentProvider注册自己
	caProvider.AddListener(comp)

	return comp
}

// 实现Listener接口，当证书变化时获取证书的内容。这里仅仅是简单的进行了属性赋值。实际上这里组件可以根据证书变化干许多事情。
func (c *Component) Enqueue() {
	c.caBundle = c.caProvider.CurrentCABundleContent()
}
*/
// TODO 4、总结这种设计模式 CAContentProvider的核心目标就是为了获取最新的CABundle。 因此我们设计CurrentCABundleContent让用户可以获取
// CABundle内容，其次设计VerifyOptions让用户可以获取到证书的X509验证参数。 为了让关心CABundle变化的组件可以感知到，设计了Notifier接口。
// 让关心CABundle变化的组件全部都注册进来，一旦CABundle发生变化，CAContentProvider就有责任通知每一个组件。
type CAContentProvider interface {
	// Notifier 证书发生变化时，Provider应该通知每一个关注证书变化的组件
	Notifier

	// Name is just an identifier.
	Name() string
	// CurrentCABundleContent provides ca bundle byte content. Errors can be
	// contained to the controllers initializing the value. By the time you get
	// here, you should always be returning a value that won't fail.
	// CA Bundle是指证书颁发机构（Certificate Authority，简称CA）证书的集合。该集合用于验证域名的安全性，确保网站与用户之间的通信
	// 不会被第三方窃取或篡改。CA Bundle通常包含多个证书，每个证书都是一个公共CA所颁发的证书，它们可以用于确认网站证书的真实性和安全性。
	CurrentCABundleContent() []byte
	// VerifyOptions provides VerifyOptions for authenticators.
	// 获取证书的X509校验参数，可以用来校验证书
	VerifyOptions() (x509.VerifyOptions, bool)
}

// CertKeyContentProvider provides a certificate and matching private key.
// 1、用于获取证书、私钥
// 2、CertKeyContentProvider和CAContentProvider设计思想基本上是一摸一样的，都是一样的套路，只不过想要获取的东西从CABundle变化为了
// 证书和私钥
type CertKeyContentProvider interface {
	Notifier

	// Name is just an identifier.
	Name() string
	// CurrentCertKeyContent provides cert and key byte content.
	CurrentCertKeyContent() ([]byte, []byte)
}

// SNICertKeyContentProvider provides a certificate and matching private key as
// well as optional explicit names.
type SNICertKeyContentProvider interface {
	Notifier

	CertKeyContentProvider
	// SNINames provides names used for SNI. May return nil.
	SNINames() []string
}
