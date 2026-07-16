# 易微联家庭遍历与 switches 设备支持设计

## 背景

真实云端联调已经证明 OAuth 授权和凭据持久化正常，但现有设备列表返回空数组。根因有两个：

1. `GET /v2/device/thing` 需要家庭 ID，现有客户端没有先调用 `GET /v2/family`。
2. 现有客户端只识别标量 `params.switch`，而 UIID 138 设备通过 `params.switches[]` 表示开关状态。

本设计不记录真实家庭 ID、device ID、设备名称、Token 或 App Secret。

## 目标

- 查询账号下全部家庭，并返回所有家庭中受支持的设备。
- 同时支持标量 `params.switch` 和数组 `params.switches` 两种单通道控制格式。
- 对数组格式固定读取和控制 `outlet: 0`；其他 outlet 不对外暴露。
- 控制前实时读取设备元数据，选择正确的 `itemType` 和控制参数。
- 保持无设备缓存、无家庭缓存、无 LAN 控制的既有架构。

## 非目标

- 不支持由调用者选择 outlet。
- 不支持 toggle、批量控制或多通道 REST 模型。
- 不缓存家庭、设备元数据或设备状态。
- 不改变现有 REST 路径、请求 JSON 或响应 JSON。

## 客户端设计

### 家庭列表

新增内部家庭结构和 `listFamilies` 辅助方法，请求 `GET /v2/family`。只使用响应中的家庭 `id`；名称、房间和成员信息不进入公开模型。

响应必须满足以下条件：

- 家庭列表字段存在且可解码。
- 每个家庭的 `id` 非空。
- 重复家庭 ID 只查询一次。

没有家庭时返回空设备列表。请求失败、响应异常或家庭 ID 为空时返回错误，不返回部分结果。

### 跨家庭设备列表

`ListDevices` 每次调用均执行以下流程：

1. 获取当前账号的全部家庭。
2. 对每个家庭调用 `GET /v2/device/thing`，携带 `familyid`、`num=30` 和分页游标。
3. 每个家庭独立从 `beginIndex=-9999999` 开始分页。
4. 解析受支持设备，并按 `device_id` 去重；首次出现的设备决定输出顺序和内容。
5. 任一家庭请求失败、分页游标不前进或响应异常时，整个调用失败。

### 开关格式识别

设备被视为受支持，当且仅当满足以下一种格式：

- `params.switch` 是 `on` 或 `off`。
- `params.switches` 是数组，并且包含 `outlet: 0`，其 `switch` 是 `on` 或 `off`。

数组中其他 outlet 被忽略。`outlet: 0` 缺失、重复、状态非法或字段类型异常时，设备不受支持。若响应同时包含有效的标量和数组格式，优先使用标量格式，保持现有设备行为。

公开 `Device.state` 继续只返回 `on` 或 `off`，不新增 outlet 字段。

### 指定设备读取

抽取内部 `getThing` 辅助方法，通过 `POST /v2/device/thing` 同时尝试自有设备 `itemType: 1` 和共享设备 `itemType: 2`。它返回匹配 device ID 的原始 Thing 及其实际 `itemType`。

`GetDevice` 复用该方法并按上述格式解析。找不到设备时返回 `ErrDeviceNotFound`；找到但格式不受支持时返回 `ErrUnsupportedDevice`。

### 设置开关

`SetSwitch` 仍只接受 `on` 和 `off`。验证输入后执行：

1. 调用 `getThing` 实时读取设备，不使用缓存。
2. 识别设备的开关格式。
3. 使用设备真实 `itemType` 构造 `POST /v2/device/thing/status`：
   - 标量格式发送 `params: {"switch":"on|off"}`。
   - 数组格式发送 `params: {"switches":[{"outlet":0,"switch":"on|off"}]}`。
4. 不额外查询写入后的状态；现有 REST 成功语义保持为“易微联已接受并处理命令”。

设备在预读取阶段不存在或不受支持时返回现有 typed error。预读取或写入阶段的云端错误继续经过现有错误分类和 Token 重试逻辑。

## Gateway 与 Token 重试

公开 Gateway 接口签名保持不变。一次 `SetSwitch` Gateway 调用内部可能产生一次设备读取和一次状态写入，这两步属于同一个云端操作。

若任一步返回 Token 401/402，Gateway 继续按既有规则条件刷新 Token，并从完整 `SetSwitch` 操作开头重试一次。不得只重放状态写入，也不得重试超过一次。

## 安全和日志

- 不记录家庭响应、Thing 原始响应、Token、App Secret 或设备控制参数。
- 错误继续使用稳定分类，响应和日志不得包含上游原始 payload。
- 新增实现不得持久化家庭 ID、device ID 或设备元数据。

## 测试

使用假上游服务器覆盖：

- 多家庭查询、每个家庭独立分页、跨家庭 device ID 去重和稳定顺序。
- 家庭为空、家庭 ID 无效、家庭请求失败、某个家庭 Thing 请求失败及游标不前进。
- 标量 `switch` 设备保持兼容。
- `switches` 中有效 outlet 0、缺失 outlet 0、重复 outlet 0、非法状态和其他 outlet。
- `GetDevice` 对自有和共享数组格式设备的识别。
- `SetSwitch` 对标量、数组和共享设备发送正确的 type 与 JSON；控制前确实执行元数据读取。
- 无效 state 在发起任何云端请求前被拒绝。
- Gateway 在读取或写入遇到 Token 错误时只刷新并重试完整操作一次。

完成单元测试后，使用已授权的真实账号进行只读验收：`GET /api/v1/devices` 应返回目标设备及 `outlet: 0` 的当前状态。真实开关测试必须由用户指定设备并明确同意后再执行。

## 文档

README 补充以下说明：

- 设备列表会实时遍历账号下全部家庭。
- 对使用 `switches` 的设备仅控制 `outlet: 0`。
- 每次设置开关会先读取设备元数据，因此会产生额外一次云端请求。
