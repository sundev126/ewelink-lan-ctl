# eWeLink Cloud Gateway Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a Go 1.22 service that performs one-account eWeLink OAuth, refreshes credentials safely, and exposes unauthenticated REST endpoints for iPhone Shortcuts to query and control separate single-channel plugs through eWeLink v2 REST APIs.

**Architecture:** A standard-library HTTP server delegates OAuth and device calls to a typed eWeLink client. A token manager owns the minimum persisted credential state and coalesces refreshes; an authenticated gateway retries one cloud request after token rejection. Device metadata and state are never cached.

**Tech Stack:** Go 1.22 standard library, `net/http` ServeMux method patterns, JSON persistence, `httptest`, Docker, Compose, systemd.

## Global Constraints

- Support exactly one eWeLink account and devices exposing scalar `params.switch` values `on` or `off`.
- Do not implement LAN control, multiple accounts, groups, multi-channel devices, scenes, sensors, `toggle`, or an iOS app.
- Keep `/api/v1` intentionally unauthenticated.
- Persist only region, access/refresh tokens, and expiry timestamps; never cache device metadata or state.
- Read App ID and App Secret only from environment variables and never log secrets, tokens, OAuth codes, or signatures.
- Space eWeLink requests by at least 500ms and retry at most once after token error `401` or `402`.
- Target Debian 12, native binary, systemd, Docker, and Docker Compose.

---

## File Map

- `go.mod`, `.gitignore`, `.env.example`: module and safe configuration examples.
- `internal/config/config.go`: environment parsing and defaults.
- `internal/store/store.go`: atomic OAuth JSON persistence.
- `internal/ewelink/{types,oauth,client,gateway}.go`: eWeLink types, OAuth, raw REST calls, and token retry.
- `internal/token/manager.go`: token lifecycle, scheduler, and refresh coalescing.
- `internal/httpapi/api.go`: routes, DTOs, OAuth state, validation, and stable errors.
- `cmd/ewelink-lan-ctl/main.go`: dependency wiring and graceful shutdown.
- `Dockerfile`, `compose.yaml`, `deploy/ewelink-lan-ctl.service`, `README.md`: deployment and use.
- Each Go implementation file has a sibling `_test.go` file.

---

### Task 1: Module, Configuration, and Credential Store

