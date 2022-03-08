
```go

package workqueue

import (
	"math"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type RateLimiter interface {
	// When gets an item and gets to decide how long that item should wait
	// 返回元素需要等待多长时间
	When(item interface{}) time.Duration
	// Forget indicates that an item is finished being retried.  Doesn't matter whether it's for failing
	// or for success, we'll stop tracking it
	// 放弃该元素(已经被处理了)
	Forget(item interface{})
	// NumRequeues returns back how many failures the item has had
	// 元素放入队列中的次数
	NumRequeues(item interface{}) int
}

// DefaultControllerRateLimiter is a no-arg constructor for a default rate limiter for a workqueue.  It has
// both overall and per-item rate limiting.  The overall is a token bucket and the per-item is exponential
func DefaultControllerRateLimiter() RateLimiter {
	return NewMaxOfRateLimiter(
		NewItemExponentialFailureRateLimiter(5*time.Millisecond, 1000*time.Second),
		// 10 qps, 100 bucket size.  This is only for retry speed and its only the overall factor (not per item)
		&BucketRateLimiter{Limiter: rate.NewLimiter(rate.Limit(10), 100)},
	)
}

// BucketRateLimiter adapts a standard bucket to the workqueue ratelimiter API
// 固定速率(qps)的限速器
type BucketRateLimiter struct {
	*rate.Limiter
}

var _ RateLimiter = &BucketRateLimiter{}

// 获取延迟，这个延迟会是个相对固定的周期
func (r *BucketRateLimiter) When(item interface{}) time.Duration {
	return r.Limiter.Reserve().Delay()
}

func (r *BucketRateLimiter) NumRequeues(item interface{}) int {
	return 0
}

func (r *BucketRateLimiter) Forget(item interface{}) {
}

// ItemExponentialFailureRateLimiter does a simple baseDelay*2^<num-failures> limit
// dealing with max failures and expiration are up to the caller
// 根据元素错误次数逐渐累加等待时间
type ItemExponentialFailureRateLimiter struct {
	// 互斥锁
	failuresLock sync.Mutex
	// 记录每个元素错误次数，每次调用when累加
	failures     map[interface{}]int

	// 元素延迟基数
	baseDelay time.Duration
	// 元素最大的延迟时间
	maxDelay  time.Duration
}

var _ RateLimiter = &ItemExponentialFailureRateLimiter{}

func NewItemExponentialFailureRateLimiter(baseDelay time.Duration, maxDelay time.Duration) RateLimiter {
	return &ItemExponentialFailureRateLimiter{
		failures:  map[interface{}]int{},
		baseDelay: baseDelay,
		maxDelay:  maxDelay,
	}
}

func DefaultItemBasedRateLimiter() RateLimiter {
	return NewItemExponentialFailureRateLimiter(time.Millisecond, 1000*time.Second)
}

// 限速的when接口
func (r *ItemExponentialFailureRateLimiter) When(item interface{}) time.Duration {
	r.failuresLock.Lock()
	defer r.failuresLock.Unlock()

	// 累加错误计数
	exp := r.failures[item]
	r.failures[item] = r.failures[item] + 1

	// The backoff is capped such that 'calculated' value never overflows.
	// 通过错误次数计算延迟时间，公式是2^i * baseDelay,按指数递增，符合Exponential名字
	backoff := float64(r.baseDelay.Nanoseconds()) * math.Pow(2, float64(exp))
	if backoff > math.MaxInt64 {
		return r.maxDelay
	}
	// 计算后的延迟值和最大延迟值二者取最小值
	calculated := time.Duration(backoff)
	if calculated > r.maxDelay {
		return r.maxDelay
	}
	
	return calculated
}

// 实现计算放入次数的计算
func (r *ItemExponentialFailureRateLimiter) NumRequeues(item interface{}) int {
	r.failuresLock.Lock()
	defer r.failuresLock.Unlock()

	return r.failures[item]
}

func (r *ItemExponentialFailureRateLimiter) Forget(item interface{}) {
	r.failuresLock.Lock()
	defer r.failuresLock.Unlock()

	delete(r.failures, item)
}

// ItemFastSlowRateLimiter does a quick retry for a certain number of attempts, then a slow retry after that
// 尝试次数超过阈值用长延迟，否则用短延迟
type ItemFastSlowRateLimiter struct {
	//互斥锁
	failuresLock sync.Mutex
	// 错误次数计数
	failures     map[interface{}]int

	// 错误次数的阀值
	maxFastAttempts int
	// 短延迟时间
	fastDelay       time.Duration
	// 长延迟时间
	slowDelay       time.Duration
}

var _ RateLimiter = &ItemFastSlowRateLimiter{}

func NewItemFastSlowRateLimiter(fastDelay, slowDelay time.Duration, maxFastAttempts int) RateLimiter {
	return &ItemFastSlowRateLimiter{
		failures:        map[interface{}]int{},
		fastDelay:       fastDelay,
		slowDelay:       slowDelay,
		maxFastAttempts: maxFastAttempts,
	}
}

// 实现when
func (r *ItemFastSlowRateLimiter) When(item interface{}) time.Duration {
	r.failuresLock.Lock()
	defer r.failuresLock.Unlock()
	// 累加错误计数
	r.failures[item] = r.failures[item] + 1
	// 错误次数超过阈值用长延迟，否则用短延迟
	if r.failures[item] <= r.maxFastAttempts {
		return r.fastDelay
	}

	return r.slowDelay
}

func (r *ItemFastSlowRateLimiter) NumRequeues(item interface{}) int {
	r.failuresLock.Lock()
	defer r.failuresLock.Unlock()

	return r.failures[item]
}

func (r *ItemFastSlowRateLimiter) Forget(item interface{}) {
	r.failuresLock.Lock()
	defer r.failuresLock.Unlock()

	delete(r.failures, item)
}

// MaxOfRateLimiter calls every RateLimiter and returns the worst case response
// When used with a token bucket limiter, the burst could be apparently exceeded in cases where particular items
// were separately delayed a longer time.
// 多个限速器，每次返回延时最长的
type MaxOfRateLimiter struct {
	limiters []RateLimiter
}

// 限速器实现When接口
func (r *MaxOfRateLimiter) When(item interface{}) time.Duration {
	ret := time.Duration(0)
	// 这里在获取所有限速里面时间最大的
	for _, limiter := range r.limiters {
		curr := limiter.When(item)
		if curr > ret {
			ret = curr
		}
	}

	return ret
}

func NewMaxOfRateLimiter(limiters ...RateLimiter) RateLimiter {
	return &MaxOfRateLimiter{limiters: limiters}
}

// 限速器实现NumRequeues接口
func (r *MaxOfRateLimiter) NumRequeues(item interface{}) int {
	ret := 0
	// Requeues也是取最大值
	for _, limiter := range r.limiters {
		curr := limiter.NumRequeues(item)
		if curr > ret {
			ret = curr
		}
	}

	return ret
}

// 限速器实现Forget接口
func (r *MaxOfRateLimiter) Forget(item interface{}) {
	// 逐一遍历forget
	for _, limiter := range r.limiters {
		limiter.Forget(item)
	}
}

// WithMaxWaitRateLimiter have maxDelay which avoids waiting too long
type WithMaxWaitRateLimiter struct {
	limiter  RateLimiter
	maxDelay time.Duration
}

func NewWithMaxWaitRateLimiter(limiter RateLimiter, maxDelay time.Duration) RateLimiter {
	return &WithMaxWaitRateLimiter{limiter: limiter, maxDelay: maxDelay}
}

func (w WithMaxWaitRateLimiter) When(item interface{}) time.Duration {
	delay := w.limiter.When(item)
	if delay > w.maxDelay {
		return w.maxDelay
	}

	return delay
}

func (w WithMaxWaitRateLimiter) Forget(item interface{}) {
	w.limiter.Forget(item)
}

func (w WithMaxWaitRateLimiter) NumRequeues(item interface{}) int {
	return w.limiter.NumRequeues(item)
}

```