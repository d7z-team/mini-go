# Mini-Go WebAssembly SDK

使用同一套 TypeScript API，在浏览器和 Node.js 中运行预编译的 Mini-Go 镜像或直接连接 MRPC 服务。
每个实例拥有一个 Worker。分发包包含 ESM、类型声明、Worker、WASM 及绑定，
使用者无需安装 Rust 工具链或额外运行时 npm 依赖。

按任务查阅：[Node.js](#安装与-nodejs-接入) · [浏览器部署](#浏览器部署) ·
[执行与关闭](#执行取消与关闭) · [热更新](#热更新) · [宿主与 RPC](#宿主能力与-rpc) ·
[值与限制](#值与资源限制) · [语言工具](#编译器与语言工具) · [调试](#调试会话)。

## 安装与 Node.js 接入

从 npm 的 `git` 标签安装当前提交快照：

```sh
npm install @d7z-team/mini-go@git
```

包名为 `@d7z-team/mini-go`，根入口按环境选择 Node 或浏览器实现；
也可显式导入 `/node`、`/browser`。Node.js 要求 22.18 或更新版本。

```ts
import { readFile } from "node:fs/promises";
import { MiniGo, values } from "@d7z-team/mini-go";

const vm = await MiniGo.create(await readFile("./arithmetic.json"));
try {
  const call = vm.start("default", [values.int(10n)]);
  console.log((await call.result).roots);
  await call.settled;
} finally {
  await vm.close();
}
```

在 Go 宿主上通过 `mini-go-dev runtime-blocks` 编译镜像，步骤见
[Rust 快速开始](https://github.com/d7z-team/go-mini/blob/main/playground/runtime-rust/README.md#快速开始)。
镜像与 runtime 应来自同一工具链。
JSON 或 gzip 镜像按原始字节传入，以保留 64 位整数精度。

## 浏览器部署

直接使用浏览器模块时，将完整 `dist` 复制到应用静态资源目录：

```sh
cp -R node_modules/@d7z-team/mini-go/dist public/runtime-wasm
```

```js
import { MiniGo, values } from "/runtime-wasm/browser.js";

const image = await (await fetch("/arithmetic.json")).arrayBuffer();
const vm = await MiniGo.create(image);
try {
  const call = vm.start("default", [values.int(10n)]);
  console.log((await call.result).roots);
  await call.settled;
} finally {
  await vm.close();
}
```

打包器也可导入 `@d7z-team/mini-go/browser`。独立部署 Worker 时，使用
`workerUrl` 指向 `browser-worker.js`，`wasmUrl` 指向 WASM；
包导出 `@d7z-team/mini-go/worker` 和 `@d7z-team/mini-go/wasm` 可定位资源。
保留完整分发目录。Node 的 `wasmUrl` 还接受 `file:` URL。

通过 HTTP(S) 提供资源，WASM 使用 `application/wasm` 类型。
CSP 应允许脚本、Worker、WASM 编译及所需网络连接。runtime 资源与镜像作为同一版本部署。

## 执行、取消与关闭

`MiniGo.create(image, options)` 等待初始化，包括 guest timer 和异步宿主调用。
`start(entry, arguments, options)` 返回执行对象：

| 成员 | 含义 |
| --- | --- |
| `result` | 入口返回结果 |
| `settled` | 该 scope 的后台工作全部结束 |
| `cancel()` | 取消执行 |

调用通过有界队列进入，每次只有一个前台入口。`timeoutMs` 包含排队时间，`signal` 可取消构造或执行。
每个 WASM 实例在自己的 Worker 中单线程协作推进；需要并行隔离时创建多个实例，由应用划分状态与请求。

`timeoutMs` 是有限的非负毫秒数，包含排队和后台 scope；缺省不设期限。

例如，在已创建的 `vm` 上限制一次调用的时间，并观察 scope 的最终清理状态：

```ts
const call = vm.start("default", [values.int(10n)], { timeoutMs: 2_000 });
const [result, settled] = await Promise.allSettled([call.result, call.settled]);
if (result.status === "rejected") throw result.reason;
if (settled.status === "rejected") throw settled.reason;
console.log(result.value.roots);
```

主动取消可调用 `call.cancel()`，或向 `start` 传入 `{ signal: controller.signal }`。
取消后的 Promise 可能拒绝，应同时处理 `result` 和 `settled`；实例仍由创建方在 finally 中关闭。

正常停机时，先停止提交新调用和补丁，通知常驻脚本退出，等待 `result` 和 `settled`，再关闭实例。

`close()` 停止接收工作、取消执行并等待宿主清理；signal 只限制本次等待，之后仍可再次等待终态。
`terminate()` 直接销毁 Worker，不保证异步清理完成。

### 热更新

调用 `await vm.patch(nextImage)`，其中 `nextImage` 是新 JSON 镜像的 `Uint8Array` 原始字节，
gzip 镜像需先解压。
已有帧与闭包保留旧版本，新的命名调用使用新版本。此接口只提交执行镜像；调试目标需要从新构建结果
装配对应符号。patch 成功后，已有 FFI 会话、调用和资源继续由原 owner 管理。

长期使用宜复用实例执行有限调用并等待 `settled`，结束旧循环或替换闭包以释放旧版本。
业务状态的持久化与恢复由宿主负责。

## 宿主能力与 RPC

时钟和随机熵使用平台实现。应用通过显式 provider 提供额外能力：

```ts
const vm = await MiniGo.create(image, {
  provider: async ({ route, payload, signal }) => {
    const response = await fetch(`https://host.example/${encodeURIComponent(route)}`, {
      method: "POST", body: Uint8Array.from(payload), signal,
    });
    if (!response.ok) throw new Error(`host HTTP ${response.status}`);
    return new Uint8Array(await response.arrayBuffer());
  },
});
```

也可使用 `providerModule` 在 Worker 内加载 provider：浏览器使用 HTTP(S) URL，Node 使用 `file:` URL。
模块实现导出的 `WorkerProvider` 类型，提供 `call` 和可选 `close`；实例关闭会等待 provider 清理。

`capabilities` 声明实际提供的能力。标准库能力遵循 MRPC 契约；
存储、console、HTTP 与 DOM 行为由应用实现，需要浏览器用户激活的操作在页面处理。

设置 `rpcUrl` 可通过 WebSocket 连接 Mini-Go peer，支持双向调用、契约校验、取消与资源清理。
断线会使调用和资源失效，SDK 不自动重连或重放；认证遵循浏览器或 Node 的 WebSocket 环境。
`rpcOptions` 可覆盖 Endpoint 的租期、接纳期限和服务端调用上限；默认值与长期调用语义见
[RPC 指南](https://github.com/d7z-team/go-mini/blob/main/RPC.md#错误与超时)。

### 独立 TypeScript RPC

JavaScript/TypeScript 自身调用或提供 MRPC 服务时，从 `/rpc` 入口建立独立连接，无需创建 `MiniGo`
或加载 Program：

```ts
import { RPC } from "@d7z-team/mini-go/rpc";
import { LaboratoryClient } from "./generated/service.js";

