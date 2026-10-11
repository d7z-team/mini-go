# Mini-Go TypeScript Runtime

该包在 Browser 和 Node.js 中执行 Mini-Go bytecode，并提供源码工具、调试和 MRPC。VM 与生命周期管理
使用 TypeScript；源码编译在独立 Worker 中运行 Go compiler WASM。

```bash
npm install @d7z-team/mini-go
```

| 入口 | 职责 |
| --- | --- |
| `@d7z-team/mini-go` | 执行 bytecode、热更新和值转换 |
| `@d7z-team/mini-go/rpc` | 连接独立 WebSocket MRPC Endpoint |
| `@d7z-team/mini-go/tools` | 源码构建、语言服务和调试 |

## 执行镜像

`createRuntime` 为实例创建独立 Worker。输入镜像在发送前复制，调用方继续拥有原 buffer。

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

`result` 在入口返回时完成；`settled` 等待所属 task、timer 和 FFI 调用收敛。`cancel()` 取消整个
execution scope，`close()` 停止接纳工作并等待清理。只有宿主无法继续等待时才使用 `terminate()`。

64 位 guest 整数映射为 `bigint`，bytes 映射为 `Uint8Array`。返回的 `Snapshot` 独立拥有数据，并通过
`objects` 与 `HostAddress` 保留别名和循环。

### 浏览器部署

完整部署包内 `dist/`，保留入口、`workers/` 和 `compiler/` 的相对结构：

```bash
cp -R node_modules/@d7z-team/mini-go/dist public/mini-go
```

```ts
import { createRuntime } from "/mini-go/index.browser.js";

const image = new Uint8Array(await (await fetch("/program.json.gz")).arrayBuffer());
const runtime = await createRuntime(image);
```

部署位置变化时通过对应 API 的 `workerUrl` 覆盖 Worker 地址；语言工具还可用 `compilerUrl` 指定
`compiler/mini-go-compiler.wasm`。HTTP 服务需正确提供 `.wasm` MIME 类型并允许 Worker 与网络连接。

## 宿主能力与热更新

`provider` 接收 route、payload 和取消 signal：

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

浏览器可通过 `providerModule` 在 Worker 内加载宿主实现。`runtime.patch(nextImage)` 先校验完整候选，
再在安全点原子发布；失败保持当前程序。旧帧、闭包和 defer 继续使用原 revision，新命名调用进入新版本。

`limits` 控制步数、帧、task、FFI、序列和 guest 逻辑内存。`UNLIMITED_STEPS` 只放开累计步数。
`stats()` 返回 guest 统计，不代表 JavaScript heap 或进程 RSS。

## 编译器与调试

`@d7z-team/mini-go/tools` 使用独立 Compiler Worker。会话串行处理请求并限制队列、输入和期限；取消或
超时会重建 Worker，并只恢复宿主已经确认的状态。

```ts
import { createLanguageService } from "@d7z-team/mini-go/tools";

const service = await createLanguageService({ timeoutMs: 30_000 });
try {
  await service.open({
    Root: "example",
    Packages: [{
      Namespace: "app",
      ModulePath: "example",
      Files: [{ Path: "main.mgo", Text: "package main\nfunc main() int { return 42 }" }],
    }],
  });
  const analysis = await service.analyze();
  const build = await service.prepare({
    Symbols: true,
    EntryPoints: [{ Name: "default", ModulePath: "example", Function: "main" }],
  });
  console.log(analysis.Diagnostics, build.Image, build.Symbols);
} finally {
  await service.dispose();
}
```

`update` 提交文档变化，`query` 执行语言查询，`sources` 解析源码树。`upgrade` 只在候选 Worker 完成
恢复和分析后切换。`createDebugSession` 创建自有 runtime，`attachDebugSession` 连接现有 runtime；
调试符号必须与镜像匹配。

## 独立 RPC

`@d7z-team/mini-go/rpc` 无需执行镜像。它在 Worker 中连接 WebSocket，可绑定服务或发布 TypeScript
provider；`.mrpc` 生成的 codec、client 和 provider 适配器直接使用该入口。

```ts
import { connectRPC } from "@d7z-team/mini-go/rpc";
import { GreeterClient } from "./generated/greeter.js";

const connection = await connectRPC("wss://example.test/rpc");
try {
  const greeter = await GreeterClient.bind(connection);
  try {
    console.log(await greeter.hello({ name: "Mini-Go" }));
  } finally {
    await greeter.close();
  }
} finally {
  await connection.close();
}
```

resource 属于创建它的 binding，应显式关闭。连接 `close()` 等待本地资源和 publication 清理；
`timeoutMs` 与 `AbortSignal` 只约束所属操作。生成和跨语言语义见
[RPC 指南](../../RPC.md#typescript--javascript-api)。

## 项目结构与开发

| 路径 | 职责 |
| --- | --- |
| `src/vm/` | bytecode VM |
| `src/runtime/` | runtime Worker 与宿主桥 |
| `src/compiler/` | Go compiler WASM 会话 |
| `src/rpc/` | 独立 RPC Endpoint |
| `src/platform/`、`src/workers/` | Browser/Node 适配和 Worker 入口 |

完整构建从仓库根执行：

```bash
make deps artifacts build-typescript
npm exec --prefix playground/runtime-typescript -- playwright install chromium firefox
make lint-typescript test-typescript pack-typescript
```

维护流程见[开发指南](../../DEVELOPMENT.md#typescript-runtime-与-go-compiler-wasm)。

## 许可证

TypeScript 包使用 [MIT License](LICENSE)。Go compiler WASM 所含 Go 源码适用 [Go License](LICENSE-Go)。
