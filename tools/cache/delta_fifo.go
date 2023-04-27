/*
Copyright 2014 The Kubernetes Authors.

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
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/sets"

	"k8s.io/klog/v2"
	utiltrace "k8s.io/utils/trace"
)

// DeltaFIFOOptions is the configuration parameters for DeltaFIFO. All are
// optional.
// 生产者-消费者队列，生产者为Reflector，消费者为Pop()函数
// 消费的数据存储到indexer中，可以通过informer的handler处理
// informer的handler处理的数据需要与存储在Indexer中的数据匹配。
// Pop的单位是一个Deltas，而不是Delta,同时实现了Queue和Store接口
// DeltaFIFO使用Deltas保存了对象状态的变更(Add/Delete/Update)信息(如Pod的删除添加..)
// Deltas缓存了针对相同对象的多个状态变更信息,如Pod的Deltas[0]可能更新了标签，
// Deltas[1]可能删除了该Pod.最老的状态变更信息为Newest(),最新的状态变更信息为Oldest()。
// 使用中，获取DeltaFIFO中对象的key以及获取DeltaFIFO都以最新状态为准。
type DeltaFIFOOptions struct {

	// KeyFunction is used to figure out what key an object should have. (It's
	// exposed in the returned DeltaFIFO's KeyOf() method, with additional
	// handling around deleted objects and queue state).
	// Optional, the default is MetaNamespaceKeyFunc.
	// 对象键生成
	KeyFunction KeyFunc

	// KnownObjects is expected to return a list of keys that the consumer of
	// this queue "knows about". It is used to decide which items are missing
	// when Replace() is called; 'Deleted' deltas are produced for the missing items.
	// KnownObjects may be nil if you can tolerate missing deletions on Replace().
	// 实质上indexer
	KnownObjects KeyListerGetter

	// EmitDeltaTypeReplaced indicates that the queue consumer
	// understands the Replaced DeltaType. Before the `Replaced` event type was
	// added, calls to Replace() were handled the same as Sync(). For
	// backwards-compatibility purposes, this is false by default.
	// When true, `Replaced` events will be sent for items passed to a Replace() call.
	// When false, `Sync` events will be sent instead.
	EmitDeltaTypeReplaced bool
}

// DeltaFIFO is like FIFO, but differs in two ways.  One is that the
// accumulator associated with a given object's key is not that object
// but rather a Deltas, which is a slice of Delta values for that
// object.  Applying an object to a Deltas means to append a Delta
// except when the potentially appended Delta is a Deleted and the
// Deltas already ends with a Deleted.  In that case the Deltas does
// not grow, although the terminal Deleted will be replaced by the new
// Deleted if the older Deleted's object is a
// DeletedFinalStateUnknown.
//
// The other difference is that DeltaFIFO has two additional ways that
// an object can be applied to an accumulator: Replaced and Sync.
// If EmitDeltaTypeReplaced is not set to true, Sync will be used in
// replace events for backwards compatibility.  Sync is used for periodic
// resync events.
//
// DeltaFIFO is a producer-consumer queue, where a Reflector is
// intended to be the producer, and the consumer is whatever calls
// the Pop() method.
//
// DeltaFIFO solves this use case:
//   - You want to process every object change (delta) at most once.
//   - When you process an object, you want to see everything
//     that's happened to it since you last processed it.
//   - You want to process the deletion of some of the objects.
//   - You might want to periodically reprocess objects.
//
// DeltaFIFO's Pop(), Get(), and GetByKey() methods return
// interface{} to satisfy the Store/Queue interfaces, but they
// will always return an object of type Deltas. List() returns
// the newest object from each accumulator in the FIFO.
//
// A DeltaFIFO's knownObjects KeyListerGetter provides the abilities
// to list Store keys and to get objects by Store key.  The objects in
// question are called "known objects" and this set of objects
// modifies the behavior of the Delete, Replace, and Resync methods
// (each in a different way).
//
// A note on threading: If you call Pop() in parallel from multiple
// threads, you could end up with multiple threads processing slightly
// different versions of the same object.
// DeltaFIFO一个按序的(先入先出)kubernetes对象变化的队列
type DeltaFIFO struct {
	// lock/cond protects access to 'items' and 'queue'.
	// 读写锁，涉及到同时读写，读写锁性能要高
	lock sync.RWMutex
	// Pop()接口使用，在没有对象的时候可以阻塞，内部锁复用读写锁
	cond sync.Cond

	// `items` maps a key to a Deltas.
	// Each such Deltas has at least one Delta.
	// 按照kv存储对象,对象的键->但是存储的是对象的Deltas数组
	items map[string]Deltas

	// `queue` maintains FIFO order of keys for consumption in Pop().
	// There are no duplicates in `queue`.
	// A key is in `queue` if and only if it is in `items`.
	// map存储无序，实现先入先出，存储是对象的键
	queue []string

	/*******
	判断是否已同步populated和initialPopulationCount
	是否已同步指的是第一次从apiserver获取全量对象是否已经全部通知到外部，
	也就是通过Pop()被取走。所谓的同步就是指apiserver的状态已经同步到缓存中了，
	也就是Indexer中
	*******/
	// populated is true if the first batch of items inserted by Replace() has been populated
	// or Delete/Add/Update/AddIfNotPresent was called first.
	// 通过Replace()接口将第一批对象放入队列，，或者第一次调用增删，改接口的标记
	populated bool
	// initialPopulationCount is the number of items inserted by the first call of Replace()
	// 通过Replace()接口将第一批对象放入队列的对象数量
	initialPopulationCount int

	// keyFunc is used to make the key used for queued item
	// insertion and retrieval, and should be deterministic.
	// 对象键计算函数
	keyFunc KeyFunc

	// knownObjects list keys that are "known" --- affecting Delete(),
	// Replace(), and Resync()
	// KeyListerGetter接口中的方法ListKeys和GetByKey也是Store接口中的方法
	// knownObjects能够被赋值为实现了Store的类型指针
	knownObjects KeyListerGetter

	// Used to indicate a queue is closed so a control loop can exit when a queue is empty.
	// Currently, not used to gate any of CRUD operations.
	closed bool

	// emitDeltaTypeReplaced is whether to emit the Replaced or Sync
	// DeltaType when Replace() is called (to preserve backwards compat).
	// 是发出替换还是同步(true)
	emitDeltaTypeReplaced bool
}

