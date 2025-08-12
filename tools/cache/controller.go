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

package cache

import (
	"errors"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/clock"
)

// This file implements a low-level controller that is used in
// sharedIndexInformer, which is an implementation of
// SharedIndexInformer.  Such informers, in turn, are key components
// in the high level controllers that form the backbone of the
// Kubernetes control plane.  Look at those for examples, or the
// example in
// https://github.com/kubernetes/client-go/tree/master/examples/workqueue
// .

// Config contains all the settings for one of these low-level controllers.
// Config包含这些低级控制器之一的所有设置。
type Config struct {
	// The queue for your objects - has to be a DeltaFIFO due to
	// assumptions in the implementation. Your Process() function
	// should accept the output of this Queue's Pop() method.
	// sharedInformer使用DeltaFIFO
	Queue

	// Something that can list and watch your objects.
	// 构造Reflctor
	ListerWatcher

	// Something that can process a popped Deltas.
	// 在调用DeltaFIFO.Pop()使用，用于处理弹出对象
	Process ProcessFunc

	// ObjectType is an example object of the type this controller is
	// expected to handle.
	// 对象类型，Reflector使用
	ObjectType runtime.Object

	// ObjectDescription is the description to use when logging type-specific information about this controller.
	ObjectDescription string

	// FullResyncPeriod is the period at which ShouldResync is considered.
	// 全量同步周期,Reflector使用
	FullResyncPeriod time.Duration

	// ShouldResync is periodically used by the reflector to determine
	// whether to Resync the Queue. If ShouldResync is `nil` or
	// returns true, it means the reflector should proceed with the
	// resync.
	// Reflector在全量更新的时候会调用该函数询问
	ShouldResync ShouldResyncFunc

	// If true, when Process() returns an error, re-enqueue the object.
	// TODO: add interface to let you inject a delay/backoff or drop
	//       the object completely if desired. Pass the object in
	//       question to this interface as a parameter.  This is probably moot
	//       now that this functionality appears at a higher level.
	// 错误是否需要重试
	RetryOnError bool

	// Called whenever the ListAndWatch drops the connection with an error.
	// 处理ListAndWatch的错误
	WatchErrorHandler WatchErrorHandler

	// WatchListPageSize is the requested chunk size of initial and relist watch lists.
	// 获取资源的数量
	WatchListPageSize int64
}

// ShouldResyncFunc is a type of function that indicates if a reflector should perform a
// resync or not. It can be used by a shared informer to support multiple event handlers with custom
// resync periods.
type ShouldResyncFunc func() bool

// ProcessFunc processes a single object.
type ProcessFunc func(obj interface{}, isInInitialList bool) error

// `*controller` implements Controller
// controller 接口的实现
type controller struct {
	// 配置
	config    Config
	reflector *Reflector
	// reflector的读写锁
	reflectorMutex sync.RWMutex
	// 时钟
	clock clock.Clock
}

// Controller is a low-level controller that is parameterized by a
// Config and used in sharedIndexInformer.
// Controller的接口，
// 把Reflector、DeltaFIFO组合起来形成一个相对固定的、标准的处理流程
type Controller interface {
	// Run does two things.  One is to construct and run a Reflector
	// to pump objects/notifications from the Config's ListerWatcher
	// to the Config's Queue and possibly invoke the occasional Resync
	// on that Queue.  The other is to repeatedly Pop from the Queue
	// and process with the Config's ProcessFunc.  Both of these
	// continue until `stopCh` is closed.
	//  核心流程函数
	Run(stopCh <-chan struct{})

	// HasSynced delegates to the Config's Queue
	// apiserver中的对象是否已经同步到了Store中
	// 可调用DeltaFIFO. HasSynced()实现
	HasSynced() bool

	// LastSyncResourceVersion delegates to the Reflector when there
	// is one, otherwise returns the empty string
	// 资源的最新版本号
	// 通过Reflector实现
	LastSyncResourceVersion() string
}

// New makes a new Controller from the given Config.
func New(c *Config) Controller {
	ctlr := &controller{
		config: *c,
		clock:  &clock.RealClock{},
	}
	return ctlr
}

