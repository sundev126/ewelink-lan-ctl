# ewelink-lan-ctl

`ewelink-lan-ctl` is a small, cloud-only gateway for controlling eWeLink single-channel plugs from a Debian server or iPhone Shortcuts. It uses the eWeLink v2 cloud API; despite the repository name, it does not discover or control devices over the LAN.

> [!IMPORTANT]
> Before using this service, the owner must rotate the App Secret that was disclosed during project setup. Create a replacement in the eWeLink developer console, invalidate the disclosed value, and put only the replacement in the private runtime environment file. Never commit an App Secret, OAuth token, authorization code, or generated `.env` file.

## Security model

The entire service is intentionally unauthenticated. Anyone who can reach port `33998` can list the supported devices, turn them on or off, and call `/oauth/start` plus `/callback` to persistently rebind the service to another eWeLink account. Keep every route on a trusted LAN or VPN, or place the whole service behind an authenticated reverse proxy or equivalent access control. Do not publish port `33998` directly to the internet. OAuth's one-time `state` only protects callback correlation and replay; it does not authenticate callers or prevent an accessible client from starting a new account binding.

The service persists OAuth credentials in `state.json`. Protect that file as a secret. Device metadata and device state are not cached.

## Configuration

Copy the placeholder configuration and insert the newly rotated credentials:

```bash
cp .env.example .env
chmod 600 .env
```

Never reuse the disclosed App Secret. The supported variables are:

| Variable | Required/default | Purpose |
| --- | --- | --- |
| `EWELINK_APP_ID` | required | eWeLink developer application ID |
| `EWELINK_APP_SECRET` | required | newly rotated developer App Secret |
| `EWELINK_CALLBACK_URL` | `http://127.0.0.1:33998/callback` | registered OAuth redirect URL; it must exactly match the developer application |
| `EWELINK_LISTEN_ADDR` | `:33998` | HTTP listen address |
| `EWELINK_STATE_FILE` | `./data/state.json` | OAuth credential store |
| `EWELINK_REQUEST_TIMEOUT` | `10s` | cloud request and HTTP header timeout |
| `EWELINK_TOKEN_CHECK_INTERVAL` | `12h` | background token check interval |
| `EWELINK_TOKEN_REFRESH_AHEAD` | `168h` | refresh an access token this far before expiry |

Durations use Go duration syntax, such as `10s`, `30m`, or `168h`, and must be positive.

## Native Debian 12 installation

Install CA certificates and Go 1.22 or newer, then build the binary. Debian 12's base `golang-go` package may be older than the version required by this module, so install a current Go release from [go.dev/dl](https://go.dev/dl/) rather than relying on that package, and confirm the version before building.

```bash
sudo apt update
sudo apt install -y ca-certificates
go version  # must report go1.22 or newer
go test ./...
CGO_ENABLED=0 go build -trimpath -o ewelink-lan-ctl ./cmd/ewelink-lan-ctl
```

For a foreground test, make the state directory and load the private environment:

```bash
mkdir -p data
set -a
. ./.env
set +a
./ewelink-lan-ctl
```

### systemd

Build as above, then install the binary and unit. The supplied unit runs as a dedicated `ewelink` user, creates `/var/lib/ewelink-lan-ctl` with mode `0700`, and restricts writable filesystem access to that state directory.

```bash
sudo install -o root -g root -m 0755 ewelink-lan-ctl /usr/local/bin/ewelink-lan-ctl
sudo useradd --system --home-dir /var/lib/ewelink-lan-ctl --shell /usr/sbin/nologin ewelink
sudo install -o root -g root -m 0644 deploy/ewelink-lan-ctl.service /etc/systemd/system/ewelink-lan-ctl.service
sudo install -o root -g root -m 0600 .env /etc/ewelink-lan-ctl.env
sudo systemctl daemon-reload
sudo systemctl enable --now ewelink-lan-ctl
sudo systemctl status ewelink-lan-ctl
```

The unit forces `EWELINK_STATE_FILE=/var/lib/ewelink-lan-ctl/state.json` on its command line, so a value copied from `.env` cannot redirect the credential file outside the protected state directory. Inspect redacted structured logs with:

```bash
sudo journalctl -u ewelink-lan-ctl -f
```

After changing `/etc/ewelink-lan-ctl.env`, restart the service with `sudo systemctl restart ewelink-lan-ctl`.

## Docker Compose

Docker builds and tests the static binary with Go 1.22 on Debian Bookworm, then runs it as a nonroot user in the Debian 12 distroless image with CA certificates. The Docker build context excludes `.env`, local credentials, and generated state. Compose uses normal port publishing—not host networking—and persists credentials in a named volume mounted at `/data`.