**Files:**
- Create: `go.mod`
- Create: `.gitignore`
- Create: `.env.example`
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`
- Create: `internal/store/store.go`
- Test: `internal/store/store_test.go`

**Interfaces:**
- Produces: `config.Load() (config.Config, error)`.
- Produces: `store.Credentials`, `store.File.Load()`, `store.File.Save()`.

- [ ] **Step 1: Write failing configuration tests**

Create `go.mod` with module `github.com/zm/ewelink-lan-ctl` and Go 1.22. Test missing App ID/Secret, defaults `:33998`, loopback callback, `./data/state.json`, `10s`, `12h`, `7d`, overrides, and rejection of non-positive durations.

- [ ] **Step 2: Verify the tests fail**

Run: `go test ./internal/config -run TestLoad -v`

Expected: FAIL because `config.Load` does not exist.

- [ ] **Step 3: Implement configuration**

Use this exact public shape:

```go
type Config struct {
    AppID, AppSecret, CallbackURL, ListenAddr, StateFile string
    RequestTimeout, CheckInterval, RefreshAhead time.Duration
}
func Load() (Config, error)
```

Read `EWELINK_APP_ID`, `EWELINK_APP_SECRET`, `EWELINK_CALLBACK_URL`, `EWELINK_LISTEN_ADDR`, `EWELINK_STATE_FILE`, `EWELINK_REQUEST_TIMEOUT`, `EWELINK_TOKEN_CHECK_INTERVAL`, and `EWELINK_TOKEN_REFRESH_AHEAD`. Require credentials and positive durations.

- [ ] **Step 4: Verify configuration passes**

Run: `gofmt -w internal/config && go test ./internal/config -v`

Expected: PASS.

- [ ] **Step 5: Write failing store tests**

Use `t.TempDir()` and cover round trip, UTC timestamps, destination mode `0600`, missing file returning `os.ErrNotExist`, malformed JSON, parent mode `0700`, and an interrupted save preserving the previous state.

- [ ] **Step 6: Verify store tests fail**

Run: `go test ./internal/store -v`

Expected: FAIL because store types do not exist.

- [ ] **Step 7: Implement atomic store**

```go
type Credentials struct {
    Region string `json:"region"`
    AccessToken string `json:"access_token"`
    AccessTokenExpiresAt time.Time `json:"access_token_expires_at"`
    RefreshToken string `json:"refresh_token"`
    RefreshTokenExpiresAt time.Time `json:"refresh_token_expires_at"`
}
type File struct { Path string }
func (f File) Load() (Credentials, error)
func (f File) Save(Credentials) error
```

Save through a same-directory temporary file, `Chmod(0600)`, JSON encode, file `Sync`, close, atomic `Rename`, and directory `Sync`. Cleanup only the temporary file on failure.

- [ ] **Step 8: Add safe local files and commit**

`.gitignore` contains `.env`, `data/`, and `/ewelink-lan-ctl`. `.env.example` lists all variables with placeholders.

Run: `gofmt -w internal/store && go test ./internal/config ./internal/store -v && git diff --check`

Commit: `git commit -m "feat: add configuration and credential store"`

---

### Task 2: OAuth and Raw eWeLink Client

**Files:**
- Create: `internal/ewelink/types.go`
- Create: `internal/ewelink/oauth.go`
- Test: `internal/ewelink/oauth_test.go`
- Create: `internal/ewelink/client.go`
- Test: `internal/ewelink/client_test.go`

**Interfaces:**
- Consumes: `store.Credentials`.
- Produces: `Client.AuthorizationURL`, `ExchangeCode`, `Refresh`, `ListDevices`, `GetDevice`, `SetSwitch`.
- Produces: `UpstreamError` and `IsTokenError`.

- [ ] **Step 1: Write failing OAuth tests**

Assert `signBase64("abc", "ABC_123")` equals the official Base64 value, region mappings for `cn/as/us/eu`, invalid regions, authorization query fields, signed code exchange, signed refresh, exact body bytes, required headers, millisecond expiries, and upstream error envelopes.

- [ ] **Step 2: Verify OAuth tests fail**

Run: `go test ./internal/ewelink -run 'Test(Sign|Region|Authorization|Exchange|Refresh)' -v`

Expected: FAIL because OAuth code does not exist.

- [ ] **Step 3: Implement OAuth and shared types**

```go
type Device struct { DeviceID, Name, State, Model string; Online bool; UIID int }
type UpstreamError struct { HTTPStatus, Code int; Message string }
func (e *UpstreamError) Error() string
func IsTokenError(err error) bool
```

Create a `Client` configured with app credentials, callback URL, `*http.Client`, clock, an injectable region-to-base-URL function, and a serialized 500ms request gate. Tests point the base-URL function at `httptest.Server`; production uses only the four official regional hosts. Implement HMAC-SHA256 Base64 signatures, OAuth URL, `POST /v2/user/oauth/token`, and `POST /v2/user/refresh`. Sign the exact serialized POST body. When refresh responses omit expiry timestamps, derive the documented 30-day access and 60-day refresh expiries from the injected clock.

- [ ] **Step 4: Verify OAuth passes**

Run: `gofmt -w internal/ewelink && go test ./internal/ewelink -run 'Test(Sign|Region|Authorization|Exchange|Refresh)' -v`

Expected: PASS.

- [ ] **Step 5: Write failing device tests**

Use `httptest.Server` to cover pagination, item types 1/2/3, filtering `switches`, scalar `switch`, specified Thing lookup, control body, malformed JSON, HTTP errors, eWeLink `30022` and `412`, request cancellation while rate-limited, and no status read after control.

- [ ] **Step 6: Verify device tests fail**

Run: `go test ./internal/ewelink -run 'TestClient(List|Get|Set|Limit|Error)' -v`

Expected: FAIL because device methods do not exist.

- [ ] **Step 7: Implement device operations**

```go
func (c *Client) ListDevices(ctx context.Context, region, token string) ([]Device, error)
func (c *Client) GetDevice(ctx context.Context, region, token, deviceID string) (Device, error)
func (c *Client) SetSwitch(ctx context.Context, region, token, deviceID, state string) error
```

Set `Authorization: Bearer <token>` and `X-CK-Appid`. List in pages of 30 until total is reached or a page is empty. Get one Thing through `POST /v2/device/thing`. Set `{type:1,id,params:{switch:state}}` through `POST /v2/device/thing/status`. Reject non-`on/off` states locally.

- [ ] **Step 8: Verify and commit**

Run: `gofmt -w internal/ewelink && go test ./internal/ewelink -v && go test -race ./internal/ewelink && go vet ./internal/ewelink`

Expected: PASS.

Commit: `git commit -m "feat: add ewelink oauth and cloud client"`

---

### Task 3: Token Lifecycle Manager

**Files:**
- Create: `internal/token/manager.go`
- Test: `internal/token/manager_test.go`

**Interfaces:**
- Consumes: store load/save callbacks and an eWeLink refresh callback.
- Produces: `Load`, `Set`, `Access`, `ForceRefresh`, `Health`, and `Run`.

- [ ] **Step 1: Write failing deterministic tests**

Inject clock and callbacks. Cover missing state, malformed state, valid token, startup refresh inside seven days, request refresh inside five minutes, one refresh for concurrent requests, failed refresh preserving old state, successful persisted refresh, expired refresh token, scheduler check, and cancellation without sleeps.

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/token -v`

