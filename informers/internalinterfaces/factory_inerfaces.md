```go

package internalinterfaces

import (
	time "time"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	kubernetes "k8s.io/client-go/kubernetes"
	cache "k8s.io/client-go/tools/cache"
)

// NewInformerFunc takes kubernetes.Interface and time.Duration to return a SharedIndexInformer.
// 这个函数定义就是具体类型Informer的构造函数
type NewInformerFunc func(kubernetes.Interface, time.Duration) cache.SharedIndexInformer

// SharedInformerFactory a small interface to allow for adding an informer without an import cycle
// 通过对象类型构造Informer。因为SharedInformerFactory管理的就是SharedIndexInformer对象，
// SharedIndexInformer存储的对象类型决定了他是什么Informer，致使SharedInformerFactory无需知道具体的Informer如何构造，
// 所以需要外部传入构造函数，这样可以减低耦合性
type SharedInformerFactory interface {
	// 核心逻辑函数
	Start(stopCh <-chan struct{})
	// 通过对象类型，返回SharedIndexInformer，这个SharedIndexInformer管理的就是指定的对象
	// NewInformerFunc用于当SharedInformerFactory没有这个类型的Informer的时候创建使用
	InformerFor(obj runtime.Object, newFunc NewInformerFunc) cache.SharedIndexInformer
}

// TweakListOptionsFunc is a function that transforms a v1.ListOptions.
// 创建Informer的函数定义，这个函数需要apiserver的客户端以及同步周期，
// 这个同步周期SharedInformers反复使用
type TweakListOptionsFunc func(*v1.ListOptions)

```