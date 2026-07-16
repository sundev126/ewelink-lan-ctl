# eWeLink Cloud Gateway Design

## Purpose

Build a Go service for a Debian 12 server that lets multiple iPhone Shortcuts control separate eWeLink single-channel smart plugs. Each Shortcut supplies its plug's `device_id`. The service exposes an unauthenticated REST API and controls devices through the eWeLink v2 cloud API.

The service does not implement LAN discovery or LAN device control. Deployment networking determines whether the unauthenticated API is reachable from a LAN, VPN, or the public internet.

## Scope

The first release supports:

- One authorized eWeLink account.
- Single-channel plugs exposing the `params.switch` capability.
- Listing supported devices, querying one device's current state, and explicitly setting it to `on` or `off`.
- One-time OAuth authorization and automatic token refresh.
- Native Go binary, systemd instructions, Docker image, and Docker Compose example.
- JSON persistence for OAuth credentials only.

The first release does not support:

- LAN/mDNS discovery or `/zeroconf` control.
- Multiple eWeLink accounts.
- Multi-channel devices, groups, scenes, lights, or sensors.
- `toggle` operations.
- Authentication, authorization, IP allowlists, or rate limits on the public REST API.
- Device-list or device-state caching.
- An iOS application.

## Architecture

The program consists of focused packages:

- `cmd/ewelink-lan-ctl`: process entry point, dependency construction, HTTP startup, and graceful shutdown.
- `internal/config`: environment-variable parsing and validation.
- `internal/ewelink`: eWeLink OAuth signing and v2 REST client.
- `internal/token`: token lifecycle, scheduled refresh, request-time refresh, and concurrent refresh coalescing.
- `internal/store`: atomic JSON credential persistence.
- `internal/httpapi`: public REST, health, and OAuth handlers.

Although the executable retains the repository name `ewelink-lan-ctl`, it is a cloud-only gateway in this release.

## Configuration

Required environment variables:

```dotenv
EWELINK_APP_ID=<developer application id>
EWELINK_APP_SECRET=<developer application secret>
```

Environment variables with defaults:

```dotenv
EWELINK_CALLBACK_URL=http://127.0.0.1:33998/callback
EWELINK_LISTEN_ADDR=:33998
EWELINK_STATE_FILE=./data/state.json
```

Request timeout, token check interval, and refresh-ahead duration are configurable and default to 10 seconds, 12 hours, and 7 days respectively. A request-time safety window of 5 minutes prevents use of a token that is about to expire.

The App ID and App Secret are never written to the state file. They are supplied through the environment. The repository includes only a placeholder `.env.example`; `.env` and `data/` are ignored by Git.

## OAuth Flow

The App ID and App Secret identify the developer application but do not authorize access to a user's devices. The service therefore completes eWeLink's authorization-code flow once for the single account.

1. `GET /oauth/start` creates a cryptographically random, short-lived OAuth `state`, generates the eWeLink authorization signature, and redirects the browser to the eWeLink authorization page.
2. The user signs in and authorizes the application.
3. eWeLink redirects to `/callback` with `code`, `region`, and `state`.
4. The callback rejects missing, expired, unknown, or already-used state values.
5. It exchanges the short-lived code for access and refresh tokens using the regional API host.
6. The service atomically persists the region, tokens, and expiry timestamps.
7. The callback displays a minimal success or failure page without displaying sensitive values.

The target is a headless Debian 12 server while the registered callback is loopback. Initial authorization therefore uses a client-side SSH tunnel:

```bash
ssh -L 33998:127.0.0.1:33998 user@server
```

With the tunnel active, the local browser opens `http://127.0.0.1:33998/oauth/start`. Both the start request and callback travel through the tunnel to the service. The tunnel is unnecessary after successful authorization unless reauthorization is required.

OAuth `state` validation protects account binding and is independent of the deliberate absence of caller authentication on `/api/v1`.

## Credential Persistence

The JSON file contains only the minimum durable OAuth state:

```json
{
  "region": "as",
  "access_token": "<redacted>",
  "access_token_expires_at": "2026-08-15T00:00:00Z",
  "refresh_token": "<redacted>",
  "refresh_token_expires_at": "2026-09-14T00:00:00Z"
}
```

Writes use a temporary file in the same directory, file synchronization, mode `0600`, atomic rename, and directory synchronization. A failed write leaves the previous valid file intact. Logs never include tokens, the App Secret, authorization codes, or signatures.

No device metadata or state is persisted or cached. Device and status responses always originate from a current eWeLink request.

## Token Lifecycle

The token manager provides four safeguards:

1. On startup, it refreshes an access token that is already expired or within the configured refresh-ahead window.
2. A background task checks every 12 hours and refreshes when the access token expires within 7 days.
3. Before each cloud call, it refreshes if fewer than 5 minutes remain.
4. If eWeLink returns token error `401` or `402`, it forces one refresh and retries the original operation exactly once.

Concurrent requests share one refresh operation. Waiting requests receive the same result instead of issuing parallel refreshes. A successful refresh replaces both tokens and atomically persists the returned expiries. A failed refresh never overwrites the last stored credentials.

Transient refresh errors are logged and retried by future checks or requests. When the refresh token is expired or rejected, the service remains running, marks itself unauthenticated, and requires a new `/oauth/start` flow.

## Cloud Client

The client selects the regional base URL returned by OAuth:

