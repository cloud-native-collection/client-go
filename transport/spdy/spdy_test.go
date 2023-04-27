package spdy

import (
	"golang.org/x/net/http/spdy"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/rest"
	"net/http"
	"testing"
)

func TestServer() {

}

func TestRoundTripper() {
	// 创建一个新的 Kubernetes 客户端配置
	config := &rest.Config{
		Host: "https://api.example.com",
		// 其他配置项
	}

	// 使用 RoundTripperFor 函数创建一个支持 SPDY 的传输器和升级器
	roundTripper, upgrader, err := spdy.RoundTripperFor(config)
	if err != nil {
		// 处理错误
	}

	// 使用传输器发送 HTTP 请求
	// ...

	// 使用升级器将 HTTP 连接升级到 SPDY
	// ...
}

func TestNegotiate(t *testing.T) {
	// 创建一个新的 HTTP 客户端
	client := &http.Client{}

	// 创建一个新的 HTTP 请求
	req, err := http.NewRequest("GET", "https://api.example.com", nil)
	if err != nil {
		// 处理错误
	}

	// 添加协议升级请求头
	protocols := []string{"spdy/3.1"}
	for i := range protocols {
		req.Header.Add(httpstream.HeaderProtocolVersion, protocols[i])
	}

	// 使用 Negotiate 函数建立 SPDY 连接
	upgrader := spdy.NewRoundTripperWithConfig(spdy.RoundTripperConfig{
		TLS: tlsConfig,
		// 其他配置项
	})
	conn, protocol, err := spdy.Negotiate(upgrader, client, req, protocols...)
	if err != nil {
		// 处理错误
	}

	// 使用 conn 对象进行 SPDY 通信
	// ...
}
