
```go

package workqueue

// RateLimitingInterface is an interface that rate limits items being added to the queue.
type RateLimitingInterface interface {
	// 继承延时队列
	DelayingInterface

	// AddRateLimited adds an item to the workqueue after the rate limiter says it's ok
	// 按照限速方式添加元素的接口
	AddRateLimited(item interface{})

	// Forget indicates that an item is finished being retried.  Doesn't matter whether it's for perm failing
	// or for success, we'll stop the rate limiter from tracking it.  This only clears the `rateLimiter`, you
	// still have to call `Done` on the queue.
	// 丢弃指定元素
	Forget(item interface{})

	// NumRequeues returns back how many times the item was requeued
	// 查询元素放入队列的次数
	NumRequeues(item interface{}) int
}

// NewRateLimitingQueue constructs a new workqueue with rateLimited queuing ability
// Remember to call Forget!  If you don't, you may end up tracking failures forever.
// NewRateLimitingQueue does not emit metrics. For use with a MetricsProvider, please use
// NewNamedRateLimitingQueue instead.
func NewRateLimitingQueue(rateLimiter RateLimiter) RateLimitingInterface {
	return &rateLimitingType{
		DelayingInterface: NewDelayingQueue(),
		rateLimiter:       rateLimiter,
	}
}

func NewNamedRateLimitingQueue(rateLimiter RateLimiter, name string) RateLimitingInterface {
	return &rateLimitingType{
		DelayingInterface: NewNamedDelayingQueue(name),
		rateLimiter:       rateLimiter,
	}
}

// rateLimitingType wraps an Interface and provides rateLimited re-enquing
type rateLimitingType struct {
	//继承延迟队列
	DelayingInterface

	// 限速器
	rateLimiter RateLimiter
}

// AddRateLimited AddAfter's the item based on the time when the rate limiter says it's ok
// 通过限速器获取延迟时间，然后加入到延时队列
func (q *rateLimitingType) AddRateLimited(item interface{}) {
	q.DelayingInterface.AddAfter(item, q.rateLimiter.When(item))
}

func (q *rateLimitingType) NumRequeues(item interface{}) int {
	return q.rateLimiter.NumRequeues(item)
}

func (q *rateLimitingType) Forget(item interface{}) {
	q.rateLimiter.Forget(item)
}

```