// Run begins processing items, and will continue until a value is sent down stopCh or it is closed.
// It's an error to call Run more than once.
// Run blocks; call via go.
// contoller 业务逻辑的实现,启动relector
func (c *controller) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	// 处理退出信号的协程
	go func() {
		<-stopCh
		c.config.Queue.Close()
	}()

	// 创建reflector
	r := NewReflectorWithOptions(
		c.config.ListerWatcher,
		c.config.ObjectType,
		c.config.Queue,
		ReflectorOptions{
			ResyncPeriod:    c.config.FullResyncPeriod,
			TypeDescription: c.config.ObjectDescription,
			Clock:           c.clock,
		},
	)
	r.ShouldResync = c.config.ShouldResync
	r.WatchListPageSize = c.config.WatchListPageSize
	if c.config.WatchErrorHandler != nil {
		r.watchErrorHandler = c.config.WatchErrorHandler
	}

	// 记录reflector
	c.reflectorMutex.Lock()
	c.reflector = r
	c.reflectorMutex.Unlock()

	// 所有被group管理的协程退出调用Wait()才会退出,否则阻塞
	var wg wait.Group

	// ⭐️ StartWithChannel()会启动协程执行 Reflector.Run(),Run 方法会重复调用 reflector 的 ListAndWatch 来获取所有对象及其后续的增量变更,同时接收到stopCh信号就会退出协程
	wg.StartWithChannel(stopCh, r.Run)

	// ⭐️ wait.Until()周期性的调用c.processLoop()，这里是1秒，不用担心调用频率太高，正常情况下c.processLoop是不会返回的，
	// 除非遇到了解决不了的错误，因为他是个循环
	wait.Until(c.processLoop, time.Second, stopCh)
	wg.Wait()
}

// Returns true once this controller has completed an initial resource listing
// 调用DeltaFIFO的HasSynced
func (c *controller) HasSynced() bool {
	return c.config.Queue.HasSynced()
}

// 调用reflector的实现
func (c *controller) LastSyncResourceVersion() string {
	c.reflectorMutex.RLock()
	defer c.reflectorMutex.RUnlock()
	if c.reflector == nil {
		return ""
	}
	return c.reflector.LastSyncResourceVersion()
}

// processLoop drains the work queue.
// TODO: Consider doing the processing in parallel. This will require a little thought
// to make sure that we don't end up processing the same object multiple times
// concurrently.
//
// TODO: Plumb through the stopCh here (and down to the queue) so that this can
// actually exit when the controller is stopped. Or just give up on this stuff
// ever being stoppable. Converting this whole package to use Context would
// also be helpful.
// processLoop 处理工作队列中的项目
// TODO: 考虑并行处理，这需要仔细设计以避免并发处理同一对象
// TODO: 将 stopCh 传递到队列中，以便在控制器停止时能正确退出
// 或者放弃使这部分可停止。将整个包改为使用 Context 也会很有帮助
// ⭐️ processLoop()是Controller的核心，它会从队列中弹出一个对象，然后处理它
func (c *controller) processLoop() {
	// 持续从队列中获取并处理项目，每个循环处理一个项目
	for {
		// c.config.Process 是在创建 Controller 时通过 Config 传入的处理函数
		// 这个处理函数会被包装成 PopProcessFunc 类型，然后传给队列的 Pop 方法执行
		// 在 shared_informer.go 中，processDeltas 函数被设置为 c.config.Process
		obj, err := c.config.Queue.Pop(PopProcessFunc(c.config.Process))
		if err != nil {
			// FIFO关闭
			if err == ErrFIFOClosed {
				return
			}
			// 重试
			if c.config.RetryOnError {
				// This is the safe way to re-enqueue.
				c.config.Queue.AddIfNotPresent(obj)
			}
		}
	}
}

