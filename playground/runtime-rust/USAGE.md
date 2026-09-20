# Rust 原生使用指南

依赖安装、镜像生成和 feature 选择见 [README](README.md)。本文面向原生 Rust 应用；
浏览器与 Node.js 使用 [TypeScript SDK](runtime-wasm/README.md)。

[调用](#加载调用与限制) · [生命周期](#等待取消与关闭) · [Tokio](#在-tokio-中执行) ·
[宿主](#宿主能力) · [调试](#断点变量与单步) · [热更新](#热更新) · [语言工具](#本地源码与编译器工具)

示例使用 `mini_go` 根路径导出的共享 `Instance`，它可以克隆并跨线程协调执行。
`instance::Instance` 是需要独占可变访问的底层 VM；只有自建调度器时才直接使用它。
`Program` 可跨实例共享，每个实例拥有独立 globals、执行状态和 FFI 会话。

## 加载、调用与限制

`Program::load` 接收当前工具链的原始 JSON 镜像字节。gzip 镜像可先用
`loader::DecodedImage::decode_gzip` 解码，再交给 `Program::prepare`。
不要先通过其他语言的浮点 JSON 数值模型转码镜像。

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
cargo run -- /path/to/mini-go/playground/runtime-rust/examples/blocks/arithmetic.json
```

使用 `Limits { ..Default::default() }` 覆盖需要的限制。`LoadOptions` 约束加载，
`Limits` 约束执行；两者分别配置。错误通过 `RuntimeError.code`、`path`、`message` 返回。
共享实例正被其他操作占用时可能返回 `busy`，调用方应让出执行后重试。

`HostValue` 是宿主输入，`HostSnapshot` 是独立拥有数据的输出，可保留别名和循环。
常用输入可由 `HostValue::int` 等构造；复杂值见 [snapshot 模块](src/snapshot.rs)。
共享实例通过 `stats()` 与 `heap_stats()` 观测 VM 费用和 arena；这些数值不等于进程 RSS。

### 原生执行器与并行度

`InstanceOptions.parallelism` 限制同一实例同时执行的 guest task，零值采用 1。原生 runtime
使用进程级有界执行器，task 只占用一个有限 quantum，不对应固定系统线程；即使执行器只有
一个 worker，spawn、等待和多个实例也能继续推进。

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
# Ok::<(), mini_go::RuntimeError>(())
```

自建 Executor 的 `shutdown` 会请求关闭仍关联的实例并等待其清理，然后停止 worker；开始关闭后
不再接受新实例。进程级默认 Executor 不可关闭。不要从 VM 宿主回调中同步关闭执行器或调用
同一实例，这类重入返回 `busy`。

## 等待、取消与关闭

| 操作 | 含义 |
| --- | --- |
| `Execution::wait` | 推进入口并等待其返回值；传入的 Cancellation 被取消时也取消该 scope |
| `Execution::wait_scope` | 等待入口派生的后台工作；取消参数只停止本次等待 |
| `Execution::cancel` | 请求取消整个 scope，包括后台任务与挂起的宿主调用 |
| `Instance::shutdown` | 发起实例关闭并等待资源清理；取消参数只停止本次等待 |
| `InstanceOptions::cancellation` | 仅控制实例构造和根模块初始化 |

入口返回后，后台任务可能仍在运行。短任务可先 `wait` 再 `wait_scope`；常驻服务应根据
业务生命周期保留 scope，在退出时显式取消。执行取消不保证立即打断不配合取消的宿主操作。

取消执行后，调用 `execution.wait_scope(&Cancellation::default())` 等待 scope 收敛，并处理返回的错误。

跨线程停止 `wait` 时，克隆同一个 `Cancellation` 并调用 `cancel()`。
清理阶段使用新的 Cancellation，避免已取消的令牌让关闭等待立即返回。
先关闭实例，再关闭应用拥有的 Host、连接和 backend。最后一个 Instance 被 drop 会发起清理；
需要确认释放完成时，应显式等待 `shutdown`。

正常停机由宿主停止提交新入口与补丁，通知长循环通过业务控制通道返回，
等待相关执行的 `wait` 和 `wait_scope`，再 `shutdown`。超期时直接关闭可取消剩余工作。
调试暂停需显式恢复；外部 driver 持续 `drive`，底层 `instance::Instance` 由调用方持续 poll，
直到相关工作结束或关闭清理完成。
正常返回执行 defer；取消不会补跑 guest defer，也不证明宿主 I/O 已停止。

自建事件循环可调用 `Execution::poll_steps`；它返回状态及本次实际步数。
`Pending` 时通过 `instance.wake()` 等待事件，`Paused` 时由调试器恢复，
`Completed` 后读取 `result()`。读取 wake epoch 应在推进前完成，避免遗漏唤醒。

## 在 Tokio 中执行

VM 推进与 `wait` 是同步操作。启用 `rpc` 后，`rpc::VmExecutor` 可将它们放入
Tokio blocking pool，并限制并发作业。复用 executor，容量满时处理 `resource_exhausted`；
不要为每次请求创建一个 executor 来绕过容量限制。

```toml
[dependencies]
mini-go = { path = "/path/to/mini-go/playground/runtime-rust", features = ["rpc"] }
tokio = { version = "1", features = ["rt-multi-thread", "macros", "time"] }
```

以下完整程序等待算术调用，超过两秒则请求取消，然后仍等待 blocking 作业完成清理：

```rust
use mini_go::{
    HostValue, InstanceOptions, LoadOptions, Program, RuntimeError,
    ffi::Cancellation, rpc::VmExecutor,
};
use std::{sync::Arc, time::Duration};

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error>> {
    let path = std::env::args().nth(1).ok_or("expected image path")?;
    let image = std::fs::read(path)?;
    let executor = VmExecutor::new(tokio::runtime::Handle::current(), 2)?;
    let cancellation = Cancellation::default();
    let worker_cancel = cancellation.clone();
    let work = executor.run(move || {
        let program = Arc::new(Program::load(&image, LoadOptions::default())?);
        let instance = program.instantiate(InstanceOptions {
            cancellation: worker_cancel.clone(), ..Default::default()
        })?;
        let outcome = (|| {
            let call = instance.start_host("default", &[HostValue::int(10)])?;
            call.wait(&worker_cancel)
        })();
        let closed = instance.shutdown(&Cancellation::default());
        let result = outcome?;
        closed?;
        Ok::<_, RuntimeError>(result)
    });
    tokio::pin!(work);
    let outcome = tokio::select! {
        result = &mut work => result,
        _ = tokio::time::sleep(Duration::from_secs(2)) => {
            cancellation.cancel();
            work.await
        }
    };
    println!("{:?}", outcome??.roots);
    Ok(())
}
```

丢弃 `run` 的 Future 不会停止已启动的 blocking 工作；应用必须传递取消并等待清理。
时限是协作式的，宿主调用不响应取消时清理可能超过两秒。关闭 Tokio runtime 前等待 VM 和宿主服务关闭。

## 宿主能力

将实现 `ffi::Bridge` 的共享宿主放入 `InstanceOptions.bridge`。
纯计算可以不配置 Bridge，`clock` 与 `entropy` 可用于确定性测试。
Bridge 的 `open`/`start`、Clock 与 Entropy 回调可能从执行线程调用，必须线程安全、
快速返回，且不得同步重入同一实例。阻塞 I/O 应提交到宿主拥有的有界执行器，并通过
Completion 异步交付；关闭时等待该工作结束。`stdlib-host` 的 blocking pool 提供这种边界。
启用 `stdlib-host` 后可通过 `HostBuilder::memory` 装配 console、环境与内存文件系统；
实例只拥有自身的 FFI 会话，共享 Host 由应用关闭。

自定义 RPC 服务使用 [Rust RPC API](../../RPC.md#rust-api)；
完整宿主装配见 [RPC VM 示例](tests/rpc_vm.rs)和[标准库宿主示例](tests/stdlib_host.rs)。

## 断点、变量与单步

源码调试需要与执行镜像匹配的独立 `ProgramSymbols`。使用 Go `compiler.Prepare` 的
`Symbols: true` 或 Rust tooling 的 build/prepare 获取镜像及符号，再调用 `Program::with_symbols`。
普通 `runtime-blocks` 示例仅用于执行；源码断点需要另外提供符号。

以下函数接收加载后的 Program、符号 JSON 和一个源码断点，打印暂停帧的变量，再继续执行。
需额外依赖 `serde_json = "1"`。`module`、`file` 必须与符号内路径一致，`line` 从 1 开始。

```rust
use mini_go::{
    HostValue, InstanceOptions, Program, SnapshotLimits, ffi::Cancellation,
    instance::debug::StepMode,
};
use std::sync::Arc;

fn debug_call(
    program: Program, symbols: &[u8], entry: &str, arguments: &[HostValue],
    module: &str, file: &str, line: i64,
) -> Result<(), Box<dyn std::error::Error>> {
    let program = Arc::new(program.with_symbols(serde_json::from_slice(symbols)?)?);
    let instance = program.instantiate(InstanceOptions::default())?;
    let outcome = (|| -> Result<(), Box<dyn std::error::Error>> {
        let debugger = instance.debugger();
        if debugger.set_breakpoints(module, file, &[line])?.is_empty() {
            return Err("no executable breakpoint at this location".into());
        }
        let call = instance.start_host(entry, arguments)?;
        loop {
            match call.wait(&Cancellation::default()) {
                Ok(result) => { println!("{:?}", result.roots); break; }
                Err(error) if error.code == "paused" => {
                    for frame in debugger.stack()? {
                        println!("{:?}", debugger.bindings(&frame.reference, SnapshotLimits::default())?);
                    }
                    debugger.set_breakpoints(module, file, &[])?;
                    debugger.resume(StepMode::Continue)?;
                }
                Err(error) => return Err(error.into()),
            }
        }
        Ok(())
    })();
    let closed = instance.shutdown(&Cancellation::default());
    outcome?;
    closed?;
    Ok(())
}
```

将 Continue 换为 Into、Over、Out 或 Instruction 可执行相应单步；`pause()` 请求暂停。
恢复执行后帧与变量引用失效，需要重新获取。完整 DAP 服务见 [语言工具](#本地源码与编译器工具)。

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
已有 RPC 结果和资源继续属于原会话，服务替换与关闭见 [RPC 指南](../../RPC.md#热更新与关闭)。

### 长期宿主

实例可反复承载有限调用，工作结束后按[生命周期约定](#等待取消与关闭)等待和关闭。
`Limits::max_steps` 为 i64：0 或默认值表示 1 亿步，`UNLIMITED_STEPS`（-1）不限累计步数，
正数设置有限预算，其他负数无效。推进批次和热更新不重置预算；无限模式仍保留取消、内存及任务限制。
业务进度由宿主持久化；内存与版本存活的观测方法见[开发指南](../../DEVELOPMENT.md#缓存与性能)。

[有限宿主循环示例](examples/long_running.rs)接收两个兼容的算术镜像，交替调用和更新，
演示无限步数配置下的有限调用、scope 等待、热更新与关闭。
两个镜像的 `default` 入口均接收一个整数，镜像内容应不同：

```bash
cargo run --manifest-path playground/runtime-rust/Cargo.toml --example long_running -- \
  /path/to/arithmetic-v1.json /path/to/arithmetic-v2.json 10
```

## 本地源码与编译器工具

额外依赖 workspace 中的 `mini-go-tooling`。通过 `sources::read_directory`
读取本地文件树，或构造 `SourceTree` 提供内存文件；模块身份由 `module_path` 声明。
应用与库都传给 `LanguageService::sources`，由编译器执行统一的包发现和资源规则：

```rust
use mini_go_tooling::{language::LanguageService, session::CompilerSession, sources::read_directory};
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

调用方完成使用后执行 `language.close().await`。文件的 Data 是 base64 字节，
保留二进制资源；装配错误不会替换当前工作区。额外模块同样作为 SourceTree 提供，
compiler 按 import 选择参与编译的包。

### 语言服务与 stdio

`CompilerSession::bundled().await` 使用分发镜像。取消传入编译器 context；会话恢复使用已确认的输入，
`upgrade` 准备成功后才切换实例。工作区替换保留打开的缓冲区，`Editable` 可授权编辑额外包。

在仓库根目录启动工具：

```bash
cargo run --release --manifest-path playground/runtime-rust/Cargo.toml \
  -p mini-go-tooling --bin mini-go-tools -- lsp \
  playground/runtime-rust/tooling/assets/compiler.json.gz testdata/language/workspace.json
cargo run --release --manifest-path playground/runtime-rust/Cargo.toml \
  -p mini-go-tooling --bin mini-go-tools -- dap
```

LSP 接收准备好的工作区 JSON；DAP launch 接收 `image` 及可选的 `symbols`、`sources`、`entry`，
也可通过 `workspace` 和 `build` 编译源码。原生应用使用 `server::Server::serve(input, output)`
接入 Tokio 流，连接结束时由服务关闭会话和 IO 任务。
