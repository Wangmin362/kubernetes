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

// Package operationexecutor implements interfaces that enable execution of
// register and unregister operations with a
// goroutinemap so that more than one operation is never triggered
// on the same plugin.
package operationexecutor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"k8s.io/klog/v2"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/client-go/tools/record"
	registerapi "k8s.io/kubelet/pkg/apis/pluginregistration/v1"
	"k8s.io/kubernetes/pkg/kubelet/pluginmanager/cache"
)

const (
	dialTimeoutDuration   = 10 * time.Second
	notifyTimeoutDuration = 5 * time.Second
)

var _ OperationGenerator = &operationGenerator{}

type operationGenerator struct {

	// recorder is used to record events in the API server
	recorder record.EventRecorder
}

// NewOperationGenerator is returns instance of operationGenerator
func NewOperationGenerator(recorder record.EventRecorder) OperationGenerator {

	return &operationGenerator{
		recorder: recorder,
	}
}

// OperationGenerator interface that extracts out the functions from operation_executor to make it dependency injectable
type OperationGenerator interface {
	// Generates the RegisterPlugin function needed to perform the registration of a plugin
	GenerateRegisterPluginFunc(
		socketPath string,
		timestamp time.Time,
		pluginHandlers map[string]cache.PluginHandler,
		actualStateOfWorldUpdater ActualStateOfWorldUpdater) func() error

	// Generates the UnregisterPlugin function needed to perform the unregistration of a plugin
	GenerateUnregisterPluginFunc(
		pluginInfo cache.PluginInfo,
		actualStateOfWorldUpdater ActualStateOfWorldUpdater) func() error
}

func (og *operationGenerator) GenerateRegisterPluginFunc(
	socketPath string,
	timestamp time.Time,
	pluginHandlers map[string]cache.PluginHandler,
	actualStateOfWorldUpdater ActualStateOfWorldUpdater) func() error {

	registerPluginFunc := func() error {
		// 建立和kubelet插件的链接
		client, conn, err := dial(socketPath, dialTimeoutDuration)
		if err != nil {
			return fmt.Errorf("RegisterPlugin error -- dial failed at socket %s, err: %v", socketPath, err)
		}
		defer conn.Close()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		// 获取插件的注册信息
		infoResp, err := client.GetInfo(ctx, &registerapi.InfoRequest{})
		if err != nil {
			return fmt.Errorf("RegisterPlugin error -- failed to get plugin info using RPC GetInfo at socket %s, err: %v", socketPath, err)
		}

		// 获取插件注册回调函数
		handler, ok := pluginHandlers[infoResp.Type]
		if !ok {
			// 如果没有找到当前插件注册类型的回调函数，就要通知插件是现房，注册失败
			if err := og.notifyPlugin(client, false, fmt.Sprintf("RegisterPlugin error -- no handler registered for plugin type: %s at socket %s", infoResp.Type, socketPath)); err != nil {
				return fmt.Errorf("RegisterPlugin error -- failed to send error at socket %s, err: %v", socketPath, err)
			}
			return fmt.Errorf("RegisterPlugin error -- no handler registered for plugin type: %s at socket %s", infoResp.Type, socketPath)
		}

		// 1、如果没有指定socket路径，就认为当前的socket所在服务实现了插件的接口
		// 2、不同的插件需要实现的接口不一样，譬如，对于DevicePlugin需要实现设备插件相关的接口，而对于CSI类型的插件则需要实现CSI Spec的
		// NodeService接口用于对卷的格式化、挂载、卸载、扩容等操作
		// 3、虽然这一步看起来挺坑人的插件服务socket在不写的情况下就是注册socket，但是仔细一想，其实作为插件的实现方，我们本身就应该显示的指定
		// 服务socket所在路径，即使当前注册socket本身就实现了插件服务相关的接口，这种情况下，我们也可以声明为注册socket路径，仅此而已。
		if infoResp.Endpoint == "" {
			infoResp.Endpoint = socketPath
		}
		// 校验插件，DevicePlugin, DraPlugin, CSIPlugin插件的校验路径是不一样的
		if err := handler.ValidatePlugin(infoResp.Name, infoResp.Endpoint, infoResp.SupportedVersions); err != nil {
			if err = og.notifyPlugin(client, false, fmt.Sprintf("RegisterPlugin error -- plugin validation failed with err: %v", err)); err != nil {
				return fmt.Errorf("RegisterPlugin error -- failed to send error at socket %s, err: %v", socketPath, err)
			}
			return fmt.Errorf("RegisterPlugin error -- pluginHandler.ValidatePluginFunc failed")
		}
		// We add the plugin to the actual state of world cache before calling a plugin consumer's Register handle
		// so that if we receive a delete event during Register Plugin, we can process it as a DeRegister call.
		// 向实际状态插件状态注册当前插件，只要没有想同路径的socket，就认为注册成功
		err = actualStateOfWorldUpdater.AddPlugin(cache.PluginInfo{
			SocketPath: socketPath, // 这里的socket路径还是注册socket，因为只要知道了这个socket，我们就可以调用接口获取服务socket的位置
			Timestamp:  timestamp,
			Handler:    handler,
			Name:       infoResp.Name,
		})
		if err != nil {
			klog.ErrorS(err, "RegisterPlugin error -- failed to add plugin", "path", socketPath)
		}
		// 1、这里才是真正插件注册的地方，不同类型的插件会有一个插件管理器，用于维护插件状态
		if err := handler.RegisterPlugin(infoResp.Name, infoResp.Endpoint, infoResp.SupportedVersions); err != nil {
			// 注册有问题，就通知注册socket，插件注册失败
			return og.notifyPlugin(client, false, fmt.Sprintf("RegisterPlugin error -- plugin registration failed with err: %v", err))
		}

		// Notify is called after register to guarantee that even if notify throws an error Register will always be called after validate
		// 注册没有问题，就通知注册socket，插件注册成功
		if err := og.notifyPlugin(client, true, ""); err != nil {
			return fmt.Errorf("RegisterPlugin error -- failed to send registration status at socket %s, err: %v", socketPath, err)
		}
		return nil
	}
	return registerPluginFunc
}

