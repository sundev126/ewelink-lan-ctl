# 易微联家庭遍历与 switches 设备支持实施计划

> **供智能代理执行：** 必须使用 `superpowers:subagent-driven-development`（推荐）或 `superpowers:executing-plans`，逐项实施本计划。步骤使用复选框（`- [ ]`）跟踪。

**目标：** 修复真实账号设备列表为空的问题，并让现有 REST API 正确读取和控制使用 `params.switches[outlet=0]` 的 UIID 138 设备。

**实现方式：** 客户端每次列举设备时先获取全部家庭，再按家庭独立分页并按 device ID 去重。指定设备读取会识别标量 `switch` 或数组 `switches` 格式；设置开关前实时读取设备元数据，使用真实 `itemType` 和匹配的 JSON 格式写入，不增加任何缓存。

**技术栈：** Go 1.22、标准库 `net/http` 与 `encoding/json`、`httptest`、现有 Gateway/Token Manager、Markdown。

## 全局约束

- spec 和 plan 使用中文书写。
- 不记录或提交真实家庭 ID、device ID、设备名称、Token、App ID 或 App Secret。
- 保持现有 REST 路径、请求 JSON 和响应 JSON 不变。
- 保持无设备缓存、无家庭缓存、无 LAN 控制。
- 数组开关格式只读取和控制 `outlet: 0`，不向调用者开放 outlet 参数。
- `SetSwitch` 仍只接受 `on` 和 `off`；无效输入必须在任何云端请求前失败。
- 任一家庭查询失败时整个设备列表失败，不返回部分结果。
- 真实开关测试必须再次取得用户对具体设备和操作顺序的明确授权。

---

### Task 1：实现家庭遍历和两种开关格式解析

**文件：**

- 新建：`internal/ewelink/family.go`
- 新建：`internal/ewelink/family_test.go`
- 修改：`internal/ewelink/client.go`
- 修改：`internal/ewelink/client_test.go`

**接口：**

- 输入：`Client.ListDevices(ctx, region, token)` 与 `Client.GetDevice(ctx, region, token, deviceID)` 的既有调用。
- 产出：`listFamilies(ctx, region, token) ([]family, error)`、按家庭分页的 `listThingsForFamily`、`switchDescriptor(thing) (Device, switchMode, bool)` 和可供 Task 2 复用的 `getThing`。
- 保持：公开 `Device` 类型和 `Client` 公开方法签名不变。

- [ ] **步骤 1：为家庭列表写失败测试**

  在 `internal/ewelink/family_test.go` 建立假上游，覆盖有效家庭、空列表、重复 ID、缺失 `familyList`、`null`、空 ID 和上游错误。有效响应示例：

  ```json
  {"error":0,"data":{"familyList":[{"id":"family-a"},{"id":"family-a"},{"id":"family-b"}]}}
  ```

  断言返回顺序为 `family-a`、`family-b`；空数组合法；其余异常均返回错误。

- [ ] **步骤 2：运行家庭测试并确认 RED**

  运行：

  ```bash
  go test -count=1 ./internal/ewelink -run 'TestClientListFamilies' -v
  ```

  预期：因 `listFamilies` 和家庭类型尚不存在而编译失败。

- [ ] **步骤 3：实现严格家庭解析**

  在 `internal/ewelink/family.go` 新增：

  ```go
  type family struct {
      ID string `json:"id"`
  }

  func (c *Client) listFamilies(ctx context.Context, region, token string) ([]family, error)
  ```

  响应结构使用 `json.RawMessage` 保存 `familyList`，从而区分缺失字段、`null` 和合法空数组。解析后拒绝空 ID，按首次出现顺序去重；任何异常使用 `fmt.Errorf("list ewelink families: %w", err)` 保留 cause。

- [ ] **步骤 4：运行家庭测试并确认 GREEN**

  运行：

  ```bash
  go test -count=1 ./internal/ewelink -run 'TestClientListFamilies' -v
  ```

  预期：全部 PASS，输出无警告。

- [ ] **步骤 5：为跨家庭列表写失败测试**

  扩展 `internal/ewelink/client_test.go`，假上游必须验证：

  - 首先收到一次 `GET /v2/family`。
  - 每个 Thing 请求都携带正确 `familyid`、`num=30` 和家庭自己的分页游标。
  - 两个家庭的重复 device ID 只返回一次，首次出现决定顺序。
  - 某个家庭请求失败或游标不前进时整个调用失败。

  测试数据同时包含：

  ```json
  {"params":{"switch":"off"}}
  ```

  和：

  ```json
  {"params":{"switches":[{"outlet":0,"switch":"on"}]}}
  ```

  预期两个设备都出现在公开列表中，状态分别为 `off` 和 `on`。

