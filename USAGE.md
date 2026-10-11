# Go 与 CLI 使用指南

本文面向 Go 宿主应用和 CLI 用户。RPC、Rust 与 Browser/Node.js 分别见
[RPC 指南](RPC.md)、[Rust 指南](playground/runtime-rust/USAGE.md)和
[TypeScript 运行时](playground/runtime-typescript/README.md)。

## 嵌入 API

应用通过 `workspace.SourceSet` 提供源码，使用 `minigo.Engine` 检查或编译：

| 方法 | 用途 |
| --- | --- |
| `Check` / `CheckContext` | 检查根包及依赖，返回诊断 |
| `Compile` / `CompileContext` | 编译命名入口，返回可共享的 `runtime.Program` |
| `Run` | 编译并运行默认入口 |
| `Test` | 构建并运行 `*_test.mgo` 测试 |

Engine 由创建方关闭。共享缓存可通过 `Config.Cache` 注入，backend 仍由创建方管理。

### 完整嵌入示例

下面的程序编译脚本包并调用其导出函数：

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
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	sources, err := workspace.NewMemorySourceSet([]workspace.SourcePackage{{
		ModulePath: "example/calc",
		Files: []source.File{{Path: "calc.mgo", Text: `package calc
func Answer() int { return 42 }
`}},
	}})
	if err != nil {
		return err
	}
	engine, err := minigo.New(minigo.Config{Sources: sources})
	if err != nil {
		return err
	}
	defer engine.Close()

	program, checked, err := engine.Compile("example/calc",
		minigo.EntryPoint{Name: "answer", Function: "Answer"})
	if err != nil || !checked.OK() {
		return fmt.Errorf("compile: %v; diagnostics: %v", err, checked.Diagnostics)
	}

	ctx := context.Background()
	instance, err := program.Instantiate(ctx, runtime.InstanceOptions{})
	if err != nil {
		return err
	}
	defer instance.Close()

	result, err := instance.Call(ctx, "answer")
	if err != nil {
		return err
	}
	answer, _ := result.Values[0].Int64()
	fmt.Println(answer) // 42
	return nil
}
```

console、文件系统等能力需要显式装配，见[系统能力](#系统能力)。

## 源码装配

Engine 自动提供标准库。应用源码通常由 `workspace.DiscoverSourceTree` 或内存 `SourceSet` 创建；
额外模块通过 `NewModuleLibrary` 注册逻辑导入前缀和 `fs.FS`。宿主提供可用源码集合，compiler 根据
import 选择实际依赖。惰性文件系统在编译期间必须保持稳定，内容变化后应创建新快照。

### 编译配置

`minigo.Config.Optimization` 选择 O0、O1 或 O2；源码单步使用 O0，常规运行使用 O1。
`Symbols` 生成独立调试符号。`Config.Limits` 控制源码、语法、诊断和泛型实例化等编译容量，
有效限制参与缓存 identity，但不改变语言规则。

### 嵌入资源

包资源写入 `workspace.SourcePackage.Resources`，路径相对 package，内容参与缓存 identity：

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

Program 可复用；每个 Instance 拥有独立 globals、调度状态、限额和 FFI Session。

| 调用 | 语义 |
| --- | --- |
| `Call` / `Start` | 调用显式注册的命名入口 |
| `CallEntry` / `StartEntry` | 将 default entry 作为 library 调用 |
| `CallMain` / `StartMain` | 运行 main；返回后结束实例的其他任务 |

普通入口返回只完成入口结果，派生 task 可能继续运行。`Execution.Wait` 等待结果，`WaitScope` 或
`ScopeDone` 等待所属后台工作。需要主动推进时使用 `PollSteps`；需要终止整个 scope 时调用 `Cancel`。

`InstanceOptions.Parallelism` 限制实例内同时推进的 guest task。独立容量可通过
`runtime.NewExecutor` 提供；自建 Executor 由宿主在实例关闭后关闭。异步唤醒使用 `Execution.Ready`，
实例正由其他 owner 推进时稍后重试 `runtime.ErrBusy`。

### 资源限制与观测

`InstanceOptions.Limits` 控制步数、任务、帧、调用边界、动态类型和 guest 逻辑内存。零值使用默认限制；
`minigo.UnlimitedSteps` 只放开累计步数，取消和其他限额仍然有效。`ScopeStats`、`RuntimeStats` 和
可选 guest profile 用于观测。指标口径见[缓存与性能](DEVELOPMENT.md#缓存与性能)。

### 优雅停机

宿主先停止新调用和补丁，通知常驻脚本退出并等待相关 scope，再调用 `Shutdown` 清理实例。
共享 Host、Executor、连接和 backend 在实例终态之后关闭。业务退出和资源清理可以使用不同 context；
一次等待超时不会撤销已经开始的关闭，之后可再次等待。

正常返回会执行 guest defer；取消不保证执行 defer，也不代表不配合取消的宿主 I/O 已经停止。

## 系统能力

纯计算可使用空 `InstanceOptions`。console、文件系统和环境等能力由宿主选择并注入：

| 配置 | 用途 |
| --- | --- |
| `stdlibhost.NewDefault` | 按 capability 装配官方 provider |
| `stdlibhost.New` | 组合自定义 provider 并校验必需集合 |
| `InstanceOptions.FFI` | 注入 Host 或自定义 `ffi.Bridge` |
| `InstanceOptions.Clock` / `Entropy` | 注入时间和随机输入 |

`Engine.AvailableHostCapabilities` 列出可装配能力，`Program.RequiredHostCapabilities` 返回程序的强制
需求。未装配能力返回 unsupported；可能阻塞的调用应响应取消。

```go
host, err := stdlibhost.NewDefault(stdlibhost.DefaultOptions{
	Capabilities: []string{"console"},
	Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
})
if err != nil {
	return err
}
defer host.Close()

