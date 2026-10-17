# Rust 原生使用指南

本文介绍 crate 根路径导出的共享 `Instance` 和常用执行 API。安装、feature 选择和镜像生成见
[README](README.md)；浏览器与 Node.js 使用
[TypeScript runtime](../runtime-typescript/README.md)。

## 加载镜像并执行

`Program::load` 接收当前工具链生成的 JSON 镜像；gzip 字节先通过 `loader::DecodedImage::decode_gzip`
解码，再用 `Program::prepare` 构造可执行程序。`Program` 可以跨实例共享；每个实例拥有独立的 heap、FFI
session 和 revision。

```rust
use mini_go::{
    ffi::Cancellation, HostValue, InstanceOptions, Limits, LoadOptions, Program,
};
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

`LoadOptions` 限制镜像加载，`Limits` 控制步数、调用深度、task、frame 缓存以及两类内存预算：
`max_guest_bytes` 是所有已发布 payload 与活动 owner 控制记录共享的逻辑 guest 预算，
`max_resident_bytes` 是 Rust 后端页、目录和 worker 元数据使用的物理驻留预算，`0` 表示不设驻留上限。
逻辑预算在 reserve-build-publish 成功时提交，构造失败或 owner 结束时立即回滚/释放；collection 不会重建这笔账。
`max_boundary_bytes`、`max_boundary_depth` 和 `max_boundary_elements` 限制宿主数据输入与调试边界；
内部 slice、map、channel 和聚合值使用存储预算，动态类型使用 `max_dynamic_types` 与
`max_dynamic_type_bytes` 元数据预算。
零值使用默认限制；`UNLIMITED_STEPS` 只放开累计步数。`start` 借用 `HostValue` 输入并将数据复制到实例，
`HostSnapshot` 独立拥有输出并保留别名，实例关闭后仍可读取。
错误通过 `RuntimeError` 返回。

启动入口或派生任务时，task 与首帧必须一起满足分配预算。接收 FFI 回复时若回收后 guest 存储预算仍不足，调用返回
`allocation_limit` 执行错误；宿主操作已经执行，运行时只重试回复物化，并在失败清理时释放回复。

## 取消、等待与关闭

| API | 语义 |
| --- | --- |
| `Execution::wait` | 推进入口并等待入口结果；取消参数会取消 scope |
| `Execution::wait_scope` | 等待派生 task 和宿主调用收敛；取消参数只停止本次等待 |
| `Execution::cancel` | 取消整个 scope，包括后台 task 和宿主调用 |
| `Instance::shutdown` | 停止新调用并等待实例资源关闭 |

入口返回后仍可能有后台工作。关闭时先停止新调用和补丁，等待 scope，再关闭实例以及共享的
`Executor`、host、Endpoint 或 backend。正常返回会执行 defer；取消不保证执行 defer。

## 并行执行与 Tokio

`InstanceOptions::parallelism` 控制实例内同时推进的 task。需要多个实例共享线程容量时，可以传入同一个
`Executor`；所有实例关闭后再关闭由宿主创建的 executor。

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

VM 推进本身是同步操作。启用 `rpc` 后，`rpc::VmExecutor` 会把工作放入有界 blocking pool 并返回
Future。异步期限到达时，取消会传递给同步闭包；调用方仍应等待 Future 完成清理，不能把丢弃 Future
当作停止已启动工作的信号。

需要自建事件循环时使用 `poll_steps`，并通过 `Instance::wake` 等待异步事件。读取 wake epoch 应先于
推进，避免遗漏唤醒。长期运行示例见
[long_running.rs](examples/long_running.rs)。

## 宿主能力与 FFI

将实现 `ffi::Bridge` 的共享宿主放入 `InstanceOptions::bridge`。Bridge 回调应快速返回，不能同步重入
同一实例；阻塞 I/O 交给宿主的有界执行器，再通过 `Completion` 交付结果。

启用 `stdlib-host` 后，`HostBuilder` 可以装配 console、环境和内存文件系统。实例只拥有自己的 FFI
session；共享 host 应在所有实例关闭后再关闭。RPC 装配见
[RPC 指南](../../RPC.md#rust-api)。

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

准备失败时原程序继续运行。提交遇到 `busy` 或 stale plan 时重新准备。已有 frame、defer 和 closure
继续引用旧 revision；新的命名调用进入新 revision。identity 和 layout 匹配的 globals 保持状态。
旧 revision 由实际存活的 code anchor 保留，在 major collection 后释放。`retained_revisions`
包含尚未回收的版本；需要立即回收无引用版本或取得最新保留集合时，调用 `collect_garbage()`。
如果状态结构变化，应创建新实例并显式转移数据。

## 调试与 compiler session

需要源码调试时，将与镜像匹配的 `ProgramSymbols` 传给 `Program::with_symbols`，再通过
`instance.debugger()` 设置断点、读取栈和恢复执行。恢复后，旧的 frame 和变量引用失效。
`language-server` feature 提供完整 DAP 服务。

启用 `compiler` feature 可使用随 crate 分发的匹配 compiler 镜像；`LanguageService` 负责源码装配、分析、
构建和会话恢复。服务完成后调用 `language.close().await`。启用 `language-server` 后，
`LanguageServer::serve` 和 `DebugSession::serve` 可接入 Tokio 流。

构建与 feature 验证见[开发指南](../../DEVELOPMENT.md#rust-验证)。
