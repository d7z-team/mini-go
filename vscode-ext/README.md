# Mini-Go VS Code 扩展

扩展提供 `.mgo` 文件识别、语法高亮，并通过 `mini-go lsp` 提供诊断、跳转、重命名和格式化。
`.mrpc` 文件由语言服务器的工作区文件监视器同步。

## 安装

按[开发指南](../DEVELOPMENT.md#编辑器扩展)构建 VSIX 后安装：

```bash
code --install-extension /tmp/mini-go.vsix
```

扩展不捆绑 compiler 或语言服务器二进制。安装 `mini-go` 后，将 `mini-go.lsp.path` 设为可执行文件的
绝对路径；留空时从 VS Code 进程的 `PATH` 查找。命令安装方式见[快速开始](../README_zh.md#快速开始)。

## 配置

在工作区或用户设置中配置：

| 设置 | 默认值 | 说明 |
| --- | --- | --- |
| `mini-go.lsp.path` | 空 | `mini-go` 可执行文件路径；为空时使用 `PATH` |
| `mini-go.module` | `app` | 应用源码的逻辑导入前缀 |
| `mini-go.sources` | `[]` | 额外源码目录，每项为 `{"module":"rules","directory":"../rules"}`；目录相对工作区根 |

打开 `.mgo` 文件后扩展自动启动语言服务器。修改上述配置会重启服务；也可以执行
`Mini-Go: Restart Language Server` 手动重启。

## 调试

支持 DAP 的编辑器可以使用 stdio 命令 `mini-go dap`。launch 参数、源码目录和调试符号要求见
[使用指南](../USAGE.md#编辑器与调试)。

## 开发

扩展测试、打包和 VSIX 验证统一从仓库根执行，见[开发指南](../DEVELOPMENT.md#编辑器扩展)。