Expected: FAIL because the manager does not exist.

- [ ] **Step 3: Implement manager**

```go
var ErrOAuthRequired = errors.New("oauth authorization required")
type RefreshFunc func(context.Context, store.Credentials) (store.Credentials, error)
func New(load func()(store.Credentials,error), save func(store.Credentials)error, refresh RefreshFunc, now func()time.Time, refreshAhead time.Duration) *Manager
func (m *Manager) Load(context.Context) error
func (m *Manager) Set(store.Credentials) error
func (m *Manager) Access(context.Context) (region, token string, err error)
func (m *Manager) ForceRefresh(context.Context) error
func (m *Manager) Health() bool
func (m *Manager) Run(context.Context, time.Duration)
```

Guard state with a mutex and share one in-flight refresh result channel. Never hold the mutex during network or file I/O. Publish new credentials only after `Save` succeeds. Treat five minutes as the request safety window.

- [ ] **Step 4: Verify and commit**

Run: `gofmt -w internal/token && go test ./internal/token -v && go test -race ./internal/token && go vet ./internal/token`

Expected: PASS and refresh count is exactly one under concurrency.

Commit: `git commit -m "feat: manage oauth token lifecycle"`

---

### Task 4: Authenticated Gateway

**Files:**
- Create: `internal/ewelink/gateway.go`
- Test: `internal/ewelink/gateway_test.go`

**Interfaces:**
- Consumes: raw client and token provider.
- Produces: token-aware `ListDevices`, `GetDevice`, and `SetSwitch`.

- [ ] **Step 1: Write failing retry tests**