// DeltaType is the type of a change (addition, deletion, etc)
type DeltaType string

// Change type definition
// delta 类型
const (
	Added   DeltaType = "Added"
	Updated DeltaType = "Updated"
	Deleted DeltaType = "Deleted"
	// Replaced is emitted when we encountered watch errors and had to do a
	// relist. We don't know if the replaced object has changed.
	//
	// NOTE: Previous versions of DeltaFIFO would use Sync for Replace events
	// as well. Hence, Replaced is only emitted when the option
	// EmitDeltaTypeReplaced is true.
	Replaced DeltaType = "Replaced"
	// Sync is for synthetic events during a periodic resync.
	Sync DeltaType = "Sync"
)

// Delta is a member of Deltas (a list of Delta objects) which
// in its turn is the type stored by a DeltaFIFO. It tells you what
// change happened, and the object's state after* that change.
//
// [*] Unless the change is a deletion, and then you'll get the final
// state of the object before it was deleted.
// 操作类型
// 保存操作执行后对象
type Delta struct {
	// Delta类型，比如增、减
	Type DeltaType
	// 对象，Delta的粒度是一个对象
	Object interface{}
}

// Deltas is a list of one or more 'Delta's to an individual object.
// The oldest delta is at index 0, the newest delta is the last one.
type Deltas []Delta

