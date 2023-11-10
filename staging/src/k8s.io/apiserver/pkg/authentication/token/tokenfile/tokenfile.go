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

package tokenfile

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strings"

	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/klog/v2"
)

// TokenAuthenticator
// 1、静态Token认证实现，用于完成静态Token的认证。
// 2、map信息是从配置文件中获取的，每一行的格式为：token,user,uid,"group1,group2,group3"，譬如：d13a7184-6df5-11ee-bdac-4f1000e576bf,tom01,1001,"dev,cloud"
// 3、APIServer启动时候就会加载--token-auth-file=SOMEFILE配置指向的文件，所以只要请求中的Token在此配置文件中，就认为认证通过。只要
// 不再这个配置文件中，就认为认证不通过。
type TokenAuthenticator struct {
	// 1、key为token, Value为用户信息
	tokens map[string]*user.DefaultInfo
}

// New returns a TokenAuthenticator for a single token
func New(tokens map[string]*user.DefaultInfo) *TokenAuthenticator {
	return &TokenAuthenticator{
		tokens: tokens,
	}
}

// NewCSV returns a TokenAuthenticator, populated from a CSV file.
// The CSV file must contain records in the format "token,username,useruid"
// 这里的path其实就静态Token配置文件
func NewCSV(path string) (*TokenAuthenticator, error) {
	// 读取配置文件
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	recordNum := 0
	tokens := make(map[string]*user.DefaultInfo)
	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	for {
		// 一行一行的读取
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(record) < 3 {
			return nil, fmt.Errorf("token file '%s' must have at least 3 columns (token, user name, user uid), found %d", path, len(record))
		}

		recordNum++
		if record[0] == "" {
			klog.Warningf("empty token has been found in token file '%s', record number '%d'", path, recordNum)
			continue
		}

		obj := &user.DefaultInfo{
			Name: record[1],
			UID:  record[2],
		}
		if _, exist := tokens[record[0]]; exist {
			klog.Warningf("duplicate token has been found in token file '%s', record number '%d'", path, recordNum)
		}
		tokens[record[0]] = obj

		if len(record) >= 4 {
			obj.Groups = strings.Split(record[3], ",")
		}
	}

	return &TokenAuthenticator{
		tokens: tokens,
	}, nil
}

func (a *TokenAuthenticator) AuthenticateToken(ctx context.Context, token string) (*authenticator.Response, bool, error) {
	// 只要在配置文件中，就认为认证通过
	user, ok := a.tokens[token]
	if !ok {
		// 否则认为、认证失败
		return nil, false, nil
	}
	return &authenticator.Response{User: user}, true, nil
}
