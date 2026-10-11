# RPC 使用指南

MRPC 从一份 `.mrpc` 声明生成 Go、Mini-Go、Rust 和 TypeScript 的类型、客户端与 provider 适配代码。
同一契约可用于进程内绑定，也可通过 Endpoint 和 Gateway 跨进程连接。

| 场景 | 入口 |
| --- | --- |
| Mini-Go 调用 Go | [生成 provider 并注入 Host](#从脚本调用-go) |
| Go 客户端或 Mini-Go 服务端 | [本地绑定与发布](#go-客户端与-mini-go-服务端) |
| Browser / Node.js | [TypeScript / JavaScript API](#typescript--javascript-api) |
| Rust | [Rust API](#rust-api) |
| 动态路由或远程调用 | [Router 与 Gateway](#远程连接与路由) |

Go 侧导入 `github.com/d7z-team/mini-go/rpc`，Mini-Go 侧导入标准包 `"rpc"`。基础 Engine 与
Instance 用法见[使用指南](USAGE.md)，协议维护见[开发指南](DEVELOPMENT.md#rpc-实现维护)。

## 从脚本调用 Go

下面的示例定义问候服务，由 Mini-Go 调用 Go handler。

### 1. 声明接口

创建 `api/greeter.mrpc`：

```text
syntax = "mrpc/v2";
namespace example.greeter.v1;

option mgo_package = "example.com/app/api";
option go_package = "example.com/app/api;greeter";
option ts_module = "./greeter.js";

message Request { Name string = 1; }
message Response { Message string = 1; }

service Greeter {
    Hello(request Request = 1) returns (response Response = 1);
}
```

`namespace` 标识契约；各语言 package/module 选项只决定生成源码的位置和导入方式。

### 2. 生成代码

```bash
mini-go rpc generate \
  -mgo-out api/greeter_mrpc_gen.mgo -mgo-package greeter \
  -go-out api/greeter_mrpc_gen.go \
  api/greeter.mrpc
```

声明变化后重新生成并分发各端 binding。契约 identity 不匹配会在绑定时失败。

### 3. 编写脚本

创建同属 `greeter` 包的 `api/call.mgo`：

```go
package greeter

import "rpc"

func Greeting() string {
    client := NewGreeterClient(rpc.ClientOptions{
        TimeoutNanoseconds: 5_000_000_000,
    })
    defer client.Close()

    response, err := client.Hello(Request{Name: "Mini-Go"})
    if err != nil {
        panic(err)
    }
    return response.Message
}
```

### 4. 装配宿主

宿主发现脚本源码，注册生成的 provider，并把 RPC Host 注入 Instance：

```go
package main

import (
    "context"
    "fmt"
    "log"
    "os"

    greeter "example.com/app/api"
    minigo "github.com/d7z-team/mini-go"
    "github.com/d7z-team/mini-go/compiler/workspace"
    "github.com/d7z-team/mini-go/rpc"
    "github.com/d7z-team/mini-go/runtime"
)

type greeterHandler struct{}

func (greeterHandler) Hello(ctx context.Context, request greeter.Request) (greeter.Response, error) {
    if err := ctx.Err(); err != nil {
        return greeter.Response{}, err
    }
    return greeter.Response{Message: "Hello, " + request.Name + "!"}, nil
}

func main() {
    if err := run(); err != nil {
        log.Fatal(err)
    }
}

func run() error {
    sources, err := workspace.DiscoverIndexedSourceTree(os.DirFS("api"), ".", "example.com/app/api")
    if err != nil {
        return err
    }
    engine, err := minigo.New(minigo.Config{Sources: sources})
    if err != nil {
        return err
    }
    defer engine.Close()

    program, checked, err := engine.Compile("example.com/app/api",
        minigo.EntryPoint{Name: "greeting", Function: "Greeting"})
    if err != nil || !checked.OK() {
        return fmt.Errorf("compile: %v; diagnostics: %v", err, checked.Diagnostics)
    }

    provider, err := greeter.NewGreeterProvider(greeterHandler{})
    if err != nil {
        return err
    }
    host, err := rpc.NewHost(rpc.HostOptions{Providers: []rpc.Provider{provider}})
    if err != nil {
        return err
    }
    defer host.Close()

    ctx := context.Background()
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
    return nil
}
```

宿主 module path 需与 schema 和生成文件一致。console、文件系统等标准库能力按
[系统能力](USAGE.md#系统能力)另行装配。

## 接口与数据类型

schema 可声明 message、enum、service 和 resource。值支持固定宽度整数、浮点、复数、bool、string、
slice、map、optional 和 bytes；方法自动携带错误结果。

| 数据 | 约定 |
| --- | --- |
| message / enum | 类型属于声明它的 schema；enum 允许 int32 范围内的未知值 |
| slice / map / optional | 保留 nil、空集合和 optional 缺省的区别 |
| resource | 属于创建它的 binding；复制句柄共享关闭状态 |
| 浮点与复数 | 保留宽度、负零和可表示的特殊值 |

schema 使用 `int8/16/32/64`、`uint8/16/32/64` 等固定宽度类型，不使用平台相关的 `int`、`uint` 或
`uintptr`。共享声明通过 `import model "example/model.mrpc";` 引用，并为每种目标语言生成对应代码。

## Go 客户端与 Mini-Go 服务端

### 在 Go 中调用

本地 provider 可直接绑定，无需 VM 或网络：

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

Go 客户端通过每次调用的 `context.Context` 控制取消和期限。多实例选择使用 Router。

### 在 Mini-Go 中提供服务

生成代码包含 handler 接口和 `ServeGreeter`：

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

宿主通过 `rpc.HostOptions.PublishProvider` 决定发布方式。将 `Serve` 注册为命名入口并用
`Instance.Start` 保持运行；取消入口或关闭 Instance 会结束服务。

## 错误与超时

Go handler 返回 `rpc.StatusError`，Mini-Go handler 使用 `rpc.NewError`；调用方通过 `rpc.CodeOf`
判断状态。常用状态含义如下：

| 状态 | 含义 |
| --- | --- |
| `invalid_argument`、`not_found` | 业务输入或对象错误 |
| `canceled`、`deadline_exceeded` | 调用方已停止等待，handler 应响应 context |
| `unimplemented` | 没有匹配的服务或方法 |
| `failed_precondition` | 契约不一致 |
| `unavailable` | provider、路由、连接或关闭状态不可用 |
| `resource_exhausted` | 并发、消息或资源额度不足 |
| `protocol` | 两端 wire 契约不匹配 |

Mini-Go 的 `ClientOptions.TimeoutNanoseconds` 约束绑定和调用；Go 使用 context；TypeScript 使用
`timeoutMs` 或 `AbortSignal`。网络 Endpoint 另外配置租期、接纳期限和服务端最长调用时间。
超时或断线不表示服务端一定没有执行，框架不会自动重试有副作用的调用。

## 资源与大对象

文件、游标和会话适合声明为 resource。生成的资源客户端应显式关闭；资源固定属于创建它的 binding
和服务实例，路由改变不会迁移已有资源。连接终止后需重新绑定并创建资源。

关闭开始后资源不能再接收新调用。关闭会等待已进入 handler 的调用退出；未交付结果由生成客户端确认
或丢弃。直接使用低层 `RouteSet.Call` 时，调用方必须对结果执行一次 `Accept` 或 `Discard`。

较大的 bytes、字符串和复合值由 Endpoint 自动分片。连接、VM boundary、Host 和 Gateway 各自拥有容量
限制；调大网络消息上限不会自动调大 VM 限制。持续流使用 resource 的分块 `Read`/`Write` 方法。

## 远程连接与路由

`rpc/router.Router` 按 labels、affinity 和权重选择 provider；绑定完成后保持所选实例，重新选择需要
建立新客户端。Endpoint 支持 `ws://`、`wss://` 和 `ws+unix:///绝对路径`。

启动 Gateway：

```bash
mini-go gateway -listen ws://127.0.0.1:8080/rpc
```

脚本应用连接已发布的服务：

```bash
mini-go -C ./consumer run -rpc ws://127.0.0.1:8080/rpc .
```

library 使用 `gateway.Dial` 得到 Endpoint，并将它作为 Go 客户端的 Binder 或 Mini-Go Host 的
Fallback。Endpoint 由创建者关闭；`wss://` 监听还需配置证书和私钥。

## TypeScript / JavaScript API

`@d7z-team/mini-go/rpc` 通过独立 Worker 连接 WebSocket，无需 Mini-Go 镜像。生成 TypeScript ESM：

```bash
mini-go rpc generate -ts-out src/greeter.ts api/greeter.mrpc
```

`-ts-runtime` 覆盖生成代码导入的 RPC 模块；被导入 schema 的 `ts_module` 应使用最终 JavaScript ESM 路径。

```ts
import { connectRPC } from "@d7z-team/mini-go/rpc";
import { GreeterClient } from "./greeter.js";

const connection = await connectRPC("wss://example.test/rpc");
try {
  const client = await GreeterClient.bind(connection, {
    labels: { zone: "local" },
    affinityKey: "account-42",
  });
  try {
    console.log(await client.hello({ name: "TypeScript" }, { timeoutMs: 5_000 }));
  } finally {
    await client.close();
  }
} finally {
  await connection.close();
}
```

生成的 `createGreeterProvider` 可通过 `connection.publish` 发布 TypeScript handler。
64 位整数映射为 `bigint`，bytes 为 `Uint8Array | null`，optional 为 `undefined`，map 为 `Map`。
资源属于原 binding 并应显式关闭。`close()` 等待清理；`terminate()` 只用于宿主无法继续等待的场景。
部署与 Worker URL 配置见 [TypeScript README](playground/runtime-typescript/README.md#独立-rpc)。

## Rust API

启用 `rpc` 使用本地绑定、Router、Endpoint 和 FFI Host；远程传输增加 `rpc-gateway`。生成 Rust 模块：

```bash
mini-go rpc generate \
  -rust-out src/greeter.rs \
  -rust-module crate::greeter \
  api/greeter.mrpc
```

| 操作 | API |
| --- | --- |
| 实现服务 | generated handler trait 返回 `rpc::BoxFuture` |
| 创建 provider | `greeter_provider(Arc<dyn GreeterHandler>)` |
| 本地装配 | `LocalBinder::new(runtime_handle, limits, providers)` |
| 绑定客户端 | `GreeterClient::bind(context, &binder, options).await` |
| 接入 VM | `HostOptions::new(tokio_handle)` 配置 provider |
| 远程连接 | `gateway::dial(handle, address, options)` |

应用提供 Tokio runtime，并在关闭它之前等待 Host、Endpoint 和 Router 完成 shutdown。VM 与 Tokio
接入见 [Rust 使用指南](playground/runtime-rust/USAGE.md)，完整 RPC 例见
[gateway 测试](playground/runtime-rust/tests/rpc_gateway.rs)。

## 服务替换与关闭

代码热更新不会自动替换 provider 或迁移 resource。更新服务时先准备新 provider，通过 Router
原子替换 publication，再让新 binding 选择新实现；旧 binding 和 resource 继续由原 owner 清理。

`Drain` 停止新 binding，`ForceClose` / `ForceShutdown` 撤销现有 binding。代码提交、路由替换和旧服务
清理是三个独立事件。通常按以下顺序关闭：停止新请求，关闭 Instance，再关闭 Host、客户端、连接和
Router，最后释放业务 backend。等待 context 取消只停止本次等待，不撤销已经开始的清理。
