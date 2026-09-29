# ewelink-lan-ctl

`ewelink-lan-ctl` 是一个小型的纯云端网关，用于从 Debian 服务器或 iPhone 快捷指令控制 eWeLink 单通道插座。它使用 eWeLink v2 云 API；尽管仓库名称如此，它并不会通过 LAN 发现或控制设备。

> [!IMPORTANT]
> 使用本服务前，所有者必须轮换项目设置期间泄露的 App Secret。请在 eWeLink 开发者控制台中创建替代值、使已泄露的值失效，并且只将替代值放入私有运行时环境文件。切勿提交 App Secret、OAuth token、授权码或生成的 `.env` 文件。

## 安全模型

整个服务特意不设身份认证。任何能访问端口 `33998` 的人都可以列出支持的设备、打开或关闭设备，以及调用 `/oauth/start` 和 `/callback` 将服务重新绑定到另一个 eWeLink 账号，并持久保存新的绑定关系。请将每个路由限制在受信任的 LAN 或 VPN 内，或者将整个服务置于经过身份认证的反向代理或同等访问控制之后。不要将端口 `33998` 直接发布到互联网。OAuth 的一次性 `state` 仅用于防止回调关联错误和重放；它不会对调用者进行身份认证，也不能阻止可访问服务的客户端启动新的账号绑定。

服务将 OAuth 凭据持久化在 `state.json` 中。请将该文件作为机密保护。设备元数据和设备状态不会被缓存。

## 配置

复制占位配置并填入轮换后的新凭据：

```bash
cp .env.example .env
chmod 600 .env
```

切勿重复使用已泄露的 App Secret。支持的变量如下：

| 变量 | 必填项/默认值 | 用途 |
| --- | --- | --- |
| `EWELINK_APP_ID` | 必填 | eWeLink 开发者应用 ID |
| `EWELINK_APP_SECRET` | 必填 | 轮换后的开发者 App Secret |
| `EWELINK_CALLBACK_URL` | `http://127.0.0.1:33998/callback` | 已注册的 OAuth 重定向 URL；必须与开发者应用完全匹配 |
| `EWELINK_LISTEN_ADDR` | `:33998` | HTTP 监听地址 |
| `EWELINK_STATE_FILE` | `./data/state.json` | OAuth 凭据存储位置 |
| `EWELINK_REQUEST_TIMEOUT` | `10s` | 云请求和 HTTP header 超时时间 |
| `EWELINK_TOKEN_CHECK_INTERVAL` | `12h` | 后台 token 检查间隔 |
| `EWELINK_TOKEN_REFRESH_AHEAD` | `168h` | 在 access token 到期前提前这么长时间刷新 |

持续时间使用 Go duration 语法，例如 `10s`、`30m` 或 `168h`，且必须为正值。

## Debian 12 原生安装

### 一键安装

Linux amd64 可以直接使用一键安装脚本。先准备包含真实 App ID 和已轮换 App Secret 的 `.env`，再下载、检查并执行脚本：

```bash
curl -fsSLO https://raw.githubusercontent.com/sundev126/ewelink-lan-ctl/main/deploy/install.sh
chmod +x install.sh
less install.sh
sudo ./install.sh ./.env
```

脚本默认安装最新 Release，也可以安装指定版本：

```bash
sudo ./install.sh --version v1.0.0 ./.env
```

脚本会验证 Release 的 SHA-256 校验和，安装二进制和 systemd unit，创建 `ewelink` 专用用户，并启动服务。再次执行脚本可以升级；未传入配置文件时会保留现有 `/etc/ewelink-lan-ctl.env`，OAuth 状态始终保存在 `/var/lib/ewelink-lan-ctl/state.json`。

一键卸载默认保留配置和 OAuth 状态：

```bash
curl -fsSLO https://raw.githubusercontent.com/sundev126/ewelink-lan-ctl/main/deploy/uninstall.sh
chmod +x uninstall.sh
less uninstall.sh
sudo ./uninstall.sh
```

只有确认不再需要配置和 OAuth 凭据时，才执行彻底清理：

```bash
sudo ./uninstall.sh --purge
```

### 手动安装