instance, err := program.Instantiate(ctx, runtime.InstanceOptions{FFI: host})
if err != nil {
	return err
}
defer instance.Close()
_, err = instance.CallMain(ctx)
return err
```

Instance 只拥有自己打开的 FFI Session，共享 Host 仍由创建者关闭。

## 运行时热更新

应用先编译完整的新 Program，再准备并提交补丁：

```go
plan, err := instance.PreparePatch(ctx, nextProgram)
if err != nil {
	return err
}
defer plan.Close()

patch, err := instance.ApplyPatch(plan)
if err != nil {
	return err
}
program = nextProgram
fmt.Printf("revision %d: %s\n", patch.Current.Generation, patch.Current.Hash)
```

准备失败保持当前状态。提交要求安全点和未变化的基准 revision；失败后重新准备。
已有帧、defer 和闭包继续运行旧 revision，新命名调用进入新 revision，兼容 globals 保持状态。
需要改变状态契约时创建新实例并由应用迁移数据。

`runtime.ComparePrograms` 和 `plan.Inspect` 展示候选差异；`RevisionRetention` 与 `RevisionRoots` 用于定位
旧代码引用。FFI Session 和连接跨 revision 保持，RPC 服务替换另见
[服务替换与关闭](RPC.md#服务替换与关闭)。

## CLI

已安装的命令可直接在源码目录运行；从仓库运行时使用
`go run ./cmd/mini-go -C <应用目录> <子命令>`。

```bash
mini-go check ./...
mini-go run .
mini-go run main.mgo helper.mgo
mini-go test ./...
mini-go fmt file.mgo
```

`-C` 是全局参数。目录模式以该目录为源码根，`-module` 设置逻辑前缀；`.` 选择根包，`./...` 选择包树。
显式 `.mgo` 文件必须位于同一目录。`fmt` 同时接受 `.mgo` 和 `.mrpc`。

`run` 和 `test` 通过 `-O=0|1|2` 选择优化级别；`-symbols` 生成调试符号。
`run -max-steps=0` 使用默认预算，`-max-steps=-1` 放开累计步数。

### 本地源码装配

`run/check/test/doc` 支持重复的 `-source 导入前缀=目录`：

```bash
mini-go -C ./scripts run -module app -source company/rules=../rules .
mini-go -C ./scripts check -source company/rules=../rules ./...
```

一个包只能来自一个 SourceSet，磁盘源码根不能重叠；依赖由 import 选择。

### 编译缓存

编译产物默认保存在 Mini-Go 缓存中，`MINIGO_CACHE` 可指定绝对路径。
`mini-go cache inspect/verify/clean` 用于查看、校验和清理；开发诊断见
[缓存诊断](DEVELOPMENT.md#缓存诊断)。

## 编辑器与调试符号

[VS Code 扩展](vscode-ext/README.md) 启动 `mini-go lsp`，提供诊断、补全、导航、重命名、格式化和
语义高亮。DAP 支持 launch、断点、暂停、单步、调用栈和变量。

LSP `initializationOptions` 与 DAP launch 都接受 `module`、`sources` 和 `tags`；DAP 另外使用 `cwd` 和
`root` 选择入口。源码调试需要与 Program 匹配的独立符号，恢复执行后帧和变量引用失效。

Go 应用也可直接使用 `compiler/language` 查询语言信息，或用 `compiler/service.Session` 管理增量文档、
分析与构建。snapshot 语义见[语言服务与调试](ARCHITECTURE.md#语言服务与调试)。

## 语言与标准库

Mini-Go 的语言版本、Unicode 和标准库支持范围独立于宿主 Go 版本。标准库是精选移植，实际 API 与行为
以[生成参考](docs/reference/README.md)为准。需要宿主资源的包通过 capability 注入，默认 Local 时区为 UTC。
