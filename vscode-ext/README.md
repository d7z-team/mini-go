# Mini-Go VS Code 扩展

扩展为 Mini-Go 源码提供文件识别、语法高亮和语言服务器连接。打开 `.mgo` 文件后，它会启动
`mini-go lsp`，提供诊断、补全、跳转、重命名和格式化；工作区中的 `.mgo` 与 `.mrpc` 文件变化会同步给语言
服务器。

## 安装

扩展不包含 compiler 或语言服务器。先安装 `mini-go` 命令，再从 VSIX 安装扩展：

```bash
code --install-extension /tmp/mini-go.vsix
```

从仓库构建 VSIX：

```bash
make test-vscode
```

该命令在 `vscode-ext/` 安装 lockfile 中的依赖、运行扩展测试，并生成 `/tmp/mini-go.vsix`。

## 语言服务器路径

默认情况下，扩展从 VS Code 进程的 `PATH` 查找 `mini-go`。如果命令不在 `PATH` 中，在设置中指定
`mini-go.lsp.path` 的绝对路径：

```json
{
  "mini-go.lsp.path": "/path/to/mini-go"
}
```

语言服务器使用工作区根目录作为项目根，并通过 `mini-go.module` 和 `mini-go.sources` 接收源码配置。

| 设置 | 默认值 | 说明 |
| --- | --- | --- |
| `mini-go.lsp.path` | `""` | `mini-go` 可执行文件的绝对路径；为空时使用 `PATH` |
| `mini-go.module` | `"app"` | 应用源码的逻辑导入前缀 |
| `mini-go.sources` | `[]` | 额外源码目录；每项为 `{"module":"rules","directory":"../rules"}`，目录相对工作区根 |

修改这些设置会重启语言服务器。也可以执行命令面板中的 `Mini-Go: Restart Language Server` 手动重启。
扩展会在启动失败时显示错误；修正可执行文件路径后重新启动即可。

扩展只负责语言服务器客户端和语法高亮。DAP 调试使用 `mini-go dap` 的 stdio 接口，并按[使用指南](../USAGE.md#编辑器与调试)
配置调试器。

更多构建和验证流程见[开发指南](../DEVELOPMENT.md#编辑器扩展)。