安装 CA 证书和 Go 1.22 或更高版本，然后构建二进制文件。Debian 12 的基础 `golang-go` 软件包可能比本模块要求的版本旧，因此请从 [go.dev/dl](https://go.dev/dl/) 安装当前 Go 版本，而不要依赖该软件包，并在构建前确认版本。

```bash
sudo apt update
sudo apt install -y ca-certificates
go version  # 输出必须显示 go1.22 或更高版本
go test ./...
CGO_ENABLED=0 go build -trimpath -o ewelink-lan-ctl ./cmd/ewelink-lan-ctl
```

若要进行前台测试，请创建状态目录并加载私有环境：

```bash
mkdir -p data
set -a
. ./.env
set +a
./ewelink-lan-ctl
```

### systemd

按上述方式构建，然后安装二进制文件和 unit。随附的 unit 以专用的 `ewelink` 用户运行，创建模式为 `0700` 的 `/var/lib/ewelink-lan-ctl`，并将文件系统写入权限限制在该状态目录内。

```bash
sudo install -o root -g root -m 0755 ewelink-lan-ctl /usr/local/bin/ewelink-lan-ctl
sudo useradd --system --home-dir /var/lib/ewelink-lan-ctl --shell /usr/sbin/nologin ewelink
sudo install -o root -g root -m 0644 deploy/ewelink-lan-ctl.service /etc/systemd/system/ewelink-lan-ctl.service
sudo install -o root -g root -m 0600 .env /etc/ewelink-lan-ctl.env
sudo systemctl daemon-reload
sudo systemctl enable --now ewelink-lan-ctl
sudo systemctl status ewelink-lan-ctl
```

unit 在其命令行中强制设置 `EWELINK_STATE_FILE=/var/lib/ewelink-lan-ctl/state.json`，因此从 `.env` 复制的值无法将凭据文件重定向到受保护状态目录之外。使用以下命令查看经过脱敏的结构化日志：

```bash
sudo journalctl -u ewelink-lan-ctl -f
```

更改 `/etc/ewelink-lan-ctl.env` 后，使用 `sudo systemctl restart ewelink-lan-ctl` 重启服务。

## Docker Compose

发布镜像在 Debian Bookworm 上使用 Go 1.22 构建并测试静态二进制文件，然后在带有 CA 证书的 Debian 12 distroless 镜像中以非 root 用户运行该文件。`deploy/docker-compose.yml` 使用 GitHub Container Registry 中的 `latest` 镜像、普通端口发布（而不是 host 网络），并将凭据持久化到挂载在 `/data` 的命名 volume 中。

```bash
read -r -p 'EWELINK_APP_ID: ' EWELINK_APP_ID
read -r -s -p 'EWELINK_APP_SECRET: ' EWELINK_APP_SECRET && echo
export EWELINK_APP_ID EWELINK_APP_SECRET
# 若回调地址不是默认值，也在当前 shell 中设置 EWELINK_CALLBACK_URL。
docker compose -f deploy/docker-compose.yml pull
docker compose -f deploy/docker-compose.yml up -d
docker compose -f deploy/docker-compose.yml logs -f ewelink-lan-ctl
```

Compose 通过 `environment` 显式传递配置，不使用服务级 `env_file`。执行 Compose 前必须提供 `EWELINK_APP_ID` 和 `EWELINK_APP_SECRET`；推荐按上例从当前 shell 安全读取，避免将 Secret 写进命令历史。其他配置可选，并使用配置表中的默认值。Compose 始终强制设置 `EWELINK_LISTEN_ADDR=:33998` 和 `EWELINK_STATE_FILE=/data/state.json`。使用 `docker compose -f deploy/docker-compose.yml down` 停止容器而不删除凭据。除非确实要清除已保存的 OAuth 凭据，否则不要添加 `--volumes`。

## 版本发布

推送以 `v` 开头的 tag 会自动创建 GitHub Release，并附带 Linux amd64、Windows amd64 二进制压缩包及 SHA-256 校验和。同时会将支持 `linux/amd64` 和 `linux/arm64` 的容器镜像发布到 GitHub Container Registry：

```bash
git tag v1.0.0
git push origin v1.0.0
docker pull ghcr.io/sundev126/ewelink-lan-ctl:v1.0.0
```

语义化版本 tag 还会生成不带 `v` 的完整版本标签和 `主版本.次版本` 标签；每次发布也会更新 `latest`。

## 初始 OAuth 授权

将 `http://127.0.0.1:33998/callback` 注册为开发者应用的重定向 URL，并为 `EWELINK_CALLBACK_URL` 使用相同的值。在将要使用浏览器的计算机上，打开到服务器的 SSH 本地端口转发会话并保持运行：

```bash
ssh -L 33998:127.0.0.1:33998 user@server
```

然后在该计算机的浏览器中打开以下 URL：

```text
http://127.0.0.1:33998/oauth/start
```

服务会重定向到 eWeLink 以进行登录和授权同意。eWeLink 通过隧道返回到 `/callback`。回调成功时会显示一个最简 HTML 确认页面；回调失败时会返回下文所述的稳定 JSON 错误封装。授权完成后可以关闭隧道，仅在重新授权时需要再次建立隧道。

以下命令可用于诊断，显示 OAuth 重定向 header，但必须使用浏览器才能完成登录：

```bash
curl -i http://127.0.0.1:33998/oauth/start
```

不要手动调用 `/callback`：其中的 `code`、`region` 和一次性 `state` 值由 eWeLink 提供。

## HTTP API

将基础 URL 设置为服务器在受信任 LAN 或 VPN 中的地址。本服务既不接受也不要求 `Authorization` header、API key 或调用者凭据。

```bash
BASE_URL=http://server.lan:33998
```

检查进程健康状况以及 OAuth 凭据是否就绪：

```bash
curl --fail-with-body "$BASE_URL/healthz"
# {"status":"ok","oauth_ready":true}
```

列出所有支持的单通道设备，并复制其 `device_id`：

```bash
curl --fail-with-body "$BASE_URL/api/v1/devices"
# {"devices":[{"device_id":"1000123456","name":"iPhone Charger","online":true,"state":"off","uiid":1,"model":"example-model"}]}
```

- 每次列举设备都会实时遍历已授权账号下的全部家庭，不缓存家庭、设备元数据或状态。
- 对使用 `params.switches` 的设备，服务只读取和控制 `outlet: 0`；REST API 仍将设备表现为单个 `on|off` 开关，不暴露 outlet 参数。
- 每次设置开关前都会实时读取设备元数据以选择正确的云端协议，因此一次控制通常产生一次读取请求和一次写入请求。

读取一个设备的当前状态：

```bash
DEVICE_ID=1000123456
curl --fail-with-body "$BASE_URL/api/v1/devices/$DEVICE_ID/status"
# {"device_id":"1000123456","online":true,"state":"off"}
```

打开或关闭该设备。只接受显式状态 `on` 和 `off`；特意不支持 `toggle`，以保证重试安全。

```bash
curl --fail-with-body \
  -X PUT \
  -H 'Content-Type: application/json' \
  -d '{"state":"on"}' \
  "$BASE_URL/api/v1/devices/$DEVICE_ID/switch"

curl --fail-with-body \
  -X PUT \
  -H 'Content-Type: application/json' \
  -d '{"state":"off"}' \
  "$BASE_URL/api/v1/devices/$DEVICE_ID/switch"
# {"success":true,"device_id":"1000123456","state":"off"}
```

切换响应成功表示 eWeLink 已接受并处理该命令；需要后续读取时，请查询状态 endpoint。

### JSON 错误

所有 REST 错误和 OAuth 回调失败都使用稳定的 JSON 封装：

```json
{
  "error": {
    "code": "device_offline",
    "message": "device is offline"
  }
}
```

预期 HTTP 状态码为：无效输入返回 `400`，设备或路由缺失返回 `404`，设备不受支持返回 `409`，云端操作被拒绝、设备离线或响应格式错误返回 `502`，OAuth 或 eWeLink 不可用时返回 `503`。稳定错误码包括 `invalid_request`、`not_found`、`method_not_allowed`、`oauth_required`、`oauth_unavailable`、`device_not_found`、`unsupported_device`、`device_offline`、`ewelink_rejected`、`ewelink_unavailable` 和 `internal_error`。消息可安全地提供给调用者；请检查服务日志以了解脱敏后的运行类别。

## iPhone 自动化

为每台 iPhone 及其充电插座分别创建自动化。首先调用 `GET /api/v1/devices`，找到对应插座，然后在两项自动化中始终使用其准确的 `device_id`。iPhone 必须能够通过受信任的 LAN 或 VPN 访问服务器。

在快捷指令中设置充电自动化：

1. 为 **电池电量** 的 **低于 20%** 创建个人自动化。
2. 添加 **获取 URL 内容**，URL 为 `http://server.lan:33998/api/v1/devices/<DEVICE_ID>/switch`。
3. 将方法设为 `PUT`、请求体设为 `JSON`，并添加文本字段 `state`，其值为 `on`。
4. 如果 iOS 提供该选项，请关闭 **运行前询问**。

设置停止充电自动化：

1. 为 **电池电量** 的 **高于 80%** 创建个人自动化。
2. 使用相同的设备专属 URL 和设置，但将 `state` 设为 `off`。
3. 如果 iOS 提供该选项，请关闭 **运行前询问**。

简而言之，为每个 device ID 重复设置这一对自动化：

```text
低于 20% -> PUT /api/v1/devices/<DEVICE_ID>/switch {"state":"on"}
高于 80% -> PUT /api/v1/devices/<DEVICE_ID>/switch {"state":"off"}
```

不要将一个插座的 `device_id` 用于另一台 iPhone。
