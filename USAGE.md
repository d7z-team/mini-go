# Go 与 CLI 使用指南

本文介绍 Go 宿主如何装配源码、编译和运行 Mini-Go，以及 CLI 的常用入口。RPC、Rust 和
Browser/Node.js 分别见 [RPC 指南](RPC.md)、[Rust 使用指南](playground/runtime-rust/USAGE.md)
和 [TypeScript 运行时](playground/runtime-typescript/README.md)。

## Go 嵌入

`minigo.Engine` 使用 `workspace.SourceSet` 作为应用源码。常用 API 如下：

| API | 用途 |
| --- | --- |
| `Check` / `CheckContext` | 检查根包及依赖并返回诊断 |
| `Compile` / `CompileContext` | 编译一个或多个命名入口，得到可复用的 `runtime.Program` |
| `Run` | 编译并运行 `main`，自动关闭实例 |
| `Test` | 编译并运行 `_test.mgo` 测试 |

Engine 和 Instance 由创建方关闭；Program 是可复用的不可变编译产物。源码集合和通过 `Config.Cache`
注入的缓存 backend 仍由创建方管理。

### 完整嵌入示例

```go
package main

import (
	"context"
	"fmt"
	"log"

	minigo "github.com/d7z-team/mini-go"
	"github.com/d7z-team/mini-go/compiler/source"
	"github.com/d7z-team/mini-go/compiler/workspace"
	"github.com/d7z-team/mini-go/runtime"
)

func main() {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "example/calc",
		Files: []source.File{{Path: "calc.mgo", Text: "package calc\nfunc Answer() int { return 42 }\n"}},
	}})
	if err != nil { log.Fatal(err) }
	engine, err := minigo.New(minigo.Config{Sources: sources})
	if err != nil { log.Fatal(err) }
	defer engine.Close()

	program, checked, err := engine.Compile("example/calc",
		minigo.EntryPoint{Name: "answer", Function: "Answer"})
	if err != nil || !checked.OK() {
		log.Fatalf("compile: %v; diagnostics: %v", err, checked.Diagnostics)
	}
	instance, err := program.Instantiate(context.Background(), runtime.InstanceOptions{})
	if err != nil { log.Fatal(err) }
	defer instance.Close()
	result, err := instance.Call(context.Background(), "answer")
	if err != nil { log.Fatal(err) }
	answer, _ := result.Values[0].Int64()
	fmt.Println(answer)
}
```

应用源码也可以通过 `workspace.DiscoverSourceTree` 加载。额外模块使用 `NewModuleLibrary` 注册
导入前缀和 `fs.FS`。源码快照在编译期间应保持稳定，内容变化后创建新的 SourceSet。

`Config.Optimization` 支持 O0、O1 和 O2；`Config.Symbols` 生成独立调试符号；`Config.Limits`
限制源码、诊断和泛型实例化等资源。`workspace.SourcePackage.Resources` 可提供 embed 文件，
资源路径相对于 package，并参与缓存身份：

```go
workspace.SourcePackage{
	ModulePath: "example/assets",
	Files: []source.File{{Path: "assets.mgo", Text: "package assets\n"}},
	Resources: []workspace.ResourceFile{{Path: "templates/welcome.txt", Data: []byte("hello")}},
}
```

## Program 与 Instance

`Program` 可以实例化多次；每个 `Instance` 独立拥有全局变量、调度状态、限额和 FFI session。

| 调用 | 用途 |
| --- | --- |
| `Call` / `Start` | 调用显式注册的命名入口 |
| `CallEntry` / `StartEntry` | 调用 default entry |
| `CallMain` / `StartMain` | 运行 `main` |
| `Execution.Wait` | 等待一次入口执行完成 |
| `WaitScope` / `ScopeDone` | 等待入口派生的后台任务 |
| `Cancel` | 取消当前 scope |
| `PollSteps` | 由宿主手动推进执行 |

执行由其他 owner 推进时会返回 `runtime.ErrBusy`，调用方应稍后重试。`Execution.Ready` 可用于
观察异步唤醒；`InstanceOptions.Parallelism` 控制实例内并发 guest task。宿主提供的
`runtime.Executor` 在所有使用它的实例关闭后再关闭。

