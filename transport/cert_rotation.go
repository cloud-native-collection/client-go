/*
Copyright 2020 The Kubernetes Authors.

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
	"bytes"
	"crypto/tls"
	"fmt"
	"reflect"
	"sync"
	"time"

	utilnet "k8s.io/apimachinery/pkg/util/net"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/connrotation"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

const workItemKey = "key"

// CertCallbackRefreshDuration is exposed so that integration tests can crank up the reload speed.
// 控制证书回调函数（certCallback）的刷新速度,
// Kubernetes的TLS客户端配置中，可以指定一个证书回调函数，用于在进行TLS握手时验证服务器端的证书。这个回调函数可以定期刷新证书，以确保证书的有效性。
var CertCallbackRefreshDuration = 5 * time.Minute

type reloadFunc func(*tls.CertificateRequestInfo) (*tls.Certificate, error)

// 动态地管理客户端证书，当证书过期或失效时，该结构体可以自动重新加载证书。
type dynamicClientCert struct {
	// 户端证书
	clientCert *tls.Certificate
	certMtx    sync.RWMutex

	// 重新加载客户端证书的函数
	reload reloadFunc
	// 重新加载证书时创建新TCP连接的dialer
	connDialer *connrotation.Dialer

	// queue only ever has one item, but it has nice error handling backoff/retry semantics
	// 存储证书刷新任务的队列,确保只有一个任务在运行，并且在出现错误时具有回退和重试的语义
	queue workqueue.RateLimitingInterface
}

func certRotatingDialer(reload reloadFunc, dial utilnet.DialFunc) *dynamicClientCert {
	d := &dynamicClientCert{
		reload:     reload,
		connDialer: connrotation.NewDialer(connrotation.DialFunc(dial)),
		queue:      workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "DynamicClientCertificate"),
	}

	return d
}

// loadClientCert calls the callback and rotates connections if needed
// 调用回调函数并在需要时旋转连接
func (c *dynamicClientCert) loadClientCert() (*tls.Certificate, error) {
	cert, err := c.reload(nil)
	if err != nil {
		return nil, err
	}

	// check to see if we have a change. If the values are the same, do nothing.
	// 判断证书是否相同
	c.certMtx.RLock()
	haveCert := c.clientCert != nil
	if certsEqual(c.clientCert, cert) {
		c.certMtx.RUnlock()
		return c.clientCert, nil
	}
	c.certMtx.RUnlock()

	// 更新证书
	c.certMtx.Lock()
	c.clientCert = cert
	c.certMtx.Unlock()

	// The first certificate requested is not a rotation that is worth closing connections for
	// 第一个请求的证书不需要重新加载，因此无需关闭连接
	if !haveCert {
		return cert, nil
	}

	// 检测到证书刷新，需要关闭TCP连接并重新启动使用新的证书
	// TCP连接是与证书绑定的，因此在证书发生变化时，需要关闭所有的TCP连接，以便使用新的证书创建新的TCP连接。
	// 为了避免服务中断，第一个请求的证书不会被重新加载，只有第二个请求开始才会使用新的证书创建TCP连接。
	klog.V(1).Infof("certificate rotation detected, shutting down client connections to start using new credentials")
	c.connDialer.CloseAll()

	return cert, nil
}

// certsEqual compares tls Certificates, ignoring the Leaf which may get filled in dynamically
func certsEqual(left, right *tls.Certificate) bool {
	if left == nil || right == nil {
		return left == right
	}

	if !byteMatrixEqual(left.Certificate, right.Certificate) {
		return false
	}

	if !reflect.DeepEqual(left.PrivateKey, right.PrivateKey) {
		return false
	}

	if !byteMatrixEqual(left.SignedCertificateTimestamps, right.SignedCertificateTimestamps) {
		return false
	}

	if !bytes.Equal(left.OCSPStaple, right.OCSPStaple) {
		return false
	}

	return true
}

func byteMatrixEqual(left, right [][]byte) bool {
	if len(left) != len(right) {
		return false
	}

	for i := range left {
		if !bytes.Equal(left[i], right[i]) {
			return false
		}
	}
	return true
}

// Run starts the controller and blocks until stopCh is closed.
// 启动controller 并阻塞直到stopCh关闭
func (c *dynamicClientCert) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	klog.V(3).Infof("Starting client certificate rotation controller")
	defer klog.V(3).Infof("Shutting down client certificate rotation controller")

	go wait.Until(c.runWorker, time.Second, stopCh)

	go wait.PollImmediateUntil(CertCallbackRefreshDuration, func() (bool, error) {
		c.queue.Add(workItemKey)
		return false, nil
	}, stopCh)

	<-stopCh
}

func (c *dynamicClientCert) runWorker() {
	for c.processNextWorkItem() {
	}
}

// 从队列中获取证书刷新任务
func (c *dynamicClientCert) processNextWorkItem() bool {
	dsKey, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(dsKey)

	_, err := c.loadClientCert()
	if err == nil {
		c.queue.Forget(dsKey)
		return true
	}

	utilruntime.HandleError(fmt.Errorf("%v failed with : %v", dsKey, err))
	c.queue.AddRateLimited(dsKey)

	return true
}

// GetClientCertificate 获取证书
func (c *dynamicClientCert) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return c.loadClientCert()
}
