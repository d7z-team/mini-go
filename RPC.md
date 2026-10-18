# RPC 使用指南

MRPC 用 `.mrpc` 契约生成 Go、Mini-Go、Rust 和 TypeScript 的类型、客户端和 provider 适配代码。
同一契约支持进程内绑定，也支持通过 WebSocket Endpoint 和 Gateway 跨进程连接。

Go 侧导入 `github.com/d7z-team/mini-go/rpc`，Mini-Go 侧导入标准包 `"rpc"`。Engine、Instance
和宿主能力装配见[使用指南](USAGE.md)；协议、生成器和维护流程见[开发指南](DEVELOPMENT.md#rpc-实现维护)。

## 契约与生成

```text
syntax = "mrpc/v2";
namespace example.greeter.v1;

option mgo_package = "example.com/app/api";
option go_package = "example.com/app/api;greeter";
option ts_module = "./greeter";

message Request { Name string = 1; }
message Response { Message string = 1; }

service Greeter {
    Hello(request Request = 1) returns (response Response = 1);
}
```

生成一个或多个目标；契约变化后重新生成所有受影响的 binding：

```bash
mini-go rpc generate \
  -mgo-out api/greeter_mrpc_gen.mgo -mgo-package greeter \
  -go-out api/greeter_mrpc_gen.go \
  api/greeter.mrpc
```

可选输出包括 `-rust-out`、`-rust-module`、`-ts-out`、`-ts-runtime`，以及各语言的 package、
module 和 prefix 选项。`namespace` 标识契约，package/module 选项决定生成文件的归属；binding
会校验契约 identity，不一致时拒绝绑定。

契约支持 message、enum、service 和 resource，值支持固定宽度整数、浮点、复数、bool、string、
slice、map、optional 和 bytes。固定宽度类型使用 `int8/16/32/64`、`uint8/16/32/64` 等名称，
不使用平台相关的 `int`、`uint` 或 `uintptr`。enum 保留 int32 范围内的未知值；nil、空集合和
optional 缺省保持可区分。

## Mini-Go 调用 Go

生成客户端后，在脚本中导入 `rpc` 并调用服务：

```go
package greeter

import "rpc"

func Greeting() string {
	client := NewGreeterClient(rpc.ClientOptions{TimeoutNanoseconds: 5_000_000_000})
	defer client.Close()
	response, err := client.Hello(Request{Name: "Mini-Go"})
	if err != nil {
		panic(err)
	}
	return response.Message
}
```

按 [Go 嵌入示例](USAGE.md#完整嵌入示例)编译脚本，将 `Greeting` 注册为 `greeting` 命名入口。
宿主注册生成的 provider，并把 RPC Host 注入 Instance：

```go
provider, err := greeter.NewGreeterProvider(greeterHandler{})
if err != nil {
	return err
}
host, err := rpc.NewHost(rpc.HostOptions{Providers: []rpc.Provider{provider}})
if err != nil {
	return err
}
defer host.Close()

instance, err := program.Instantiate(ctx, runtime.InstanceOptions{FFI: host})
if err != nil {
	return err
}
defer instance.Close()
result, err := instance.Call(ctx, "greeting")
if err != nil {
	return err
}
message, _ := result.Values[0].StringValue()
fmt.Println(message)
```

生成的 handler 应检查调用 context：

```go
type greeterHandler struct{}

func (greeterHandler) Hello(ctx context.Context, request greeter.Request) (greeter.Response, error) {
	if err := ctx.Err(); err != nil {
		return greeter.Response{}, err
	}
	return greeter.Response{Message: "Hello, " + request.Name + "!"}, nil
}
```

## Go 客户端与 Mini-Go 服务

本地 provider 不需要 VM 或网络：

```go
binder, err := rpc.NewLocalBinder(rpc.LocalBinderOptions{}, provider)
if err != nil {
	return err
}
client, err := greeter.BindGreeterClient(ctx, binder, rpc.BindOptions{})
if err != nil {
	return err
}
defer client.Close()
response, err := client.Hello(ctx, greeter.Request{Name: "Go"})
```

Go 调用使用 `context.Context` 控制取消和期限。需要按 labels、affinity 或权重选择多个实例时，
使用 Router。

生成的服务端代码提供 handler 接口和 `ServeGreeter`：

```go
type greeterHandler struct{}

func (greeterHandler) Hello(ctx *rpc.Context, request Request) (Response, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	return Response{Message: "Hello, " + request.Name + "!"}, nil
}

func Serve() error {
	return ServeGreeter(greeterHandler{}, rpc.ServerOptions{Name: "worker-1"})
}
```

宿主通过 `rpc.HostOptions.PublishProvider` 配置发布方式，将 `Serve` 注册为命名入口并用
`Instance.Start` 保持服务运行；取消入口或关闭 Instance 会结束服务。

## 错误、期限与 resource

Go handler 返回 `rpc.StatusError`，Mini-Go handler 使用 `rpc.NewError`；调用方用 `rpc.CodeOf`
判断状态。常见状态包括 `invalid_argument`、`not_found`、`canceled`、`deadline_exceeded`、
`unimplemented`、`failed_precondition`、`unavailable`、`resource_exhausted` 和 `protocol`。

Mini-Go 的 `ClientOptions.TimeoutNanoseconds`、Go 的 `context`、TypeScript 的 `timeoutMs` 或
`AbortSignal` 约束调用方发起的操作，并可向服务端传播取消或期限。超时或断线不保证已经开始的业务
逻辑被撤销；框架不会自动重试有副作用的调用。

resource 客户端应显式关闭。资源属于创建它的 binding 和服务实例，路由变化不会迁移已有 resource；
连接终止后应重新绑定并创建资源。直接使用低层 `RouteSet.Call` 时，调用方必须对返回结果执行一次
`Accept` 或 `Discard`。关闭从停止新调用开始，并等待已进入 handler 的调用退出。

大 bytes、字符串和复合值会受到 Endpoint、VM boundary、Host 和 Gateway 的容量限制；调大网络
消息上限不会自动调大 VM 限制。持续传输使用 resource 的分块 `Read`/`Write` 方法。

## 远程连接与路由

`rpc/router.Router` 按 labels、affinity 和权重选择 provider。绑定完成后固定所选实例，重新选择
需要建立新客户端。Endpoint 支持 `ws://`、`wss://` 和 `ws+unix:///绝对路径`。

启动 Gateway：

```bash
mini-go gateway -listen ws://127.0.0.1:8080/rpc
```

脚本应用连接已发布的服务：

```bash
mini-go -C ./consumer run -rpc ws://127.0.0.1:8080/rpc .
```

Go 应用可用 `gateway.Dial` 创建 Endpoint，并将其作为客户端 Binder 或 Mini-Go Host 的 fallback。
Endpoint 由创建方关闭；`wss://` 监听还需要 `-tls-cert` 和 `-tls-key`。

## TypeScript / JavaScript

生成 TypeScript ESM：

```bash
mini-go rpc generate -ts-out src/greeter.ts api/greeter.mrpc
```

```ts
import { connectRPC } from "@d7z-team/mini-go/rpc";
import { GreeterClient } from "./greeter";

const connection = await connectRPC("wss://example.test/rpc");
try {
  const client = await GreeterClient.bind(connection, {
    labels: { zone: "local" },
  });
  try {
    console.log(
      await client.hello({ name: "TypeScript" }, { timeoutMs: 5_000 }),
    );
  } finally {
    await client.close();
  }
} finally {
  await connection.close();
}
```

生成的 `createGreeterProvider` 可通过 `connection.publish` 发布 handler。64 位整数映射为 `bigint`，
bytes 为 `Uint8Array | null`，optional 为 `undefined`，map 为 `Map`。浏览器部署见
[TypeScript README](playground/runtime-typescript/README.md#浏览器部署)。

## Rust

启用 `rpc` 使用本地绑定、Router、Endpoint 和 FFI Host；远程传输增加 `rpc-gateway`：

```bash
mini-go rpc generate \
  -rust-out src/greeter.rs -rust-module crate::greeter api/greeter.mrpc
```

生成的 handler trait、provider、`LocalBinder`、客户端 `bind` 和 `gateway::dial` 构成 Rust 侧主要
入口。应用应在关闭 Tokio runtime 前等待 Host、Endpoint 和 Router 完成 shutdown，详细 API 见
[Rust 使用指南](playground/runtime-rust/USAGE.md)。

## 服务替换与关闭

代码热更新不会自动替换 provider 或迁移 resource。更新服务时准备新 provider，通过 Router 原子
替换 publication；新 binding 使用新实现，旧 binding 和 resource 继续由原 owner 清理。

关闭按以下顺序处理：停止新请求，关闭 Instance，再关闭 Host、客户端、连接和 Router，最后释放
业务 backend。等待 context 取消只停止本次等待，不撤销已经开始的清理。

RPC wire、生成器维护和验证矩阵见[开发指南](DEVELOPMENT.md#rpc-实现维护)；包边界和状态所有权见
[架构说明](ARCHITECTURE.md)。