// NewDeltaFIFO returns a Queue which can be used to process changes to items.
//
// keyFunc is used to figure out what key an object should have. (It is
// exposed in the returned DeltaFIFO's KeyOf() method, with additional handling
// around deleted objects and queue state).
//
// 'knownObjects' may be supplied to modify the behavior of Delete,
// Replace, and Resync.  It may be nil if you do not need those
// modifications.
//
// TODO: consider merging keyLister with this object, tracking a list of
// "known" keys when Pop() is called. Have to think about how that
// affects error retrying.
//
//	NOTE: It is possible to misuse this and cause a race when using an
//	external known object source.
//	Whether there is a potential race depends on how the consumer
//	modifies knownObjects. In Pop(), process function is called under
//	lock, so it is safe to update data structures in it that need to be
//	in sync with the queue (e.g. knownObjects).
//
//	Example:
//	In case of sharedIndexInformer being a consumer
//	(https://github.com/kubernetes/kubernetes/blob/0cdd940f/staging/src/k8s.io/client-go/tools/cache/shared_informer.go#L192),
//	there is no race as knownObjects (s.indexer) is modified safely
//	under DeltaFIFO's lock. The only exceptions are GetStore() and
//	GetIndexer() methods, which expose ways to modify the underlying
//	storage. Currently these two methods are used for creating Lister
//	and internal tests.
//
// Also see the comment on DeltaFIFO.
//
// Warning: This constructs a DeltaFIFO that does not differentiate between
// events caused by a call to Replace (e.g., from a relist, which may
// contain object updates), and synthetic events caused by a periodic resync
// (which just emit the existing object). See https://issue.k8s.io/86015 for details.
//
// Use `NewDeltaFIFOWithOptions(DeltaFIFOOptions{..., EmitDeltaTypeReplaced: true})`
// instead to receive a `Replaced` event depending on the type.
//
// Deprecated: Equivalent to NewDeltaFIFOWithOptions(DeltaFIFOOptions{KeyFunction: keyFunc, KnownObjects: knownObjects})
func NewDeltaFIFO(keyFunc KeyFunc, knownObjects KeyListerGetter) *DeltaFIFO {
	return NewDeltaFIFOWithOptions(DeltaFIFOOptions{
		KeyFunction:  keyFunc,
		KnownObjects: knownObjects,
	})
}

// NewDeltaFIFOWithOptions returns a Queue which can be used to process changes to
// items. See also the comment on DeltaFIFO.
func NewDeltaFIFOWithOptions(opts DeltaFIFOOptions) *DeltaFIFO {
	if opts.KeyFunction == nil {
		opts.KeyFunction = MetaNamespaceKeyFunc
	}

	f := &DeltaFIFO{
		items:        map[string]Deltas{},
		queue:        []string{},
		keyFunc:      opts.KeyFunction,
		knownObjects: opts.KnownObjects,

		emitDeltaTypeReplaced: opts.EmitDeltaTypeReplaced,
	}
	f.cond.L = &f.lock
	return f
}

var (
	_ = Queue(&DeltaFIFO{}) // DeltaFIFO is a Queue
)

var (
	// ErrZeroLengthDeltasObject is returned in a KeyError if a Deltas
	// object with zero length is encountered (should be impossible,
	// but included for completeness).
	ErrZeroLengthDeltasObject = errors.New("0 length Deltas object; can't get key")
)

// Close the queue.
func (f *DeltaFIFO) Close() {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.closed = true
	f.cond.Broadcast()
}

// KeyOf exposes f's keyFunc, but also detects the key of a Deltas object or
// DeletedFinalStateUnknown objects.
// 计算对象键
// 计算对象键:如果Deltas不为空，以最新的计算的对象键？why
func (f *DeltaFIFO) KeyOf(obj interface{}) (string, error) {
	if d, ok := obj.(Deltas); ok {
		if len(d) == 0 {
			return "", KeyError{obj, ErrZeroLengthDeltasObject}
		}
		obj = d.Newest().Object
	}
	if d, ok := obj.(DeletedFinalStateUnknown); ok {
		return d.Key, nil
	}
	return f.keyFunc(obj)
}

// HasSynced returns true if an Add/Update/Delete/AddIfNotPresent are called first,
// or the first batch of items inserted by Replace() has been popped.
// 是否全量同步
// 同步就是全量内容已经进入Indexer，Indexer已经是系统中对象的全量快照
func (f *DeltaFIFO) HasSynced() bool {
	f.lock.Lock()
	defer f.lock.Unlock()
	//一次同步全量对象后，并且全部Pop()出去才能算是同步完成
	return f.hasSynced_locked()
}

func (f *DeltaFIFO) hasSynced_locked() bool {
	return f.populated && f.initialPopulationCount == 0
}

/**************DeltaFIFO store ****************/

