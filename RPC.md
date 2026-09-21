# RPC 使用指南

MRPC 从一份 `.mrpc` 声明生成 Go、Mini-Go、Rust 和 TypeScript 的类型、客户端与服务端适配代码。
同一契约可用于进程内绑定或通过 Endpoint、Gateway 跨进程连接。

| 使用场景 | 接入方式 |
| --- | --- |
| Mini-Go 调用 Go 函数 | 生成 Provider，交给 `rpc.NewHost`，通过 `InstanceOptions.FFI` 注入 |
| Go 调用 Go 服务 | 生成客户端，通过 `rpc.NewLocalBinder` 绑定 Provider |
| Go 或另一台 VM 调用 Mini-Go 服务 | 脚本运行生成的 `Serve<Service>`，宿主配置 `HostOptions.PublishProvider` |
| Browser / Node.js 直接调用或提供服务 | 生成 TypeScript binding，通过 `@d7z-team/mini-go/rpc` 建立独立连接 |
| 动态选择多个服务实例 | 使用 `rpc/router.Router`，客户端可传入 labels 或 affinity key |
| 跨进程调用 | 使用 `rpc/gateway` 的 WebSocket 连接，或 CLI 的 `gateway` 与 `run -rpc` |

Go 侧导入 `github.com/d7z-team/mini-go/rpc`，Mini-Go 侧导入标准包 `"rpc"`。基础 Engine 与 Instance 用法见
[使用指南](./USAGE.md)。仓库内部协议、生成器和测试维护见[开发指南](./DEVELOPMENT.md#rpc-实现维护)。

按任务查阅：[本地接入](#从脚本调用-go) · [接口类型](#接口与数据类型) ·
[错误与超时](#错误与超时) · [资源](#资源与大对象) · [远程连接](#远程连接与路由) ·
[TypeScript](#typescript--javascript-api) · [Rust](#rust-api)。

## 从脚本调用 Go

Rust 宿主的装配与生成见本文末尾的 [Rust API](#rust-api)。

下面的示例实现一个 Go 问候服务，由 Mini-Go 调用，最后在宿主输出 `Hello, Mini-Go!`。
宿主 Go 项目的 module path 使用 `example.com/app`，并添加依赖：

```bash
go mod init example.com/app
go get github.com/d7z-team/mini-go
```

已有宿主项目可使用自己的 module path，并相应修改后面的 Go import 和 schema package 配置。
本例由宿主直接装载脚本源码。

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

`namespace` 标识接口所属的命名空间；`mgo_package` 和 `go_package` 指定生成代码的目标包。
`go_package` 分号后的部分是 Go package 名。字段后的数字是字段标识，应在声明范围内保持唯一。

### 2. 生成代码

安装命令的方式见[中文 README](./README_zh.md#安装)。在宿主项目目录执行：

```bash
mini-go rpc generate \
  -mgo-out api/greeter_mrpc_gen.mgo -mgo-package greeter \
  -go-out api/greeter_mrpc_gen.go \
  -ts-out api/greeter.ts \
  api/greeter.mrpc
```

生成结果包含消息、handler、客户端和 Provider。只需要部分语言时可省略其他输出参数；接口修改后应重新
生成并分发各端 binding，契约不匹配会在绑定时失败。跨 schema 导入和各语言输出选项见下文对应章节。

### 3. 编写脚本

创建 `api/call.mgo`，与生成的 Mini-Go 文件同属 `greeter` 包：

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

调用对当前 Mini-Go goroutine 表现为阻塞，其他 goroutine 仍可运行。本例将失败作为脚本 panic 报告；
业务代码也可检查 `err`，或将其作为函数返回值交给调用方。

### 4. 装配宿主并调用

创建 `main.go`：

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
    if err != nil {
        return err
    }
    if !checked.OK() {
        return fmt.Errorf("compile: %v", checked.Diagnostics)
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

运行 `go run .`。此例只注入问候服务，输出由 Go 的 `fmt.Println` 完成。
脚本需要 console、文件系统等标准库宿主能力时，按[系统能力](./USAGE.md#系统能力)装配对应 provider。

## 接口与数据类型

schema 可声明 message、enum、service 和 resource。值类型支持固定宽度整数、浮点、复数、bool、string、
slice、map 及 optional；二进制数据使用 `[]uint8`。

整数使用 `int8/16/32/64` 或 `uint8/16/32/64`，浮点使用 `float32/64`，复数使用 `complex64/128`。
`int`、`uint`、`uintptr`、`byte` 和 `rune` 不是 schema 类型。每个生成方法都带错误结果，无需在 schema 中声明 `error`。

| 数据 | 使用约定 |
| --- | --- |
| message / enum | 导入类型属于其声明的 schema；enum 允许未知但在 int32 范围内的值 |
| slice / map / optional | 保留 nil、空集合与 optional 缺省的区别 |
| resource | 属于创建它的绑定；复制句柄共享关闭状态，结果可与业务 fault 一起返回空资源 |
| 浮点与复数 | 保留对应类型的宽度、负零和可表示的特殊值 |

通信双方使用匹配的 schema 生成 binding，绑定时校验契约身份。结果解码失败时，生成客户端会回收
本次尚未交付的资源。

共享声明可以独立分发，并通过显式 alias 引用：

```text
import model "example/model.mrpc";
```

各语言的 package 配置只决定源码位置，与接口 namespace 分开；导入的声明也需生成对应语言的代码。

## Go 客户端与 Mini-Go 服务端

### 在 Go 中调用

沿用示例的 `provider`，可直接通过本地 Binder 调用，无需 VM 或网络：

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

Go 客户端方法接收 `context.Context`。传入 `context.WithTimeout` 创建的 context 即可限制调用等待时间。
`LocalBinder` 为每个服务固定一个 Provider；需要多实例选择时改用 Router。

### 在 Mini-Go 中提供服务

生成代码同样包含 `GreeterHandler` 和 `ServeGreeter`。可将以下实现加入示例的 Mini-Go 包：

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

宿主必须配置 `rpc.HostOptions.PublishProvider`，决定如何发布该服务；只有 `Providers` 的客户端 Host 不承担发布。
将 `Serve` 注册为 Engine 命名入口后，用 `Instance.Start` 保持服务运行；服务结束前该入口会持续等待请求。
长任务通过 `rpc.Context.Done()` 或 `Err()` 配合取消。宿主关闭 Instance 或取消该入口时，服务随之结束。

Go、Mini-Go 和不同 VM 都可以同时提供服务与发起调用，连接方向不决定调用角色。

## 可选服务与本地实现

Mini-Go 的 NewGreeterClient 延迟到首次调用时绑定。需要预先选择宿主服务或本地实现时，
使用生成的 BindGreeterClient：它检查完整契约，不执行业务方法。

使用 `rpc.CodeOf(err)` 检查绑定错误，只有 unimplemented 适合作为缺少服务的回退条件。接口不匹配、权限、超时和
断线按错误处理；业务调用不会自动重放。绑定失败可以重试，已关闭的客户端不可重开。
官方标准库能力的可选装配见 [系统能力](USAGE.md#系统能力)。

## 错误与超时

Go handler 可返回 `rpc.StatusError{Code: rpc.CodeInvalidArgument, Message: "..."}`；Mini-Go handler 使用
`rpc.NewError("invalid_argument", "...")`。调用方通过各自 `rpc.CodeOf(err)` 获取状态码与说明。

| 状态 | 调用方处理 |
| --- | --- |
| `invalid_argument`、`not_found` 等业务错误 | 根据业务处理或向上返回 |
| `canceled`、`deadline_exceeded` | 停止等待；handler 应协作响应 context |
| `unimplemented` | 绑定所需服务或方法未提供，可显式选择本地实现 |
| `failed_precondition` | 同名方法的契约不一致，检查声明与生成代码 |
| `unavailable` | 检查 provider 健康、路由条件、关闭状态与连接 |
| `resource_exhausted` | 减少并发或单次数据量，或调整宿主额度 |
| `protocol` | 检查两端是否使用匹配的声明和生成代码 |

Mini-Go 的 `ClientOptions.TimeoutNanoseconds` 约束每次绑定、调用、释放和关闭；零值不额外施加该超时。
Go 客户端使用每次调用的 context。网络 Endpoint 使用以下配置，Rust 对应字段采用 snake_case：

| 配置 | 默认值 | 作用 |
| --- | --- | --- |
| `LeaseTTL` | 60 秒 | 接收方授予操作和绑定的租期，由实际 owner 自动续租 |
| `AdmissionTimeout` | 10 秒 | 请求完整写出后等待受理，不限制已受理操作的总时长 |
| `MaxCallDuration` | 不设 | 服务端业务执行上限，与调用方剩余预算取较小值 |

Go 的零值选择默认值，Rust 使用 `EndpointOptions::default()`；显式租期和接纳期限至少为 1 毫秒。
正常等待的调用、结果确认和资源关闭可以跨越多轮租期；心跳不延长显式业务 deadline。
失去租约授权后返回 `unavailable`，恢复连接时需要重新绑定。
同进程原生调用不使用网络租约。

生成客户端负责结果解码、确认和未交付资源的回收。直接使用底层 `RouteSet.Call` 时，调用方必须对
`Result` 执行一次 `Accept(ctx)` 或 `Discard(ctx)`。

超时或断线不证明服务端没有执行操作。框架不会自动重试可能有副作用的调用，业务重试应自行保证幂等性。
Go handler 的普通 error 和 panic 会转成内部错误；需要调用方区分的业务失败应返回明确状态码。

## 资源与大对象

文件、游标、会话等有状态对象适合声明为 resource，由服务方法返回生成的资源客户端。
resource 自动带关闭方法，不在声明中添加 `Close`。使用结束后显式关闭：Go 资源客户端的 `Close` 接收 context，
Mini-Go 资源客户端的 `Close()` 不接收参数。service 客户端使用 `Close()`。

资源绑定到创建它的服务实例和连接；路由改变不会迁移已有资源，连接终止后需重新绑定和创建资源。
Go 服务客户端关闭后，已经取得的资源仍需逐个关闭；Mini-Go 服务客户端关闭时会同时关闭绑定中的资源。
关闭 Instance 会回收会话资源，共享 Host 与业务 backend 仍由创建者关闭。

资源开始关闭后不能再调用。关闭失败时仍由原 owner 负责，调用方可以再次 Close；并发 Close 共享同一次尝试。
Close 会等待已经进入 handler 的资源调用退出，Shutdown 会回收遗留资源并报告最终错误。

较大的 bytes、字符串和复合值由连接透明分片，无需业务手动处理网络帧。默认单条逻辑消息上限为 64 MiB，
单连接在途 payload 上限为 128 MiB，可通过 `rpc.EndpointOptions.Limits` 配置。
VM 与 Host 也有各自的限制；例如 `runtime.Limits.MaxBoundaryBytes` 约束 VM 调用边界，调大网络限额不会自动调大它。
Host 和 Gateway 分别限制会话与连接数；尚未清理完成的对象继续占用额度。

持续流或超出单条消息上限的数据，使用 resource 的 `Read`/`Write` 一类分块业务方法。

## 远程连接与路由

本地固定服务使用前面的 Host 即可；动态服务使用 `rpc/router.Router` 注册 provider，再将 Router 作为 Host 的
`Fallback` 或生成 Go 客户端的 Binder。客户端的 `BindOptions.Labels` 可约束候选服务，
`AffinityKey` 可请求稳定选择。客户端绑定后保持所选 provider，重新选择需要创建新的客户端。
标签按 key 和 value 精确匹配，空的 Labels 集合表示不按标签筛选。

跨进程支持 `ws://`、`wss://` 和本地 Unix socket 地址 `ws+unix:///绝对路径`。启动中转 Gateway：

```bash
mini-go gateway -listen ws://127.0.0.1:8080/rpc
```

连接已发布服务的脚本应用：

```bash
mini-go -C ./consumer run -rpc ws://127.0.0.1:8080/rpc .
```

Gateway 启动时没有业务服务，provider 必须先通过发布接口注册。`run -rpc` 为脚本客户端连接远端服务，
本地已安装的标准库能力优先。服务端按前文的发布接口显式发布服务。

library 使用 `gateway.Dial(ctx, address, gateway.DialOptions{...})` 得到 Endpoint，将其作为 Binder 交给
生成的 Go 客户端，或设置为 `rpc.HostOptions.Fallback` 供 Mini-Go 调用。Endpoint 由创建方关闭。
监听 `wss://` 的 CLI Gateway 还需提供 `-tls-cert` 和 `-tls-key`。

## 热更新与关闭

Program 热更新保持 FFI 会话和现有客户端连接。它更新脚本代码，不自动替换 Go handler、重新注册 provider
或搬移已有 resource。需要更新服务实现时：

1. 创建并检查新的 provider，使用 Router `PrepareReplace` / `Replace` 原子替换 publication。
2. 让新绑定选择新 provider，并按应用策略关闭旧客户端和资源。
3. 等旧 publication 实际关闭后释放其业务 backend。

保存在脚本 global 中的客户端由应用显式关闭或替换；已有资源继续属于原 provider。
代码提交、路由替换和旧服务清理是不同事件。`Drain` 停止新绑定，已有绑定仍可调用；
`ForceClose` / `ForceShutdown` 撤销已有绑定并等待清理。Router 也负责关闭已替换但仍存活的旧注册。
新接口需在调用方与服务端重新生成代码，热更新方案见[使用指南](./USAGE.md#运行时热更新)。

关闭顺序通常为：停止新请求，关闭 Instance，再关闭 Host、客户端和连接，最后释放业务 backend。
`Shutdown(ctx)` 的 context 只限制本次等待，已开始的清理继续进行，之后可以再次等待终态。

| 操作 | 关闭语义 |
| --- | --- |
| Gateway `BeginDrain()` | 拒绝新 session，已接受的握手和连接可以完成 |
| Gateway `Shutdown(ctx)` | 发起关闭并等待清理，可在等待取消后再次等待 |
| Endpoint `Wait(ctx)` | 仅等待既有清理，返回清理错误，不发起关闭或报告断连原因 |
| Endpoint `Shutdown(ctx)` | 发起关闭，报告传输与清理错误 |

Gateway 在清理完成后释放连接名额。脚本和共享宿主的关闭流程见
[优雅停机](USAGE.md#优雅停机)，执行预算见[长期运行](USAGE.md#长期运行)。

## TypeScript / JavaScript API

`@d7z-team/mini-go/rpc` 在 Browser 与 Node.js 中提供相同的 API。它创建独立的 Worker、Rust Endpoint
和 WebSocket，不需要 Mini-Go 镜像。生成代码是环境无关的 TypeScript ESM；项目使用 TypeScript、bundler
或其他现有构建步骤生成 JavaScript，不需要维护另一份手写 binding。

为前面的 Greeter schema 生成 TypeScript：

```bash
mini-go rpc generate -ts-out src/greeter.ts api/greeter.mrpc
```

`-ts-runtime` 可覆盖生成代码导入的 RPC SDK 模块，缺省为 `@d7z-team/mini-go/rpc`；`-ts-prefix`
为当前文件的导出声明增加前缀。被导入 schema 必须声明 `ts_module`，路径应使用最终 JavaScript 的
ESM specifier，例如 `./model.js`。同时指定 `-go-out`、`-mgo-out`、`-rust-out` 和 `-ts-out` 时，所有目标
在同一事务中写入，任一目标生成失败都不会留下部分更新。

生成的客户端直接接收 `RPCConnection`：

```ts
import { RPC } from "@d7z-team/mini-go/rpc";
import { GreeterClient } from "./greeter.js";

const connection = await RPC.connect("wss://example.test/rpc", {
  leaseTtlMs: 60_000,
  admissionTimeoutMs: 10_000,
});
try {
  const client = await GreeterClient.bind(connection, {
    labels: { zone: "local" },
    affinityKey: "account-42",
  });
  try {
    const response = await client.hello(
      { name: "TypeScript" },
      { timeoutMs: 5_000 },
    );
    console.log(response.message);
  } finally {
    await client.close();
  }
} finally {
  await connection.close();
}
```

同一 binding 也可提供服务：

```ts
import { RPC, RPCStatus } from "@d7z-team/mini-go/rpc";
import { createGreeterProvider } from "./greeter.js";

const connection = await RPC.connect("wss://example.test/rpc");
const publication = await connection.publish(
  createGreeterProvider({
    hello(context, request) {
      if (context.signal.aborted) throw new RPCStatus("canceled", "call canceled");
      return { message: `Hello, ${request.name}!` };
    },
  }),
  { name: "web-worker", weight: 1 },
);

try {
  await runApplicationUntilShutdown();
} finally {
  await publication.close();
  await connection.close();
}
```

示例中的 `runApplicationUntilShutdown` 代表应用自己的服务生命周期。

Browser 与 Node.js 通过 conditional export 选择各自的 Worker 适配器；生成的 `greeter.ts` 无需环境分支。
浏览器直接部署 `dist` 时可导入 `browser-rpc.js`，并让生成代码的
`@d7z-team/mini-go/rpc` specifier 由 bundler 或 import map 指向该入口。`workerUrl` 和 `wasmUrl` 可覆盖
默认分发位置；独立 Worker 入口由 `@d7z-team/mini-go/rpc-worker` 导出。

TypeScript 映射保留 wire 语义：64 位整数使用 `bigint`，bytes 使用 `Uint8Array | null`，optional 使用
`undefined`，map 使用 `Map`，复数使用 `{re, im}`。生成的资源客户端只能回传给原 binding，并应显式
`close()`；Provider 资源在远端丢弃、关闭或连接结束时由 SDK 清理。

调用没有隐式 deadline。`timeoutMs` 是正整数毫秒，也可用 `AbortSignal` 取消等待；省略后由 Endpoint
租约维持存活。断线会以 `unavailable` 结束待处理操作并使资源失效，应用需要创建新 connection 并重新
bind/publish。`close()` 等待清理；`terminate()` 直接终止 Worker，不保证异步清理完成。

## Rust API

原生 VM、取消与 Tokio 接入见 [Rust 使用指南](playground/runtime-rust/USAGE.md)，本节只说明 RPC 装配。

Rust crate 位于 `playground/runtime-rust`，启用 `rpc` 使用本地服务、Router、Endpoint
与 FFI Host；`rpc-gateway` 增加 WebSocket/TLS/Unix socket。`stdlib-host` 提供原生标准库宿主。

使用同一 schema 生成 Rust 模块：

```bash
mini-go rpc generate -rust-out src/greeter.rs -rust-module crate::greeter api/greeter.mrpc
```

将输出作为 `mod greeter` 装配。依赖 schema 的 `rust_module` 指定其模块路径；
`rust_runtime` 默认 `mini_go`，`rust_prefix` 修改生成名称。
这些配置只影响源码组织，生成需要 rustfmt。多语言输出可在同一命令中指定。

| 操作 | API |
| --- | --- |
| 实现服务 | generated handler trait 返回 `rpc::BoxFuture` |
| 创建 provider | `greeter_provider(Arc<dyn GreeterHandler>)` |
| 本地装配 | `LocalBinder::new(runtime_handle, limits, providers)` |
| 绑定客户端 | `GreeterClient::bind(context, &binder, bind_options).await` |
| 接入 VM | `HostOptions::new(tokio_handle)` 配置 provider，Host 实现 `ffi::Bridge` |
| 远程连接 | `gateway::dial(handle, address, options)` 返回可作为 Binder 的 Endpoint |

生成方法使用 snake_case，结果为 tuple；集合的 nil 与 optional 缺省通过 Option 表达。
资源 client 可克隆，别名共享关闭状态，使用结束后显式关闭。

Cargo 依赖启用 `mini-go` 的 `rpc` feature，并由应用提供 Tokio runtime。生成的 handler 返回
`rpc::BoxFuture`；创建 provider 后可交给 `LocalBinder`，也可放入 `HostOptions.providers` 供 VM 使用。
关闭 Tokio runtime 前，应等待 Host、Endpoint 和 Router 完成 shutdown。完整装配可参考
[Rust Gateway 示例测试](playground/runtime-rust/tests/rpc_gateway.rs)。

共享数据与互操作验证见 [RPC 测试数据](testdata/rpc/README.md)。
