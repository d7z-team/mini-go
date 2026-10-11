# Rust 原生使用指南

依赖安装、镜像生成和 feature 选择见 [README](README.md)。本文使用 `mini_go` 根路径导出的共享
`Instance`；需要自建调度器时才直接使用底层 `instance::Instance`。Browser 与 Node.js 使用
[TypeScript 运行时](https://github.com/d7z-team/mini-go/blob/main/playground/runtime-typescript/README.md)。

## 加载、调用与限制

`Program::load` 接收当前工具链版本的 JSON 字节；gzip 镜像先通过
`loader::DecodedImage::decode_gzip` 解码。Program 可跨实例共享，每个实例拥有独立状态和 FFI Session。

下面的完整程序加载镜像并调用 `default` 入口：

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
    println!("{:?}", result.roots);
    Ok(())
}
```

`LoadOptions` 限制镜像加载，`Limits` 控制步数、帧、task 和 guest 逻辑内存。零值使用默认限制；
`UNLIMITED_STEPS` 只放开累计步数。`HostValue` 创建宿主输入，`HostSnapshot` 独立拥有输出并保留别名。
错误通过 `RuntimeError` 返回，统计与 profile 由 Instance/Execution 提供。

### 原生执行器与并行度

`InstanceOptions.parallelism` 限制实例内同时推进的 task。需要独立容量时，将共享 `Executor` 传给多个实例；
先关闭实例，最后关闭宿主创建的 Executor。进程级默认 Executor 不需要应用管理。

```rust
use mini_go::{Executor, InstanceOptions, ffi::Cancellation};

let executor = Executor::new(4)?;
let instance = program.instantiate(InstanceOptions {
    parallelism: 4,
    executor: Some(executor.clone()),
    ..Default::default()
})?;
instance.shutdown(&Cancellation::default())?;
executor.shutdown(&Cancellation::default())?;
```

## 等待、取消与关闭

| 操作 | 语义 |
| --- | --- |
| `Execution::wait` | 推进入口并等待结果；取消参数也取消 scope |
| `Execution::wait_scope` | 等待派生工作；取消参数只停止本次等待 |
| `Execution::cancel` | 取消整个 scope，包括后台 task 和宿主调用 |
| `Instance::shutdown` | 发起关闭并等待实例资源收敛 |

入口返回后仍可能存在后台任务。停机时停止新调用和补丁，通知常驻脚本退出，等待 scope，关闭实例，
再关闭共享 Host、Endpoint、Executor 和 backend。正常返回会执行 defer，取消不保证执行 defer。

自建事件循环使用 `poll_steps`，并通过 `instance.wake()` 等待异步事件。读取 wake epoch 应先于推进，
避免遗漏唤醒。长期运行示例见 [long_running.rs](examples/long_running.rs)。

## 在 Tokio 中执行

VM 推进是同步操作。启用 `rpc` 后，`rpc::VmExecutor` 将工作放入有界 blocking pool，并返回 Future。
异步期限到达时取消传给同步闭包的 `Cancellation`，随后继续等待 Future 完成清理；丢弃 Future 不会停止
已经启动的工作。关闭 Tokio runtime 前必须等待 VM 和宿主服务终态。

```toml
[dependencies]
mini-go = { version = "=<snapshot-version>", features = ["rpc"] }
tokio = { version = "1", features = ["rt-multi-thread", "macros", "time"] }
```

## 宿主能力

将实现 `ffi::Bridge` 的共享宿主放入 `InstanceOptions.bridge`。Bridge 回调必须线程安全、快速返回，且不能
同步重入同一实例；阻塞 I/O 交给宿主有界执行器，再通过 Completion 交付结果。

`stdlib-host` 的 `HostBuilder` 可装配 console、环境和内存文件系统。Instance 只拥有自己的 FFI Session，
共享 Host 由应用在所有实例关闭后关闭。RPC 装配见
[Rust RPC API](https://github.com/d7z-team/mini-go/blob/main/RPC.md#rust-api)。

## 断点、变量与单步

源码调试需要与镜像匹配的 `ProgramSymbols`。将符号传给 `Program::with_symbols` 后，通过
`instance.debugger()` 设置断点、读取栈和 binding，并用 `StepMode` 恢复。恢复执行后帧和变量引用失效。
完整 DAP 服务由 `language-server` feature 提供。

## 热更新

先加载完整候选 Program，再准备和提交补丁：

```rust
use mini_go::{Instance, LoadOptions, PatchResult, Program, RuntimeError};
use std::sync::Arc;

fn replace_program(instance: &Instance, image: &[u8]) -> Result<PatchResult, RuntimeError> {
    let next = Arc::new(Program::load(image, LoadOptions::default())?);
    let plan = instance.prepare_patch(next)?;
    instance.apply_patch(plan)
}
```

准备失败保持原状态；提交遇到 `busy` 或 stale plan 时重新准备。已有帧、defer 和闭包继续引用旧 revision，
新命名调用进入新 revision，兼容 globals 保持状态。改变状态契约时创建新实例并迁移数据。
服务替换见 [RPC 指南](https://github.com/d7z-team/mini-go/blob/main/RPC.md#服务替换与关闭)。

## 本地源码与编译器工具

启用 `compiler` feature 使用随 crate 分发的匹配编译器镜像：

```toml
[dependencies]
mini-go = { version = "=<snapshot-version>", features = ["compiler"] }
```

`language::read_directory` 读取本地源码，`LanguageService::sources` 应用与 Go 工具链一致的包发现规则：

```rust
use mini_go::{CompilerSession, LanguageService, language::read_directory};
use mini_go::{error::RuntimeError, ffi::Cancellation};

async fn open_sources(root: &std::path::Path) -> Result<LanguageService, RuntimeError> {
    let cancel = Cancellation::default();
    let tree = read_directory("app", root, &cancel).await?;
    let mut language = LanguageService { session: CompilerSession::bundled().await? };
    let packages = language.sources(&[tree], &cancel).await?;
    language.open(
        serde_json::json!({"Root": "app", "Packages": packages.packages}),
        &cancel,
    ).await?;
    Ok(language)
}
```

应用完成后调用 `language.close().await`。额外模块同样作为 `SourceTree` 提供，由 import 选择依赖。

### 预算与生命周期

`CompilerOptions` 配置 VM 与加载限制，请求期限由构造或调用 API 指定。配置在恢复和升级时保持一致；
恢复只重建已确认输入，旧 snapshot 失效。候选升级完成恢复和分析后才切换，失败保留当前会话。

### stdio 服务

启用 `language-server` 后，`LanguageServer::serve` 和 `DebugSession::serve` 接入 Tokio 流；分发的
`mini-go-tools lsp` / `dap` 提供 stdio 命令。构建与验证见
[开发指南](https://github.com/d7z-team/mini-go/blob/main/DEVELOPMENT.md#rust-验证)。