// Add inserts an item, and puts it in the queue. The item is only enqueued
// if it doesn't already exist in the set.
// 添加对象
func (f *DeltaFIFO) Add(obj interface{}) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	// 队列第一次写入操作，将标志设为true
	f.populated = true
	return f.queueActionLocked(Added, obj)
}

// Update is just like Add, but makes an Updated Delta.
func (f *DeltaFIFO) Update(obj interface{}) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	f.populated = true
	return f.queueActionLocked(Updated, obj)
}

// Delete is just like Add, but makes a Deleted Delta. If the given
// object does not already exist, it will be ignored. (It may have
// already been deleted by a Replace (re-list), for example.)  In this
// method `f.knownObjects`, if not nil, provides (via GetByKey)
// _additional_ objects that are considered to already exist.
// 删除对象接口
func (f *DeltaFIFO) Delete(obj interface{}) error {
	// 得到对象键
	id, err := f.KeyOf(obj)
	if err != nil {
		return KeyError{obj, err}
	}
	f.lock.Lock()
	defer f.lock.Unlock()
	f.populated = true
	// !!! knownObjects就是Indexer，里面存有已知全部的对象
	if f.knownObjects == nil {
		// 没有Indexer的条件下只能通过自己存储的对象查一下
		if _, exists := f.items[id]; !exists {
			// Presumably, this was deleted when a relist happened.
			// Don't provide a second report of the same deletion.
			return nil
		}
	} else {
		// We only want to skip the "deletion" action if the object doesn't
		// exist in knownObjects and it doesn't have corresponding item in items.
		// Note that even if there is a "deletion" action in items, we can ignore it,
		// because it will be deduped automatically in "queueActionLocked"
		// DeltaFIFO本身和Indexer里面有任何一个有这个对象多算存在
		_, exists, err := f.knownObjects.GetByKey(id)
		_, itemsExist := f.items[id]
		if err == nil && !exists && !itemsExist {
			// Presumably, this was deleted when a relist happened.
			// Don't provide a second report of the same deletion.
			return nil
		}
	}

	// exist in items and/or KnownObjects
	return f.queueActionLocked(Deleted, obj)
}

// AddIfNotPresent inserts an item, and puts it in the queue. If the item is already
// present in the set, it is neither enqueued nor added to the set.
//
// This is useful in a single producer/consumer scenario so that the consumer can
// safely retry items without contending with the producer and potentially enqueueing
// stale items.
//
// Important: obj must be a Deltas (the output of the Pop() function). Yes, this is
// different from the Add/Update/Delete functions.
// 添加不存在的对象
// AddIfNotPresent()在Pop()函数中使用了一次，但是在调用这个接口的时候已经从map中删除了，
// 用来保险的，因为Pop()本身就存在重入队列的可能，外部如果判断返回错误重入队列就可能会重复
func (f *DeltaFIFO) AddIfNotPresent(obj interface{}) error {
	// 对象必须是Deltas数组，就是通过Pop（）弹出的对象
	deltas, ok := obj.(Deltas)
	if !ok {
		return fmt.Errorf("object must be of type deltas, but got: %#v", obj)
	}
	// 计算对象键,多个Delta都是一个对象，所以用最新的就
	id, err := f.KeyOf(deltas)
	if err != nil {
		return KeyError{obj, err}
	}
	f.lock.Lock()
	defer f.lock.Unlock()
	// 添加
	f.addIfNotPresent(id, deltas)
	return nil
}

// addIfNotPresent inserts deltas under id if it does not exist, and assumes the caller
// already holds the fifo lock.
// 添加不存在对象的实现
func (f *DeltaFIFO) addIfNotPresent(id string, deltas Deltas) {
	f.populated = true
	// 判断对象是否存在
	if _, exists := f.items[id]; exists {
		return
	}

	// 添加队列
	f.queue = append(f.queue, id)
	f.items[id] = deltas
	// 通知等待
	f.cond.Broadcast()
}