```bash
cp .env.example .env
chmod 600 .env
# Edit .env and insert the newly rotated App Secret.
docker compose up -d --build
docker compose logs -f ewelink-lan-ctl
```

Compose forces `EWELINK_LISTEN_ADDR=:33998` and `EWELINK_STATE_FILE=/data/state.json`, regardless of those two values in `.env`. Stop the container without deleting credentials using `docker compose down`. Do not add `--volumes` unless you intend to erase the saved OAuth credentials.

## Initial OAuth authorization

Register `http://127.0.0.1:33998/callback` as the developer application's redirect URL and use the same value for `EWELINK_CALLBACK_URL`. On the computer where you will use a browser, open an SSH local-forwarding session to the server and leave it running:

```bash
ssh -L 33998:127.0.0.1:33998 user@server
```

Then open this URL in that computer's browser:

```text
http://127.0.0.1:33998/oauth/start
```

The service redirects to eWeLink for sign-in and consent. eWeLink returns through the tunnel to `/callback`. A successful callback displays a minimal HTML confirmation; callback failures return the stable JSON error envelope described below. The tunnel can be closed after authorization and is needed again only for reauthorization.

For diagnostic purposes, this command shows the OAuth redirect headers, but a browser is required to complete sign-in:

```bash
curl -i http://127.0.0.1:33998/oauth/start
```

Do not invoke `/callback` manually: its `code`, `region`, and one-time `state` values are supplied by eWeLink.

## HTTP API

Set the base URL to the server's trusted-LAN or VPN address. No `Authorization` header, API key, or caller credential is accepted or required.

```bash
BASE_URL=http://server.lan:33998
```

Check process health and whether OAuth credentials are ready:

```bash
curl --fail-with-body "$BASE_URL/healthz"
# {"status":"ok","oauth_ready":true}
```

List every supported single-channel device and copy its `device_id`:

```bash
curl --fail-with-body "$BASE_URL/api/v1/devices"
# {"devices":[{"device_id":"1000123456","name":"iPhone Charger","online":true,"state":"off","uiid":1,"model":"example-model"}]}
```

Read one device's current status:

```bash
DEVICE_ID=1000123456
curl --fail-with-body "$BASE_URL/api/v1/devices/$DEVICE_ID/status"
# {"device_id":"1000123456","online":true,"state":"off"}
```

Turn that device on or off. Only the explicit states `on` and `off` are accepted; `toggle` is deliberately unsupported so retries are safe.

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

A successful switch response means eWeLink accepted and processed the command; query the status endpoint when a follow-up read is needed.

### JSON errors

All REST errors and OAuth callback failures use a stable JSON envelope:

```json
{
  "error": {
    "code": "device_offline",
    "message": "device is offline"
  }
}
```

Expected HTTP statuses are `400` for invalid input, `404` for a missing device or route, `409` for an unsupported device, `502` for a rejected/offline/malformed cloud operation, and `503` when OAuth or eWeLink is unavailable. Stable codes include `invalid_request`, `not_found`, `method_not_allowed`, `oauth_required`, `oauth_unavailable`, `device_not_found`, `unsupported_device`, `device_offline`, `ewelink_rejected`, `ewelink_unavailable`, and `internal_error`. Messages are safe for callers; inspect the service logs for redacted operational categories.

## iPhone automations

Create separate automations for each iPhone and its charger plug. First call `GET /api/v1/devices`, identify that plug, and keep its exact `device_id` in both automations. The iPhone must be able to reach the server over the trusted LAN or VPN.

For the charging automation in Shortcuts:

1. Create a personal **Battery Level** automation for **Falls Below 20%**.
2. Add **Get Contents of URL** with URL `http://server.lan:33998/api/v1/devices/<DEVICE_ID>/switch`.
3. Set Method to `PUT`, Request Body to `JSON`, and add text field `state` with value `on`.
4. Disable **Ask Before Running** if iOS offers that option.

For the stop-charging automation:

1. Create a personal **Battery Level** automation for **Rises Above 80%**.
2. Use the same per-device URL and settings, but set `state` to `off`.
3. Disable **Ask Before Running** if iOS offers that option.

In compact form, repeat this pair for each device ID:

```text
Below 20% -> PUT /api/v1/devices/<DEVICE_ID>/switch {"state":"on"}
Above 80% -> PUT /api/v1/devices/<DEVICE_ID>/switch {"state":"off"}
```

Do not reuse one plug's `device_id` for another iPhone.