`InstanceOptions.Limits` 控制步数、任务、调用、动态类型和 guest 存储等资源；零值使用默认值。
`minigo.UnlimitedSteps` 只解除累计步数限制，取消和其他限额仍然生效。`ScopeStats`、
`RuntimeStats` 和 guest profile 用于诊断，指标口径见[开发指南的缓存与性能](DEVELOPMENT.md#缓存与性能)。

关闭实例时先停止新调用和补丁，等待相关 scope，再调用 `Shutdown`。关闭等待的 context 只限制
本次等待；超时后可以再次等待，已经开始的清理会继续。正常返回执行 guest defer，取消不保证执行
defer，也不表示未配合取消的宿主 I/O 已停止。

## 宿主能力

纯计算可以使用空的 `InstanceOptions`。需要 console、文件系统或环境等能力时，使用标准 provider
或自定义 `ffi.Bridge`：

| 配置 | 用途 |
| --- | --- |
| `host.NewDefault` | 按 capability 装配标准 provider |
| `host.New` | 组合自定义 provider |
| `InstanceOptions.FFI` | 注入 Host 或自定义 bridge |
| `InstanceOptions.Clock` / `Entropy` | 注入时间和随机输入 |

`Engine.AvailableHostCapabilities` 列出可装配能力，`Program.RequiredHostCapabilities` 列出程序
要求。缺少能力会返回 unsupported；可能阻塞的 provider 应响应调用取消。

```go
// import host "github.com/d7z-team/mini-go/stdlib/host"
host, err := host.NewDefault(host.DefaultOptions{Capabilities: []string{"console"}})
if err != nil { return err }
defer host.Close()
instance, err := program.Instantiate(ctx, runtime.InstanceOptions{FFI: host})
if err != nil { return err }
defer instance.Close()
_, err = instance.CallMain(ctx)
return err
```

Instance 只拥有自己打开的 FFI session，共享 Host 仍由创建者关闭。

## 热更新

先编译完整的新 Program，再准备并提交补丁：

```go
plan, err := instance.PreparePatch(ctx, nextProgram)
if err != nil { return err }
defer plan.Close()
if _, err := instance.ApplyPatch(plan); err != nil { return err }
program = nextProgram
```

准备失败保持当前状态；提交要求安全点和未变化的基准 revision。已有帧、defer 和闭包继续运行旧
revision，新命名调用进入新 revision。状态结构变化时创建新实例并由应用转移数据。`ComparePrograms`
和 `plan.Inspect` 可用于查看候选差异；旧 revision 的引用可用 `RevisionRetention` 和
`RevisionRoots` 诊断。RPC provider 替换见 [RPC 服务替换与关闭](RPC.md#服务替换与关闭)。

## CLI

安装后的命令可以直接使用；仓库中可运行 `go run ./cmd/mini-go -C <应用目录> <子命令>`：

```bash
mini-go check ./...
mini-go run .
mini-go run main.mgo helper.mgo
mini-go test ./...
mini-go fmt file.mgo
```

`-C` 在解析子命令前切换工作目录。目录模式使用该目录作为源码根，`-module` 设置逻辑前缀；`.`
选择根包，`./...` 选择包树。显式 `.mgo` 文件必须位于同一目录，`fmt` 也接受 `.mrpc`。

`run` 和 `test` 支持 `-O=0|1|2`、`-symbols`；`run -max-steps=0` 使用默认预算，`-max-steps=-1`
解除累计步数限制。`run`、`check` 和 `test` 可重复 `-source import/path=directory`：

```bash
mini-go -C ./app run -module app -source company/rules=../rules .
mini-go -C ./app check -source company/rules=../rules ./...
```

## 编译缓存

编译缓存默认使用 Mini-Go 缓存目录，`MINIGO_CACHE` 可指定绝对路径；Go 嵌入方可通过 `Config.Cache`
注入 `compiler/cache.Backend`。CLI 提供：

```bash
mini-go cache inspect
mini-go cache verify
mini-go cache clean
```

缓存诊断和维护流程见[开发指南](DEVELOPMENT.md#缓存诊断)。

## 编辑器与调试

[VS Code 扩展](vscode-ext/README.md)通过 `mini-go lsp` 提供诊断、补全、导航、重命名和格式化；
DAP 提供启动、断点、暂停、单步、调用栈和变量。源码调试需要与 Program 匹配的独立符号。

语言服务可直接使用 `compiler/language` 和 `compiler/service.Session`。

## 标准库

Mini-Go 的语言版本、Unicode 和标准库支持范围独立于宿主 Go；标准库 API 与行为以
[生成参考](docs/reference/README.md)为准，
需要宿主资源的包通过 capability 注入，默认 Local 时区为 UTC。