- [ ] **步骤 6：运行跨家庭列表测试并确认 RED**

  运行：

  ```bash
  go test -count=1 ./internal/ewelink -run 'TestClientListDevicesAcrossFamilies' -v
  ```

  预期：当前实现没有请求 `/v2/family`，测试因请求顺序或设备结果不符而失败。

- [ ] **步骤 7：重构列表并实现开关描述符**

  在 `internal/ewelink/client.go`：

  ```go
  type switchMode uint8

  const (
      switchModeScalar switchMode = iota + 1
      switchModeOutletZero
  )

  type switchEntry struct {
      Outlet *int `json:"outlet"`
      Switch any  `json:"switch"`
  }
  ```

  将 `thingData.Params` 扩展为标量 `Switch any` 和 `Switches json.RawMessage`。实现：

  ```go
  func switchDescriptor(item thing) (Device, switchMode, bool)
  func (c *Client) listThingsForFamily(ctx context.Context, region, token, familyID string) ([]thing, error)
  ```

  `switchDescriptor` 优先接受合法标量；否则严格解码数组，只接受唯一且状态为 `on|off` 的 `outlet: 0`。`ListDevices` 调用 `listFamilies`，逐家庭分页，按 device ID 首次出现去重。

- [ ] **步骤 8：为 switches 边界和 GetDevice 写失败测试**

  表驱动覆盖：合法 outlet 0、只有其他 outlet、重复 outlet 0、缺失 outlet、非法状态、错误 JSON 类型、标量与数组同时有效。另增加自有 `itemType:1` 和共享 `itemType:2` 的 `GetDevice` 测试，要求数组格式均能返回 outlet 0 状态，标量优先。

- [ ] **步骤 9：实现可复用的指定 Thing 读取**

  抽取：

  ```go
  func (c *Client) getThing(ctx context.Context, region, token, deviceID string) (thing, Device, switchMode, error)
  ```

  它只发起一次 `POST /v2/device/thing`，请求中包含 `itemType:1` 和 `itemType:2`。按响应顺序扫描匹配 device ID 的候选，返回第一个受支持候选；存在匹配但均不受支持时包装 `ErrUnsupportedDevice`，完全没有匹配时包装 `ErrDeviceNotFound`。`GetDevice` 仅返回其中的公开 `Device`。

- [ ] **步骤 10：运行 Task 1 测试和完整包测试**

  运行：

  ```bash
  gofmt -w internal/ewelink/family.go internal/ewelink/family_test.go internal/ewelink/client.go internal/ewelink/client_test.go
  go test -count=1 ./internal/ewelink
  go test -count=1 ./...
  git diff --check
  ```

  预期：全部退出 0，旧标量设备测试与新增家庭/switches 测试全部 PASS。

- [ ] **步骤 11：提交 Task 1**

  ```bash
  git add internal/ewelink/family.go internal/ewelink/family_test.go internal/ewelink/client.go internal/ewelink/client_test.go
  git commit -m "fix: list switch devices across families"
  ```

  预期：提交成功，只包含 Task 1 的客户端和测试文件。

---

### Task 2：实现协议感知控制、Token 重试验证和文档

**文件：**

- 修改：`internal/ewelink/client.go`
- 修改：`internal/ewelink/client_test.go`
- 修改：`internal/ewelink/gateway_test.go`
- 修改：`README.md`

**接口：**

- 输入：Task 1 的 `getThing(ctx, region, token, deviceID) (thing, Device, switchMode, error)`。
- 产出：保持签名不变的 `Client.SetSwitch(ctx, region, token, deviceID, state) error`，根据实时 Thing 元数据发送标量或 outlet 0 控制。
- 保持：Gateway、HTTP API 和 iPhone 快捷指令调用格式不变。

- [ ] **步骤 1：为协议感知控制写失败测试**

  在 `internal/ewelink/client_test.go` 增加三个测试：

  1. 标量自有设备：先收到 `POST /v2/device/thing`，再收到状态写入：

     ```json
     {"type":1,"id":"device-1","params":{"switch":"off"}}
     ```

  2. 数组自有设备：状态写入必须为：

     ```json
     {"type":1,"id":"device-2","params":{"switches":[{"outlet":0,"switch":"on"}]}}
     ```

  3. 数组共享设备：同一数组格式，但 `type` 必须为 `2`。

  三个测试都断言控制前恰好执行一次设备读取，写入后没有额外状态读取。

