# Go 与 CLI 使用指南

本文介绍 Go 宿主如何装配源码、编译和运行 Mini-Go，以及 CLI 的常用命令。RPC、Rust
和 Browser/Node.js 的入口分别见 [RPC 指南](RPC.md)、[Rust 使用指南](playground/runtime-rust/USAGE.md)
和 [TypeScript 运行时](playground/runtime-typescript/README.md)。

## 嵌入 Go

`minigo.Engine` 接收一个 `workspace.SourceSet`。宿主可以检查源码、编译出可复用的
`runtime.Program`，或直接运行默认入口：

| API | 用途 |
| --- | --- |
| `Check` / `CheckContext` | 检查根包及依赖并返回诊断 |
| `Compile` / `CompileContext` | 编译一个或多个命名入口 |
| `Run` | 编译并运行默认入口，自动关闭实例 |
| `Test` | 构建并运行 `_test.mgo` 测试 |

Engine 由创建方关闭；通过 `Config.Cache` 注入的缓存 backend 和源码集合仍由创建方管理。

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
	if err != nil || !checked.OK() { log.Fatalf("compile: %v; diagnostics: %v", err, checked.Diagnostics) }

	instance, err := program.Instantiate(context.Background(), runtime.InstanceOptions{})
	if err != nil { log.Fatal(err) }
	defer instance.Close()
	result, err := instance.Call(context.Background(), "answer")
	if err != nil { log.Fatal(err) }
	answer, _ := result.Values[0].Int64()
	fmt.Println(answer)
}
```

### 源码与编译配置

应用源码通常来自 `workspace.DiscoverSourceTree` 或内存 `SourceSet`；额外模块通过
`NewModuleLibrary` 注册导入前缀和 `fs.FS`。编译期间源码和惰性文件系统应保持稳定；内容变化后
创建新的源码快照。

`Config.Optimization` 支持 O0、O1 和 O2，`Symbols` 生成独立调试符号，`Config.Limits`
限制源码、语法、诊断和泛型实例化容量。限制和资源内容都会参与编译缓存 identity。

资源通过 `workspace.SourcePackage.Resources` 提供，路径相对于 package，内容随源码参与缓存：

```go
workspace.SourcePackage{
	ModulePath: "example/assets",
	Files: []source.File{{Path: "assets.mgo", Text: `package assets
import "embed"
//go:embed templates/*.txt
var Files embed.FS
`}},
	Resources: []workspace.ResourceFile{{Path: "templates/welcome.txt", Data: []byte("hello")}},
}
```

## 运行实例

`Program` 可以在多个实例间复用；每个 `Instance` 独立拥有 globals、调度状态、限额和 FFI
session。

| 调用 | 语义 |
| --- | --- |
| `Call` / `Start` | 调用显式注册的命名入口 |
| `CallEntry` / `StartEntry` | 调用 default entry |
| `CallMain` / `StartMain` | 运行 `main`；返回后结束实例的其他任务 |

`Execution.Wait` 等待入口结果，`WaitScope` 或 `ScopeDone` 等待入口派生的后台工作；`Cancel`
取消整个 scope。需要宿主自行推进时使用 `PollSteps`，异步唤醒通过 `Execution.Ready` 观察。
实例正由其他 owner 推进时，调用方应稍后重试 `runtime.ErrBusy`。

`InstanceOptions.Parallelism` 限制实例内并发 guest task。宿主提供的 `runtime.Executor` 由宿主
在所有实例关闭后关闭。

### 限额、统计与关闭

`InstanceOptions.Limits` 控制步数、任务、帧、调用边界、动态类型和 guest 逻辑内存；零值使用
默认值。`minigo.UnlimitedSteps` 只解除累计步数限制，取消和其他限额仍有效。`ScopeStats`、
`RuntimeStats` 和 guest profile 用于观测，指标口径见 [开发指南的缓存与性能](DEVELOPMENT.md#缓存与性能)。

关闭时先停止新调用和补丁，通知常驻脚本退出并等待相关 scope，再调用 `Shutdown`。共享 Host、
Executor、连接和 backend 在实例终态后关闭。关闭等待使用的 context 只限制本次等待；超时后可以
再次等待，已经开始的清理会继续。

正常返回会执行 guest defer；取消不保证执行 defer，也不表示未配合取消的宿主 I/O 已经停止。

## 系统能力

纯计算可以使用空的 `InstanceOptions`。console、文件系统和环境等能力需要宿主显式注入：

| 配置 | 用途 |
| --- | --- |
| `host.NewDefault` | 按 capability 装配标准 provider |
| `host.New` | 组合自定义 provider 并校验集合 |
| `InstanceOptions.FFI` | 注入 Host 或自定义 `ffi.Bridge` |
| `InstanceOptions.Clock` / `Entropy` | 注入时间和随机输入 |

`Engine.AvailableHostCapabilities` 列出可装配能力，`Program.RequiredHostCapabilities` 列出程序
要求。缺少能力会返回 unsupported；可能阻塞的 provider 应响应调用取消。

```go
// import "github.com/d7z-team/mini-go/stdlib/host"
stdlibhost, err := host.NewDefault(host.DefaultOptions{Capabilities: []string{"console"}})
if err != nil { return err }
defer stdlibhost.Close()

