
```go
package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"reflect"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/naming"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/tools/pager"
	"k8s.io/klog/v2"
	"k8s.io/utils/clock"
	"k8s.io/utils/trace"
)

const defaultExpectedTypeName = "<unspecified>"

// Reflector watches a specified resource and causes all changes to be reflected in the given store.
type Reflector struct {
	// name identifies this reflector. By default it will be a file:line if possible.
	//名字
	name string

	// The name of the type we expect to place in the store. The name
	// will be the stringification of expectedGVK if provided, and the
	// stringification of expectedType otherwise. It is for display
	// only, and should not be used for parsing or comparison.
	expectedTypeName string
	// An example object of the type we expect to place in the store.
	// Only the type needs to be right, except that when that is
	// `unstructured.Unstructured` the object's `"apiVersion"` and
	// `"kind"` must also be right.
	// 要监控的对象类型
	expectedType reflect.Type
	// The GVK of the object we expect to place in the store if unstructured.
	expectedGVK *schema.GroupVersionKind
	// The destination to sync up with the watch source
	// 存储，就是DeltaFIFO
	store Store
	// listerWatcher is used to perform lists and watches.
	// 监听apiserver
	listerWatcher ListerWatcher

	// backoff manages backoff of ListWatch
	backoffManager wait.BackoffManager
	// initConnBackoffManager manages backoff the initial connection with the Watch call of ListAndWatch.
	initConnBackoffManager wait.BackoffManager

	// 重新同步的周期，很多人肯定认为这个同步周期指的是从apiserver的同步周期
	// 其实这里面同步指的是shared_informer使用者需要定期同步全量对象
	resyncPeriod time.Duration
	// ShouldResync is invoked periodically and whenever it returns `true` the Store's Resync operation is invoked
	// 是否需要同步
	ShouldResync func() bool
	// clock allows tests to manipulate time
	// 时钟
	clock clock.Clock
	// paginatedResult defines whether pagination should be forced for list calls.
	// It is set based on the result of the initial list call.
	paginatedResult bool
	// lastSyncResourceVersion is the resource version token last
	// observed when doing a sync with the underlying store
	// it is thread safe, but not synchronized with the underlying store
	// 最后一次同步的资源版本
	lastSyncResourceVersion string
	// isLastSyncResourceVersionUnavailable is true if the previous list or watch request with
	// lastSyncResourceVersion failed with an "expired" or "too large resource version" error.
	isLastSyncResourceVersionUnavailable bool
	// lastSyncResourceVersionMutex guards read/write access to lastSyncResourceVersion
	// 最后一次同步的资源版本加锁
	lastSyncResourceVersionMutex sync.RWMutex
	// WatchListPageSize is the requested chunk size of initial and resync watch lists.
	// If unset, for consistent reads (RV="") or reads that opt-into arbitrarily old data
	// (RV="0") it will default to pager.PageSize, for the rest (RV != "" && RV != "0")
	// it will turn off pagination to allow serving them from watch cache.
	// NOTE: It should be used carefully as paginated lists are always served directly from
	// etcd, which is significantly less efficient and may lead to serious performance and
	// scalability problems.
	WatchListPageSize int64
	// Called whenever the ListAndWatch drops the connection with an error.
	watchErrorHandler WatchErrorHandler
}

// ResourceVersionUpdater is an interface that allows store implementation to
// track the current resource version of the reflector. This is especially
// important if storage bookmarks are enabled.
type ResourceVersionUpdater interface {
	// UpdateResourceVersion is called each time current resource version of the reflector
	// is updated.
	UpdateResourceVersion(resourceVersion string)
}

// The WatchErrorHandler is called whenever ListAndWatch drops the
// connection with an error. After calling this handler, the informer
// will backoff and retry.
//
// The default implementation looks at the error type and tries to log
// the error message at an appropriate level.
//
// Implementations of this handler may display the error message in other
// ways. Implementations should return quickly - any expensive processing
// should be offloaded.
type WatchErrorHandler func(r *Reflector, err error)

// DefaultWatchErrorHandler is the default implementation of WatchErrorHandler
// WatchErrorHandler的实现
func DefaultWatchErrorHandler(r *Reflector, err error) {
	switch {
	case isExpiredError(err):
		// Don't set LastSyncResourceVersionUnavailable - LIST call with ResourceVersion=RV already
		// has a semantic that it returns data at least as fresh as provided RV.
		// So first try to LIST with setting RV to resource version of last observed object.
		klog.V(4).Infof("%s: watch of %v closed with: %v", r.name, r.expectedTypeName, err)
	case err == io.EOF:
		// watch closed normally
	case err == io.ErrUnexpectedEOF:
		klog.V(1).Infof("%s: Watch for %v closed with unexpected EOF: %v", r.name, r.expectedTypeName, err)
	default:
		utilruntime.HandleError(fmt.Errorf("%s: Failed to watch %v: %v", r.name, r.expectedTypeName, err))
	}
}

var (
	// We try to spread the load on apiserver by setting timeouts for
	// watch requests - it is random in [minWatchTimeout, 2*minWatchTimeout].
	minWatchTimeout = 5 * time.Minute
)

// NewNamespaceKeyedIndexerAndReflector creates an Indexer and a Reflector
// The indexer is configured to key on namespace
func NewNamespaceKeyedIndexerAndReflector(lw ListerWatcher, expectedType interface{}, resyncPeriod time.Duration) (indexer Indexer, reflector *Reflector) {
	indexer = NewIndexer(MetaNamespaceKeyFunc, Indexers{NamespaceIndex: MetaNamespaceIndexFunc})
	reflector = NewReflector(lw, expectedType, indexer, resyncPeriod)
	return indexer, reflector
}

// NewReflector creates a new Reflector object which will keep the
// given store up to date with the server's contents for the given
// resource. Reflector promises to only put things in the store that
// have the type of expectedType, unless expectedType is nil. If
// resyncPeriod is non-zero, then the reflector will periodically
// consult its ShouldResync function to determine whether to invoke
// the Store's Resync operation; `ShouldResync==nil` means always
// "yes".  This enables you to use reflectors to periodically process
// everything as well as incrementally processing the things that
// change.
func NewReflector(lw ListerWatcher, expectedType interface{}, store Store, resyncPeriod time.Duration) *Reflector {
	return NewNamedReflector(naming.GetNameFromCallsite(internalPackages...), lw, expectedType, store, resyncPeriod)
}

// NewNamedReflector same as NewReflector, but with a specified name for logging
// 构造函数
func NewNamedReflector(name string, lw ListerWatcher, expectedType interface{}, store Store, resyncPeriod time.Duration) *Reflector {
	realClock := &clock.RealClock{}
	r := &Reflector{
		name:          name,
		listerWatcher: lw,
		store:         store,
		// We used to make the call every 1sec (1 QPS), the goal here is to achieve ~98% traffic reduction when
		// API server is not healthy. With these parameters, backoff will stop at [30,60) sec interval which is
		// 0.22 QPS. If we don't backoff for 2min, assume API server is healthy and we reset the backoff.
		backoffManager:         wait.NewExponentialBackoffManager(800*time.Millisecond, 30*time.Second, 2*time.Minute, 2.0, 1.0, realClock),
		initConnBackoffManager: wait.NewExponentialBackoffManager(800*time.Millisecond, 30*time.Second, 2*time.Minute, 2.0, 1.0, realClock),
		resyncPeriod:           resyncPeriod,
		clock:                  realClock,
		watchErrorHandler:      WatchErrorHandler(DefaultWatchErrorHandler),
	}
	r.setExpectedType(expectedType)
	return r
}

func (r *Reflector) setExpectedType(expectedType interface{}) {
	r.expectedType = reflect.TypeOf(expectedType)
	if r.expectedType == nil {
		r.expectedTypeName = defaultExpectedTypeName
		return
	}

	r.expectedTypeName = r.expectedType.String()

	if obj, ok := expectedType.(*unstructured.Unstructured); ok {
		// Use gvk to check that watch event objects are of the desired type.
		gvk := obj.GroupVersionKind()
		if gvk.Empty() {
			klog.V(4).Infof("Reflector from %s configured with expectedType of *unstructured.Unstructured with empty GroupVersionKind.", r.name)
			return
		}
		r.expectedGVK = &gvk
		r.expectedTypeName = gvk.String()
	}
}

// internalPackages are packages that ignored when creating a default reflector name. These packages are in the common
// call chains to NewReflector, so they'd be low entropy names for reflectors
var internalPackages = []string{"client-go/tools/cache/"}

// Run repeatedly uses the reflector's ListAndWatch to fetch all the
// objects and subsequent deltas.
// Run will exit when stopCh is closed.
func (r *Reflector) Run(stopCh <-chan struct{}) {
	klog.V(3).Infof("Starting reflector %s (%s) from %s", r.expectedTypeName, r.resyncPeriod, r.name)
	// func BackoffUntil(f func(), backoff BackoffManager, sliding bool, stopCh <-chan struct{}) 是下面函数的声明
	// 这里面我们不用关心wait.Until是如何实现的，只要知道他调用函数f会被每BackoffManager.Duration周期执行一次
	// 意思就是f()函数执行完毕再等BackoffManager.Duration时间后在执行一次，也就是r.ListAndWatch()会被周期性的调用
	wait.BackoffUntil(func() {
		if err := r.ListAndWatch(stopCh); err != nil {
			r.watchErrorHandler(r, err)
		}
	}, r.backoffManager, true, stopCh)
	klog.V(3).Infof("Stopping reflector %s (%s) from %s", r.expectedTypeName, r.resyncPeriod, r.name)
}

var (
	// nothing will ever be sent down this channel
	neverExitWatch <-chan time.Time = make(chan time.Time)

	// Used to indicate that watching stopped because of a signal from the stop
	// channel passed in from a client of the reflector.
	errorStopRequested = errors.New("stop requested")
)

// resyncChan returns a channel which will receive something when a resync is
// required, and a cleanup function.
// 获取resync定时器
func (r *Reflector) resyncChan() (<-chan time.Time, func() bool) {
	// 周期为0,不用定时同步，返回永久的超时定时器
	if r.resyncPeriod == 0 {
		return neverExitWatch, func() bool { return false }
	}
	// The cleanup function is required: imagine the scenario where watches
	// always fail so we end up listing frequently. Then, if we don't
	// manually stop the timer, we could end up with many timers active
	// concurrently.
	// 构建定时器
	t := r.clock.NewTimer(r.resyncPeriod)
	return t.C(), t.Stop
}

// ListAndWatch first lists all items and get the resource version at the moment of call,
// and then use the resource version to watch.
// It returns error if ListAndWatch didn't even try to initialize watch.
func (r *Reflector) ListAndWatch(stopCh <-chan struct{}) error {
	klog.V(3).Infof("Listing and watching %v from %s", r.expectedTypeName, r.name)
	var resourceVersion string

	// 数据采用版本的方式记录，数据每变化(添加、删除、更新)都会触发版本更新，
	// 避免全量数据访问。以apiserver资源监控为例，只要监控比缓存中资源版本大的对象就可以了，
	// 把变化的部分更新到缓存中就可以达到与apiserver一致的效果，一般资源的初始版本为0，从0版本开始列举就是全量的对象了
	options := metav1.ListOptions{ResourceVersion: r.relistResourceVersion()}

	if err := func() error {
		initTrace := trace.New("Reflector ListAndWatch", trace.Field{Key: "name", Value: r.name})
		defer initTrace.LogIfLong(10 * time.Second)
		var list runtime.Object
		var paginatedResult bool
		var err error
		listCh := make(chan struct{}, 1)
		panicCh := make(chan interface{}, 1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					panicCh <- r
				}
			}()
			// Attempt to gather list in chunks, if supported by listerWatcher, if not, the first
			// list request will return the full response.
			pager := pager.New(pager.SimplePageFunc(func(opts metav1.ListOptions) (runtime.Object, error) {
				// 列举资源，这部分是apimachery相关的内容
				return r.listerWatcher.List(opts)
			}))
			switch {
			case r.WatchListPageSize != 0:
				pager.PageSize = r.WatchListPageSize
			case r.paginatedResult:
				// We got a paginated result initially. Assume this resource and server honor
				// paging requests (i.e. watch cache is probably disabled) and leave the default
				// pager size set.
			case options.ResourceVersion != "" && options.ResourceVersion != "0":
				// User didn't explicitly request pagination.
				//
				// With ResourceVersion != "", we have a possibility to list from watch cache,
				// but we do that (for ResourceVersion != "0") only if Limit is unset.
				// To avoid thundering herd on etcd (e.g. on master upgrades), we explicitly
				// switch off pagination to force listing from watch cache (if enabled).
				// With the existing semantic of RV (result is at least as fresh as provided RV),
				// this is correct and doesn't lead to going back in time.
				//
				// We also don't turn off pagination for ResourceVersion="0", since watch cache
				// is ignoring Limit in that case anyway, and if watch cache is not enabled
				// we don't introduce regression.
				pager.PageSize = 0
			}

			list, paginatedResult, err = pager.List(context.Background(), options)
			if isExpiredError(err) || isTooLargeResourceVersionError(err) {
				r.setIsLastSyncResourceVersionUnavailable(true)
				// Retry immediately if the resource version used to list is unavailable.
				// The pager already falls back to full list if paginated list calls fail due to an "Expired" error on
				// continuation pages, but the pager might not be enabled, the full list might fail because the
				// resource version it is listing at is expired or the cache may not yet be synced to the provided
				// resource version. So we need to fallback to resourceVersion="" in all to recover and ensure
				// the reflector makes forward progress.
				list, paginatedResult, err = pager.List(context.Background(), metav1.ListOptions{ResourceVersion: r.relistResourceVersion()})
			}
			close(listCh)
		}()
		select {
		case <-stopCh:
			return nil
		case r := <-panicCh:
			panic(r)
		case <-listCh:
		}
		initTrace.Step("Objects listed", trace.Field{Key: "error", Value: err})
		if err != nil {
			klog.Warningf("%s: failed to list %v: %v", r.name, r.expectedTypeName, err)
			return fmt.Errorf("failed to list %v: %v", r.expectedTypeName, err)
		}

		// We check if the list was paginated and if so set the paginatedResult based on that.
		// However, we want to do that only for the initial list (which is the only case
		// when we set ResourceVersion="0"). The reasoning behind it is that later, in some
		// situations we may force listing directly from etcd (by setting ResourceVersion="")
		// which will return paginated result, even if watch cache is enabled. However, in
		// that case, we still want to prefer sending requests to watch cache if possible.
		//
		// Paginated result returned for request with ResourceVersion="0" mean that watch
		// cache is disabled and there are a lot of objects of a given type. In such case,
		// there is no need to prefer listing from watch cache.
		if options.ResourceVersion == "0" && paginatedResult {
			r.paginatedResult = true
		}

		r.setIsLastSyncResourceVersionUnavailable(false) // list was successful
		// 下面的代码主要是利用apimachinery相关的函数实现，就是把列举返回的结果转换为对象数组
		// 下面的代码大部分来自apimachinery，此处不做过多说明，读者只要知道实现什么功能就行了
		listMetaInterface, err := meta.ListAccessor(list)
		if err != nil {
			return fmt.Errorf("unable to understand list result %#v: %v", list, err)
		}
		resourceVersion = listMetaInterface.GetResourceVersion()
		initTrace.Step("Resource version extracted")
		items, err := meta.ExtractList(list)
		if err != nil {
			return fmt.Errorf("unable to understand list result %#v (%v)", list, err)
		}
		initTrace.Step("Objects extracted")
		// 以上部分都是对象实例化的过程，可以称之为反射，也是Reflector这个名字的主要来源，本文不是讲解反射原理的，
		// 而是作为SharedInformer的前端，所以我们重点介绍的是对象在SharedInformer中流转过程，所以反射原理部分不做为重点讲解
		// 这可是真正从apiserver同步过来的全量对象，所以要同步到DeltaFIFO中
		if err := r.syncWith(items, resourceVersion); err != nil {
			return fmt.Errorf("unable to sync list result: %v", err)
		}
		initTrace.Step("SyncWith done")
		// 设置最新的同步的对象版本
		r.setLastSyncResourceVersion(resourceVersion)
		initTrace.Step("Resource version updated")
		return nil
	}(); err != nil {
		return err
	}

	// 启动一个后台协程实现定期的同步操作，这个同步就是将SharedInformer里面的对象全量以同步事件的方式通知使用者
	// 我们暂且称之为“后台同步协程”，Run()函数退出需要后台同步协程退出，所以下面的cancelCh就是干这个用的
	// 利用defer close(cancelCh)实现的，而resyncerrc是后台同步协程反向通知Run()函数的报错通道
	// 当后台同步协程出错，Run()函数接收到信号就可以退出了
	resyncerrc := make(chan error, 1)
	cancelCh := make(chan struct{})
	defer close(cancelCh)
	//后台同步协程
	go func() {
		// resyncCh返回的就是一个定时器，如果resyncPeriod这个为0那么就会返回一个永久定时器，cleanup函数是用来清理定时器的
		resyncCh, cleanup := r.resyncChan()
		defer func() {
			cleanup() // Call the last one written into cleanup
		}()
		// 死循环等待各种信号
		for {
			// 只有定时器有信号才继续处理，其他的都会退出
			select {
			case <-resyncCh:
			case <-stopCh:
				return
			case <-cancelCh:
				return
			}
			// ShouldResync是个函数地址，创建反射器对象的时候传入，即便时间到了，也要通过函数问问是否需要同步
			if r.ShouldResync == nil || r.ShouldResync() {
				// store是DeltaFIFO，DeltaFIFO.Resync()做了什么
				// 就在这里实现了同步，同步以全量对象同步事件的方式通知使用者
				klog.V(4).Infof("%s: forcing resync", r.name)
				if err := r.store.Resync(); err != nil {
					resyncerrc <- err
					return
				}
			}
			// 清理掉当前的计时器，获取下一个同步时间定时器
			cleanup()
			resyncCh, cleanup = r.resyncChan()
		}
	}()

	for {
		// give the stopCh a chance to stop the loop, even in case of continue statements further down on errors
		// 如果有退出信号就立刻返回，否则就会往下走，因为有default.
		select {
		case <-stopCh:
			return nil
		default:
		}

		// 计算wath的超时时间
		timeoutSeconds := int64(minWatchTimeout.Seconds() * (rand.Float64() + 1.0))
		// 设置watch的选项，因为前期列举了全量对象，从这里只要监听最新版本以后的资源就可以了
		// 如果没有资源变化总不能一直挂着吧？也不知道是卡死了还是怎么了，所以有一个超时会好一点
		options = metav1.ListOptions{
			ResourceVersion: resourceVersion,
			// We want to avoid situations of hanging watchers. Stop any watchers that do not
			// receive any events within the timeout window.
			TimeoutSeconds: &timeoutSeconds,
			// To reduce load on kube-apiserver on watch restarts, you may enable watch bookmarks.
			// Reflector doesn't assume bookmarks are returned at all (if the server do not support
			// watch bookmarks, it will ignore this field).
			AllowWatchBookmarks: true,
		}

		// start the clock before sending the request, since some proxies won't flush headers until after the first watch event is sent
		start := r.clock.Now()
		// 开始监控对象
		w, err := r.listerWatcher.Watch(options)
		if err != nil {
			// If this is "connection refused" error, it means that most likely apiserver is not responsive.
			// It doesn't make sense to re-list all objects because most likely we will be able to restart
			// watch where we ended.
			// If that's the case begin exponentially backing off and resend watch request.
			// Do the same for "429" errors.
			if utilnet.IsConnectionRefused(err) || apierrors.IsTooManyRequests(err) {
				<-r.initConnBackoffManager.Backoff().C()
				continue
			}
			return err
		}

		// watch返回是流，apiserver会将变化的资源通过这个流发送出来，client-go最终通过chan实现的
		// 所以watchHandler()是一个需要持续从chan读取数据的流程，所以需要传入resyncerrc和stopCh
		// 用于异步通知退出或者后台同步协程错误
		if err := r.watchHandler(start, w, &resourceVersion, resyncerrc, stopCh); err != nil {
			if err != errorStopRequested {
				switch {
				case isExpiredError(err):
					// Don't set LastSyncResourceVersionUnavailable - LIST call with ResourceVersion=RV already
					// has a semantic that it returns data at least as fresh as provided RV.
					// So first try to LIST with setting RV to resource version of last observed object.
					klog.V(4).Infof("%s: watch of %v closed with: %v", r.name, r.expectedTypeName, err)
				case apierrors.IsTooManyRequests(err):
					klog.V(2).Infof("%s: watch of %v returned 429 - backing off", r.name, r.expectedTypeName)
					<-r.initConnBackoffManager.Backoff().C()
					continue
				default:
					klog.Warningf("%s: watch of %v ended with: %v", r.name, r.expectedTypeName, err)
				}
			}
			return nil
		}
	}
}

// syncWith replaces the store's items with the given list.
// 实现apiserver的全量同步
func (r *Reflector) syncWith(items []runtime.Object, resourceVersion string) error {
	// slice类型转换
	found := make([]interface{}, 0, len(items))
	for _, item := range items {
		found = append(found, item)
	}
	// 调用DeltaFIFO的Replace()接口，这个接口就是用于同步全量对象的
	return r.store.Replace(found, resourceVersion)
}

// watchHandler watches w and keeps *resourceVersion up to date.
// 从watch返回的chan中持续读取变化的资源，并转换为DeltaFIFO相应的调用
func (r *Reflector) watchHandler(start time.Time, w watch.Interface, resourceVersion *string, errc chan error, stopCh <-chan struct{}) error {
	eventCount := 0

	// Stopping the watcher should be idempotent and if we return from this function there's no way
	// we're coming back in with the same watch interface.
	defer w.Stop()
	// 无限循环的从chan中持续读取资源变化，增量的变化同时监控变化
loop:
	for {
		select {
		// 退出信号
		case <-stopCh:
			return errorStopRequested
			// 后台协程同步错误
		case err := <-errc:
			return err
		case event, ok := <-w.ResultChan():
			if !ok {
				break loop
			}
			if event.Type == watch.Error {
				return apierrors.FromObject(event.Object)
			}
			if r.expectedType != nil {
				if e, a := r.expectedType, reflect.TypeOf(event.Object); e != a {
					utilruntime.HandleError(fmt.Errorf("%s: expected type %v, but watch event object had type %v", r.name, e, a))
					continue
				}
			}
			if r.expectedGVK != nil {
				if e, a := *r.expectedGVK, event.Object.GetObjectKind().GroupVersionKind(); e != a {
					utilruntime.HandleError(fmt.Errorf("%s: expected gvk %v, but watch event object had gvk %v", r.name, e, a))
					continue
				}
			}
			meta, err := meta.Accessor(event.Object)
			if err != nil {
				utilruntime.HandleError(fmt.Errorf("%s: unable to understand watch event %#v", r.name, event))
				continue
			}
			newResourceVersion := meta.GetResourceVersion()
			switch event.Type {
			case watch.Added:
				err := r.store.Add(event.Object)
				if err != nil {
					utilruntime.HandleError(fmt.Errorf("%s: unable to add watch event object (%#v) to store: %v", r.name, event.Object, err))
				}
			case watch.Modified:
				err := r.store.Update(event.Object)
				if err != nil {
					utilruntime.HandleError(fmt.Errorf("%s: unable to update watch event object (%#v) to store: %v", r.name, event.Object, err))
				}
			case watch.Deleted:
				// TODO: Will any consumers need access to the "last known
				// state", which is passed in event.Object? If so, may need
				// to change this.
				err := r.store.Delete(event.Object)
				if err != nil {
					utilruntime.HandleError(fmt.Errorf("%s: unable to delete watch event object (%#v) from store: %v", r.name, event.Object, err))
				}
			case watch.Bookmark:
				// A `Bookmark` means watch has synced here, just update the resourceVersion
			default:
				utilruntime.HandleError(fmt.Errorf("%s: unable to understand watch event %#v", r.name, event))
			}
			*resourceVersion = newResourceVersion
			r.setLastSyncResourceVersion(newResourceVersion)
			if rvu, ok := r.store.(ResourceVersionUpdater); ok {
				rvu.UpdateResourceVersion(newResourceVersion)
			}
			eventCount++
		}
	}

	watchDuration := r.clock.Since(start)
	if watchDuration < 1*time.Second && eventCount == 0 {
		return fmt.Errorf("very short watch: %s: Unexpected watch close - watch lasted less than a second and no items received", r.name)
	}
	klog.V(4).Infof("%s: Watch close - %v total %v items received", r.name, r.expectedTypeName, eventCount)
	return nil
}

// LastSyncResourceVersion is the resource version observed when last sync with the underlying store
// The value returned is not synchronized with access to the underlying store and is not thread-safe
func (r *Reflector) LastSyncResourceVersion() string {
	r.lastSyncResourceVersionMutex.RLock()
	defer r.lastSyncResourceVersionMutex.RUnlock()
	return r.lastSyncResourceVersion
}

func (r *Reflector) setLastSyncResourceVersion(v string) {
	r.lastSyncResourceVersionMutex.Lock()
	defer r.lastSyncResourceVersionMutex.Unlock()
	// 设置已经获取到资源的最新版本
	r.lastSyncResourceVersion = v
}

// relistResourceVersion determines the resource version the reflector should list or relist from.
// Returns either the lastSyncResourceVersion so that this reflector will relist with a resource
// versions no older than has already been observed in relist results or watch events, or, if the last relist resulted
// in an HTTP 410 (Gone) status code, returns "" so that the relist will use the latest resource version available in
// etcd via a quorum read.
// relistResourceVersion确定反射器应列出或重新列出的资源版本
func (r *Reflector) relistResourceVersion() string {
	r.lastSyncResourceVersionMutex.RLock()
	defer r.lastSyncResourceVersionMutex.RUnlock()

	if r.isLastSyncResourceVersionUnavailable {
		// Since this reflector makes paginated list requests, and all paginated list requests skip the watch cache
		// if the lastSyncResourceVersion is unavailable, we set ResourceVersion="" and list again to re-establish reflector
		// to the latest available ResourceVersion, using a consistent read from etcd.
		return ""
	}
	if r.lastSyncResourceVersion == "" {
		// For performance reasons, initial list performed by reflector uses "0" as resource version to allow it to
		// be served from the watch cache if it is enabled.
		return "0"
	}
	return r.lastSyncResourceVersion
}

// setIsLastSyncResourceVersionUnavailable sets if the last list or watch request with lastSyncResourceVersion returned
// "expired" or "too large resource version" error.
// 设置“上次同步资源版本不可用”
func (r *Reflector) setIsLastSyncResourceVersionUnavailable(isUnavailable bool) {
	r.lastSyncResourceVersionMutex.Lock()
	defer r.lastSyncResourceVersionMutex.Unlock()
	r.isLastSyncResourceVersionUnavailable = isUnavailable
}

func isExpiredError(err error) bool {
	// In Kubernetes 1.17 and earlier, the api server returns both apierrors.StatusReasonExpired and
	// apierrors.StatusReasonGone for HTTP 410 (Gone) status code responses. In 1.18 the kube server is more consistent
	// and always returns apierrors.StatusReasonExpired. For backward compatibility we can only remove the apierrors.IsGone
	// check when we fully drop support for Kubernetes 1.17 servers from reflectors.
	return apierrors.IsResourceExpired(err) || apierrors.IsGone(err)
}

func isTooLargeResourceVersionError(err error) bool {
	if apierrors.HasStatusCause(err, metav1.CauseTypeResourceVersionTooLarge) {
		return true
	}
	// In Kubernetes 1.17.0-1.18.5, the api server doesn't set the error status cause to
	// metav1.CauseTypeResourceVersionTooLarge to indicate that the requested minimum resource
	// version is larger than the largest currently available resource version. To ensure backward
	// compatibility with these server versions we also need to detect the error based on the content
	// of the error message field.
	if !apierrors.IsTimeout(err) {
		return false
	}
	apierr, ok := err.(apierrors.APIStatus)
	if !ok || apierr == nil || apierr.Status().Details == nil {
		return false
	}
	for _, cause := range apierr.Status().Details.Causes {
		// Matches the message returned by api server 1.17.0-1.18.5 for this error condition
		if cause.Message == "Too large resource version" {
			return true
		}
	}
	return false
}
```