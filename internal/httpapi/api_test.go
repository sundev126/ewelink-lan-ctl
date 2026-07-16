package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/zm/ewelink-lan-ctl/internal/ewelink"
	"github.com/zm/ewelink-lan-ctl/internal/store"
	"github.com/zm/ewelink-lan-ctl/internal/token"
)

var (
	errNotFound    = errors.New("device not found")
	errUnsupported = errors.New("unsupported device")
	errOffline     = errors.New("device offline")
)

type fakeGateway struct {
	devices  []ewelink.Device
	device   ewelink.Device
	err      error
	setID    string
	setState string
}

func (g *fakeGateway) ListDevices(context.Context) ([]ewelink.Device, error) { return g.devices, g.err }
func (g *fakeGateway) GetDevice(context.Context, string) (ewelink.Device, error) {
	return g.device, g.err
}
func (g *fakeGateway) SetSwitch(_ context.Context, id, state string) error {
	g.setID, g.setState = id, state
	return g.err
}

type fakeOAuth struct {
	url         string
	credentials store.Credentials
	urlErr      error
	exchangeErr error
	states      []string
	region      string
	code        string
}

func (o *fakeOAuth) AuthorizationURL(state string) (string, error) {
	o.states = append(o.states, state)
	if o.url == "" {
		return "https://auth.example/authorize?state=" + url.QueryEscape(state), o.urlErr
	}
	return o.url, o.urlErr
}
func (o *fakeOAuth) ExchangeCode(_ context.Context, region, code string) (store.Credentials, error) {
	o.region, o.code = region, code
	return o.credentials, o.exchangeErr
}

type fakeTokens struct {
	ready bool
	err   error
	set   store.Credentials
}

func (t *fakeTokens) Health() bool                            { return t.ready }
func (t *fakeTokens) Set(credentials store.Credentials) error { t.set = credentials; return t.err }

func testHandler(gateway *fakeGateway, tokens *fakeTokens, oauth *fakeOAuth, now func() time.Time, logs io.Writer) http.Handler {
	if gateway == nil {
		gateway = &fakeGateway{}
	}
	if tokens == nil {
		tokens = &fakeTokens{}
	}
	if oauth == nil {
		oauth = &fakeOAuth{}
	}
	if now == nil {
		now = time.Now
	}
	if logs == nil {
		logs = io.Discard
	}
	return New(Config{Gateway: gateway, OAuth: oauth, Tokens: tokens, Now: now, Logger: slog.New(slog.NewTextHandler(logs, nil))})
}

