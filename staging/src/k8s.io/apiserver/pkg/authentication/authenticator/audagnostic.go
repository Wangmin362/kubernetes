/*
Copyright 2018 The Kubernetes Authors.

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
	"context"
	"fmt"
	"net/http"
)

// 认证一个请求，步骤如下：
// 1、从当前请求的上下文当中获取当前请求的Audience,如果没有获取到Audience，那么委托给authenticate进行认证
// 2、如果从请求上下文中取到了Audience，那么和和K8S合法的Audience做交集，如果交集为空，直接认为认证失败
// 3、否则将请求委托给authenticate进行认证，如果认证有错误，就认为认证失败
func authenticate(ctx context.Context, implicitAuds Audiences, authenticate func() (*Response, bool, error)) (*Response, bool, error) {
	// 获取当前请求的Audience
	targetAuds, ok := AudiencesFrom(ctx)
	// We can remove this once api audiences is never empty. That will probably
	// be N releases after TokenRequest is GA.
	if !ok {
		// 如果取不到Audience，那么委托给authenticate进行认证
		return authenticate()
	}
	auds := implicitAuds.Intersect(targetAuds)
	if len(auds) == 0 {
		return nil, false, nil
	}
	resp, ok, err := authenticate()
	if err != nil || !ok {
		return nil, false, err
	}
	if len(resp.Audiences) > 0 {
		// maybe the authenticator was audience aware after all.
		return nil, false, fmt.Errorf("audience agnostic authenticator wrapped an authenticator that returned audiences: %q", resp.Audiences)
	}
	resp.Audiences = auds
	return resp, true, nil
}

type audAgnosticRequestAuthenticator struct {
	implicit Audiences
	delegate Request
}

var _ = Request(&audAgnosticRequestAuthenticator{})

func (a *audAgnosticRequestAuthenticator) AuthenticateRequest(req *http.Request) (*Response, bool, error) {
	return authenticate(req.Context(), a.implicit, func() (*Response, bool, error) {
		return a.delegate.AuthenticateRequest(req)
	})
}

// WrapAudienceAgnosticRequest wraps an audience agnostic request authenticator
// to restrict its accepted audiences to a set of implicit audiences.
func WrapAudienceAgnosticRequest(implicit Audiences, delegate Request) Request {
	return &audAgnosticRequestAuthenticator{
		implicit: implicit,
		delegate: delegate,
	}
}

type audAgnosticTokenAuthenticator struct {
	implicit Audiences
	delegate Token
}

var _ = Token(&audAgnosticTokenAuthenticator{})

func (a *audAgnosticTokenAuthenticator) AuthenticateToken(ctx context.Context, tok string) (*Response, bool, error) {
	return authenticate(ctx, a.implicit, func() (*Response, bool, error) {
		return a.delegate.AuthenticateToken(ctx, tok)
	})
}

// WrapAudienceAgnosticToken wraps an audience agnostic token authenticator to
// restrict its accepted audiences to a set of implicit audiences.
// TODO 这个wrapper是干嘛用的?
func WrapAudienceAgnosticToken(implicit Audiences, delegate Token) Token {
	return &audAgnosticTokenAuthenticator{
		implicit: implicit,
		delegate: delegate,
	}
}
