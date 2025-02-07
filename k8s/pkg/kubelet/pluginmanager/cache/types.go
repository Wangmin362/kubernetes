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

package cache

// PluginHandler is an interface a client of the pluginwatcher API needs to implement in
// order to consume plugins
// The PluginHandler follows the simple following state machine:
//
//	                       +--------------------------------------+
//	                       |            ReRegistration            |
//	                       | Socket created with same plugin name |
//	                       |                                      |
//	                       |                                      |
//	  Socket Created       v                                      +        Socket Deleted
//	+------------------> Validate +---------------------------> Register +------------------> DeRegister
//	                       +                                      +                              +
//	                       |                                      |                              |
//	                       | Error                                | Error                        |
//	                       |                                      |                              |
//	                       v                                      v                              v
//	                      Out                                    Out                            Out
//
// The pluginwatcher module follows strictly and sequentially this state machine for each *plugin name*.
// e.g: If you are Registering a plugin foo, you cannot get a DeRegister call for plugin foo
// until the Register("foo") call returns. Nor will you get a Validate("foo", "Different endpoint", ...)
// call until the Register("foo") call returns.
//
// ReRegistration: Socket created with same plugin name, usually for a plugin update
// e.g: plugin with name foo registers at foo.com/foo-1.9.7 later a plugin with name foo
// registers at foo.com/foo-1.9.9
//
// DeRegistration: When ReRegistration happens only the deletion of the new socket will trigger a DeRegister call
// TODO 如何理解这个接口的定义？
type PluginHandler interface {
	// ValidatePlugin Validate returns an error if the information provided by
	// the potential plugin is erroneous (unsupported version, ...)
	ValidatePlugin(pluginName string, endpoint string, versions []string) error
	// RegisterPlugin is called so that the plugin can be register by any
	// plugin consumer
	// Error encountered here can still be Notified to the plugin.
	// 1、pluginName为插件的名字，endpoint为插件的服务socket监听路径， version为当前插件支持的版本
	RegisterPlugin(pluginName, endpoint string, versions []string) error
	// DeRegisterPlugin is called once the pluginwatcher observes that the socket has
	// been deleted.
	// 当插件所监听的socket文件被移除时，该插件就会被注销
	DeRegisterPlugin(pluginName string)
}
