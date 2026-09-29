# Tag 自动发布工作流设计

## 目标

当仓库收到以 `v` 开头的 tag 时，由 GitHub Actions 自动完成二进制构建、容器镜像发布和 GitHub Release 发布。

## 触发规则

- 仅响应 `push` 的 `v*` tag，例如 `v1.2.3`。
- 工作流使用 tag 指向的提交构建，GitHub Release 也使用同一个 tag。

## 发布产物

- `ewelink-lan-ctl_<tag>_linux_amd64.tar.gz`
- `ewelink-lan-ctl_<tag>_windows_amd64.zip`
- `checksums.txt`，包含上述压缩包的 SHA-256 校验和
- `ghcr.io/<owner>/<repository>` 容器镜像，同时支持 `linux/amd64` 和 `linux/arm64`

容器镜像始终生成原始 tag 和 `latest` 标签。符合语义化版本格式的 tag（例如 `v1.2.3`）还会生成 `1.2.3` 和 `1.2` 标签。

## 权限和实现约束

- 使用工作流自带的 `GITHUB_TOKEN`，无需配置额外 token。
- `contents: write` 用于创建 Release，`packages: write` 用于推送 GHCR 镜像。
- GitHub Actions 安装 Go Task，并通过 `task release VERSION=<tag>` 统一构建发布包。
- Go 二进制使用 `CGO_ENABLED=0`、`-trimpath` 和去除调试信息的链接参数构建。
- 发布流程直接执行跨平台构建，不运行自动化测试。
