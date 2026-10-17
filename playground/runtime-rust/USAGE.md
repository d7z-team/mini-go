# Rust 原生使用指南

本文介绍 crate 根路径导出的执行 API。安装、feature 选择和镜像生成见
[README](README.md)；通用维护流程见根[开发指南](../../DEVELOPMENT.md#rust-验证)。浏览器与 Node.js 使用
[TypeScript runtime](../runtime-typescript/README.md)。

根路径的 `mini_go::Instance` 是线程安全的共享执行句柄（底层类型为
`execution::SharedInstance`），适合异步宿主和多个调用方共享。`mini_go::instance::Instance` 是单一执行
owner 使用的同步 VM；需要手动推进、精确控制 `poll_steps` 或自行管理宿主循环时使用它。两者共享同一套
`Program`、FFI 和资源语义，但生命周期由各自的 owner 管理。

## 加载镜像并执行

`Program::load` 接收当前工具链生成的未压缩 JSON 镜像；gzip 镜像先用
`loader::DecodedImage::decode_gzip` 解码，再交给 `Program::prepare`。`Program` 可以跨实例共享，每个实例
拥有独立的 heap、FFI session 和 revision。

```rust
use mini_go::{ffi::Cancellation, HostValue, InstanceOptions, Limits, LoadOptions, Program};
use std::sync::Arc;

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let path = std::env::args().nth(1).ok_or("expected image path")?;
    let program = Arc::new(Program::load(&std::fs::read(path)?, LoadOptions::default())?);
    let instance = program.instantiate(InstanceOptions {
        limits: Limits {
            max_steps: 1_000_000,
            max_guest_bytes: 64 << 20,
            max_resident_bytes: 256 << 20,
            ..Default::default()
        },
        ..Default::default()
    })?;
    let cancellation = Cancellation::default();

    let execution = instance.start("default", &[HostValue::int(10)])?;
    let snapshot = execution.wait(&cancellation)?;
    execution.wait_scope(&cancellation)?;
    instance.shutdown(&cancellation)?;
    println!("{:?}", snapshot.roots);
    Ok(())
}
```

`LoadOptions` 限制镜像大小、包数量和类型节点数量。`Limits` 提供步数、调用深度、task、frame 缓存、guest
存储、驻留内存以及宿主边界限制；零值使用默认值，`UNLIMITED_STEPS` 只取消累计步数限制。`start` 借用
输入并复制到实例；返回的 `HostSnapshot` 独立拥有结果并保留 guest 别名，实例关闭后仍可读取。失败通过
`RuntimeError` 返回。

## 等待、取消与关闭

| API | 语义 |
| --- | --- |
| `Execution::wait` | 推进入口并等待入口结果；取消参数会取消 scope |
| `Execution::wait_scope` | 等待派生 task 和宿主调用收敛；取消参数只停止本次等待 |
| `Execution::cancel` | 取消整个 scope，包括后台 task 和宿主调用 |
| `Instance::shutdown` | 停止新调用并等待实例资源关闭 |

入口返回后仍可能有后台 task。正常返回会执行 defer，取消不保证执行 defer。关闭时应先等待 scope，再关闭
实例以及共享的 `Executor`、host、Endpoint 或 backend。取消等待不等于已停止工作；已经启动的同步执行仍需
由 owner 完成清理。

## 手动推进与并行执行

同步 owner 可以使用 `mini_go::instance::Instance::poll_steps` 精确推进指定数量的 guest 指令，并通过
`Instance::wake` 等待异步事件。读取 wake epoch 应先于推进，避免遗漏唤醒。长期运行示例见
[long_running.rs](examples/long_running.rs)。

共享句柄通过 `InstanceOptions::parallelism` 设置实例内同时推进的 task；多个实例可以共享由宿主创建的
`Executor`，并在所有实例关闭后关闭 executor：

```rust
use mini_go::{ffi::Cancellation, Executor, InstanceOptions};

let executor = Executor::new(4)?;
let instance = program.instantiate(InstanceOptions {
    parallelism: 4,
    executor: Some(executor.clone()),
    ..Default::default()
})?;
instance.shutdown(&Cancellation::default())?;
executor.shutdown(&Cancellation::default())?;
```

启用 `rpc` 后，`rpc::VmExecutor` 将同步 VM 工作放入有界 blocking pool 并返回 Future。调用方应等待 Future
完成清理，不能把丢弃 Future 当作停止已启动工作的信号。

## 宿主能力与 FFI

将实现 `ffi::Bridge` 的共享宿主放入 `InstanceOptions::bridge`。Bridge 回调应快速返回，不能同步重入
同一实例；阻塞 I/O 交给宿主的有界执行器，再通过 `Completion` 交付结果。

启用 `stdlib-host` 后，`HostBuilder` 可以装配 console、environment 和内存文件系统。实例只拥有自己的
FFI session；共享 host 应在所有实例关闭后再关闭。RPC 装配见根
[RPC 指南](../../RPC.md#rust)。

## 热更新

先加载完整候选 `Program`，再准备和提交补丁：

```rust
use mini_go::{Instance, LoadOptions, PatchResult, Program, RuntimeError};
use std::sync::Arc;

fn replace_program(instance: &Instance, image: &[u8]) -> Result<PatchResult, RuntimeError> {
    let next = Arc::new(Program::load(image, LoadOptions::default())?);
    let plan = instance.prepare_patch(next)?;
    instance.apply_patch(plan)
}
```

准备失败时原程序继续运行。提交遇到 `busy` 或 stale plan 时重新准备。已有 frame、defer 和 closure 继续
引用旧 revision；新的命名调用进入新 revision。identity 和 layout 匹配的 globals 保持状态。
`retained_revisions` 查看仍保留的版本，`collect_garbage` 回收无引用版本。状态结构变化时创建新实例，
由应用转移数据。

## 调试与 compiler session

需要源码调试时，将与镜像匹配的 `ProgramSymbols` 传给 `Program::with_symbols`，再通过
`instance.debugger()` 设置断点、读取栈和恢复执行。恢复后，旧的 frame 和变量引用失效。

启用 `compiler` feature 可使用随 crate 分发的匹配 compiler 镜像；`LanguageService` 负责源码装配、分析、
构建和会话恢复，完成后调用 `language.close().await`。启用 `language-server` 后，
`LanguageServer::serve` 和 `DebugSession::serve` 可接入 Tokio 流。

构建与 feature 验证见根[开发指南](../../DEVELOPMENT.md#rust-验证)。
