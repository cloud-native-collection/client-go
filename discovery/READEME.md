`discovery_client`，主要用于发现`Kubenetes API Server`所支持的资源组、资源版本、资源信息。
`kubectl`的`api-versions`和`api-resources`命令输出也是通过`DiscoveryClient`实现的。
其同样是在`RESTClient`的基础上进行的封装。`DiscoveryClient`还可以将资源组、资源版本、
资源信息等存储在本地，用于本地缓存，减轻对`kubernetes api sever`的访问压力，
缓存信息默认存储在：`~/.kube/cache`和`~/.kube/http-cache`下