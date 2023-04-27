/*
Copyright 2015 The Kubernetes Authors.

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

package transport

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
)

// Config holds various options for establishing a transport.
// 用于建立传输连接的各种选项
type Config struct {
	// UserAgent is an optional field that specifies the caller of this
	// request.
	// 一个可选的字段,指定此请求的调用者
	UserAgent string

	// The base TLS configuration for this transport.
	// 传输的基本TLS配置。
	TLS TLSConfig

	// Username and password for basic authentication
	Username string
	Password string `datapolicy:"password"`

	// Bearer token for authentication
	// 用于身份验证的Bearer令牌。
	BearerToken string `datapolicy:"token"`

	// Path to a file containing a BearerToken.
	// If set, the contents are periodically read.
	// The last successfully read value takes precedence over BearerToken.
	// 包含BearerToken的文件的路径。
	// 如果设置，将定期读取其内容。
	// 最后成功读取的值优先于BearerToken。
	BearerTokenFile string

	// Impersonate is the config that this Config will impersonate using
	// Impersonate是此Config将使用的配置的代理。
	Impersonate ImpersonationConfig

	// DisableCompression bypasses automatic GZip compression requests to the
	// server.
	// DisableCompression绕过自动GZip压缩请求到服务器。
	DisableCompression bool

	// Transport may be used for custom HTTP behavior. This attribute may
	// not be specified with the TLS client certificate options. Use
	// WrapTransport for most client level operations.
	// Transport可用于自定义HTTP行为。此属性可能不与TLS客户端证书选项一起指定。
	// 对于大多数客户端级别的操作，请使用WrapTransport。
	Transport http.RoundTripper

	// WrapTransport will be invoked for custom HTTP behavior after the
	// underlying transport is initialized (either the transport created
	// from TLSClientConfig, Transport, or http.DefaultTransport). The
	// config may layer other RoundTrippers on top of the returned
	// RoundTripper.
	//
	// A future release will change this field to an array. Use config.Wrap()
	// instead of setting this value directly.
	// 在底层传输初始化后，WrapTransport将被调用以进行自定义HTTP行为
	//（无论是从TLSClientConfig、Transport还是http.DefaultTransport创建的传输）。
	// 配置可能在返回的RoundTripper上叠加其他RoundTripper。
	//
	// 未来的版本将将此字段更改为数组。请使用config.Wrap()代替直接设置此值。
	WrapTransport WrapperFunc

	// DialHolder specifies the dial function for creating unencrypted TCP connections.
	// This struct indirection is used to make transport configs cacheable.
	// DialHolder指定用于创建未加密TCP连接的拨号函数。
	// 此结构体间接引用用于使传输配置可缓存。
	DialHolder *DialHolder

	// Proxy is the proxy func to be used for all requests made by this
	// transport. If Proxy is nil, http.ProxyFromEnvironment is used. If Proxy
	// returns a nil *URL, no proxy is used.
	//
	// socks5 proxying does not currently support spdy streaming endpoints.
	// Proxy是要用于此传输发出的所有请求的代理函数。如果Proxy为nil，则使用http.ProxyFromEnvironment。
	// 如果代理返回一个nil *URL，则不使用代理。
	//
	// 目前不支持socks5代理对spdy流式端点的支持。
	Proxy func(*http.Request) (*url.URL, error)
}

// DialHolder is used to make the wrapped function comparable so that it can be used as a map key.
// 一个用于包装函数的类型，它被用作可比较的类型，以便它可以被用作映射的键。
// 在Go语言中，只有一些特定的类型才可以被用作映射的键，如整数、字符串、指针等。
// 如果要将一个函数作为映射的键，那么需要用一个可比较的类型来包装它。
type DialHolder struct {
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// ImpersonationConfig has all the available impersonation options
// 包含所有可用的模拟选项。
type ImpersonationConfig struct {
	// UserName matches user.Info.GetName()
	UserName string
	// UID matches user.Info.GetUID()
	UID string
	// Groups matches user.Info.GetGroups()
	Groups []string
	// Extra matches user.Info.GetExtra()
	Extra map[string][]string
}

// HasCA returns whether the configuration has a certificate authority or not.
func (c *Config) HasCA() bool {
	return len(c.TLS.CAData) > 0 || len(c.TLS.CAFile) > 0
}

// HasBasicAuth returns whether the configuration has basic authentication or not.
func (c *Config) HasBasicAuth() bool {
	return len(c.Username) != 0
}

// HasTokenAuth returns whether the configuration has token authentication or not.
func (c *Config) HasTokenAuth() bool {
	return len(c.BearerToken) != 0 || len(c.BearerTokenFile) != 0
}

// HasCertAuth returns whether the configuration has certificate authentication or not.
func (c *Config) HasCertAuth() bool {
	return (len(c.TLS.CertData) != 0 || len(c.TLS.CertFile) != 0) && (len(c.TLS.KeyData) != 0 || len(c.TLS.KeyFile) != 0)
}

// HasCertCallback returns whether the configuration has certificate callback or not.
func (c *Config) HasCertCallback() bool {
	return c.TLS.GetCertHolder != nil
}

// Wrap adds a transport middleware function that will give the caller
// an opportunity to wrap the underlying http.RoundTripper prior to the
// first API call being made. The provided function is invoked after any
// existing transport wrappers are invoked.
// 添加一个传输中间件函数，使调用者有机会在进行第一次API调用之前包装底层的http.RoundTripper。
// 提供的函数在调用任何现有传输包装器之后被调用。
func (c *Config) Wrap(fn WrapperFunc) {
	// Wrappers是一个函数，用于将多个WrapperFunc串联起来形成一个链式传输中间件。
	// 这里使用Wrappers函数将现有的WrapTransport和提供的函数fn进行串联。
	c.WrapTransport = Wrappers(c.WrapTransport, fn)
}

// TLSConfig holds the information needed to set up a TLS transport.
// 设置TLS传输所需的信息
type TLSConfig struct {
	// PEM编码的服务器信任根证书的路径。
	CAFile string // Path of the PEM-encoded server trusted root certificates.
	// PEM编码的客户端证书的路径
	CertFile string // Path of the PEM-encoded client certificate.
	// PEM编码的客户端密钥的路径
	KeyFile string // Path of the PEM-encoded client key.
	// 设置为指示原始配置提供了文件，并且应重新加载它们。
	ReloadTLSFiles bool // Set to indicate that the original config provided files, and that they should be reloaded

	// 仅用于测试，表示应该无需验证证书即可访问服务器
	Insecure bool // Server should be accessed without verifying the certificate. For testing only.
	// 用于SNI的服务器名称的覆盖，以及用于验证证书
	ServerName string // Override for the server name passed to the server for SNI and used to verify certificates.

	// PEM编码的服务器信任根证书的字节。替代CAFile
	CAData []byte // Bytes of the PEM-encoded server trusted root certificates. Supercedes CAFile.
	// PEM编码的客户端证书的字节。替代CertFile
	CertData []byte // Bytes of the PEM-encoded client certificate. Supercedes CertFile.
	// PEM编码的客户端密钥的字节。替代KeyFile
	KeyData []byte // Bytes of the PEM-encoded client key. Supercedes KeyFile.

	// NextProtos is a list of supported application level protocols, in order of preference.
	// Used to populate tls.Config.NextProtos.
	// To indicate to the server http/1.1 is preferred over http/2, set to ["http/1.1", "h2"] (though the server is free to ignore that preference).
	// To use only http/1.1, set to ["http/1.1"].
	// NextProtos是支持的应用层协议列表，按优先级排序。
	// 用于填充tls.Config.NextProtos。
	// 若要指示服务器优先使用http/1.1而不是http/2，请设置为["http/1.1", "h2"]（但服务器可以忽略该偏好）。
	// 若要仅使用http/1.1，请设置为["http/1.1"]。
	NextProtos []string

	// Callback that returns a TLS client certificate. CertData, CertFile, KeyData and KeyFile supercede this field.
	// This struct indirection is used to make transport configs cacheable.
	// 返回TLS客户端证书的回调函数。CertData、CertFile、KeyData和KeyFile替代此字段。
	// 此结构体间接引用用于使传输配置可缓存。
	GetCertHolder *GetCertHolder
}

// GetCertHolder is used to make the wrapped function comparable so that it can be used as a map key.
// 包装函数的类型，它被用作可比较的类型，以便它可以被用作映射的键
type GetCertHolder struct {
	GetCert func() (*tls.Certificate, error)
}