- [ ] **步骤 2：运行控制测试并确认 RED**

  运行：

  ```bash
  go test -count=1 ./internal/ewelink -run 'TestClientSetSwitchUsesDeviceProtocol' -v
  ```

  预期：当前实现直接写入标量 `switch`，请求顺序或 JSON 断言失败。

- [ ] **步骤 3：实现协议感知 SetSwitch**

  保留本地 `on|off` 验证并确保它位于任何云请求之前。随后调用 `getThing`，根据 `switchMode` 构造两个独立的强类型请求结构：

  ```go
  type scalarSwitchParams struct {
      Switch string `json:"switch"`
  }

  type outletSwitchParams struct {
      Switches []struct {
          Outlet int    `json:"outlet"`
          Switch string `json:"switch"`
      } `json:"switches"`
  }
  ```

  请求的 `type` 使用 `thing.ItemType`，不得固定为 1。只发送与设备格式匹配的一个参数字段。

- [ ] **步骤 4：验证无效输入和 typed error**

  增加测试确认 `toggle` 不产生任何请求；设备不存在和不支持时不发送状态写入，并分别保留 `ErrDeviceNotFound`、`ErrUnsupportedDevice`；预读取上游错误原样保留 cause。

- [ ] **步骤 5：运行客户端控制测试并确认 GREEN**

  运行：

  ```bash
  gofmt -w internal/ewelink/client.go internal/ewelink/client_test.go
  go test -count=1 ./internal/ewelink -run 'TestClientSetSwitch' -v
  ```

  预期：全部 PASS，输出无警告。

- [ ] **步骤 6：增加完整操作 Token 重试测试**

  在 `internal/ewelink/gateway_test.go` 使用真实 `Client` 和假 HTTP 上游，分别覆盖：

  - 第一次设备预读取返回 Token 401，刷新后重新执行读取并成功写入。
  - 第一次状态写入返回 Token 402，刷新后必须重新执行设备读取，再写入一次。

  两种情况都断言刷新只发生一次、Gateway 最多重试完整 `SetSwitch` 一次，第二次 Token 错误直接返回。

- [ ] **步骤 7：运行 Gateway 测试并确认 GREEN**

  运行：

  ```bash
  gofmt -w internal/ewelink/gateway_test.go
  go test -count=1 ./internal/ewelink -run 'TestGatewaySetSwitchRetriesWholeProtocolAwareOperation' -v
  go test -count=1 -race ./internal/ewelink
  ```

  预期：全部 PASS，无 data race。

- [ ] **步骤 8：更新中文 README**

  在 HTTP API 或设备列表说明附近补充三点：

  - 每次列举设备都会实时遍历账号下全部家庭。
  - 对 `params.switches` 格式仅使用 `outlet: 0`，REST API 仍表现为单个 `on|off` 开关。
  - 每次设置开关前会读取设备元数据，因此一次控制通常产生一次读取和一次写入请求。

  不记录真实家庭或设备信息，不改变已有 curl 和快捷指令示例。

- [ ] **步骤 9：运行完整验证**

  运行：

  ```bash
  go test -count=1 ./...
  go test -count=1 -race ./...
  go vet ./...
  go build ./cmd/ewelink-lan-ctl
  git diff --check
  ```

  预期：全部退出 0。

- [ ] **步骤 10：提交 Task 2**

  ```bash
  git add internal/ewelink/client.go internal/ewelink/client_test.go internal/ewelink/gateway_test.go README.md
  git commit -m "fix: control outlet-zero switch devices"
  ```

  预期：提交成功，工作区干净。

- [ ] **步骤 11：真实只读验收**

  使用用户已经完成 OAuth 的现有 `state.json` 启动修复后的二进制，但不得打印文件内容或环境变量值。调用：

  ```bash
  curl --noproxy '*' http://127.0.0.1:33998/healthz
  curl --noproxy '*' http://127.0.0.1:33998/api/v1/devices
  ```

  预期：health 为 `oauth_ready:true`；设备列表包含已授权账号下受支持的目标设备，状态来自 `outlet: 0`。到此停止；没有用户对具体设备的再次授权，不调用 PUT 开关接口。
