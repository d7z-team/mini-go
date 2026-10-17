# Mini-Go TypeScript Runtime

这个包在浏览器和 Node.js 中执行 Mini-Go bytecode，并提供源码工具、调试和 MRPC 客户端。VM 与实例
生命周期由 TypeScript 管理；源码编译在独立 Worker 中运行 Go compiler WASM。

Node.js 部署使用 Node.js 24.11 或更高版本。浏览器入口由 ESM 打包器提供 Worker 和 WASM 资源。

## 安装与入口

```bash
npm install @d7z-team/mini-go
```

| 入口                      | 用途                                    |
| ------------------------- | --------------------------------------- |
| `@d7z-team/mini-go`       | 加载并执行 bytecode、传递宿主值和热更新 |
| `@d7z-team/mini-go/tools` | Go compiler WASM、语言服务和调试        |
| `@d7z-team/mini-go/rpc`   | 独立 WebSocket MRPC 连接                |

## 执行 bytecode

`createRuntime` 为每个实例创建独立 Worker。输入镜像会在发送前复制，调用方仍拥有原来的
`Uint8Array` 或 `ArrayBuffer`。

```ts
import { readFile } from "node:fs/promises";
import { createRuntime, values } from "@d7z-team/mini-go";

const image = await readFile("program.json.gz");
const runtime = await createRuntime(image, {
  limits: { maxSteps: 1_000_000 },
});

try {
  const execution = runtime.start("default", [values.int(10n)]);
  const snapshot = await execution.result;
  await execution.settled;
  console.log(snapshot.roots);
} finally {
  await runtime.close();
}
```

`execution.result` 在入口返回或失败时完成；`execution.settled` 会继续等待该 scope 的 task、timer 和
宿主调用收敛。`execution.cancel()` 取消整个 scope。`runtime.close()` 停止接纳新工作并等待清理，只有
宿主无法继续等待时才使用 `runtime.terminate()`。

64 位 guest 整数使用 `bigint`，bytes 使用 `Uint8Array`。返回的 `Snapshot` 独立拥有数据，并通过
`objects` 和 `HostAddress` 保留对象别名与循环。

调用参数中的宿主值可以共享子对象，但不能包含循环引用或已 detach 的 `ArrayBuffer`；调用方应在发送前
保持传入的 typed array 有效。

## 浏览器部署

应用通过 Vite 或其他 ESM 打包器导入公开入口：

```ts
import { createRuntime } from "@d7z-team/mini-go";

const image = new Uint8Array(await (await fetch("/program.json.gz")).arrayBuffer());
const runtime = await createRuntime(image);
```

构建产物包含浏览器入口、运行时 Worker、compiler Worker 和 `mini-go-compiler.wasm`。部署这些资源时，
可通过 `workerUrl` 覆盖运行时 Worker，通过 tools 的 `compilerUrl` 覆盖 compiler WASM 地址。HTTP 服务需
为 `.wasm` 返回正确的 MIME 类型，并允许 Worker 访问所需资源。

## 宿主能力与热更新

宿主 provider 接收路由、二进制 payload 和取消信号：

```ts
const runtime = await createRuntime(image, {
  capabilities: ["example.echo"],
  async provider({ route, payload, signal }) {
    signal.throwIfAborted();
    if (route !== "example.echo") throw new Error(`unknown route: ${route}`);
    return Uint8Array.from(payload);
  },
});
```

也可以用 `providerModule` 让 Worker 加载导出 `call` 和可选 `close` 的宿主模块。provider 应响应
`AbortSignal`，并返回 `Uint8Array`。

`runtime.patch(nextImage)` 会先完整校验候选镜像，再在安全点原子发布；准备失败时当前程序继续运行。
已有 frame、closure 和 defer 继续使用原 revision，普通命名调用使用当前 revision，
`revision_local` 调用跟随 caller frame 的 revision。

`limits.maxGuestBytes` 限制已占用与预留的逻辑存储，`limits.maxResidentBytes` 限制受管页、frame
和运行时元数据的实际接纳容量。`UNLIMITED_STEPS` 只放开累计步数，任务、FFI 和内存限额仍有效。

`stats()` 返回累计账本，不触发回收。`occupiedBytes` 包含尚未回收的垃圾，`reservedBytes` 是
未提交额度，`lastMajorReachableBytes` 是上次 major 的可达快照。分配与回收的完整统计口径见
[开发指南](../../DEVELOPMENT.md#缓存与性能)，存储和生命周期见[架构](../../ARCHITECTURE.md#执行模型与状态所有权)。

## 源码工具与调试

`@d7z-team/mini-go/tools` 使用独立的 compiler Worker：

```ts
import { createLanguageService } from "@d7z-team/mini-go/tools";

const service = await createLanguageService({ timeoutMs: 30_000 });
try {
  await service.open({
    Root: "example",
    Packages: [
      {
        Namespace: "module:example",
        ModulePath: "example",
        Files: [{ Path: "answer.mgo", Text: "package example\nfunc Answer() int { return 42 }" }],
      },
    ],
  });
  const analysis = await service.analyze();
  const build = await service.prepare({
    Symbols: true,
    EntryPoints: [{ Name: "default", ModulePath: "example", Function: "Answer" }],
  });
  console.log(analysis.Diagnostics, build.Image, build.Symbols);
} finally {
  await service.dispose();
}
```

语言服务按顺序处理会话请求。取消或超时会回收当前 Worker，并只恢复已经确认的输入；`upgrade` 在候选
Worker 完成恢复和分析后再切换。`createDebugSession` 创建由 tools 管理的 runtime，
`attachDebugSession` 连接已有 runtime；调试符号必须与镜像匹配。

## 独立 RPC

通过 `@d7z-team/mini-go/rpc` 的 `connectRPC` 在 Worker 中连接 WebSocket，再绑定生成的客户端或发布
provider。连接 `close()` 等待本地资源和 publication 清理；resource 应显式关闭，操作级 `timeoutMs`
与 `AbortSignal` 只约束所属操作。契约生成和完整调用示例见 [RPC 指南](../../RPC.md#typescript--javascript-api)。

## 开发与许可

构建、测试、打包和性能诊断统一从仓库根执行，见[开发指南](../../DEVELOPMENT.md#typescript-runtime-与-go-compiler-wasm)。

TypeScript 包使用 [MIT License](LICENSE)。随包分发的 Go compiler WASM 所含 Go 源码适用
[Go License](LICENSE-Go)。
