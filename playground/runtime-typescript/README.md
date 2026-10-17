# Mini-Go TypeScript Runtime

这个包在浏览器和 Node.js 中执行 Mini-Go bytecode，并提供源码编译、调试和 MRPC 客户端。每个运行时实例
在独立 Worker 中执行；源码工具在另一个 Worker 中运行 Go compiler WASM。

Node.js 入口要求 Node.js 24.11 或更高版本。浏览器入口使用 ESM Worker，应用应通过 Vite 等打包器构建。

## 安装与入口

```bash
npm install @d7z-team/mini-go
```

| 入口 | 用途 |
| --- | --- |
| `@d7z-team/mini-go` | 加载 bytecode、运行实例、宿主调用和热更新 |
| `@d7z-team/mini-go/tools` | Go compiler WASM、语言服务和调试会话 |
| `@d7z-team/mini-go/rpc` | 连接 WebSocket MRPC 服务 |

同一个公开入口会根据 Node.js 或浏览器的 ESM 条件选择对应实现。应用不需要直接导入 Worker 文件。

## 运行 bytecode

`createRuntime` 接收编译后的执行镜像，并为实例创建独立 Worker：

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

`execution.result` 在入口返回或失败时完成；`execution.settled` 等待该 scope 派生的 task、timer 和宿主调用
完成。`execution.cancel()` 取消整个 scope。`runtime.close()` 停止接纳新工作并等待 Worker 清理；只有调用方
无法继续等待时才使用 `runtime.terminate()`。

64 位 guest 整数使用 `bigint`，字节使用 `Uint8Array`。返回的 `Snapshot` 独立拥有数据，并用 `objects` 和
`HostAddress` 保留对象别名与循环。执行镜像、调用参数和补丁输入在 Worker 边界复制，调用方保留原始
buffer 的所有权。调用参数中的宿主值可以共享子对象，但不能包含循环引用或已 detach 的 `ArrayBuffer`。

## 浏览器部署

浏览器应用直接导入公开入口，镜像可以从静态资源加载：

```ts
import { createRuntime } from "@d7z-team/mini-go";

const response = await fetch("/program.json.gz");
const runtime = await createRuntime(new Uint8Array(await response.arrayBuffer()));
try {
  const execution = runtime.start("default");
  console.log(await execution.result);
  await execution.settled;
} finally {
  await runtime.close();
}
```

Vite 构建包会同时生成浏览器/Node.js 入口、运行时 Worker、compiler Worker、类型声明和
`mini-go-compiler.wasm` 资源。部署自有静态资源目录时，保持这些资源的相对关系；运行时可用 `workerUrl`
覆盖运行时 Worker，tools 可用 `workerUrl` 覆盖 compiler Worker、用 `compilerUrl` 覆盖 compiler WASM。
服务器应为 `.wasm` 返回正确的 MIME 类型，并允许 Worker 访问这些资源。

## 宿主能力与热更新

宿主 provider 接收路由、二进制 payload 和取消信号，并返回 `Uint8Array`：

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

也可以使用 `providerModule`，让运行时 Worker 加载导出 `call` 和可选 `close` 的模块。provider 应及时响应
`AbortSignal`，并在关闭时释放自己的资源。

`runtime.patch(nextImage)` 会先校验候选镜像，再在安全点原子发布。准备失败时当前程序继续运行；已有 frame、
closure 和 defer 保持原 revision，新启动的普通命名调用使用当前 revision，`revision_local` 调用跟随 caller
frame 的 revision。

`limits` 中的 `maxGuestBytes`、`maxResidentBytes`、`maxTasks` 和 `maxSteps` 可限制实例资源。需要放开累计步数
时使用导出的 `UNLIMITED_STEPS`；任务、宿主调用和内存限额仍然有效。`stats()` 用于查看实例状态和累计执行
统计，不会因为读取而触发回收。

## 源码工具与调试

`@d7z-team/mini-go/tools` 在独立 Worker 中运行 Go compiler WASM。语言服务请求按顺序处理，创建后应在使用完毕
调用 `dispose()`：

```ts
import { createLanguageService } from "@d7z-team/mini-go/tools";

const service = await createLanguageService({ timeoutMs: 30_000 });
try {
  await service.open({
    Root: "example",
    Packages: [{
      Namespace: "module:example",
      ModulePath: "example",
      Files: [{ Path: "answer.mgo", Text: "package example\nfunc Answer() int { return 42 }" }],
    }],
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

`createLanguageService` 支持 `workerUrl` 和 `compilerUrl` 覆盖 Worker 与 WASM 资源，也支持通过 `AbortSignal`
取消请求。`createDebugSession` 创建并拥有一个运行时，`attachDebugSession` 连接已有运行时；调试符号必须与
执行镜像匹配。取消或超时后，会话按已确认输入恢复 Worker；`upgrade` 在候选 Worker 恢复和身份校验成功后
才切换，升级准备失败保留当前会话。

## MRPC

`@d7z-team/mini-go/rpc` 的 `connectRPC(url, options)` 在 Worker 中建立 WebSocket 连接。生成的 TypeScript
binding 可以通过 `RPCConnection.bind` 使用远端服务，也可以通过 `publish` 提供服务。

64 位 MRPC 整数使用 `bigint`。`close()` 等待本地 binding、publication 和 resource 清理；使用过程中创建的
resource 应显式关闭。操作可以通过 `timeoutMs` 或 `AbortSignal` 取消，连接关闭等待只影响调用方的等待，不会
跳过已经开始的资源清理。

契约生成与完整调用示例见 [RPC 指南](../../RPC.md#typescript--javascript)。

## 许可证

项目使用 [MIT License](LICENSE)。随包分发的 Go compiler WASM 所含 Go 源码适用 [Go License](LICENSE-Go)。

构建、测试和发布流程见[开发指南](../../DEVELOPMENT.md#typescript-runtime-与-go-compiler-wasm)。