// ResourceEventHandler can handle notifications for events that
// happen to a resource. The events are informational only, so you
// can't return an error.  The handlers MUST NOT modify the objects
// received; this concerns not only the top level of structure but all
// the data structures reachable from it.
//   - OnAdd is called when an object is added.
//   - OnUpdate is called when an object is modified. Note that oldObj is the
//     last known state of the object-- it is possible that several changes
//     were combined together, so you can't use this to see every single
//     change. OnUpdate is also called when a re-list happens, and it will
//     get called even if nothing changed. This is useful for periodically
//     evaluating or syncing something.
//   - OnDelete will get the final state of the item if it is known, otherwise
//     it will get an object of type DeletedFinalStateUnknown. This can
//     happen if the watch is closed and misses the delete event and we don't
//     notice the deletion until the subsequent re-list.
//
// ResourceEventHandler 用于处理资源事件通知的接口。这些事件是只读的，因此不能返回错误。
// 所有处理器都不得修改接收到的对象，这包括顶层结构及其所有可访问的子结构。
//
// 方法说明：
//   - OnAdd: 当对象被添加时调用
//   - OnUpdate: 当对象被修改时调用。注意 oldObj 是对象的最后已知状态。
//     多个变更可能被合并，因此不能依赖此回调查看每个单独的变更。
//     在重新列出时也会调用 OnUpdate，即使没有任何变化也会触发。
//     这适用于定期评估或同步某些内容。
//   - OnDelete: 当对象被删除时调用。如果知道对象的最终状态，则传入最终状态；
//     否则会收到 DeletedFinalStateUnknown 类型的对象。
//     这种情况通常发生在 watch 关闭并错过删除事件，直到后续重新列出时才注意到删除。
//
// ⭐️ 回调函数函数的接口，controller通过这个接口来通知事件,开发人员需要实现这个接口
type ResourceEventHandler interface {
	// OnAdd 添加对象回调函数
	OnAdd(obj interface{}, isInInitialList bool)
	// OnUpdate 更新对象回调函数
	OnUpdate(oldObj, newObj interface{})
	// OnDelete 删除对象回调函数
	OnDelete(obj interface{})
}

// ResourceEventHandlerFuncs is an adaptor to let you easily specify as many or
// as few of the notification functions as you want while still implementing
// ResourceEventHandler.  This adapter does not remove the prohibition against
// modifying the objects.
//
// See ResourceEventHandlerDetailedFuncs if your use needs to propagate
// HasSynced.
// ResourceEventHandlerFuncs 是一个适配器，让你可以方便地指定任意数量的事件处理函数，
// 同时仍然实现 ResourceEventHandler 接口。
// 注意：此适配器不会解除对修改对象的限制（事件对象仍然是只读的）。
//
// 如果需要传播 HasSynced 状态，请使用 ResourceEventHandlerDetailedFuncs。
type ResourceEventHandlerFuncs struct {
	AddFunc    func(obj interface{})
	UpdateFunc func(oldObj, newObj interface{})
	DeleteFunc func(obj interface{})
}

// OnAdd calls AddFunc if it's not nil.
func (r ResourceEventHandlerFuncs) OnAdd(obj interface{}, isInInitialList bool) {
	if r.AddFunc != nil {
		r.AddFunc(obj)
	}
}

// OnUpdate calls UpdateFunc if it's not nil.
func (r ResourceEventHandlerFuncs) OnUpdate(oldObj, newObj interface{}) {
	if r.UpdateFunc != nil {
		r.UpdateFunc(oldObj, newObj)
	}
}

// OnDelete calls DeleteFunc if it's not nil.
func (r ResourceEventHandlerFuncs) OnDelete(obj interface{}) {
	if r.DeleteFunc != nil {
		r.DeleteFunc(obj)
	}
}

// ResourceEventHandlerDetailedFuncs is exactly like ResourceEventHandlerFuncs
// except its AddFunc accepts the isInInitialList parameter, for propagating
// HasSynced.
type ResourceEventHandlerDetailedFuncs struct {
	AddFunc    func(obj interface{}, isInInitialList bool)
	UpdateFunc func(oldObj, newObj interface{})
	DeleteFunc func(obj interface{})
}

// OnAdd calls AddFunc if it's not nil.
func (r ResourceEventHandlerDetailedFuncs) OnAdd(obj interface{}, isInInitialList bool) {
	if r.AddFunc != nil {
		r.AddFunc(obj, isInInitialList)
	}
}

// OnUpdate calls UpdateFunc if it's not nil.
func (r ResourceEventHandlerDetailedFuncs) OnUpdate(oldObj, newObj interface{}) {
	if r.UpdateFunc != nil {
		r.UpdateFunc(oldObj, newObj)
	}
}

// OnDelete calls DeleteFunc if it's not nil.
func (r ResourceEventHandlerDetailedFuncs) OnDelete(obj interface{}) {
	if r.DeleteFunc != nil {
		r.DeleteFunc(obj)
	}
}