func (og *operationGenerator) GenerateUnregisterPluginFunc(
	pluginInfo cache.PluginInfo,
	actualStateOfWorldUpdater ActualStateOfWorldUpdater) func() error {

	unregisterPluginFunc := func() error {
		// 如果handler不存在，那么无法完成注销动作
		if pluginInfo.Handler == nil {
			return fmt.Errorf("UnregisterPlugin error -- failed to get plugin handler for %s", pluginInfo.SocketPath)
		}
		// We remove the plugin to the actual state of world cache before calling a plugin consumer's Unregister handle
		// so that if we receive a register event during Register Plugin, we can process it as a Register call.
		// 从缓存中删除
		actualStateOfWorldUpdater.RemovePlugin(pluginInfo.SocketPath)

		// 注销插件
		pluginInfo.Handler.DeRegisterPlugin(pluginInfo.Name)

		klog.V(4).InfoS("DeRegisterPlugin called", "pluginName", pluginInfo.Name, "pluginHandler", pluginInfo.Handler)
		return nil
	}
	return unregisterPluginFunc
}

func (og *operationGenerator) notifyPlugin(client registerapi.RegistrationClient, registered bool, errStr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), notifyTimeoutDuration)
	defer cancel()

	status := &registerapi.RegistrationStatus{
		PluginRegistered: registered,
		Error:            errStr,
	}

	if _, err := client.NotifyRegistrationStatus(ctx, status); err != nil {
		return fmt.Errorf("%s: %w", errStr, err)
	}

	if errStr != "" {
		return errors.New(errStr)
	}

	return nil
}

// Dial establishes the gRPC communication with the picked up plugin socket. https://godoc.org/google.golang.org/grpc#Dial
func dial(unixSocketPath string, timeout time.Duration) (registerapi.RegistrationClient, *grpc.ClientConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	c, err := grpc.DialContext(ctx, unixSocketPath,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
		grpc.WithContextDialer(func(ctx context.Context, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", addr)
		}),
	)

	if err != nil {
		return nil, nil, fmt.Errorf("failed to dial socket %s, err: %v", unixSocketPath, err)
	}

	return registerapi.NewRegistrationClient(c), c, nil
}