Test success without refresh, first `401/402` then success, second token failure without another retry, OAuth-required propagation, and non-token failures without refresh.

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/ewelink -run TestGateway -v`

Expected: FAIL because gateway does not exist.

- [ ] **Step 3: Implement exact interfaces**

```go
type TokenProvider interface {
    Access(context.Context) (string,string,error)
    ForceRefresh(context.Context) error
}
type DeviceClient interface {
    ListDevices(context.Context,string,string)([]Device,error)
    GetDevice(context.Context,string,string,string)(Device,error)
    SetSwitch(context.Context,string,string,string,string) error
}
type Gateway struct { Tokens TokenProvider; Client DeviceClient }
```

Each method obtains region/token, calls once, forces refresh only for `IsTokenError`, obtains the new token, and retries exactly once.

- [ ] **Step 4: Verify and commit**

Run: `gofmt -w internal/ewelink && go test ./internal/ewelink -run TestGateway -v && go test -race ./internal/ewelink`

Commit: `git commit -m "feat: retry cloud calls after token refresh"`

---

### Task 5: HTTP API and OAuth State

**Files:**
- Create: `internal/httpapi/api.go`
- Test: `internal/httpapi/api_test.go`

**Interfaces:**
- Consumes: gateway, OAuth URL/exchange, token setter/health, clock, logger.
- Produces: the approved `http.Handler`.

- [ ] **Step 1: Write failing REST contract tests**

Assert exact status and JSON for list, status, switch, and health. Cover invalid JSON, trailing JSON, unknown fields, `toggle`, invalid IDs, unsupported device, missing OAuth, offline, upstream unavailable, and no auth header.

- [ ] **Step 2: Verify REST tests fail**

Run: `go test ./internal/httpapi -run 'Test(Device|Switch|Health)' -v`

Expected: FAIL because `httpapi.New` does not exist.

- [ ] **Step 3: Implement REST routes**

```go
mux.HandleFunc("GET /healthz", api.health)
mux.HandleFunc("GET /oauth/start", api.oauthStart)
mux.HandleFunc("GET /callback", api.callback)
mux.HandleFunc("GET /api/v1/devices", api.devices)
mux.HandleFunc("GET /api/v1/devices/{device_id}/status", api.status)
mux.HandleFunc("PUT /api/v1/devices/{device_id}/switch", api.setSwitch)
```

Use `DisallowUnknownFields`, require decoder EOF, stable error envelopes, approved DTO fields only, and redacted logging. Map malformed input to 400, not found to 404, unsupported to 409, upstream/device failure to 502, and OAuth/unavailable/timeout to 503.

- [ ] **Step 4: Write failing OAuth handler tests**

Cover redirect, unique state, missing/unknown/expired/replayed state, exchange failure, persistence failure, and success HTML. Assert no sensitive value is returned or logged.

- [ ] **Step 5: Implement OAuth state lifecycle**

Store 32-byte base64url states in a mutex-protected in-memory map for 10 minutes. Consume before exchange. Callback validates `code`, `region`, and `state`, exchanges the code, and calls `TokenSetter.Set`. Per the owner's response-format decision, callback failures return the stable JSON error envelope and callback success returns only minimal HTML.

- [ ] **Step 6: Verify and commit**

Run: `gofmt -w internal/httpapi && go test ./internal/httpapi -v && go test -race ./internal/httpapi && go vet ./internal/httpapi`

Commit: `git commit -m "feat: expose oauth and device rest api"`

---

### Task 6: Process Wiring and Shutdown

**Files:**
- Create: `cmd/ewelink-lan-ctl/main.go`
- Test: `cmd/ewelink-lan-ctl/main_test.go`

**Interfaces:**
- Consumes: every internal package.
- Produces: runnable binary.

- [ ] **Step 1: Write failing construction smoke test**

Factor `buildHandler(config.Config, *http.Client, *slog.Logger) (http.Handler, *token.Manager, error)`. With a missing temp state file, assert `/healthz` returns `200` and `oauth_ready:false`.

- [ ] **Step 2: Verify failure**

Run: `go test ./cmd/ewelink-lan-ctl -v`

Expected: FAIL because wiring does not exist.

- [ ] **Step 3: Implement process composition**

Load config, create safe `slog`, file store, timeout HTTP client, raw eWeLink client, manager, gateway, and handler. Missing state is normal; malformed state is logged and leaves cloud calls unavailable. Use `signal.NotifyContext`, start scheduler, configure `ReadHeaderTimeout`, serve, and call `Shutdown` with a 10-second timeout. Treat `http.ErrServerClosed` as normal.

- [ ] **Step 4: Verify and commit**

Run: `gofmt -w cmd && go test ./... && go test -race ./... && go vet ./... && go build ./cmd/ewelink-lan-ctl`

Expected: PASS.

Commit: `git commit -m "feat: run ewelink cloud gateway service"`

---

### Task 7: Deployment and User Guide

**Files:**
- Create: `Dockerfile`
- Create: `compose.yaml`
- Create: `deploy/ewelink-lan-ctl.service`
- Create: `README.md`

**Interfaces:**
- Produces: native, systemd, Docker, OAuth, REST, and Shortcuts workflows.

- [ ] **Step 1: Add container assets**

Use `golang:1.22-bookworm` to test/build with `CGO_ENABLED=0`, then `gcr.io/distroless/static-debian12:nonroot` with CA certificates. Compose publishes `33998`, passes `.env`, stores `/data`, and uses `restart: unless-stopped`; do not use host networking.

- [ ] **Step 2: Add systemd unit**

Use `/etc/ewelink-lan-ctl.env`, `StateDirectory=ewelink-lan-ctl`, `/var/lib/ewelink-lan-ctl/state.json`, a dedicated `ewelink` user, restart-on-failure, and writable access restricted to the state directory.

- [ ] **Step 3: Document operation**

Document secret rotation, environment variables, Debian build, systemd, Compose, SSH tunnel OAuth, every `curl` endpoint, error JSON, no API authentication, and per-device Shortcuts:

```text
Below 20% -> PUT /api/v1/devices/<DEVICE_ID>/switch {"state":"on"}
Above 80% -> PUT /api/v1/devices/<DEVICE_ID>/switch {"state":"off"}
```

- [ ] **Step 4: Verify and commit**

Run:

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/ewelink-lan-ctl
docker build -t ewelink-lan-ctl:test .
docker compose config
```

Expected: all commands pass without real credentials.

Commit: `git commit -m "docs: add deployment and shortcuts guide"`

---

### Task 8: Final Security and Acceptance Verification

**Files:**
- Modify only files implicated by failures.

**Interfaces:**
- Produces: verified release candidate.

- [ ] **Step 1: Scan for secrets and forbidden LAN/cache implementation**

Use `rg` for the originally disclosed credentials, `deviceCache`, `zeroconf`, and `_ewelink._tcp`. Expected: disclosed values absent; no LAN implementation; OAuth field names appear only where required.

- [ ] **Step 2: Run complete verification**

Run Task 7 Step 4 plus `git diff --check`. Expected: all pass.

- [ ] **Step 3: Exercise redaction paths**

Against a fake upstream, exercise OAuth failure, token failure, malformed upstream JSON, and offline device. Expected: stable public errors and no credentials, OAuth code, signature, or raw upstream payload in bodies/logs.

- [ ] **Step 4: Commit only necessary fixes**

If changes were required: `git commit -m "fix: harden gateway error handling"`. Do not create an empty commit.