// re-listing and watching can deliver the same update multiple times in any
// order. This will combine the most recent two deltas if they are the same.
// 合并操作，去掉冗余的delta
// 合并的也就是删除，删除操作其实可以视为一个
func dedupDeltas(deltas Deltas) Deltas {
	n := len(deltas)
	if n < 2 {
		return deltas
	}
	// 每次add都会执行,所以只需要取出最后两个
	a := &deltas[n-1]
	b := &deltas[n-2]
	// 判断如果是重复的，那就删除这两个delta把合并后的追加到Deltas数组尾部
	if out := isDup(a, b); out != nil {
		deltas[n-2] = *out
		return deltas[:n-1]
	}
	return deltas
}

// If a & b represent the same event, returns the delta that ought to be kept.
// Otherwise, returns nil.
// TODO: is there anything other than deletions that need deduping?
// 判断两个Delta是否是重复的
func isDup(a, b *Delta) *Delta {
	// 只能判断是否为删除类操作，和我们上面的判断相同
	if out := isDeletionDup(a, b); out != nil {
		return out
	}
	// TODO: Detect other duplicate situations? Are there any?
	return nil
}

// keep the one with the most information if both are deletions.
// 判断两个操作是否为删除
func isDeletionDup(a, b *Delta) *Delta {
	if b.Type != Deleted || a.Type != Deleted {
		return nil
	}
	// Do more sophisticated checks, or is this sufficient?
	//做更复杂的检查，还是足够？
	// 理论上返回最后一个比较好，但是对象已经不再系统监控范围，前一个删除状态是好
	if _, ok := b.Object.(DeletedFinalStateUnknown); ok {
		return a
	}
	return b
}

// queueActionLocked appends to the delta list for the object.
// Caller must lock first.
// 队列操作，把“动作”放入deltas中，加锁
func (f *DeltaFIFO) queueActionLocked(actionType DeltaType, obj interface{}) error {
	// 计算对象键
	id, err := f.KeyOf(obj)
	if err != nil {
		return KeyError{obj, err}
	}
	// 旧的对象操作数组
	oldDeltas := f.items[id]
	// 同一个对象的多次操作，所以要追加到Deltas数组中
	newDeltas := append(oldDeltas, Delta{actionType, obj})
	// 合并操作，去掉冗余的delta
	newDeltas = dedupDeltas(newDeltas)

	if len(newDeltas) > 0 {
		// 判断对象是否已经存在
		if _, exists := f.items[id]; !exists {
			// 如果对象没有存在过，那就放入队列中，如果存在说明已经在queue中了，也就没必要再添加了
			f.queue = append(f.queue, id)
		}
		f.items[id] = newDeltas
		// 更新Deltas数组，通知所有调用Pop()的人
		f.cond.Broadcast()
	} else {
		// This never happens, because dedupDeltas never returns an empty list
		// when given a non-empty list (as it is here).
		// If somehow it happens anyway, deal with it but complain.
		if oldDeltas == nil {
			klog.Errorf("Impossible dedupDeltas for id=%q: oldDeltas=%#+v, obj=%#+v; ignoring", id, oldDeltas, obj)
			return nil
		}
		klog.Errorf("Impossible dedupDeltas for id=%q: oldDeltas=%#+v, obj=%#+v; breaking invariant by storing empty Deltas", id, oldDeltas, obj)
		// 更新Deltas数组，为空？？
		f.items[id] = newDeltas
		return fmt.Errorf("Impossible dedupDeltas for id=%q: oldDeltas=%#+v, obj=%#+v; broke DeltaFIFO invariant by storing empty Deltas", id, oldDeltas, obj)
	}
	return nil
}

// List returns a list of all the items; it returns the object
// from the most recent Delta.
// You should treat the items returned inside the deltas as immutable.
func (f *DeltaFIFO) List() []interface{} {
	f.lock.RLock()
	defer f.lock.RUnlock()
	return f.listLocked()
}

func (f *DeltaFIFO) listLocked() []interface{} {
	list := make([]interface{}, 0, len(f.items))
	for _, item := range f.items {
		list = append(list, item.Newest().Object)
	}
	return list
}

// ListKeys returns a list of all the keys of the objects currently
// in the FIFO.
// 获取DeltaFIFO.items的所有的key
func (f *DeltaFIFO) ListKeys() []string {
	f.lock.RLock()
	defer f.lock.RUnlock()
	list := make([]string, 0, len(f.queue))
	for _, key := range f.queue {
		list = append(list, key)
	}
	return list
}

