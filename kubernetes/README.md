`ClientSet`在`RESTClient`的基础上封装`Resource`和`Version`的管理方法,
每个`resource`当作一个客户端,`ClientSet`是多个客户端的的集合,每个`Resourcehe Version`均以函数的方式暴露