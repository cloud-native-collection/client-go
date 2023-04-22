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

package spdy

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/util/httpstream/spdy"
	restclient "k8s.io/client-go/rest"
)

// Upgrader validates a response from the server after a SPDY upgrade.
// 校验升级spdy后服务端的响应
type Upgrader interface {
	// NewConnection validates the response and creates a new Connection.
	NewConnection(resp *http.Response) (httpstream.Connection, error)
}

// RoundTripperFor returns a round tripper and upgrader to use with SPDY.
// 返回一个用于 SPDY 的传输器（round tripper）和升级器（upgrader）
// SPDY 是由 Google 开发的一种协议，旨在通过使用多路复用、优先级和压缩的数据流来减少网页的延迟。
// RoundTripperFor 函数可用于创建一个支持 SPDY 的传输器round tripper)。该传输器可以用于在 SPDY 上进行 HTTP 请求和接收 HTTP 响应。
// RoundTripperFor 返回的升级器(upgrader)可用于将现有的 HTTP 连接升级到 SPDY。
func RoundTripperFor(config *restclient.Config) (http.RoundTripper, Upgrader, error) {
	// TLS 配置对象
	tlsConfig, err := restclient.TLSConfigFor(config)
	if err != nil {
		return nil, nil, err
	}
	// 创建一个 HTTP 代理
	proxy := http.ProxyFromEnvironment
	if config.Proxy != nil {
		proxy = config.Proxy
	}
	// 创建一个 SPDY 传输器
	upgradeRoundTripper := spdy.NewRoundTripperWithConfig(spdy.RoundTripperConfig{
		TLS:        tlsConfig,
		Proxier:    proxy,
		PingPeriod: time.Second * 5,
	})
	// 为传输器添加一些必要的 HTTP 包装器
	wrapper, err := restclient.HTTPWrappersForConfig(config, upgradeRoundTripper)
	if err != nil {
		return nil, nil, err
	}
	return wrapper, upgradeRoundTripper, nil
}

// dialer implements the httpstream.Dialer interface.
// 实现接口k8s.io/apimachinery/pkg/util/httpstream
// https://github.com/kubernetes/apimachinery/blob/master/pkg/util/httpstream/httpstream.go#L44
// 定义了一个方法 Dial，用于建立到远程服务器的 TCP 连接，并返回一个实现了 io.ReadWriteCloser 接口的对象，
// 该对象可以用于在连接上发送和接收数据。
// 用于在 Kubernetes 中创建到 API 服务器的长连接，以便在 Kubernetes 集群中进行资源的创建、修改、删除等操作。
// 使用HTTP-over-SPDY 协议来实现 API 服务器与客户端之间的通信，httpstream.Dialer 则提供一个方便的方式来管理这些长连接。
type dialer struct {
	client   *http.Client
	upgrader Upgrader
	method   string
	url      *url.URL
}

var _ httpstream.Dialer = &dialer{}

// NewDialer will create a dialer that connects to the provided URL and upgrades the connection to SPDY.
func NewDialer(upgrader Upgrader, client *http.Client, method string, url *url.URL) httpstream.Dialer {
	return &dialer{
		client:   client,
		upgrader: upgrader,
		method:   method,
		url:      url,
	}
}

func (d *dialer) Dial(protocols ...string) (httpstream.Connection, string, error) {
	req, err := http.NewRequest(d.method, d.url.String(), nil)
	if err != nil {
		return nil, "", fmt.Errorf("error creating request: %v", err)
	}
	return Negotiate(d.upgrader, d.client, req, protocols...)
}

// Negotiate opens a connection to a remote server and attempts to negotiate
// a SPDY connection. Upon success, it returns the connection and the protocol selected by
// the server. The client transport must use the upgradeRoundTripper - see RoundTripperFor.
// 用于与远程服务器建立 SPDY 连接。
// 该函数使用传输器升级机制，即先使用 HTTP 协议发送一个带有协议升级请求头的请求，
// 如果远程服务器支持 SPDY，会响应一个带有 SPDY 协议版本号的响应头，此后双方就可以使用 SPDY 协议进行通信。
func Negotiate(upgrader Upgrader, client *http.Client, req *http.Request, protocols ...string) (httpstream.Connection, string, error) {
	// 请求头中添加协议升级请求头
	for i := range protocols {
		req.Header.Add(httpstream.HeaderProtocolVersion, protocols[i])
	}
	// 发送 HTTP 请求，并等待远程服务器的响应。
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("error sending request: %v", err)
	}
	defer resp.Body.Close()
	// 从响应头中获取 SPDY 协议版本号，Upgrader 对象创建一个新的 httpstream.Connection 对象
	conn, err := upgrader.NewConnection(resp)
	if err != nil {
		return nil, "", err
	}
	return conn, resp.Header.Get(httpstream.HeaderProtocolVersion), nil
}
