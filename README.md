# grpc-api-gw

This is experimental/researching project.

The purpose is to make use of grpc-proxy module and reflection feature to make grpc api-gateway or  you call it proxy with zero configutarion.
All is needed - grpc reflection enabled and the proxy will automatically find methods for every connection.

Article in russian with some explanations can be found at:

https://habr.com/en/post/645433/

## try in action

first, build docker-compose:

```
cd example
docker-compose build
docker-compose up
```

It will start three services: echo, greeter and our proxy.


then connect with evans to proxy

```
evans --host localhost --port 42001 -r
```

now you can select appropriate package, service, call methods.

You can connect to each service separately as well(use ports 50051 and 50052).

## based on:

* https://github.com/mwitkow/grpc-proxy
* https://github.com/jhump/protoreflect