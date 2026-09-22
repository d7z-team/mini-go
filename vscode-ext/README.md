# Mini-Go VS Code 扩展

提供 `.mgo` 文件识别、语法高亮，并启动 `mini-go lsp` 接入诊断、跳转、重命名和格式化服务。

## 安装与配置

按[开发指南](https://github.com/d7z-team/mini-go/blob/main/DEVELOPMENT.md#编辑器扩展)构建 VSIX 后安装：

```bash
code --install-extension /tmp/mini-go.vsix
```

也可在 VS Code 扩展视图中选择“从 VSIX 安装”，打开生成的文件。

安装扩展后，在 VS Code 设置中将 `mini-go.lsp.path` 设为已安装的 `mini-go` 可执行文件的绝对路径。
留空时从 VS Code 进程的 `PATH` 查找 `mini-go`；扩展不捆绑编译器二进制。命令安装方式见
[项目中文 README](https://github.com/d7z-team/mini-go/blob/main/README_zh.md#安装)。

`mini-go.module` 设置应用导入前缀（默认 `app`）；`mini-go.sources` 接受
`[{"module":"rules","directory":"../rules"}]`，相对工作区根装配其他源码。

打开 `.mgo` 文件时扩展自动启动语言服务。修改配置会重启服务，也可通过命令面板执行
`Mini-Go: Restart Language Server` 手动重启。

接入 DAP 的编辑器另行配置 stdio 命令 `mini-go dap`，launch 参数与工作区约定见
[使用指南](https://github.com/d7z-team/mini-go/blob/main/USAGE.md#编辑器与调试符号)。
