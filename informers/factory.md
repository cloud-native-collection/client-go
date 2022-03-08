```go
package informers

import (
	reflect "reflect"
	sync "sync"
	time "time"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	admissionregistration "k8s.io/client-go/informers/admissionregistration"
	apiserverinternal "k8s.io/client-go/informers/apiserverinternal"
	apps "k8s.io/client-go/informers/apps"
	autoscaling "k8s.io/client-go/informers/autoscaling"
	batch "k8s.io/client-go/informers/batch"
	certificates "k8s.io/client-go/informers/certificates"
	coordination "k8s.io/client-go/informers/coordination"
	core "k8s.io/client-go/informers/core"
	discovery "k8s.io/client-go/informers/discovery"
	events "k8s.io/client-go/informers/events"
	extensions "k8s.io/client-go/informers/extensions"
	flowcontrol "k8s.io/client-go/informers/flowcontrol"
	internalinterfaces "k8s.io/client-go/informers/internalinterfaces"
	networking "k8s.io/client-go/informers/networking"
	node "k8s.io/client-go/informers/node"
	policy "k8s.io/client-go/informers/policy"
	rbac "k8s.io/client-go/informers/rbac"
	scheduling "k8s.io/client-go/informers/scheduling"
	storage "k8s.io/client-go/informers/storage"
	kubernetes "k8s.io/client-go/kubernetes"
	cache "k8s.io/client-go/tools/cache"
)

// SharedInformerOption defines the functional option type for SharedInformerFactory.
// 回调函数 是一个函数指针，传入的是工厂指针，返回也是工厂指针
// 很明显，选项函数直接修改工厂对象，然后把修改的对象返回
// 把选项函数直接操作类成员，扩展比较容易
type SharedInformerOption func(*sharedInformerFactory) *sharedInformerFactory

type sharedInformerFactory struct {
	// apiserver的客户端
	client           kubernetes.Interface
	// 非必需
	namespace        string
	// 这是个函数指针，用来调整列举选项的，这个选项用来client列举对象使用
	tweakListOptions internalinterfaces.TweakListOptionsFunc
	// 互斥锁
	lock             sync.Mutex
	// 默认的同步周期，SharedInformer
	defaultResync    time.Duration
	// 每个类型的Informer有自己自定义的同步周期
	customResync     map[reflect.Type]time.Duration

	// 每类对象一个Informer，但凡使用SharedInformerFactory构建的Informer同一个类型其实都是同一个Informer
	informers map[reflect.Type]cache.SharedIndexInformer
	// startedInformers is used for tracking which informers have been started.
	// This allows Start() to be called multiple times safely.
	// 各种Informer启动的标记
	startedInformers map[reflect.Type]bool
}

// WithCustomResyncConfig sets a custom resync period for the specified informer types.
// 把每个对象类型的同步周期这个参数转换为SharedInformerOption类型
func WithCustomResyncConfig(resyncConfig map[v1.Object]time.Duration) SharedInformerOption {
	return func(factory *sharedInformerFactory) *sharedInformerFactory {
		for k, v := range resyncConfig {
			factory.customResync[reflect.TypeOf(k)] = v
		}
		return factory
	}
}

// WithTweakListOptions sets a custom filter on all listers of the configured SharedInformerFactory.
// 选项函数用于使用者自定义client的列举选项
func WithTweakListOptions(tweakListOptions internalinterfaces.TweakListOptionsFunc) SharedInformerOption {
	return func(factory *sharedInformerFactory) *sharedInformerFactory {
		factory.tweakListOptions = tweakListOptions
		return factory
	}
}

// WithNamespace limits the SharedInformerFactory to the specified namespace.
// 选项函数用来设置namesapce过滤
func WithNamespace(namespace string) SharedInformerOption {
	return func(factory *sharedInformerFactory) *sharedInformerFactory {
		factory.namespace = namespace
		return factory
	}
}

// NewSharedInformerFactory constructs a new instance of sharedInformerFactory for all namespaces.
// 通用的构造SharedInformerFactory的接口函数，没有任何其他的选项，只包含了apiserver的client以及同步周期
func NewSharedInformerFactory(client kubernetes.Interface, defaultResync time.Duration) SharedInformerFactory {
	return NewSharedInformerFactoryWithOptions(client, defaultResync)
}

// NewFilteredSharedInformerFactory constructs a new instance of sharedInformerFactory.
// Listers obtained via this SharedInformerFactory will be subject to the same filters
// as specified here.
// Deprecated: Please use NewSharedInformerFactoryWithOptions instead
func NewFilteredSharedInformerFactory(client kubernetes.Interface, defaultResync time.Duration, namespace string, tweakListOptions internalinterfaces.TweakListOptionsFunc) SharedInformerFactory {
	return NewSharedInformerFactoryWithOptions(client, defaultResync, WithNamespace(namespace), WithTweakListOptions(tweakListOptions))
}

// NewSharedInformerFactoryWithOptions constructs a new instance of a SharedInformerFactory with additional options.
// 构造SharedInformerFactory核心函数了，其实SharedInformerOption是个有意思的东西
// 我们写程序喜欢Option是个结构体，但是这种方式的扩展很麻烦，这里面用的是回调函数
func NewSharedInformerFactoryWithOptions(client kubernetes.Interface, defaultResync time.Duration, options ...SharedInformerOption) SharedInformerFactory {
	factory := &sharedInformerFactory{
		client:           client,
		namespace:        v1.NamespaceAll,
		defaultResync:    defaultResync,
		informers:        make(map[reflect.Type]cache.SharedIndexInformer),
		startedInformers: make(map[reflect.Type]bool),
		customResync:     make(map[reflect.Type]time.Duration),
	}

	// Apply all options
	// 逐一遍历各个选项函数，opt是选项函数
	for _, opt := range options {
		factory = opt(factory)
	}

	return factory
}

// Start initializes all requested informers.
// 启动所有具体类型的Informer
// 每个类型的Informer都是SharedIndexInformer，
// 需要需要把每个SharedIndexInformer都要启动起来
func (f *sharedInformerFactory) Start(stopCh <-chan struct{}) {
	f.lock.Lock()
	defer f.lock.Unlock()

	// 遍历informers这个map
	for informerType, informer := range f.informers {
		// informer是否已经启动过
		if !f.startedInformers[informerType] {
			// 如果没启动过，那就启动一个协程执行SharedIndexInformer的Run()函数，即启动controller
			go informer.Run(stopCh)
			// 设置Informer已经启动的标记
			f.startedInformers[informerType] = true
		}
	}
}

// WaitForCacheSync waits for all started informers' cache were synced.
func (f *sharedInformerFactory) WaitForCacheSync(stopCh <-chan struct{}) map[reflect.Type]bool {
	informers := func() map[reflect.Type]cache.SharedIndexInformer {
		f.lock.Lock()
		defer f.lock.Unlock()

		informers := map[reflect.Type]cache.SharedIndexInformer{}
		for informerType, informer := range f.informers {
			if f.startedInformers[informerType] {
				informers[informerType] = informer
			}
		}
		return informers
	}()

	res := map[reflect.Type]bool{}
	for informType, informer := range informers {
		res[informType] = cache.WaitForCacheSync(stopCh, informer.HasSynced)
	}
	return res
}

// InternalInformerFor returns the SharedIndexInformer for obj using an internal
// client.
// InformerFor()即每个类型Informer的构造函数了，即便具体实现构造的地方是使用者提供的
// 这个函数需要使用者传入对象类型，因为在sharedInformerFactory里面是按照对象类型组织的Informer
// 更有趣的是这些Informer不是sharedInformerFactory创建的，需要使用者传入构造函数
// 这样做既保证了每个类型的Informer只构造一次，同时又保证了具体Informer构造函数的私有化能力
func (f *sharedInformerFactory) InformerFor(obj runtime.Object, newFunc internalinterfaces.NewInformerFunc) cache.SharedIndexInformer {
	f.lock.Lock()
	defer f.lock.Unlock()

	// 通过反射获取obj的类型
	informerType := reflect.TypeOf(obj)
	// 通过反射获取obj的类型
	informer, exists := f.informers[informerType]
	if exists {
		return informer
	}

	// 获取类型定制的同步周期，如果定制的同步周期那就用统一的默认周期
	resyncPeriod, exists := f.customResync[informerType]
	if !exists {
		resyncPeriod = f.defaultResync
	}

	// 调用使用者提供构造函数，然后把创建的Informer保存起来
	informer = newFunc(f.client, resyncPeriod)
	f.informers[informerType] = informer

	return informer
}

// SharedInformerFactory provides shared informers for resources in all known
// API group versions.
type SharedInformerFactory interface {
	// 包内抽象
	internalinterfaces.SharedInformerFactory
	// 为schema注册的资源生成informer
	ForResource(resource schema.GroupVersionResource) (GenericInformer, error)
	// 等待所有的Informer都已经同步完成，即遍历调用SharedInformer.HasSynced()
	// 所以函数需要周期性的调用指导所有的Informer都已经同步完毕
	WaitForCacheSync(stopCh <-chan struct{}) map[reflect.Type]bool

	// admissionregistration相关的informer组
	Admissionregistration() admissionregistration.Interface
	// apiserviceInternal相关的informer组
	Internal() apiserverinternal.Interface
	// app相关的Informer组
	Apps() apps.Interface
	// autoscaling相关的informer组
	Autoscaling() autoscaling.Interface
	// job相关的inforner组
	Batch() batch.Interface
	// certificates相关的informer组
	Certificates() certificates.Interface
	// coordination相关的informer组
	Coordination() coordination.Interface
	// core相关的informer组
	Core() core.Interface
	// discovery相关的informer组
	Discovery() discovery.Interface
	// event相关的informer组
	Events() events.Interface
	// extension相关的informer组
	Extensions() extensions.Interface
	// flowcontrol相关的informer组
	Flowcontrol() flowcontrol.Interface
	// networking相关的informer组
	Networking() networking.Interface
	//node相关的informer组
	Node() node.Interface
	// policy相关的informer组
	Policy() policy.Interface
	// rbac相关的informer组
	Rbac() rbac.Interface
	// scheduling相关的informer组
	Scheduling() scheduling.Interface
	// storage相关的informer组
	Storage() storage.Interface
}

func (f *sharedInformerFactory) Admissionregistration() admissionregistration.Interface {
	return admissionregistration.New(f, f.namespace, f.tweakListOptions)
}

func (f *sharedInformerFactory) Internal() apiserverinternal.Interface {
	return apiserverinternal.New(f, f.namespace, f.tweakListOptions)
}

func (f *sharedInformerFactory) Apps() apps.Interface {
	return apps.New(f, f.namespace, f.tweakListOptions)
}

func (f *sharedInformerFactory) Autoscaling() autoscaling.Interface {
	return autoscaling.New(f, f.namespace, f.tweakListOptions)
}

func (f *sharedInformerFactory) Batch() batch.Interface {
	return batch.New(f, f.namespace, f.tweakListOptions)
}

func (f *sharedInformerFactory) Certificates() certificates.Interface {
	return certificates.New(f, f.namespace, f.tweakListOptions)
}

func (f *sharedInformerFactory) Coordination() coordination.Interface {
	return coordination.New(f, f.namespace, f.tweakListOptions)
}

func (f *sharedInformerFactory) Core() core.Interface {
	// 调用了内核包里面的New()函数
	return core.New(f, f.namespace, f.tweakListOptions)
}

func (f *sharedInformerFactory) Discovery() discovery.Interface {
	return discovery.New(f, f.namespace, f.tweakListOptions)
}

func (f *sharedInformerFactory) Events() events.Interface {
	return events.New(f, f.namespace, f.tweakListOptions)
}

func (f *sharedInformerFactory) Extensions() extensions.Interface {
	return extensions.New(f, f.namespace, f.tweakListOptions)
}

func (f *sharedInformerFactory) Flowcontrol() flowcontrol.Interface {
	return flowcontrol.New(f, f.namespace, f.tweakListOptions)
}

func (f *sharedInformerFactory) Networking() networking.Interface {
	return networking.New(f, f.namespace, f.tweakListOptions)
}

func (f *sharedInformerFactory) Node() node.Interface {
	return node.New(f, f.namespace, f.tweakListOptions)
}

func (f *sharedInformerFactory) Policy() policy.Interface {
	return policy.New(f, f.namespace, f.tweakListOptions)
}

func (f *sharedInformerFactory) Rbac() rbac.Interface {
	return rbac.New(f, f.namespace, f.tweakListOptions)
}

func (f *sharedInformerFactory) Scheduling() scheduling.Interface {
	return scheduling.New(f, f.namespace, f.tweakListOptions)
}

func (f *sharedInformerFactory) Storage() storage.Interface {
	return storage.New(f, f.namespace, f.tweakListOptions)
}

```