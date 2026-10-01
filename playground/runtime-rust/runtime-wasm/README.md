# Mini-Go WebAssembly SDK

在浏览器和 Node.js 中运行 Mini-Go、编译源码或接入 MRPC。分发包包含 ESM、类型声明、Worker、WASM
和匹配的编译器镜像，无需安装 Rust 工具链或额外运行时 npm 依赖。

| 入口 | 用途 |
| --- | --- |
| `@d7z-team/mini-go` | 执行预编译镜像，每个实例拥有一个 Worker |
| `@d7z-team/mini-go/tools` | 编译源码、语言查询与调试会话 |
| `@d7z-team/mini-go/rpc` | 独立调用或提供 RPC 服务 |

按任务查阅：[Node.js](#安装与-nodejs-接入) · [浏览器部署](#浏览器部署) ·
[执行与关闭](#执行取消与关闭) · [热更新](#热更新) · [宿主与 RPC](#宿主能力与-rpc) ·
[值与限制](#值与资源限制) · [语言工具](#编译器与语言工具) · [调试](#调试会话)。

## 安装与 Node.js 接入

从 npm 的 `git` 标签安装当前提交快照：

```sh
npm install @d7z-team/mini-go@git
```

入口自动选择 Node 或浏览器实现，也可显式导入 `/node`、`/browser`。Node.js 要求 22.18 或更新版本。

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

镜像生成见[预编译示例](https://github.com/d7z-team/mini-go/blob/main/DEVELOPMENT.md#生成预编译示例)。
镜像与 runtime 应来自同一工具链；JSON 或 gzip 镜像按原始字节传入，以保留 64 位整数精度。

## 浏览器部署

直接使用浏览器模块时，将完整 `dist` 复制到应用静态资源目录：

```sh
cp -R node_modules/@d7z-team/mini-go/dist public/runtime-wasm
```

将 Node.js 示例中的导入和文件读取替换为：

```js
import { MiniGo, values } from "/runtime-wasm/browser.js";

const image = await (await fetch("/arithmetic.json")).arrayBuffer();
```

随后通过 `MiniGo.create(image)` 创建实例，调用与关闭方式和 Node.js 相同。

打包器也可导入 `@d7z-team/mini-go/browser`。独立部署 Worker 时，使用
`workerUrl` 指向 `browser-worker.js`，`wasmUrl` 指向 WASM；
包导出 `@d7z-team/mini-go/worker` 和 `@d7z-team/mini-go/wasm` 可定位资源。
Node 的 `wasmUrl` 还接受 `file:` URL。

通过 HTTP(S) 提供资源，WASM 使用 `application/wasm` 类型。
CSP 应允许脚本、Worker、WASM 编译及所需网络连接。runtime 资源与镜像作为同一版本部署。
静态 gzip 镜像可直接以字节传入 SDK；HTTP `Content-Encoding: gzip` 由浏览器解码，调用方无需再手动解压。

## 执行、取消与关闭

`MiniGo.create(image, options)` 等待初始化完成；`start(entry, arguments, options)` 返回执行对象：

| 成员 | 含义 |
| --- | --- |
| `result` | 入口返回结果 |
| `settled` | 该 scope 的后台工作全部结束 |
| `cancel()` | 取消执行 |

调用通过有界队列进入，每次只有一个前台入口。`timeoutMs` 是有限的非负毫秒数，包含排队和后台 scope，
缺省不设期限；`signal` 可取消构造或执行。每个 Worker 内单线程协作推进，需要并行时由应用创建多个实例。

设置调用期限时，同时处理入口结果与 scope 清理结果：

```ts
const call = vm.start("default", [values.int(10n)], { timeoutMs: 2_000 });
const [result, settled] = await Promise.allSettled([call.result, call.settled]);
if (result.status === "rejected") throw result.reason;
if (settled.status === "rejected") throw settled.reason;
console.log(result.value.roots);
```

主动取消使用 `call.cancel()` 或 `signal`。正常停机先停止新调用和补丁，通知脚本退出，等待入口与 scope，
最后 `close()`。`close()` 取消剩余执行并等待宿主清理，其 signal 只限制本次等待；
`terminate()` 直接销毁 Worker，不保证异步清理完成。

### 热更新

调用 `await vm.patch(nextImage)`，其中 `nextImage` 是新 JSON 镜像的 `Uint8Array` 原始字节，
gzip 镜像需先解压。
已有帧与闭包保留旧版本，新的命名调用使用新版本。此接口只提交执行镜像；调试目标需要从新构建结果
装配对应符号。patch 成功后，已有 FFI 会话、调用和资源继续由原 owner 管理。

结束旧循环、替换保存的闭包可释放旧版本。业务状态的持久化与恢复由宿主负责。

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

用 `capabilities` 声明实际提供的能力。标准库能力遵循 MRPC 契约；需要浏览器用户激活的操作在页面处理。

设置 `rpcUrl` 可通过 WebSocket 连接 Mini-Go peer，支持双向调用、契约校验、取消与资源清理。
断线使调用和资源失效，需要应用重新连接；认证遵循所在环境的 WebSocket 规则。
`rpcOptions` 可覆盖租期、接纳期限和调用上限，配置及重试语义见
[RPC 指南](https://github.com/d7z-team/mini-go/blob/main/RPC.md#错误与超时)。

### 独立 TypeScript RPC

独立 RPC 接入无需 Mini-Go 镜像；生成 binding、客户端和 Provider 的完整示例见
[RPC 指南](https://github.com/d7z-team/mini-go/blob/main/RPC.md#typescript--javascript-api)。

直接部署 `dist` 时导入 `browser-rpc.js`，并通过 bundler 或 import map 解析生成代码中的
`@d7z-team/mini-go/rpc`。自定义部署可覆盖 Worker 与 WASM URL，资源目录要求与[浏览器部署](#浏览器部署)相同。

## 值与资源限制

HostValue 使用显式类型；64 位整数及浮点位模式使用 BigInt，字节使用 Uint8Array。
`values.int/bool/string/bytes` 构造常用输入。输入会复制，返回快照独立拥有数据并保留别名和循环。

通过 `maxSteps`、`maxHeapBytes` 和 `maxPendingCalls` 限制执行。
镜像加载、FFI 和快照也受各自预算约束。guest 计费不代表全部 JS、网络或进程内存。

每个 scope 默认 1 亿步，热更新不重置预算。`maxSteps` 缺省或 0 使用默认值，
`UNLIMITED_STEPS`（-1）放开累计步数，正数设置预算；超出安全整数的值使用 bigint，最大为有符号 64 位整数。
无限模式仍保留取消和其他限额。

`stats()` 提供 VM 步数、分配与 WASM 线性内存观测。

## 编译器与语言工具

tools 入口在独立 Worker 中加载分发的编译器，管理源码、资源配置和会话恢复：

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

编译器在 WASM VM 中执行，首次分析包含完整 import 依赖。频繁编辑时复用会话并用 `update` 提交变化；
较大工作区可能触发预算或请求期限。仅执行脚本的应用可由 Go 预先编译镜像。
自行通过 runtime 加载 compiler 镜像时设置 `workload: "compiler"`。
性能测量见[开发指南](https://github.com/d7z-team/mini-go/blob/main/DEVELOPMENT.md#缓存与性能)。

默认只允许编辑根工作区，`Editable` 可授权额外源码根。请求支持 AbortSignal；取消、Worker 丢失或
未确认请求失败后，下一次请求从最后成功交付的输入重建，旧 snapshot 失效。

tools 请求默认 30 秒，可通过 `createLanguageService(image, { timeoutMs })` 配置；
`timeoutMs` 是 1 到 2,147,483,647 的整数毫秒，作用于初始化、请求和升级。
排队和恢复计入同一预算，请求中的 `Deadline` 可进一步缩短期限；取消最多给予 2 秒清理时间。队列、输入和响应均有界，
超出限额的请求会失败；宿主还需为编译器镜像和 VM 保留内存。

`upgrade(image, signal)` 在候选会话恢复并分析成功后切换，失败保留当前会话。
返回的 `cleanupError` 表示切换后旧 Worker 的清理异常，新会话仍有效。使用结束后调用 `dispose()` 释放 Worker。

### 调试会话

DebugSession 与原生工具共用 Rust DAP 适配器。`createDebugSession(image, {symbols, sources})` 创建并拥有
目标实例；`DebugSession.bind(vm)` 借用已有实例，实例仍由调用方关闭。`DebugSession.fromBuild`
可直接接入 `language.prepare` 的结果。通过 `request` 发送 DAP 命令，并用 `watch` 消费事件；恢复执行后
帧引用失效。

## 源码构建

从 Git checkout 构建、测试及生成本地安装包，见
[WASM 开发流程](https://github.com/d7z-team/mini-go/blob/main/DEVELOPMENT.md#wasm-与-typescript)；
上游发布见[发布流程](https://github.com/d7z-team/mini-go/blob/main/DEVELOPMENT.md#rust-与-npm-发布)。