// FilteringResourceEventHandler applies the provided filter to all events coming
// in, ensuring the appropriate nested handler method is invoked. An object
// that starts passing the filter after an update is considered an add, and an
// object that stops passing the filter after an update is considered a delete.
// Like the handlers, the filter MUST NOT modify the objects it is given.
type FilteringResourceEventHandler struct {
	FilterFunc func(obj interface{}) bool
	Handler    ResourceEventHandler
}

// OnAdd calls the nested handler only if the filter succeeds
func (r FilteringResourceEventHandler) OnAdd(obj interface{}, isInInitialList bool) {
	if !r.FilterFunc(obj) {
		return
	}
	r.Handler.OnAdd(obj, isInInitialList)
}

// OnUpdate ensures the proper handler is called depending on whether the filter matches
func (r FilteringResourceEventHandler) OnUpdate(oldObj, newObj interface{}) {
	newer := r.FilterFunc(newObj)
	older := r.FilterFunc(oldObj)
	switch {
	case newer && older:
		r.Handler.OnUpdate(oldObj, newObj)
	case newer && !older:
		r.Handler.OnAdd(newObj, false)
	case !newer && older:
		r.Handler.OnDelete(oldObj)
	default:
		// do nothing
	}
}

// OnDelete calls the nested handler only if the filter succeeds
func (r FilteringResourceEventHandler) OnDelete(obj interface{}) {
	if !r.FilterFunc(obj) {
		return
	}
	r.Handler.OnDelete(obj)
}

// DeletionHandlingMetaNamespaceKeyFunc checks for
// DeletedFinalStateUnknown objects before calling
// MetaNamespaceKeyFunc.
func DeletionHandlingMetaNamespaceKeyFunc(obj interface{}) (string, error) {
	if d, ok := obj.(DeletedFinalStateUnknown); ok {
		return d.Key, nil
	}
	return MetaNamespaceKeyFunc(obj)
}

// NewInformer returns a Store and a controller for populating the store
// while also providing event notifications. You should only used the returned
// Store for Get/List operations; Add/Modify/Deletes will cause the event
// notifications to be faulty.
//
// Parameters:
//   - lw is list and watch functions for the source of the resource you want to
//     be informed of.
//   - objType is an object of the type that you expect to receive.
//   - resyncPeriod: if non-zero, will re-list this often (you will get OnUpdate
//     calls, even if nothing changed). Otherwise, re-list will be delayed as
//     long as possible (until the upstream source closes the watch or times out,
//     or you stop the controller).
//   - h is the object you want notifications sent to.
//
// NewInformer 返回一个 Store 和 Controller，用于填充存储并提供事件通知。
// 返回的 Store 应仅用于 Get/List 操作；Add/Modify/Delete 操作将导致事件通知出错。
//
// 参数：
//   - lw: 用于监听资源的 List 和 Watch 函数
//   - objType: 期望接收的对象类型
//   - resyncPeriod: 非零值表示重新全量同步的周期（即使没有变化也会触发 OnUpdate 回调）
//     如果为零，则尽可能延迟重新同步（直到上游关闭 watch 或超时，或控制器停止）
//   - h: 接收通知的对象
func NewInformer(
	lw ListerWatcher,
	objType runtime.Object,
	resyncPeriod time.Duration,
	h ResourceEventHandler,
) (Store, Controller) {
	// This will hold the client state, as we know it.
	// 状态存储，基本的存储功能
	clientState := NewStore(DeletionHandlingMetaNamespaceKeyFunc)

	return clientState, newInformer(lw, objType, resyncPeriod, h, clientState, nil)
}