- `cn`: `https://cn-apia.coolkit.cn`
- `as`: `https://as-apia.coolkit.cc`
- `us`: `https://us-apia.coolkit.cc`
- `eu`: `https://eu-apia.coolkit.cc`

All calls use context-aware HTTP requests, a configurable timeout, the required eWeLink headers, and strict response decoding. A process-wide limiter spaces eWeLink requests by at least 500 milliseconds. Concurrent iPhone calls wait for the limiter or terminate if their request context is canceled.

The device-list operation follows pagination until all returned pages are consumed. It includes only device items whose current parameters contain a scalar `switch` value of `on` or `off`. Groups and devices exposing only `switches` are excluded.

Status reads use `POST /v2/device/thing` for the specified device so that one real-time response supplies both `online` and `params.switch`. Control writes send `{ "switch": "on" }` or `{ "switch": "off" }` through `POST /v2/device/thing/status` with `type=1`. The service never exposes `toggle`, making retries idempotent.

## Public REST API

All `/api/v1` routes are intentionally unauthenticated.

### List devices

`GET /api/v1/devices`

```json
{
  "devices": [
    {
      "device_id": "1000123456",
      "name": "iPhone 1 Charger",
      "online": true,
      "state": "off",
      "uiid": 1,
      "model": "example-model"
    }
  ]
}
```

### Get status

`GET /api/v1/devices/{device_id}/status`

```json
{
  "device_id": "1000123456",
  "online": true,
  "state": "off"
}
```

### Set switch state

`PUT /api/v1/devices/{device_id}/switch`

```json
{
  "state": "on"
}
```

Success:

```json
{
  "success": true,
  "device_id": "1000123456",
  "state": "on"
}
```

A successful response means eWeLink accepted and successfully processed the control request. The handler does not perform a second status read. Clients can use the status endpoint when confirmation is required.

### Health

`GET /healthz` reports whether the process is running and whether valid or refreshable OAuth credentials are available. It exposes no application credentials, tokens, account data, or device data.

## Error Contract

Errors use a stable envelope:

```json
{
  "error": {
    "code": "device_offline",
    "message": "device is offline"
  }
}
```

HTTP mapping:

- `400 Bad Request`: malformed JSON, invalid device ID syntax, or a state other than `on`/`off`.
- `404 Not Found`: eWeLink reports that the device does not exist or is not visible to the authorized account.
- `409 Conflict`: the target does not expose the supported single-channel `switch` capability.
- `502 Bad Gateway`: eWeLink rejects control, reports the device offline, returns malformed data, or otherwise fails an operation.
- `503 Service Unavailable`: OAuth has not completed, credentials cannot be refreshed, the regional service is unavailable, or the client request times out before reaching a definitive cloud result.

Specific stable error codes distinguish `oauth_required`, `invalid_request`, `device_not_found`, `unsupported_device`, `device_offline`, `ewelink_rejected`, `ewelink_unavailable`, and `internal_error`. Internal and upstream details are logged in redacted form but are not returned to callers.

## Runtime and Shutdown

Startup validates configuration, loads the credential file if present, starts the token scheduler, and then serves HTTP. A missing state file is a normal unauthenticated state; a malformed existing state file is reported prominently and leaves cloud operations unavailable rather than silently discarding credentials.

SIGINT and SIGTERM stop accepting new requests, cancel the scheduler, allow in-flight HTTP requests a bounded shutdown period, and then exit. Request cancellation propagates to rate-limiter waits and upstream HTTP calls.

## Deployment Deliverables

The repository includes:

- A Go module and production binary.
- `.env.example` with placeholders only.
- Debian 12 native build and execution instructions.
- A sample systemd unit using an external environment file and writable state directory.
- A multi-stage Dockerfile whose runtime image contains the binary and CA certificates.
- Docker Compose with port `33998` published and a persistent `/data` volume.
- Initial OAuth instructions using SSH local forwarding.
- iPhone Shortcut examples for each device ID: below 20 percent sends `on`; above 80 percent sends `off`.

## Testing and Acceptance

Automated tests cover:

- OAuth URL generation, HMAC-SHA256 signatures, nonce/state creation, state expiry, one-time use, and callback validation.
- Regional host selection and required request headers.
- Credential file creation, permissions, atomic replacement, and failed-write preservation.
- Startup, scheduled, request-time, forced, failed, and concurrent token refresh behavior using an injected clock.
- Device pagination, supported-device filtering, status decoding, switch requests, cloud error mapping, timeout, and cancellation using an `httptest` upstream.
- Public handlers for valid operations, malformed JSON, invalid state, unsupported devices, missing OAuth, and upstream failures.
- Graceful shutdown behavior where practical.

Verification commands are:

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/ewelink-lan-ctl
docker build .
```

Manual acceptance uses a rotated App Secret and a real account:

1. Start the binary on Debian 12 and establish the SSH tunnel.
2. Complete OAuth once and confirm the JSON credential file is created with mode `0600`.
3. List devices and copy each single-channel plug's `device_id` into its iPhone Shortcut.
4. Query each plug's current state.
5. Set each plug on and off and confirm physical behavior.
6. Restart the service and confirm no reauthorization is needed.
7. Exercise a forced near-expiry token in a controlled test and confirm refresh plus atomic persistence.
8. Confirm separate iPhones controlling separate device IDs do not interfere.

Real credentials are never committed or used by automated tests.
