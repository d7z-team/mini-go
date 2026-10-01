# 使用指南

本文面向 Go 宿主应用和 CLI 用户。首次接入从[完整嵌入示例](#完整嵌入示例)开始；
RPC、Rust 和浏览器接入分别见 [RPC 指南](RPC.md)、[Rust 指南](playground/runtime-rust/USAGE.md)
与 [TypeScript SDK](playground/runtime-rust/runtime-wasm/README.md)。

按任务查阅：[嵌入](#嵌入-api) · [源码装配](#源码装配) · [资源](#嵌入资源) · [执行与取消](#运行实例) ·
[停机](#优雅停机) · [宿主能力](#系统能力) · [热更新](#运行时热更新) ·
[命令行](#cli) · [编辑器](#编辑器与调试符号)。

## 嵌入 API

应用通过 `workspace.SourceSet` 提供源码，并创建可复用的 `minigo.Engine`。

| Engine 方法 | 用途 |
| --- | --- |
| `Check` / `CheckContext` | 检查根包及其依赖，返回诊断 |
| `Compile` / `CompileContext` | 编译命名入口，返回可共享的 `runtime.Program` |
| `Run` | 编译并运行默认入口 |
| `Test` | 构建并运行全部或指定的 `*_test.mgo` 测试 |

Engine 由创建方关闭。共享缓存可通过 `Config.Cache` 注入，backend 仍由创建方管理；
自定义实现见[缓存接入约定](DEVELOPMENT.md#自定义缓存)。

### 完整嵌入示例

以下完整示例编译一个没有 `main` 的脚本包，并调用其导出函数：

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

console、文件系统等宿主能力需要显式装配，见[系统能力](#系统能力)。

## 源码装配

Engine 自动提供标准库源码。额外源码可通过 `NewStandardLibrary`、
`NewModuleLibrary` 和 `NewLibrarySet` 注册到 `Config.Libraries`。
模块通过逻辑导入前缀和 `fs.FS` 提供，例如 `NewModuleLibrary("rules", "company/rules", rulesFS)`。
应用使用 `workspace.DiscoverSourceTree(appFS, ".", "app")` 构造源码快照；
也可通过 `NewTreeSourceSet` 提供内存文件，再用 `MergeSourceSets` 合并多个来源。
宿主提供全部可用源码，compiler 根据 import 选择实际参与编译的包。

惰性源码树要求底层 FS 在使用期间保持稳定；目录内容改变后应创建新快照再编译。

### 编译配置

`minigo.Config` 的 `Optimization`、`Symbols` 选择优化级别与独立调试符号，零值为 O0、不生成符号。
O0 适合源码单步，O1 为常规优化，O2 增加保持求值顺序的函数内优化。

`Config.Limits` 控制包、文件、源码字节、token、AST、诊断和泛型实例化容量。
省略时使用默认值，正数可提高或收紧容量；`MaxSyntaxDepth` 保留解析器结构上限。
例如 `compiler.Limits{MaxSourceBytes: 32 << 20}` 允许更大的单文件源码。
直接使用 compiler 时，对应配置位于 `compiler.Options`，请求可通过 `compiler.Request.Limits` 指定限制。
有效限制参与缓存身份，调整容量不改变类型检查规则。

### 嵌入资源

`workspace.SourcePackage.Resources` 保存 package-relative 资源，资源内容参与编译缓存 identity：

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

文件系统源码发现会将 `.mgo` 作为源码，包目录中的普通文件可通过 `//go:embed` 嵌入。

## 运行实例

Program 可复用，每个 Instance 持有独立状态。创建实例后，根据入口类型选择调用方式，
并分别管理入口结果、后台 scope 和实例的生命周期。

### 入口与生命周期

| 调用 | 语义 |
| --- | --- |
| `Call` / `Start` | 按名称调用显式注册的入口 |
| `CallEntry` / `StartEntry` | 将 default entry 作为普通 library 调用 |
| `CallMain` / `StartMain` | 执行真实 main，返回后结束 Instance 的其余任务 |

根包为 main 且未指定入口时，main 成为 default。普通入口返回后，其派生任务可以继续运行。
`Execution.Wait` 等待入口结果；`WaitScope` 或 `ScopeDone` 等待该入口的全部后台工作。

### 调度与取消

需要主动调度时使用 Start 得到 Execution，再调用 Poll、Wait 或 Cancel。
`PollSteps(maxSteps)` 限制本次推进的指令数，返回状态、实际步数和错误；
Pending 或暂停时实际步数可以为零；步数预算不等于墙钟 CPU 限速。

`InstanceOptions.Parallelism` 限制同一实例并行执行的 guest task，零值采用 1。需要独立容量时，
创建 `runtime.NewExecutor(workers)` 并传入 `InstanceOptions.Executor`；自建 Executor 由宿主关闭。

`Execution.Ready` 提供事件唤醒；owner 正忙时 Poll 返回 `runtime.ErrBusy`，调用方稍后重试。
`Wait` 的 context 取消会请求取消执行；`WaitScope` 的 context 只限制本次等待。需要终止后台工作时，
显式调用 `Cancel`，再用新的 context 等待 scope 收敛。

library scope 超过限制时仅该 scope 失败；main scope 超限会结束实例。后台任务未恢复的 panic 和
无法推进的等待分别通过 Instance 错误与 `AllBlockedError` 报告。

### 资源限制与观测

`InstanceOptions.Limits` 限制步数、任务、内存、调用边界和动态类型等资源，零值采用默认限制，正值覆盖。
`MaxAllocatedBytes` 约束逻辑存活 guest 数据及尚未重新统计的分配，不是累计分配量或进程 RSS。
每个 scope 默认最多 1 亿步；`PollSteps` 和热更新都不重置预算。`MaxSteps` 可设为
`minigo.UnlimitedSteps`（-1）以保留取消和其他限制、放开累计步数，其他负值无效。
`Execution.ScopeStats` 与 `Instance.RuntimeStats` 提供状态与计费快照。
热点定位可配置 `InstanceOptions.GuestProfile` 并读取 `Execution.GuestProfile()`；
统计口径与测量方式见[性能诊断](DEVELOPMENT.md#缓存与性能)。

### 优雅停机

宿主先停止提交新入口和补丁，通过业务通道通知常驻脚本正常返回，再等待相关执行与 scope。
调试暂停需要显式恢复；仅等待 scope 不会推进尚未返回的前台入口。
最后调用 `Shutdown` 取消剩余工作并释放实例拥有的资源。共享 Host 和 backend 在实例清理完成后关闭。

业务退出和清理可使用不同的 context 期限。`Shutdown` 等待超时后，保留宿主资源并再次等待终态。

正常返回会执行 guest defer；取消关闭不保证执行 defer。VM scope 已结束也不能证明不配合取消的宿主 I/O 已退出。

## 系统能力

纯计算可直接使用 `InstanceOptions{}`。console、文件系统和环境由宿主显式提供；
CLI 的 run/test/DAP 按包闭包装配官方 provider，嵌入应用自行选择。

| 配置 | 用途 |
| --- | --- |
| `stdlibhost.NewDefault` | 按 Capabilities 选择官方 provider，配置目录和标准流 |
| `stdlibhost.New` | 组合自定义 Providers，并校验最低 Required 集合 |
| `InstanceOptions.FFI` | 注入 Host 或自定义 `ffi.Bridge` |
| `InstanceOptions.Clock` / `Entropy` | 注入时钟或随机输入，默认使用宿主系统实现 |

`Engine.AvailableHostCapabilities` 列出可装配能力，`Program.RequiredHostCapabilities`
返回强制需求；承载强制需求的 Bridge 需实现 `ffi.CapabilityBridge`。

没有装配的能力返回 unsupported。可能阻塞的输入应支持取消，或由宿主保证能够结束。

例如，导入 `stdlibhost "github.com/d7z-team/mini-go/stdlib/host"` 后装配 console：

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

输出写入宿主的 `io.Writer`，`RunResult.Values` 只包含入口返回值。需要捕获输出时传入自己的 Writer。
Instance 只拥有自己打开的 FFI Session；共享宿主的关闭顺序见[优雅停机](#优雅停机)。
`Close` 使用 background context；需要限制清理等待时间时使用 `Shutdown(ctx)`。

## 运行时热更新

用更新后的 SourceSet 编译完整新 Program，再准备和提交补丁。
以下片段同样位于返回 `error` 的函数内：

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
return nil
```

PreparePatch 失败时保持原状态，未提交的 plan 应 Close。ApplyPatch 在实例
安全点执行；执行中可能返回 busy，基准 revision 已变化则返回 stale_plan。
这两类提交失败会关闭 plan，重试前需重新 PreparePatch；成功后 Close 可安全调用。

已有帧、defer 和闭包保留旧 revision，新命名调用使用当前 revision。
宿主需结束旧循环或 scope，并替换保存的回调，才能释放其引用的旧代码。补丁保持现有
globals、导出与命名类型/函数的状态契约；需要改变这些契约时创建新实例并由应用迁移业务状态。
新 Program 的强制能力必须已安装，FFI Session 与宿主连接跨 revision 保持稳定。
服务替换与资源关闭另见 [RPC.md](RPC.md#服务替换与关闭)。

### 检查补丁与版本引用

提交前用 `runtime.ComparePrograms(currentProgram, candidate)` 查看代码、契约、能力与符号变化；
准备成功后可用 `plan.Inspect()` 查看带基准 generation 的报告。报告只描述差异，最终准入仍由
`PreparePatch` 与 `ApplyPatch` 决定。

`Instance.RevisionRetention(ctx)` 提供版本存活摘要，`RevisionRoots` 在指定扫描限额内列出任务、帧和
全局变量等保留来源。`Complete=false` 表示结果被截断；查询不改变引用关系，引用解除后旧版本自然回收。

## CLI

下例使用已安装的 `mini-go`，目录和 package pattern 示例在应用源码根执行。
从本仓库直接运行时可使用 `go run ./cmd/mini-go -C <应用目录> <子命令>`。

```bash
mini-go check ./...
mini-go run .
mini-go run main.mgo helper.mgo
mini-go test ./...
mini-go fmt file.mgo
```

`-C` 是全局参数，写在子命令之前，缺省当前目录。目录模式以该目录为源码根，
`-module` 指定逻辑前缀，缺省 `app`；`.` 选择根包，`./...` 选择其下包树。
用 `-C` 指向应用源码目录，并将大型构建产物和依赖安装目录放在源码根之外，避免扫描无关文件。
run/check 的操作数全部为 `.mgo` 文件时，文件必须位于同一目录，组成
`command-line-arguments`，只选入点名源码；该模式使用 `-source` 提供其他模块。

Go 宿主与生成的 Go binding 可和 `.mgo` 文件放在同一目录。`fmt` 接受 `.mgo` 和 `.mrpc`；
RPC binding 生成见 [RPC 指南](RPC.md)。

`run -max-steps=0` 使用默认预算，`-max-steps=-1` 不限制 guest 累计步数。
`run` 与 `gateway` 支持 `-shutdown-timeout=30s`，清理各阶段共用总截止时间。
首次信号请求取消，二次信号立即退出；同步宿主阻塞超过截止时间时进程非成功退出并报告清理未完成。

### 编译选项

`run` 和 `test` 通过 `-O=0|1|2` 选择优化级别，默认 O1。
`-symbols` 生成独立的 ProgramSymbols，ExecutionImage 只保存执行代码。
优化级别与嵌入配置见[编译配置](#编译配置)。

### 本地源码装配

`run/check/test/doc` 支持重复的 `-source 导入前缀=目录`，目录相对 `-C`：

```bash
mini-go -C ./scripts run -module app -source company/rules=../rules .
mini-go -C ./scripts check -source company/rules=../rules ./...
```

脚本通过 `import "company/rules"` 使用该源码。一个包只能有一个来源；
磁盘装配的各根目录不能重合或互相包含。普通子目录属于注册的源码树，宿主可通过
较窄的 FS/root 控制输入范围。库按 import 加入，包模式只选择应用包。

### 编译缓存

编译产物和执行镜像默认保存在临时目录下的 Mini-Go 缓存；`MINIGO_CACHE` 可指定其他绝对路径。
`mini-go cache clean/verify/inspect` 用于清理、校验和查看缓存。开发阶段的命中跟踪与一致性诊断见
[开发指南](DEVELOPMENT.md#缓存与性能)。

## 编辑器与调试符号

[VS Code 扩展](vscode-ext/README.md) 启动 `mini-go lsp`，提供诊断、补全、导航、
重命名、格式化和语义高亮。LSP 与 DAP 都通过标准 stdio 协议接入编辑器。

DAP 支持 launch、源码断点、暂停、单步、调用栈与变量。launch 参数示例：

```json
{
  "cwd": "/workspace/app",
  "module": "app",
  "root": ".",
  "sources": [{"module": "rules", "directory": "../rules"}],
  "tags": ["tag"]
}
```

cwd 默认当前目录，module 默认 app；root 是入口包选择，也可为显式 .mgo 文件（此时省略 module）。
LSP initializationOptions 接受相同的 module、sources、tags，目录相对 workspace root。
额外库支持诊断与导航，默认只读；DAP 可在注册的外部源码上设置断点。
DAP 使用 O0 并生成调试符号；普通 run/test 通过 `-symbols` 启用符号。

library 调试接口包括 `SetBreakpoints`、`DebugSnapshot`、`DebugScopes` 和 `DebugVariables`。
源码单步和变量检查需要 `ProgramSymbols`；变量与源码引用在恢复执行后失效。
热更新后断点按新 revision 解析，历史帧保留对应符号。

Go 应用通过 `compiler/language` 查询语言信息，或用 `compiler/service.Session` 管理文档、分析和构建，
使用完毕调用 `Close`。版本与快照的关系见[架构](ARCHITECTURE.md#语言服务与调试)。

## 语言与标准库

Mini-Go 支持 Go 1.27 的泛型方法、提升字段初始化和函数值上下文类型推导，使用 Unicode 17 字符规则；
宿主工具链最低版本仍为 Go 1.26。标准库为精选移植，具体 API 和行为以[生成参考](docs/reference/README.md)为准。
系统能力由宿主注入；默认 Local 时区为 UTC，时间二进制格式属于 Mini-Go 自身契约。
标准库源码组织与维护入口见 [stdlib README](stdlib/README.md)。