// NewIndexerInformer returns an Indexer and a Controller for populating the index
// while also providing event notifications. You should only used the returned
// Index for Get/List operations; Add/Modify/Deletes will cause the event
// notifications to be faulty.
//
// Parameters:
//   - lw is list and watch functions for the source of the resource you want to
//     be informed of.
//   - objType is an object of the type that you expect to receive.
//   - resyncPeriod: if non-zero, will re-list this often (you will get OnUpdate
//     calls, even if nothing changed). Otherwise, re-list will be delayed as
//     long as possible (until the upstream source closes the watch or times out,
//     or you stop the controller).
//   - h is the object you want notifications sent to.
//   - indexers is the indexer for the received object type.
//
// NewIndexerInformer 返回一个 Indexer 和 Controller，用于填充索引并提供事件通知。
// 返回的 Indexer 应仅用于 Get/List 操作；Add/Modify/Delete 操作将导致事件通知出错。
//
// 参数：
//   - lw: 用于监听资源的 List 和 Watch 函数
//   - objType: 期望接收的对象类型
//   - resyncPeriod: 非零值表示重新全量同步的周期（即使没有变化也会触发 OnUpdate 回调）
//     如果为零，则尽可能延迟重新同步（直到上游关闭 watch 或超时，或控制器停止）
//   - h: 接收通知的对象
//   - indexers: 支持基于索引的高效查询，适用于需要按字段或标签快速查找的场景
func NewIndexerInformer(
	lw ListerWatcher,
	objType runtime.Object,
	resyncPeriod time.Duration,
	h ResourceEventHandler,
	indexers Indexers,
) (Indexer, Controller) {
	// This will hold the client state, as we know it.
	// 状态存储，支持基于索引的高效查询，适用于需要按字段或标签快速查找的场景
	clientState := NewIndexer(DeletionHandlingMetaNamespaceKeyFunc, indexers)

	return clientState, newInformer(lw, objType, resyncPeriod, h, clientState, nil)
}

// NewTransformingInformer returns a Store and a controller for populating
// the store while also providing event notifications. You should only used
// the returned Store for Get/List operations; Add/Modify/Deletes will cause
// the event notifications to be faulty.
// The given transform function will be called on all objects before they will
// put into the Store and corresponding Add/Modify/Delete handlers will
// be invoked for them.
// NewTransformingInformer 返回一个 Store 和 Controller，用于填充存储并提供事件通知。
// 返回的 Store 应仅用于 Get/List 操作；Add/Modify/Delete 操作将导致事件通知出错。
//
// 特性：
// - 在对象存入 Store 之前，会先通过 transform 函数进行处理
// - 处理后的对象会触发相应的事件处理器（OnAdd/OnUpdate/OnDelete）
// - 适用于需要在缓存前修改或转换对象的场景
func NewTransformingInformer(
	lw ListerWatcher,
	objType runtime.Object,
	resyncPeriod time.Duration,
	h ResourceEventHandler,
	transformer TransformFunc,
) (Store, Controller) {
	// This will hold the client state, as we know it.
	clientState := NewStore(DeletionHandlingMetaNamespaceKeyFunc)

	return clientState, newInformer(lw, objType, resyncPeriod, h, clientState, transformer)
}

// NewTransformingIndexerInformer returns an Indexer and a controller for
// populating the index while also providing event notifications. You should
// only used the returned Index for Get/List operations; Add/Modify/Deletes
// will cause the event notifications to be faulty.
// The given transform function will be called on all objects before they will
// be put into the Index and corresponding Add/Modify/Delete handlers will
// be invoked for them.
// NewTransformingIndexerInformer 返回一个 Indexer 和 Controller，用于填充索引并提供事件通知。
// 返回的 Indexer 应仅用于 Get/List 操作；Add/Modify/Delete 操作将导致事件通知出错。
//
// 特性：
// - 在对象存入 Indexer 之前，会先通过 transform 函数进行处理
// - 处理后的对象会触发相应的事件处理器（OnAdd/OnUpdate/OnDelete）
// - 适用于需要在缓存前修改或转换对象的场景
func NewTransformingIndexerInformer(
	lw ListerWatcher,
	objType runtime.Object,
	resyncPeriod time.Duration,
	h ResourceEventHandler,
	indexers Indexers,
	transformer TransformFunc,
) (Indexer, Controller) {
	// This will hold the client state, as we know it.
	clientState := NewIndexer(DeletionHandlingMetaNamespaceKeyFunc, indexers)

	return clientState, newInformer(lw, objType, resyncPeriod, h, clientState, transformer)
}