// Get returns the complete list of deltas for the requested item,
// or sets exists=false.
// You should treat the items returned inside the deltas as immutable.
// 获取对象键是相同的对象
func (f *DeltaFIFO) Get(obj interface{}) (item interface{}, exists bool, err error) {
	key, err := f.KeyOf(obj)
	if err != nil {
		return nil, false, KeyError{obj, err}
	}
	return f.GetByKey(key)
}

// GetByKey returns the complete list of deltas for the requested item,
// setting exists=false if that list is empty.
// You should treat the items returned inside the deltas as immutable.
// 通过对象键获取对象,对象为复制的对象，修改不会影响到原始数据
func (f *DeltaFIFO) GetByKey(key string) (item interface{}, exists bool, err error) {
	f.lock.RLock()
	defer f.lock.RUnlock()
	d, exists := f.items[key]
	if exists {
		// Copy item's slice so operations on this slice
		// won't interfere with the object we return.
		d = copyDeltas(d)
	}
	return d, exists, nil
}

// IsClosed checks if the queue is closed
// 判断是否关闭
func (f *DeltaFIFO) IsClosed() bool {
	f.lock.Lock()
	defer f.lock.Unlock()
	return f.closed
}

// Pop blocks until the queue has some items, and then returns one.  If
// multiple items are ready, they are returned in the order in which they were
// added/updated. The item is removed from the queue (and the store) before it
// is returned, so if you don't successfully process it, you need to add it back
// with AddIfNotPresent().
// process function is called under lock, so it is safe to update data structures
// in it that need to be in sync with the queue (e.g. knownKeys). The PopProcessFunc
// may return an instance of ErrRequeue with a nested error to indicate the current
// item should be requeued (equivalent to calling AddIfNotPresent under the lock).
// process should avoid expensive I/O operation so that other queue operations, i.e.
// Add() and Get(), won't be blocked for too long.
//
// Pop returns a 'Deltas', which has a complete list of all the things
// that happened to the object (deltas) while it was sitting in the queue.
// 消费reflector的数据
func (f *DeltaFIFO) Pop(process PopProcessFunc) (interface{}, error) {
	f.lock.Lock()
	defer f.lock.Unlock()
	for {
		for len(f.queue) == 0 {
			// When the queue is empty, invocation of Pop() is blocked until new item is enqueued.
			// When Close() is called, the f.closed is set and the condition is broadcasted.
			// Which causes this loop to continue and return from the Pop().
			// 先判断的是否有数据，后判断是否关闭
			if f.closed {
				return nil, ErrFIFOClosed
			}

			// 等待数据
			f.cond.Wait()
		}
		isInInitialList := !f.hasSynced_locked()
		//取出第一个对象
		id := f.queue[0]
		// 数组缩小，弹出第一个数据
		f.queue = f.queue[1:]
		depth := len(f.queue)
		// 同步对象计数减一，当减到0就说明外部已经全部同步完毕
		if f.initialPopulationCount > 0 {
			f.initialPopulationCount--
		}
		// 依据对象键取出对象
		item, ok := f.items[id]
		if !ok {
			// This should never happen
			klog.Errorf("Inconceivable! %q was in f.queue but not f.items; ignoring.", id)
			continue
		}
		// 删除对象
		delete(f.items, id)
		// Only log traces if the queue depth is greater than 10 and it takes more than
		// 100 milliseconds to process one item from the queue.
		// Queue depth never goes high because processing an item is locking the queue,
		// and new items can't be added until processing finish.
		// https://github.com/kubernetes/kubernetes/issues/103789
		if depth > 10 {
			trace := utiltrace.New("DeltaFIFO Pop Process",
				utiltrace.Field{Key: "ID", Value: id},
				utiltrace.Field{Key: "Depth", Value: depth},
				utiltrace.Field{Key: "Reason", Value: "slow event handlers blocking the queue"})
			defer trace.LogIfLong(100 * time.Millisecond)
		}
		// 处理对象的回调函数，由controller传入
		err := process(item, isInInitialList)
		// 重入队列
		if e, ok := err.(ErrRequeue); ok {
			f.addIfNotPresent(id, item)
			err = e.Err
		}
		// Don't need to copyDeltas here, because we're transferring
		// ownership to the caller.
		return item, err
	}
}

