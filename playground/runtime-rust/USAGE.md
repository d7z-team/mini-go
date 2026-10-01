# Rust 原生使用指南

依赖安装、镜像生成和 feature 选择见 [README](README.md)。本文面向原生 Rust 应用；
浏览器与 Node.js 使用 [TypeScript SDK](https://github.com/d7z-team/mini-go/blob/main/playground/runtime-rust/runtime-wasm/README.md)。

[调用](#加载调用与限制) · [生命周期](#等待取消与关闭) · [Tokio](#在-tokio-中执行) ·
[宿主](#宿主能力) · [调试](#断点变量与单步) · [热更新](#热更新) ·
[语言工具](#本地源码与编译器工具)

示例使用 `mini_go` 根路径导出的共享 `Instance`，它可以克隆并跨线程协调执行。
`instance::Instance` 是需要独占可变访问的底层 VM；只有自建调度器时才直接使用它。
`Program` 可跨实例共享，每个实例拥有独立 globals、执行状态和 FFI 会话。

## 加载、调用与限制

`Program::load` 接收当前工具链的原始 JSON 字节，以保留整数精度。gzip 镜像先用
`loader::DecodedImage::decode_gzip` 解码，再交给 `Program::prepare`。

下面是完整的 `src/main.rs`，以命令行参数读取镜像，调用算术示例的 `default` 入口：

```rust
use mini_go::{
    HostValue, InstanceOptions, Limits, LoadOptions, Program, ffi::Cancellation,
};
use std::sync::Arc;

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let path = std::env::args().nth(1).ok_or("expected image path")?;
    let program = Arc::new(Program::load(&std::fs::read(path)?, LoadOptions::default())?);
    let instance = program.instantiate(InstanceOptions {
        limits: Limits { max_steps: 1_000_000, ..Default::default() },
        ..Default::default()
    })?;
    let outcome = (|| {
        let execution = instance.start_host("default", &[HostValue::int(10)])?;
        let result = execution.wait(&Cancellation::default())?;
        execution.wait_scope(&Cancellation::default())?;
        Ok::<_, mini_go::RuntimeError>(result)
    })();
    let closed = instance.shutdown(&Cancellation::default());
    let result = outcome?;
    closed?;
    println!("{:?}", result.roots); // arithmetic.json 返回 55
    Ok(())
}
```

```bash
cargo run -- /path/to/go-mini/playground/runtime-rust/examples/blocks/arithmetic.json
```

`LoadOptions` 约束加载，`Limits { ..Default::default() }` 覆盖执行限制：

| 执行配置 | 含义 |
| --- | --- |
| `max_steps` | 每个 scope 默认 1 亿步；0 使用默认，正数设置预算，`UNLIMITED_STEPS`（-1）不限累计步数 |
| `max_allocated_bytes` | 逻辑存活 guest 数据及尚未重新统计的分配 |
| `max_heap_bytes` | VM arena 存储计费 |

推进批次和热更新不重置预算；不限步数仍保留取消和其他限额。内存计费不等于累计分配量或进程 RSS。
错误通过 `RuntimeError.code`、`path`、`message` 返回；共享实例占用时的 `busy` 可让出执行后重试。

`HostValue` 是宿主输入，`HostSnapshot` 是独立拥有数据的输出，可保留别名和循环。
常用输入可由 `HostValue::int` 等构造；复杂值见 [snapshot 模块](src/snapshot.rs)。
通过 `stats()`、`heap_stats()` 观测执行与内存；通过 `start_profile` 定位源码热点。
统计口径与性能测量见[开发指南](https://github.com/d7z-team/mini-go/blob/main/DEVELOPMENT.md#缓存与性能)。

### 原生执行器与并行度

`InstanceOptions.parallelism` 限制同一实例同时执行的 guest task，零值采用 1。

需要独立控制容量时可创建 `Executor::new(workers)`，克隆后传入多个实例：

```rust
use mini_go::{Executor, InstanceOptions, ffi::Cancellation};

let executor = Executor::new(4)?;
let instance = program.instantiate(InstanceOptions {
    parallelism: 4,
    executor: Some(executor.clone()),
    ..Default::default()
})?;
// 调用完成后先关闭实例；最后关闭宿主拥有的执行器。
instance.shutdown(&Cancellation::default())?;
executor.shutdown(&Cancellation::default())?;
```

自建 Executor 的 `shutdown` 会先关闭关联实例，再停止 worker；进程级默认 Executor 不可关闭。

## 等待、取消与关闭

| 操作 | 含义 |
| --- | --- |
| `Execution::wait` | 推进入口并等待其返回值；传入的 Cancellation 被取消时也取消该 scope |
| `Execution::wait_scope` | 等待入口派生的后台工作；取消参数只停止本次等待 |
| `Execution::cancel` | 请求取消整个 scope，包括后台任务与挂起的宿主调用 |
| `Instance::shutdown` | 发起实例关闭并等待资源清理；取消参数只停止本次等待 |
| `InstanceOptions::cancellation` | 仅控制实例构造和根模块初始化 |

入口返回后，后台任务可能仍在运行。有限调用先 `wait` 再 `wait_scope`；常驻服务在业务结束时通知脚本
返回或显式取消。跨线程取消使用同一个 `Cancellation` 的克隆，取消后用新令牌等待 scope 收敛。

停机顺序为：停止新调用和补丁，通知脚本退出，等待入口及 scope，关闭实例，最后关闭共享 Host、连接
和 backend。最后一个 Instance 被 drop 会发起清理，需要确认完成时显式等待 `shutdown`。
调试暂停须先恢复。正常返回执行 defer；取消不保证执行 defer，也不证明不配合取消的宿主 I/O 已停止。

反复调用、热更新和停机的完整流程见[长期实例示例](examples/long_running.rs)。

自建事件循环可调用 `Execution::poll_steps`；它返回状态及本次实际步数。
`Pending` 时通过 `instance.wake()` 等待事件，`Paused` 时由调试器恢复，
`Completed` 后读取 `result()`。读取 wake epoch 应在推进前完成，避免遗漏唤醒。

## 在 Tokio 中执行

VM 推进与 `wait` 是同步操作。启用 `rpc` 后，`rpc::VmExecutor` 可将它们放入
Tokio blocking pool，并限制并发作业。应用复用 executor，容量满时处理 `resource_exhausted`。

```toml
[dependencies]
mini-go = { version = "=<snapshot-version>", features = ["rpc"] }
tokio = { version = "1", features = ["rt-multi-thread", "macros", "time"] }
```

`run` 接收同步闭包并返回 Future。应用将 `Cancellation` 的克隆传入闭包；异步期限到达时取消原令牌，
然后继续等待 Future，让闭包完成实例关闭。丢弃 Future 不会停止已经启动的工作，关闭 Tokio runtime 前
必须等待 VM 与宿主服务清理完成。

## 宿主能力

将实现 `ffi::Bridge` 的共享宿主放入 `InstanceOptions.bridge`。
纯计算可以不配置 Bridge，`clock` 与 `entropy` 可用于确定性测试。
Bridge 的 `open`/`start`、Clock 与 Entropy 回调可能从执行线程调用，必须线程安全、
快速返回，且不得同步重入同一实例。阻塞 I/O 应提交到宿主拥有的有界执行器，并通过
Completion 异步交付；关闭时等待该工作结束。`stdlib-host` 的 blocking pool 提供这种边界。
启用 `stdlib-host` 后可通过 `HostBuilder::memory` 装配 console、环境与内存文件系统；
实例只拥有自身的 FFI 会话，共享 Host 由应用关闭。

自定义 RPC 服务使用 [Rust RPC API](https://github.com/d7z-team/mini-go/blob/main/RPC.md#rust-api)；完整装配见
[RPC VM 示例](https://github.com/d7z-team/mini-go/blob/main/playground/runtime-rust/tests/rpc_vm.rs)和
[标准库宿主示例](https://github.com/d7z-team/mini-go/blob/main/playground/runtime-rust/tests/stdlib_host.rs)。

## 断点、变量与单步

源码调试需要与执行镜像匹配的独立 `ProgramSymbols`。使用 Go `compiler.Prepare` 的
`Symbols: true` 或 `LanguageService::prepare` 获取镜像及符号，再调用 `Program::with_symbols`。
普通 `runtime-blocks` 示例仅用于执行；源码断点需要另外提供符号。

给 Program 附加符号后，通过 `instance.debugger()` 设置源码断点、读取栈和 bindings，并用
`StepMode::{Continue, Into, Over, Out, Instruction}` 恢复执行；`pause()` 请求暂停。module、file 必须与
符号路径一致，源码行从 1 开始。恢复执行后帧与变量引用失效。完整 DAP 服务见
[语言工具](#本地源码与编译器工具)。

## 热更新

应用先编译新镜像，再加载为 Program。下面的函数在现有共享实例上准备并提交补丁：

```rust
use mini_go::{Instance, LoadOptions, PatchResult, Program, RuntimeError};
use std::sync::Arc;

fn replace_program(instance: &Instance, image: &[u8]) -> Result<PatchResult, RuntimeError> {
    let next = Arc::new(Program::load(image, LoadOptions::default())?);
    let plan = instance.prepare_patch(next)?;
    instance.apply_patch(plan)
}
```

需要源码调试时，先给新 Program 附加新符号。准备失败保留旧实例状态；未提交的 plan 直接 drop。
`apply_patch` 消耗 plan，提交失败后需重新准备；遇到 `busy` 时先让出执行，再准备和提交。
已有帧、defer 与闭包保留旧 revision，新命名调用使用当前 revision，兼容 globals 保持状态。
需要改变状态契约时创建新实例并由应用迁移数据。FFI 会话与共享 Host 不随补丁重建。
宿主应结束旧循环并替换保存的闭包，以释放旧 revision。
RPC 服务替换和资源关闭见[RPC 指南](https://github.com/d7z-team/mini-go/blob/main/RPC.md#服务替换与关闭)。

## 本地源码与编译器工具

启用 `compiler` feature；发布的 crate 已包含匹配的编译器镜像：

```toml
[dependencies]
mini-go = { version = "=<snapshot-version>", features = ["compiler"] }
```

源码 checkout 的镜像准备见[生成与派生物](https://github.com/d7z-team/mini-go/blob/main/DEVELOPMENT.md#生成与派生物)。
通过 `language::read_directory` 读取本地文件树，或构造 `SourceTree` 提供内存文件；模块身份由 `module_path` 声明。
应用与库都传给 `LanguageService::sources`，由编译器执行统一的包发现和资源规则：

```rust
use mini_go::{LanguageService, CompilerSession, language::read_directory};
use mini_go::{error::RuntimeError, ffi::Cancellation};

async fn open_sources(root: &std::path::Path) -> Result<LanguageService, RuntimeError> {
    let cancel = Cancellation::default();
    let tree = read_directory("app", root, &cancel).await?;
    let mut language = LanguageService { session: CompilerSession::bundled().await? };
    let packages = language.sources(&[tree], &cancel).await?;
    language.open(serde_json::json!({"Root": "app", "Packages": packages.packages}), &cancel).await?;
    Ok(language)
}
```

调用方完成使用后执行 `language.close().await`。额外模块同样作为 SourceTree 提供，
compiler 按 import 选择依赖。装配错误保留当前工作区，`Editable` 可授权编辑额外包。

### 预算与生命周期

编译器在 VM 中执行，默认不限累计 steps，guest heap 上限为 128 MiB。
频繁编辑时复用会话；较大工作区可通过 `CompilerOptions` 调整执行和加载预算：

```rust
use mini_go::{CompilerOptions, CompilerSession};
use std::time::Duration;

async fn compiler(image: &[u8]) -> Result<CompilerSession, mini_go::RuntimeError> {
    let mut options = CompilerOptions::default();
    options.limits.max_heap_bytes = 256 << 20;
    options.load.max_image_bytes = 96 << 20;
    CompilerSession::new_with_options(image, options, Duration::from_secs(60)).await
}
```

编译入口的 `max_steps = 0` 使用编译默认值；正数设置预算，-1 不限累计步数。
配置在恢复和升级时保持不变；步数放开后，统计、调度、取消和请求期限仍生效。

原生异步 API 需要启用时间支持的 Tokio runtime。`bundled()` 使用分发镜像，`new(image)` 使用指定镜像，
默认期限均为 30 秒；`new_with_timeout`、`call_with_timeout` 和 `upgrade_with_timeout` 可覆盖期限。
每次操作的初始化或恢复计入同一期限，请求 `Deadline` 可进一步缩短预算。

取消最多给予 guest 2 秒清理时间；丢弃调用 future 会丢弃未确认状态。
恢复只重建成功交付的输入，旧 snapshot 失效。`upgrade` 在候选恢复并分析成功后切换，失败保留原会话；
成功结果中的 `cleanup_error` 表示旧 owner 清理异常，新会话仍有效。

自建事件循环的同步推进接口见
[编译会话驱动](https://github.com/d7z-team/mini-go/blob/main/DEVELOPMENT.md#编译会话驱动)。

### stdio 服务

`mini-go-tools lsp` 接收 compiler 镜像和准备好的工作区 JSON；`mini-go-tools dap` 接收 DAP launch 请求，
可加载镜像或从 workspace 构建。启用 `language-server` 后，原生应用通过
`LanguageServer::serve(input, output)` 或 `DebugSession::serve(input, output)` 接入 Tokio 流。
仓库内构建和测试命令见
[开发指南](https://github.com/d7z-team/mini-go/blob/main/DEVELOPMENT.md#rust-验证)。
