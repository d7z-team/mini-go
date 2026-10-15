# RPC 使用指南

MRPC 从 `.mrpc` 契约生成 Go、Mini-Go、Rust 和 TypeScript 的类型、客户端与 provider 适配代码。
同一契约既可以在进程内绑定，也可以通过 WebSocket Endpoint 和 Gateway 跨进程连接。

| 任务 | 入口 |
| --- | --- |
| Mini-Go 调用 Go | [从脚本调用 Go](#从脚本调用-go) |
| Go 客户端或 Mini-Go 服务端 | [Go 客户端与 Mini-Go 服务端](#go-客户端与-mini-go-服务端) |
| Browser / Node.js | [TypeScript / JavaScript API](#typescript--javascript-api) |
| Rust | [Rust API](#rust-api) |
| 远程连接和动态路由 | [远程连接与路由](#远程连接与路由) |

Go 侧导入 `github.com/d7z-team/mini-go/rpc`，Mini-Go 侧导入标准包 `"rpc"`。Engine、Instance
和能力装配见 [使用指南](USAGE.md)；协议、生成器和验证流程见 [开发指南](DEVELOPMENT.md#rpc-实现维护)。

## 从脚本调用 Go

下面的流程生成一个问候服务，由 Mini-Go 脚本调用 Go handler。

### 声明与生成

创建 `api/greeter.mrpc`：

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

生成需要的语言输出；声明变化后重新生成所有受影响的 binding：

```bash
mini-go rpc generate \
  -mgo-out api/greeter_mrpc_gen.mgo -mgo-package greeter \
  -go-out api/greeter_mrpc_gen.go \
  api/greeter.mrpc
```

`namespace` 标识契约；各语言的 package/module 选项决定生成代码的位置和导入方式。绑定时会
校验契约 identity，契约不一致会失败。

### 编写脚本

```go
package greeter

import "rpc"

func Greeting() string {
    client := NewGreeterClient(rpc.ClientOptions{TimeoutNanoseconds: 5_000_000_000})
    defer client.Close()
    response, err := client.Hello(Request{Name: "Mini-Go"})
    if err != nil { panic(err) }
    return response.Message
}
```

### 装配宿主

按 [Go 嵌入示例](USAGE.md#完整嵌入示例)编译脚本，将 `Greeting` 注册为 `greeting` 命名入口。
宿主注册生成的 provider，并把 RPC Host 注入 Instance：

```go
provider, err := greeter.NewGreeterProvider(greeterHandler{})
if err != nil { return err }
host, err := rpc.NewHost(rpc.HostOptions{Providers: []rpc.Provider{provider}})
if err != nil { return err }
defer host.Close()

instance, err := program.Instantiate(ctx, runtime.InstanceOptions{FFI: host})
if err != nil { return err }
defer instance.Close()
result, err := instance.Call(ctx, "greeting")
if err != nil { return err }
message, _ := result.Values[0].StringValue()
fmt.Println(message)
```

handler 应检查调用 context：

```go
type greeterHandler struct{}

func (greeterHandler) Hello(ctx context.Context, request greeter.Request) (greeter.Response, error) {
    if err := ctx.Err(); err != nil { return greeter.Response{}, err }
    return greeter.Response{Message: "Hello, " + request.Name + "!"}, nil
}
```

宿主 module path 必须与 schema 和生成文件一致。console、文件系统等标准库能力按
[系统能力](USAGE.md#系统能力)装配。

## 接口与数据类型

契约可以声明 message、enum、service 和 resource。值支持固定宽度整数、浮点、复数、bool、string、
slice、map、optional 和 bytes；方法自动携带错误结果。

| 类型 | 规则 |
| --- | --- |
| message / enum | 类型属于声明它的 schema；enum 接受 int32 范围内的未知值 |
| slice / map / optional | 保留 nil、空集合和 optional 缺省的区别 |
| resource | 属于创建它的 binding；复制句柄共享关闭状态 |
| 浮点与复数 | 保留宽度、负零和可表示的特殊值 |

schema 使用 `int8/16/32/64`、`uint8/16/32/64` 等固定宽度类型，不使用平台相关的 `int`、`uint`
或 `uintptr`。共享声明使用 `import model "example/model.mrpc";`，每种目标语言都会生成相应代码。

## Go 客户端与 Mini-Go 服务端

### 在 Go 中调用

本地 provider 可以直接绑定，不需要 VM 或网络：

```go
binder, err := rpc.NewLocalBinder(rpc.LocalBinderOptions{}, provider)
if err != nil { return err }
client, err := greeter.BindGreeterClient(ctx, binder, rpc.BindOptions{})
if err != nil { return err }
defer client.Close()
response, err := client.Hello(ctx, greeter.Request{Name: "Go"})
```

每次 Go 调用都用 `context.Context` 控制取消和期限。需要按 labels、affinity 或权重选择多个
实例时使用 Router。

### 在 Mini-Go 中提供服务

生成代码包含 handler 接口和 `ServeGreeter`：

```go
type greeterHandler struct{}

func (greeterHandler) Hello(ctx *rpc.Context, request Request) (Response, error) {
    if err := ctx.Err(); err != nil { return Response{}, err }
    return Response{Message: "Hello, " + request.Name + "!"}, nil
}

func Serve() error {
    return ServeGreeter(greeterHandler{}, rpc.ServerOptions{Name: "worker-1"})
}
```

宿主通过 `rpc.HostOptions.PublishProvider` 配置发布方式。将 `Serve` 注册为命名入口并用
`Instance.Start` 保持服务运行；取消入口或关闭 Instance 会结束服务。

## 错误、超时与资源

Go handler 返回 `rpc.StatusError`，Mini-Go handler 使用 `rpc.NewError`；调用方使用 `rpc.CodeOf`
判断状态。常见状态如下：

| 状态 | 含义 |
| --- | --- |
| `invalid_argument`、`not_found` | 输入或对象错误 |
| `canceled`、`deadline_exceeded` | 调用方停止等待 |
| `unimplemented` | 没有匹配的服务或方法 |
| `failed_precondition` | 契约不一致 |
| `unavailable` | provider、路由、连接或关闭状态不可用 |
| `resource_exhausted` | 并发、消息或资源额度不足 |
| `protocol` | 两端 wire 契约不匹配 |

Mini-Go 的 `ClientOptions.TimeoutNanoseconds` 约束绑定和调用，Go 使用 context，TypeScript 使用
`timeoutMs` 或 `AbortSignal`。超时或断线不表示服务端一定没有执行；框架不会自动重试有副作用的调用。

resource 客户端应显式关闭。资源属于创建它的 binding 和服务实例；路由改变不会迁移已有资源，
连接终止后应重新绑定并创建资源。关闭开始后不再接收新调用，并等待已经进入 handler 的调用退出。
直接使用低层 `RouteSet.Call` 时，调用方必须对返回结果执行一次 `Accept` 或 `Discard`。

较大的 bytes、字符串和复合值由 Endpoint 分片，连接、VM boundary、Host 和 Gateway 都有容量限制。
调大网络消息上限不会自动调大 VM 限制；持续流应使用 resource 的分块 `Read`/`Write` 方法。

## 远程连接与路由

`rpc/router.Router` 按 labels、affinity 和权重选择 provider；绑定完成后固定所选实例，重新选择需要
建立新客户端。Endpoint 支持 `ws://`、`wss://` 和 `ws+unix:///绝对路径`。

启动 Gateway：

```bash
mini-go gateway -listen ws://127.0.0.1:8080/rpc
```

脚本应用连接已发布的服务：

```bash
mini-go -C ./consumer run -rpc ws://127.0.0.1:8080/rpc .
```

library 使用 `gateway.Dial` 创建 Endpoint，并把它作为 Go 客户端的 Binder 或 Mini-Go Host 的
Fallback。Endpoint 由创建者关闭；`wss://` 监听还需要证书和私钥。

## TypeScript / JavaScript API

`@d7z-team/mini-go/rpc` 在独立 Worker 中连接 WebSocket。生成 TypeScript ESM：

```bash
mini-go rpc generate -ts-out src/greeter.ts api/greeter.mrpc
```

```ts
import { connectRPC } from "@d7z-team/mini-go/rpc";
import { GreeterClient } from "./greeter";

const connection = await connectRPC("wss://example.test/rpc");
try {
  const client = await GreeterClient.bind(connection, { labels: { zone: "local" } });
  try {
    console.log(await client.hello({ name: "TypeScript" }, { timeoutMs: 5_000 }));
  } finally { await client.close(); }
} finally { await connection.close(); }
```

生成的 `createGreeterProvider` 可通过 `connection.publish` 发布 TypeScript handler。64 位整数映射
为 `bigint`，bytes 为 `Uint8Array | null`，optional 为 `undefined`，map 为 `Map`。
部署方式见 [TypeScript README](playground/runtime-typescript/README.md#浏览器部署)。

## Rust API

启用 `rpc` 使用本地绑定、Router、Endpoint 和 FFI Host；远程传输增加 `rpc-gateway`。生成 Rust 模块：

```bash
mini-go rpc generate \
  -rust-out src/greeter.rs -rust-module crate::greeter api/greeter.mrpc
```

| 操作 | API |
| --- | --- |
| 实现服务 | generated handler trait，返回 `rpc::BoxFuture` |
| 创建 provider | `greeter_provider(Arc<dyn GreeterHandler>)` |
| 本地绑定 | `LocalBinder::new(runtime_handle, limits, providers)` |
| 绑定客户端 | `GreeterClient::bind(context, &binder, options).await` |
| 接入 VM | `HostOptions::new(tokio_handle)` |
| 远程连接 | `gateway::dial(handle, address, options)` |

应用应在关闭 Tokio runtime 前等待 Host、Endpoint 和 Router 完成 shutdown。VM 接入见
[Rust 使用指南](playground/runtime-rust/USAGE.md)。

## 服务替换与关闭

代码热更新不会自动替换 provider 或迁移 resource。更新服务时准备新 provider，通过 Router 原子替换
publication；新 binding 使用新实现，旧 binding 和 resource 继续由原 owner 清理。

`Drain` 停止新 binding，`ForceClose` / `ForceShutdown` 撤销现有 binding。代码提交、路由替换和旧
服务清理是独立事件。通常按以下顺序关闭：停止新请求，关闭 Instance，再关闭 Host、客户端、连接
和 Router，最后释放业务 backend。等待 context 取消只停止本次等待，不撤销已经开始的清理。

RPC 的跨语言 wire、生成器维护和验证矩阵见 [开发指南](DEVELOPMENT.md#rpc-实现维护)；组件边界和
状态所有权见 [架构说明](ARCHITECTURE.md)。