// Replace atomically does two things: (1) it adds the given objects
// using the Sync or Replace DeltaType and then (2) it does some deletions.
// In particular: for every pre-existing key K that is not the key of
// an object in `list` there is the effect of
// `Delete(DeletedFinalStateUnknown{K, O})` where O is current object
// of K.  If `f.knownObjects == nil` then the pre-existing keys are
// those in `f.items` and the current object of K is the `.Newest()`
// of the Deltas associated with K.  Otherwise the pre-existing keys
// are those listed by `f.knownObjects` and the current object of K is
// what `f.knownObjects.GetByKey(K)` returns.
// DeltaFIFO对外输出的就是所有目标的增量变化
// 所以每次全量更新都要判断对象是否已经删除，因为在全量更新前可能没有收到目标删除的请求。
//这一点与cache不同，cache的Replace()相当于重建，因为cache就是对象全量的一种内存映射，所以Replace()就等于重建
func (f *DeltaFIFO) Replace(list []interface{}, _ string) error {
	f.lock.Lock()
	defer f.lock.Unlock()
	keys := make(sets.String, len(list))

	// keep backwards compat for old clients
	// 同步
	action := Sync
	if f.emitDeltaTypeReplaced {
		action = Replaced
	}

	// Add Sync/Replaced action for each new item.
	// 遍历所有的输入目标
	for _, item := range list {
		// 计算目标键
		key, err := f.KeyOf(item)
		if err != nil {
			return KeyError{item, err}
		}
		// 记录处理过的目标键，采用set存储，是为了后续快速查找
		keys.Insert(key)
		// 放入队列
		if err := f.queueActionLocked(action, item); err != nil {
			return fmt.Errorf("couldn't enqueue object: %v", err)
		}
	}

	// 如果没有存储(indxer)的话，自己存储的就是所有的老对象，
	// 查找不在全量集合中的老对象即删除的对象
	if f.knownObjects == nil {
		// Do deletion detection against our own list.
		queuedDeletions := 0
		for k, oldItem := range f.items {
			// 目标在输入的对象中存在就忽略
			if keys.Has(k) {
				continue
			}
			// Delete pre-existing items not in the new list.
			// This could happen if watch deletion event was missed while
			// disconnected from apiserver.
			var deletedObj interface{}
			// 输入对象中没有，说明对象已经被删除
			if n := oldItem.Newest(); n != nil {
				deletedObj = n.Object
			}
			// 累计删除对象
			queuedDeletions++
			//队列中存储对象的Deltas数组中,可能已经存在Delete了，
			//避免重复，采用DeletedFinalStateUnknown这种类型
			if err := f.queueActionLocked(Deleted, DeletedFinalStateUnknown{k, deletedObj}); err != nil {
				return err
			}
		}

		// 如果populated还没有设置，说明是第一次并且还没有任何修改操作执行过
		if !f.populated {
			f.populated = true
			// While there shouldn't be any queued deletions in the initial
			// population of the queue, it's better to be on the safe side.
			// 记录第一次通过来的对象数量
			f.initialPopulationCount = keys.Len() + queuedDeletions
		}

		return nil
	}

	// Detect deletions not already in the queue.
	//处理检测某些目标删除但是Delta没有在队列中
	// 从存储中获取所有对象键
	knownKeys := f.knownObjects.ListKeys()
	queuedDeletions := 0
	for _, k := range knownKeys {
		// 忽略对象存在的情况
		if keys.Has(k) {
			continue
		}

		//获取对象
		deletedObj, exists, err := f.knownObjects.GetByKey(k)
		if err != nil {
			deletedObj = nil
			klog.Errorf("Unexpected error %v during lookup of key %v, placing DeleteFinalStateUnknown marker without object", err, k)
		} else if !exists {
			deletedObj = nil
			klog.Infof("Key %v does not exist in known objects store, placing DeleteFinalStateUnknown marker without object", k)
		}
		//累计删除对象
		queuedDeletions++
		if err := f.queueActionLocked(Deleted, DeletedFinalStateUnknown{k, deletedObj}); err != nil {
			return err
		}
	}

	// 计算initialPopulationCount值的时候增加了删除对象的数量
	if !f.populated {
		f.populated = true
		f.initialPopulationCount = keys.Len() + queuedDeletions
	}

	return nil
}