func request(t *testing.T, handler http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func assertResponse(t *testing.T, rec *httptest.ResponseRecorder, status int, body string) {
	t.Helper()
	if rec.Code != status {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, status, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.Body.String(); got != body {
		t.Fatalf("body = %q, want %q", got, body)
	}
}

func TestDevicesListContractAndNoAuthentication(t *testing.T) {
	gateway := &fakeGateway{devices: []ewelink.Device{{DeviceID: "1000AbCd", Name: "Charger", Online: true, State: "off", UIID: 1, Model: "M"}}}
	rec := request(t, testHandler(gateway, nil, nil, nil, nil), http.MethodGet, "/api/v1/devices", "")
	assertResponse(t, rec, http.StatusOK, "{\"devices\":[{\"device_id\":\"1000AbCd\",\"name\":\"Charger\",\"online\":true,\"state\":\"off\",\"uiid\":1,\"model\":\"M\"}]}\n")
}

func TestDeviceStatusContract(t *testing.T) {
	gateway := &fakeGateway{device: ewelink.Device{DeviceID: "abc123", Name: "secret name", Online: true, State: "on", UIID: 9, Model: "secret model"}}
	rec := request(t, testHandler(gateway, nil, nil, nil, nil), http.MethodGet, "/api/v1/devices/abc123/status", "")
	assertResponse(t, rec, http.StatusOK, "{\"device_id\":\"abc123\",\"online\":true,\"state\":\"on\"}\n")
}

func TestSwitchContract(t *testing.T) {
	gateway := &fakeGateway{}
	rec := request(t, testHandler(gateway, nil, nil, nil, nil), http.MethodPut, "/api/v1/devices/aB09/switch", `{"state":"off"}`)
	assertResponse(t, rec, http.StatusOK, "{\"success\":true,\"device_id\":\"aB09\",\"state\":\"off\"}\n")
	if gateway.setID != "aB09" || gateway.setState != "off" {
		t.Fatalf("SetSwitch = (%q, %q)", gateway.setID, gateway.setState)
	}
}

func TestSwitchRejectsMalformedBodiesAndInvalidIDs(t *testing.T) {
	tests := []struct{ name, path, body string }{
		{"invalid json", "/api/v1/devices/abc/switch", `{`},
		{"trailing json", "/api/v1/devices/abc/switch", `{"state":"on"}{}`},
		{"unknown field", "/api/v1/devices/abc/switch", `{"state":"on","token":"secret"}`},
		{"toggle", "/api/v1/devices/abc/switch", `{"state":"toggle"}`},
		{"slash-like id", "/api/v1/devices/%2e%2e/switch", `{"state":"on"}`},
		{"punctuated id", "/api/v1/devices/a.b/switch", `{"state":"on"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := request(t, testHandler(nil, nil, nil, nil, nil), http.MethodPut, tt.path, tt.body)
			assertResponse(t, rec, http.StatusBadRequest, "{\"error\":{\"code\":\"invalid_request\",\"message\":\"invalid request\"}}\n")
		})
	}
}

func TestDeviceAndSwitchErrorMappings(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		status        int
		code, message string
	}{
		{"missing oauth", token.ErrOAuthRequired, 503, "oauth_required", "OAuth authorization required"},
		{"not found", errNotFound, 404, "device_not_found", "device not found"},
		{"unsupported", errUnsupported, 409, "unsupported_device", "device does not support a single switch"},
		{"offline", errOffline, 502, "device_offline", "device is offline"},
		{"upstream not found", &ewelink.UpstreamError{HTTPStatus: 200, Code: 404, Message: "device not found"}, 404, "device_not_found", "device not found"},
		{"upstream http not found", &ewelink.UpstreamError{HTTPStatus: 404, Code: 0, Message: "redacted"}, 404, "device_not_found", "device not found"},
		{"upstream unsupported", &ewelink.UpstreamError{HTTPStatus: 200, Code: 409, Message: "redacted"}, 409, "unsupported_device", "device does not support a single switch"},
		{"upstream offline", &ewelink.UpstreamError{HTTPStatus: 200, Code: 400, Message: "device is offline"}, 502, "device_offline", "device is offline"},
		{"upstream unavailable", &ewelink.UpstreamError{HTTPStatus: 503, Code: 500, Message: "secret"}, 503, "ewelink_unavailable", "eWeLink service unavailable"},
		{"upstream code unavailable", &ewelink.UpstreamError{HTTPStatus: 200, Code: 503, Message: "secret"}, 503, "ewelink_unavailable", "eWeLink service unavailable"},
		{"upstream rejected", &ewelink.UpstreamError{HTTPStatus: 400, Code: 400, Message: "secret"}, 502, "ewelink_rejected", "eWeLink rejected request"},
		{"timeout", context.DeadlineExceeded, 503, "ewelink_unavailable", "eWeLink service unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := request(t, testHandler(&fakeGateway{err: tt.err}, nil, nil, nil, nil), http.MethodPut, "/api/v1/devices/abc/switch", `{"state":"on"}`)
			want := `{"error":{"code":"` + tt.code + `","message":"` + tt.message + `"}}` + "\n"
			assertResponse(t, rec, tt.status, want)
			if strings.Contains(rec.Body.String(), "secret") {
				t.Fatal("response leaked upstream detail")
			}
		})
	}
}

func TestSwitchRejectsOversizedBody(t *testing.T) {
	body := `{"state":"` + strings.Repeat("x", maxSwitchBodyBytes) + `"}`
	rec := request(t, testHandler(nil, nil, nil, nil, nil), http.MethodPut, "/api/v1/devices/abc/switch", body)
	assertResponse(t, rec, http.StatusBadRequest, "{\"error\":{\"code\":\"invalid_request\",\"message\":\"invalid request\"}}\n")
}

func TestDeviceRoutesRejectRawDotSegmentsWithoutRedirect(t *testing.T) {
	rec := request(t, testHandler(nil, nil, nil, nil, nil), http.MethodPut, "/api/v1/devices/../switch", `{"state":"on"}`)
	assertResponse(t, rec, http.StatusBadRequest, "{\"error\":{\"code\":\"invalid_request\",\"message\":\"invalid request\"}}\n")
}

func TestHealthContract(t *testing.T) {
	for _, ready := range []bool{false, true} {
		rec := request(t, testHandler(nil, &fakeTokens{ready: ready}, nil, nil, nil), http.MethodGet, "/healthz", "")
		assertResponse(t, rec, http.StatusOK, `{"status":"ok","oauth_ready":`+strings.ToLower(strings.TrimSpace(string(mustJSON(t, ready))))+"}\n")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.TrimSpace(b)
}

func startOAuth(t *testing.T, handler http.Handler) string {
	t.Helper()
	rec := request(t, handler, http.MethodGet, "/oauth/start", "")
	if rec.Code != http.StatusFound {
		t.Fatalf("oauth start status = %d, want 302; body = %s", rec.Code, rec.Body.String())
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return location.Query().Get("state")
}

func TestOAuthStartRedirectsWithUniqueRandomState(t *testing.T) {
	oauth := &fakeOAuth{}
	handler := testHandler(nil, nil, oauth, nil, nil)
	first := startOAuth(t, handler)
	second := startOAuth(t, handler)
	if first == second {
		t.Fatal("successive OAuth states are equal")
	}
	for _, state := range []string{first, second} {
		decoded, err := base64.RawURLEncoding.DecodeString(state)
		if err != nil || len(decoded) != 32 {
			t.Fatalf("state %q decoded to %d bytes, error %v; want 32 bytes of base64url", state, len(decoded), err)
		}
	}
	if len(oauth.states) != 2 || oauth.states[0] != first || oauth.states[1] != second {
		t.Fatalf("AuthorizationURL states = %#v", oauth.states)
	}
}

func TestOAuthCallbackRejectsMissingUnknownExpiredAndReplayedState(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	current := now
	oauth := &fakeOAuth{credentials: validCredentials(now)}
	tokens := &fakeTokens{}
	handler := testHandler(nil, tokens, oauth, func() time.Time { return current }, nil)

	for _, target := range []string{
		"/callback?region=us&code=secret-code",
		"/callback?region=us&code=secret-code&state=unknown",
		"/callback?region=invalid&code=secret-code&state=unknown",
		"/callback?region=us&state=unknown",
	} {
		rec := request(t, handler, http.MethodGet, target, "")
		assertResponse(t, rec, http.StatusBadRequest, "{\"error\":{\"code\":\"invalid_request\",\"message\":\"invalid OAuth callback\"}}\n")
	}

	expired := startOAuth(t, handler)
	current = now.Add(oauthStateLifetime)
	rec := request(t, handler, http.MethodGet, "/callback?region=us&code=secret-code&state="+url.QueryEscape(expired), "")
	assertResponse(t, rec, http.StatusBadRequest, "{\"error\":{\"code\":\"invalid_request\",\"message\":\"invalid OAuth callback\"}}\n")

	current = now
	state := startOAuth(t, handler)
	success := request(t, handler, http.MethodGet, "/callback?region=us&code=secret-code&state="+url.QueryEscape(state), "")
	if success.Code != http.StatusOK {
		t.Fatalf("first callback status = %d, body = %s", success.Code, success.Body.String())
	}
	replay := request(t, handler, http.MethodGet, "/callback?region=us&code=secret-code&state="+url.QueryEscape(state), "")
	assertResponse(t, replay, http.StatusBadRequest, "{\"error\":{\"code\":\"invalid_request\",\"message\":\"invalid OAuth callback\"}}\n")
}

func TestOAuthCallbackConsumesStateBeforeFailedExchange(t *testing.T) {
	oauth := &fakeOAuth{exchangeErr: errors.New("exchange failed with secret-code")}
	logs := new(bytes.Buffer)
	handler := testHandler(nil, nil, oauth, nil, logs)
	state := startOAuth(t, handler)
	target := "/callback?region=eu&code=secret-code&state=" + url.QueryEscape(state)
	first := request(t, handler, http.MethodGet, target, "")
	assertResponse(t, first, http.StatusBadGateway, "{\"error\":{\"code\":\"ewelink_rejected\",\"message\":\"eWeLink rejected authorization\"}}\n")
	second := request(t, handler, http.MethodGet, target, "")
	assertResponse(t, second, http.StatusBadRequest, "{\"error\":{\"code\":\"invalid_request\",\"message\":\"invalid OAuth callback\"}}\n")
	if strings.Contains(first.Body.String()+logs.String(), "secret-code") {
		t.Fatal("authorization code leaked in body or logs")
	}
}

func TestOAuthCallbackPersistenceFailureIsRedacted(t *testing.T) {
	now := time.Now()
	credentials := validCredentials(now)
	oauth := &fakeOAuth{credentials: credentials}
	tokens := &fakeTokens{err: errors.New("save failed for refresh-secret")}
	logs := new(bytes.Buffer)
	handler := testHandler(nil, tokens, oauth, nil, logs)
	state := startOAuth(t, handler)
	rec := request(t, handler, http.MethodGet, "/callback?region=as&code=one-time-secret&state="+url.QueryEscape(state), "")
	assertResponse(t, rec, http.StatusInternalServerError, "{\"error\":{\"code\":\"internal_error\",\"message\":\"could not save authorization\"}}\n")
	if strings.Contains(rec.Body.String()+logs.String(), "one-time-secret") || strings.Contains(rec.Body.String()+logs.String(), "refresh-secret") {
		t.Fatal("sensitive value leaked in body or logs")
	}
}

func TestOAuthCallbackSuccessHTMLAndCredentials(t *testing.T) {
	now := time.Now()
	credentials := validCredentials(now)
	oauth := &fakeOAuth{credentials: credentials}
	tokens := &fakeTokens{}
	handler := testHandler(nil, tokens, oauth, nil, nil)
	state := startOAuth(t, handler)
	rec := request(t, handler, http.MethodGet, "/callback?region=cn&code=authorization-code&state="+url.QueryEscape(state), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	const want = "<!doctype html><html><body>Authorization complete. You may close this window.</body></html>\n"
	if rec.Body.String() != want {
		t.Fatalf("body = %q, want %q", rec.Body.String(), want)
	}
	if oauth.region != "cn" || oauth.code != "authorization-code" {
		t.Fatalf("ExchangeCode = (%q, %q)", oauth.region, oauth.code)
	}
	if tokens.set != credentials {
		t.Fatalf("Set credentials = %#v, want %#v", tokens.set, credentials)
	}
	if strings.Contains(rec.Body.String(), "authorization-code") || strings.Contains(rec.Body.String(), credentials.AccessToken) {
		t.Fatal("success HTML leaked credentials")
	}
}

func validCredentials(now time.Time) store.Credentials {
	return store.Credentials{
		Region: "us", AccessToken: "access-secret", RefreshToken: "refresh-secret",
		AccessTokenExpiresAt: now.Add(time.Hour), RefreshTokenExpiresAt: now.Add(24 * time.Hour),
	}
}
