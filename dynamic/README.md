`DynamicClient`客户端是一种**动态客户端**，可以对任意的`Kubernetes`资源进行`RESTful`操作，包括`CRD`资源。
`DynamicClient`内部实现了`Unstructured`，用于处理非结构化数据结构（即无法提前预知的数据结构），
这也是`DynamicClient`能够处理`CRD`资源的关键。

> `DynamicClient`不是类型安全的，因此在访问`CRD`自定义资源是要注意，例如，在操作不当时可能会导致程序崩溃。
> `DynamicClient`的处理过程是将`Resource`(如`PodList`)转换成`Unstructured`结构类型，
> Kubernetes的所有Resource都可以转换为该结构类型。处理完后再将Unstructured转换成PodList。
> 过程类似于Go语言的interface{}断言转换过程。另外，Unstructured结构类型是通过map[string]interface{}转换的。