// Multiplexes updates in the form of a list of Deltas into a Store, and informs
// a given handler of events OnUpdate, OnAdd, OnDelete
// 将增量更新列表(Deltas)多路复用到存储(Store)中，
// 并通过事件处理器(handler)通知更新(OnUpdate)、添加(OnAdd)、删除(OnDelete)事件
// ⭐️ 连接增量队列(DeltaFIFO)和事件处理器，将增量变更转换为具体的存储操作和事件通知
func processDeltas(
	// Object which receives event notifications from the given deltas
	// handler是事件处理器，用于处理事件通知
	handler ResourceEventHandler,
	// clientState是存储，用于存储对象
	clientState Store,
	// deltas是增量更新列表
	deltas Deltas,
	// isInInitialList表示是否是第一批对象
	isInInitialList bool,
) error {
	// from oldest to newest
	// Deltas里面包含了一个对象的多个增量操作,要从最老的Delta到最先的Delta遍历处理
	for _, d := range deltas {
		obj := d.Object

		// 根据不同的Delta做不同的操作，大致分为对象添加、删除两大类操作
		// 所有的操作都要先同步到cache在通知处理器，
		// 这样保持处理器和cache的状态是一致的
		switch d.Type {
		//  同步、添加、更新都是对象添加类，至于是否是更新还要看cache是否有这个对象
		case Sync, Replaced, Added, Updated:

			if old, exists, err := clientState.Get(obj); err == nil && exists {
				// 如果cache中存在这个对象，那么就是更新操作
				if err := clientState.Update(obj); err != nil {
					return err
				}
				// 通知处理器更新
				handler.OnUpdate(old, obj)
			} else {
				// 将对象添加到cache
				if err := clientState.Add(obj); err != nil {
					return err
				}
				handler.OnAdd(obj, isInInitialList)
			}
		//对象被删除
		case Deleted:
			if err := clientState.Delete(obj); err != nil {
				return err
			}
			handler.OnDelete(obj)
		}
	}
	return nil
}

// newInformer returns a controller for populating the store while also
// providing event notifications.
//
// Parameters
//   - lw is list and watch functions for the source of the resource you want to
//     be informed of.
//   - objType is an object of the type that you expect to receive.
//   - resyncPeriod: if non-zero, will re-list this often (you will get OnUpdate
//     calls, even if nothing changed). Otherwise, re-list will be delayed as
//     long as possible (until the upstream source closes the watch or times out,
//     or you stop the controller).
//   - h is the object you want notifications sent to.
//   - clientState is the store you want to populate
//
// newInformer 返回一个控制器，用于填充存储并同时提供事件通知。
//
// 参数：
//   - lw: 用于监听资源的 List 和 Watch 函数
//   - objType: 期望接收的对象类型
//   - resyncPeriod: 非零值表示重新全量同步的周期（即使没有变化也会触发 OnUpdate 回调）
//     如果为零，则尽可能延迟重新同步（直到上游关闭 watch 或超时，或控制器停止）
//   - h: 接收通知的对象
//   - clientState: 要填充的存储
//   - transformer: 用于转换对象的函数
func newInformer(
	lw ListerWatcher,
	objType runtime.Object,
	resyncPeriod time.Duration,
	h ResourceEventHandler,
	clientState Store,
	transformer TransformFunc,
) Controller {
	// This will hold incoming changes. Note how we pass clientState in as a
	// KeyLister, that way resync operations will result in the correct set
	// of update/delete deltas.
	// 创建 DeltaFIFO 队列，用于处理增量变更
	// 传入 clientState 作为 KeyLister，确保重新同步操作能生成正确的更新/删除增量
	fifo := NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KnownObjects:          clientState,
		EmitDeltaTypeReplaced: true,
		Transformer:           transformer,
	})

	cfg := &Config{
		Queue:            fifo,         //  DeltaFIFO 队列
		ListerWatcher:    lw,           // List和Watch函数
		ObjectType:       objType,      // 期望接收的对象类型
		FullResyncPeriod: resyncPeriod, // 重新全量同步的周期
		RetryOnError:     false,

		// deltaFIFO的回调函数
		Process: func(obj interface{}, isInInitialList bool) error {
			// 确保处理的是 Deltas 类型
			if deltas, ok := obj.(Deltas); ok {
				// 处理增量变更Deltas
				return processDeltas(h, clientState, deltas, isInInitialList)
			}
			return errors.New("object given as Process argument is not Deltas")
		},
	}
	return New(cfg)
}