const connection = await RPC.connect("wss://example.test/rpc");
try {
  const client = await LaboratoryClient.bind(connection);
  try {
    const echoed = await client.echo(packet, { timeoutMs: 5_000 });
    console.log(echoed);
  } finally {
    await client.close();
  }
} finally {
  await connection.close();
}
```

binding 由 `mini-go rpc generate -ts-out generated/service.ts schema/service.mrpc` 生成，同一份源码可在
Browser 与 Node.js 中使用。直接部署 `dist` 时导入 `browser-rpc.js`，并通过 bundler 或 import map
解析 `@d7z-team/mini-go/rpc`；自定义部署可覆盖 Worker 与 WASM URL。

客户端、Provider、精确类型映射、超时、资源归属和关闭语义统一见
[RPC 指南](../../../RPC.md#typescript--javascript-api)。

## 值与资源限制

HostValue 使用显式类型；64 位整数及浮点位模式使用 BigInt，字节使用 Uint8Array。
`values.int/bool/string/bytes` 构造常用输入。输入会复制，返回快照独立拥有数据并保留别名和循环。

通过 `maxSteps`、`maxHeapBytes` 和 `maxPendingCalls` 限制执行。
镜像加载、FFI 和快照也受各自预算约束。guest 计费不代表全部 JS、网络或进程内存。

每个 scope 默认 1 亿步，热更新不重置预算。`maxSteps` 缺省或 0 使用默认值，
`UNLIMITED_STEPS`（-1）不限累计步数，正数设置有限预算。
number 必须是安全整数；更大的值使用 bigint，最大为 9223372036854775807n。
无限模式仍保留分片推进、取消和其他限额。

`stats()` 提供 VM 计费、分配与 WASM 线性内存观测。

语言查询和源码装配使用下方 tools 入口，由它加载匹配的编译器、选择预算并管理会话。
直接加载 compiler 镜像时设置 `workload: "compiler"`。

## 编译器与语言工具

`@d7z-team/mini-go/tools` 同时支持浏览器和 Node，在独立 Worker 中加载分发的编译器：

```ts
import { createLanguageService } from "@d7z-team/mini-go/tools";

const language = await createLanguageService();
try {
  await language.open({
    Root: "sample",
    Packages: [{ Namespace: "module:sample", ModulePath: "sample", Files: [
      { Path: "main.mgo", Text: "package sample\nfunc Answer() int { return 42 }\n" },
    ] }],
  });
  await language.analyze();
  const hover = await language.query("hover", {
    URI: "mini-go://sample/main.mgo", Position: { line: 1, character: 6 },
  });
  console.log(hover);
} finally {
  await language.dispose();
}
```

`update` 按序提交文档编辑，`analyze` 发布快照，`query` 查询该快照，`prepare` 返回镜像、符号和源码。
文件树可通过 `language.sources(trees, signal)` 装配为 Packages；Data 使用 base64 保存文件字节和二进制资源。

请求支持 AbortSignal。正常取消保留会话；Worker 故障后重建已确认输入，旧快照句柄失效。
`upgrade(image, signal)` 准备新编译器并恢复输入后切换，失败保留当前会话。
默认只允许编辑根工作区；`Editable` 可授权额外源码根。

### 调试会话

DebugSession 与原生工具共用 Rust DAP 适配器。`createDebugSession(image, {symbols, sources})` 创建并拥有
目标实例；`DebugSession.bind(vm)` 借用已有实例，实例仍由调用方关闭。`DebugSession.fromBuild`
可直接接入 `language.prepare` 的结果。通过 `request` 发送 DAP 命令，并用 `watch` 消费事件；恢复执行后
帧引用失效。

## 源码构建

从 Git checkout 运行 `make runtime-wasm-pack` 生成本地可安装 tarball；完整的 Rust/npm 发布产物使用
`make release-package release-verify` 在隔离 staging 中构建和验证。
构建工具链、浏览器依赖、无 RPC 构建与验证命令统一见
[开发指南](https://github.com/d7z-team/go-mini/blob/main/DEVELOPMENT.md#wasm-与-typescript)。