// Resync adds, with a Sync type of Delta, every object listed by
// `f.knownObjects` whose key is not already queued for processing.
// If `f.knownObjects` is `nil` then Resync does nothing.
// 重新同步，在cache实现是空的，这里面有具体实现
func (f *DeltaFIFO) Resync() error {
	f.lock.Lock()
	defer f.lock.Unlock()

	// 是否有indexer
	// 没有Indexer那么重新同步是没有意义的，因为连同步了哪些对象都不知道
	if f.knownObjects == nil {
		return nil
	}

	// 列举Indexer里面所有的对象键
	keys := f.knownObjects.ListKeys()
	for _, k := range keys {
		// 具体对象同步实现
		if err := f.syncKeyLocked(k); err != nil {
			return err
		}
	}
	return nil
}

// 具体对象同步实现
func (f *DeltaFIFO) syncKeyLocked(key string) error {
	// 获取对象
	obj, exists, err := f.knownObjects.GetByKey(key)
	if err != nil {
		klog.Errorf("Unexpected error %v during lookup of key %v, unable to queue object for sync", err, key)
		return nil
	} else if !exists {
		klog.Infof("Key %v does not exist in known objects store, unable to queue object for sync", key)
		return nil
	}

	// If we are doing Resync() and there is already an event queued for that object,
	// we ignore the Resync for it. This is to avoid the race, in which the resync
	// comes with the previous value of object (since queueing an event for the object
	// doesn't trigger changing the underlying store <knownObjects>.
	// 重新计算对象键值，传入的key是存在Indexer里面的对象键，可能与这里的计算方式
	id, err := f.KeyOf(obj)
	if err != nil {
		return KeyError{obj, err}
	}
	// 对象已经在存在,后续会通知对象的新变化，不需要再加更新
	if len(f.items[id]) > 0 {
		return nil
	}

	// 添加对象同步的Delta
	if err := f.queueActionLocked(Sync, obj); err != nil {
		return fmt.Errorf("couldn't queue object: %v", err)
	}
	return nil
}

// A KeyListerGetter is anything that knows how to list its keys and look up by key.
type KeyListerGetter interface {
	KeyLister
	KeyGetter
}

// A KeyLister is anything that knows how to list its keys.
// 返回所有的keys
type KeyLister interface {
	ListKeys() []string
}

// A KeyGetter is anything that knows how to get the value stored under a given key.
// 通过key获取对象
type KeyGetter interface {
	// GetByKey returns the value associated with the key, or sets exists=false.
	GetByKey(key string) (value interface{}, exists bool, err error)
}

// Oldest is a convenience function that returns the oldest delta, or
// nil if there are no deltas.
func (d Deltas) Oldest() *Delta {
	if len(d) > 0 {
		return &d[0]
	}
	return nil
}

// Newest is a convenience function that returns the newest delta, or
// nil if there are no deltas.
// 获取最后一个操作对象
func (d Deltas) Newest() *Delta {
	if n := len(d); n > 0 {
		return &d[n-1]
	}
	return nil
}

// copyDeltas returns a shallow copy of d; that is, it copies the slice but not
// the objects in the slice. This allows Get/List to return an object that we
// know won't be clobbered by a subsequent modifications.
// copyDeltas返回d的浅表副本；也就是说，它复制切片，但不复制切片中的对象。
// 这使Get / List返回一个我们知道不会因后续修改而破坏的对象
func copyDeltas(d Deltas) Deltas {
	d2 := make(Deltas, len(d))
	copy(d2, d)
	return d2
}

// DeletedFinalStateUnknown is placed into a DeltaFIFO in the case where an object
// was deleted but the watch deletion event was missed while disconnected from
// apiserver. In this case we don't know the final "resting" state of the object, so
// there's a chance the included `Obj` is stale.
type DeletedFinalStateUnknown struct {
	Key string
	Obj interface{}
}
