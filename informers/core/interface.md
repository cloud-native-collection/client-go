```go

package core

import (
	v1 "k8s.io/client-go/informers/core/v1"
	internalinterfaces "k8s.io/client-go/informers/internalinterfaces"
)

// Interface provides access to each of this group's versions.
type Interface interface {
	// V1 provides access to shared informers for resources in V1.
	// 版本
	V1() v1.Interface
}

// Interface的实现类,供外部使用，group:Core是内核Informer的分组
type group struct {
	// 工厂对象的指针
	factory          internalinterfaces.SharedInformerFactory
	// Core这个分组对于SharedInformerFactory来说只有以下两个选项
	namespace        string
	tweakListOptions internalinterfaces.TweakListOptionsFunc
}

// New returns a new Interface.
// 构造Interface的接口
func New(f internalinterfaces.SharedInformerFactory, namespace string, tweakListOptions internalinterfaces.TweakListOptionsFunc) Interface {
	return &group{factory: f, namespace: namespace, tweakListOptions: tweakListOptions}
}

// V1 returns a new v1.Interface.
// 实现V1()的接口函数
func (g *group) V1() v1.Interface {
	// 通过调用v1包的New()函数实现
	return v1.New(g.factory, g.namespace, g.tweakListOptions)
}

```