instance, err := program.Instantiate(ctx, runtime.InstanceOptions{FFI: stdlibhost})
if err != nil { return err }
defer instance.Close()
_, err = instance.CallMain(ctx)
return err
```

Instance 只拥有自己打开的 FFI session，共享 Host 仍由创建者关闭。

## 运行时热更新

先编译完整的新 Program，再准备并提交补丁：

```go
plan, err := instance.PreparePatch(ctx, nextProgram)
if err != nil { return err }
defer plan.Close()
if _, err := instance.ApplyPatch(plan); err != nil { return err }
program = nextProgram
```

准备失败保持当前状态；提交要求安全点和未变化的基准 revision，失败后重新准备。已有帧、defer
和闭包继续运行旧 revision，新命名调用进入新 revision；identity 与 layout 匹配的 global 保留
原存储。状态结构变化时创建新实例并由应用显式转移数据。

`runtime.ComparePrograms` 和 `plan.Inspect` 用于查看候选差异，`RevisionRetention` 与
`RevisionRoots` 用于定位旧代码引用。FFI session 和连接跨 revision 保持；RPC provider 替换见
[服务替换与关闭](RPC.md#服务替换与关闭)。

## CLI

已安装的命令可以直接运行；在仓库中使用 `go run ./cmd/mini-go -C <应用目录> <子命令>`。

```bash
mini-go check ./...
mini-go run .
mini-go run main.mgo helper.mgo
mini-go test ./...
mini-go fmt file.mgo
```

全局选项 `-C` 先切换工作目录。目录模式以该目录为源码根，`-module` 设置逻辑前缀；`.` 选择
根包，`./...` 选择包树。显式 `.mgo` 文件必须位于同一目录，`fmt` 也接受 `.mrpc`。

`run` 和 `test` 支持 `-O=0|1|2`、`-symbols`；`run -max-steps=0` 使用默认预算，`-max-steps=-1`
解除累计步数限制。`run`、`check` 和 `test` 可以重复 `-source 导入前缀=目录`：

```bash
mini-go -C ./app run -module app -source company/rules=../rules .
mini-go -C ./app check -source company/rules=../rules ./...
```

一个 package 只能来自一个 SourceSet，磁盘源码根不能重叠；依赖由 import 选择。

### 编译缓存

编译产物默认保存在 Mini-Go 缓存中，`MINIGO_CACHE` 可指定绝对路径。Go 嵌入方也可以通过
`Config.Cache` 注入 `compiler/cache.Backend`，例如 `cache.NewMemoryBackend()`；backend 由创建者管理，
在 Engine 关闭后释放。

CLI 提供缓存检查与清理命令：

```bash
mini-go cache inspect
mini-go cache verify
mini-go cache clean
```

缓存诊断和维护流程见 [开发指南](DEVELOPMENT.md#缓存诊断)。

## 编辑器与调试

[VS Code 扩展](vscode-ext/README.md) 启动 `mini-go lsp`，提供诊断、补全、导航、重命名、格式化和
语义高亮；DAP 支持 launch、断点、暂停、单步、调用栈和变量。

LSP `initializationOptions` 与 DAP launch 接受 `module`、`sources` 和 `tags`；DAP 还使用 `cwd`
和 `root` 选择入口。源码调试需要与 Program 匹配的独立符号，恢复执行后帧和变量引用失效。

Go 应用也可以使用 `compiler/language` 查询语言信息，或用 `compiler/service.Session` 管理增量
文档、分析和构建。状态快照语义见 [架构说明](ARCHITECTURE.md#语言服务与调试)。

## 语言与标准库

Mini-Go 的语言版本、Unicode 和标准库支持范围独立于宿主 Go 版本。标准库是精选移植，API 与行为
以[生成参考](docs/reference/README.md)为准。需要宿主资源的包通过 capability 注入，默认 Local
时区为 UTC。
