# Mini-Go WebAssembly SDK

使用同一套 TypeScript API，在浏览器和 Node.js 中运行预编译的 Mini-Go 镜像。
每个实例拥有一个 Worker。分发包包含 ESM、类型声明、Worker、WASM 及绑定，
使用者无需安装 Rust 工具链或额外运行时 npm 依赖。

按任务查阅：[Node.js](#安装与-nodejs-接入) · [浏览器部署](#浏览器部署) ·
[执行与关闭](#执行取消与关闭) · [热更新](#热更新) · [宿主与 RPC](#宿主能力与-rpc) ·
[值与限制](#值与资源限制) · [语言工具](#编译器与语言工具) · [调试](#调试会话)。

## 安装与 Node.js 接入

安装 `make runtime-wasm-pack` 产出的 tarball：

```sh
npm install ./d7z-team-mini-go-0.1.0.tgz
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
[Rust 快速开始](https://github.com/d7z-team/mini-go/blob/main/playground/runtime-rust/README.md#快速开始)。
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

`timeoutMs` 接受 0 到 `Number.MAX_SAFE_INTEGER` 的有限毫秒数（允许小数），缺省不设期限。
0 异步请求取消；入口返回后，期限仍覆盖后台 scope，直到 `settled`。
期限以当前进程的单调时钟计量，跨重启的业务期限由宿主持久化。取消请求不等于清理完成。

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

正常停机时，宿主先停止提交新调用和补丁，通过业务通道通知常驻脚本退出，
等待已提交调用的 `result` 和 `settled`，再关闭实例。入口有 result 但尚未 settled 时仍需等待；
暂停的调试目标需恢复。超过宿主设定的退出期限时，可取消执行或直接 close。

`close()` 停止接收工作、取消执行并等待宿主清理；其 signal 只限制当前调用方的等待，
之后仍可再次等待关闭。并发关闭等待共享最终结果，取消某个等待不影响其他等待者。
`terminate()` 直接销毁 Worker，不保证异步清理完成。
Node Worker 意外退出会拒绝所有未完成操作，正常使用应显式关闭实例。

`pause`、`resume`、`stack`、`bindings`、`breakpoints`、`stats` 和 `patch`
提供调试、观测与热更新。源码定位通过 `symbols` 传入原始符号 JSON；
恢复执行后帧引用失效，补丁准备失败保留当前版本。

### 热更新

调用 `await vm.patch(nextImage)`，其中 `nextImage` 是新 JSON 镜像的 `Uint8Array` 原始字节，
gzip 镜像需先解压。
已有帧与闭包保留旧版本，新的命名调用使用新版本。此接口只提交执行镜像；
需要装配新版本源码与符号的调试目标时，从新构建结果创建 DebugSession。
patch 成功表示代码已经提交，已有 FFI 会话、调用和资源仍由原 owner 管理。
若 Worker 在提交后退出而回复未送达，等待失败不能证明补丁未提交；应用应处理实例失效。

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

也可使用 `providerModule` 在 Worker 内加载 provider：浏览器使用 HTTP(S) 绝对 URL，
Node 使用 `file:` URL。模块满足导出的 WorkerProvider 类型，提供 `call` 和可选 `close`。
`call` 返回字节或 `{payload, consumed?, discard?}`；所有权回调允许异步，
每个结果只执行一次接收或丢弃决策，包括取消后的迟到结果。
provider 的清理或调用一直不结束时，会延迟实例关闭。

`capabilities` 声明实际提供的能力。标准库能力遵循 MRPC 契约；
存储、console、HTTP 与 DOM 行为由应用实现，需要浏览器用户激活的操作在页面处理。

设置 `rpcUrl` 可通过 WebSocket 连接匹配的 Mini-Go peer，支持双向客户端/服务端调用、
契约校验、取消与资源清理。断线使调用失败，应用决定何时创建新实例。
浏览器 cookie/Origin 与 Node 内建 WebSocket 的认证方式遵循各自平台规则。
RPC 续租由 Worker 内的 Rust Endpoint 负责。暂停脚本不会暂停网络维护；
整个 Worker 被冻结或同步加载、补丁阻塞事件循环超过租期时，远端可以撤销授权。
恢复后旧资源不能重新生效，应建立新会话；SDK 不重放业务调用。
`rpcOptions` 可设置 `leaseTtlMs`、`admissionTimeoutMs` 和 `maxCallDurationMs`，单位为正整数毫秒；
不传时使用 [Endpoint 默认配置](https://github.com/d7z-team/mini-go/blob/main/RPC.md#错误与超时)。
租期由接收方授予，客户端与服务端分别配置自己的授权窗口。

## 值与资源限制

HostValue 使用显式类型与数据描述；64 位整数及浮点位模式使用 BigInt，
字节使用 Uint8Array。`values.int/bool/string/bytes` 构造常用输入。
快照保留别名和循环；输入会复制，返回快照独立拥有数据。
运行时和语言服务接收的镜像、符号字节也会复制；传入 Buffer 或偏移视图时只复制可见范围，
向 Worker 转移数据不会分离调用方的缓冲区。

通过 `maxSteps`、`maxHeapBytes` 和 `maxPendingCalls` 限制执行。
镜像加载、FFI 和快照也受各自预算约束。guest 计费不代表全部 JS、网络或进程内存。

每个 scope 默认 1 亿步，热更新不重置预算。`maxSteps` 缺省或 0 使用默认值，
`UNLIMITED_STEPS`（-1）不限累计步数，正数设置有限预算。
number 必须是安全整数；更大的值使用 bigint，最大为 9223372036854775807n。
无限模式仍保留分片推进、取消和其他限额。

`stats()` 提供 VM 计费、分配与 WASM 线性内存观测；累计分配与当前驻留内存含义不同。

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

`update` 按序提交文档编辑，`analyze` 发布快照，`query` 查询该快照。
`prepare` 返回规范编码的镜像、符号 JSON 和对应源码。
文件树可通过 `language.sources(trees, signal)` 装配为 Packages，再传给 `open`：

```typescript
const { Packages } = await language.sources([{
  ModulePath: "app",
  Files: [{ Path: "main.mgo", Data: btoa("package main\nfunc main() {}\n") }],
}]);
await language.open({ Root: "app", Packages });
```

Data 是文件字节的 base64，支持二进制资源；非 ASCII 文本先编码为 UTF-8。
URI 可选，用于编辑器显示位置。每个树声明导入前缀，宿主提供完整文件集合；
装配本身不改变当前工作区，失败后原会话继续有效。

请求支持 AbortSignal。正常取消保留会话；Worker 故障后重建已确认输入，旧快照句柄失效。
`upgrade(image, signal)` 准备新编译器并恢复输入后切换，失败保留当前会话。
默认只允许编辑根工作区；`Editable` 可授权额外源码根。

### 调试会话

DebugSession 与原生工具共用 Rust DAP 适配器。绑定 runtime 或从构建结果创建目标后，
通过 `request` 发送 DAP 命令，使用 `events` 或 `watch` 消费事件，最后 `dispose`。
provider 可用 `output(category, text)` 交付调试输出。

已有编译镜像时，从 tools 入口导入 `createDebugSession`，传入原始镜像、符号字节和源码映射。
以下片段假定 `image`、`symbols`、源码文本 `sourceText` 和模块路径 `modulePath` 已加载，
符号中的文件为 `main.mgo`。映射的 key 是 DAP 使用的路径，value 中的 module/path 对应符号：

```ts
import { createDebugSession } from "@d7z-team/mini-go/tools";

const debug = await createDebugSession(image, {
  symbols,
  sources: { "/app/main.mgo": { module: modulePath, path: "main.mgo", text: sourceText } },
});
try {
  await debug.request("setBreakpoints", {
    source: { path: "/app/main.mgo" }, breakpoints: [{ line: 2 }],
  });
  await debug.request("configurationDone");
  for await (const event of debug.watch()) {
    console.log(event);
    if (event.event === "stopped" || event.event === "terminated") break;
  }
} finally {
  await debug.dispose();
}
```

`createDebugSession` 拥有目标实例，dispose 会关闭它；`DebugSession.bind(vm)` 默认借用实例，
dispose 结束调试后仍需由调用方关闭 vm。使用 `DebugSession.fromBuild(MiniGo.create, build)`
可直接接入 `language.prepare` 的结果，自动装配镜像、符号和源码。

## 源码构建

从 Git checkout 运行 `make runtime-wasm-pack` 生成可安装 tarball。
构建工具链、浏览器依赖、无 RPC 构建与验证命令统一见
[开发指南](https://github.com/d7z-team/mini-go/blob/main/DEVELOPMENT.md#wasm-与-typescript)。
