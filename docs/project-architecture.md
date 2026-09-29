# 项目功能与架构

## 项目定位

`ewelink-lan-ctl` 是一个使用 Go 1.22 实现的 eWeLink 云端控制网关。它面向部署在 Debian 服务器上的单账号场景，通过 eWeLink v2 云 API 为 iPhone 快捷指令等局域网或 VPN 客户端提供简单的 HTTP 接口。

尽管项目名称包含 `lan`，当前实现不进行 LAN/mDNS 发现，也不直接通过局域网协议控制设备。

## 已实现功能

- 完成单个 eWeLink 账号的 OAuth 授权，并按区域访问对应的云端 API。
- 自动刷新 access token；并发请求共用同一次刷新操作。
- 实时列出账号全部家庭中的受支持设备。
- 实时读取单个设备的在线状态和开关状态。
- 以幂等的 `on`、`off` 操作控制开关，不提供 `toggle`。
- 支持标量 `params.switch` 和 `params.switches` 中的 `outlet: 0`。
- 提供健康检查、稳定的 JSON 错误结构和脱敏日志。
- 支持原生二进制、systemd、Docker 和 Docker Compose 部署。
- 根目录 README 使用简体中文说明配置、部署、OAuth、API 和快捷指令用法。

## 软件结构

| 目录 | 职责 |
| --- | --- |
| `cmd/ewelink-lan-ctl` | 进程入口、依赖组装、HTTP 服务和优雅退出 |
| `internal/config` | 环境变量读取、默认值和配置校验 |
| `internal/ewelink` | OAuth 签名、区域 API 客户端、家庭及设备访问、Token 失败重试 |
| `internal/token` | Token 生命周期、定时刷新和并发刷新合并 |
| `internal/store` | OAuth 凭据的安全、原子化 JSON 持久化 |
| `internal/httpapi` | OAuth、健康检查和设备 REST API |

所有云端请求均使用可取消的 context 和可配置超时时间。客户端在进程内限制请求间隔，避免短时间内并发请求直接冲击上游服务。设备、家庭和设备状态均不缓存。

## OAuth 与凭据

授权流程如下：

1. `GET /oauth/start` 创建随机、短期且只能使用一次的 OAuth `state`，随后跳转到 eWeLink 授权页面。
2. eWeLink 将 `code`、`region` 和 `state` 返回到 `/callback`。
3. 服务校验并立即消费 `state`，再使用授权码换取 Token。
4. 服务仅持久化区域、access/refresh token 及其过期时间。

凭据文件以 `0600` 权限写入。保存时先在同一目录写临时文件并同步，然后原子替换旧文件；保存失败不会覆盖上一份有效凭据。App ID 和 App Secret 仅从环境变量读取，不写入状态文件。

Token 生命周期包含以下处理：

- 启动时检查并按配置提前刷新即将到期的 access token。
- 后台按固定周期检查 Token。
- 每次请求前避免使用即将到期的 Token。
- 云端返回 Token 错误时强制刷新，并将完整操作最多重试一次。
- 多个并发请求需要刷新时，只执行一次刷新并共享结果。
- 刷新失败时保留旧凭据；refresh token 失效时要求重新 OAuth 授权。

## HTTP API

当前公开接口为：

| 方法与路径 | 功能 |
| --- | --- |
| `GET /healthz` | 返回进程和 OAuth 就绪状态 |
| `GET /oauth/start` | 启动 OAuth 授权 |
| `GET /callback` | 接收 OAuth 回调 |
| `GET /api/v1/devices` | 实时列出受支持设备 |
| `GET /api/v1/devices/{device_id}/status` | 实时读取一个设备的状态 |
| `PUT /api/v1/devices/{device_id}/switch` | 将一个设备显式设置为 `on` 或 `off` |

`/api/v1` 接口有意不实现调用方认证。部署时必须通过可信 LAN、VPN 或带认证的反向代理限制访问，不能直接暴露到互联网。能够访问服务的调用者也可以启动 OAuth 并重新绑定账号。

错误响应使用稳定结构：

```json
{
  "error": {
    "code": "device_offline",
    "message": "device is offline"
  }
}
```

输入错误、资源不存在、设备不受支持、上游失败和 OAuth 不可用分别映射到对应的 `4xx` 或 `5xx` 状态码。响应与日志不会包含 Token、App Secret、授权码、签名或上游原始敏感载荷。

## 运行和部署

- 原生运行目标为 Debian 12，支持随附的 systemd unit。
- Docker 使用多阶段构建；运行镜像为非 root 的 distroless 镜像，并包含 CA 证书。
- Docker Compose 将状态保存到 `/data` 持久卷，并发布端口 `33998`。
- 收到 SIGINT 或 SIGTERM 时停止后台任务并在限定时间内优雅关闭 HTTP 服务。
- 推送 `v*` tag 后自动构建版本二进制、容器镜像和 GitHub Release，详见 [Tag 自动发布工作流设计](2026-09-29-tag-release-action-design.md)。

## 功能边界

当前不支持：

- LAN 发现或 LAN 直连控制。
- 多个 eWeLink 账号。
- 由 API 调用者指定 outlet、多通道 REST 模型或批量控制。
- 分组、场景、灯具、传感器等非当前开关模型。
- `toggle` 操作。
- 家庭、设备元数据或设备状态缓存。
- 内置 API 身份认证和 iOS 原生应用。

## 测试覆盖

自动化测试覆盖配置校验、OAuth 签名与回调状态、区域选择、凭据原子存储、Token 刷新、家庭与设备分页、两种开关格式、上游错误映射、HTTP 契约以及进程组装。常规验收命令为：

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/ewelink-lan-ctl
```
