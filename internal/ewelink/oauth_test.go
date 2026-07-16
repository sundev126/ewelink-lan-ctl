package ewelink

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestSignBase64OfficialExample(t *testing.T) {
	if got, want := signBase64("abc", "ABC_123"), "v1+mfNY2ukxswM8sZOTg99srZsVnUVv9DGXeav1096M="; got != want {
		t.Fatalf("signBase64() = %q, want %q", got, want)
	}
}

func TestRegionBaseURL(t *testing.T) {
	tests := map[string]string{
		"cn": "https://cn-apia.coolkit.cn",
		"as": "https://as-apia.coolkit.cc",
		"us": "https://us-apia.coolkit.cc",
		"eu": "https://eu-apia.coolkit.cc",
	}
	for region, want := range tests {
		t.Run(region, func(t *testing.T) {
			got, err := regionBaseURL(region)
			if err != nil || got != want {
				t.Fatalf("regionBaseURL(%q) = %q, %v; want %q, nil", region, got, err, want)
			}
		})
	}
}

func TestRegionBaseURLRejectsInvalidRegion(t *testing.T) {
	if _, err := regionBaseURL("moon"); err == nil {
		t.Fatal("regionBaseURL(moon) error = nil, want error")
	}
}

func TestAuthorizationURLFields(t *testing.T) {
	now := time.UnixMilli(1_721_111_222_333)
	client := NewClient(Config{
		AppID:       "ABC",
		AppSecret:   "abc",
		CallbackURL: "http://127.0.0.1:33998/callback?from=local",
		Now:         func() time.Time { return now },
	})

	raw, err := client.AuthorizationURL("state value")
	if err != nil {
		t.Fatalf("AuthorizationURL() error = %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if got, want := u.Scheme+"://"+u.Host+u.Path, "https://c2ccdn.coolkit.cc/oauth/index.html"; got != want {
		t.Fatalf("authorization endpoint = %q, want %q", got, want)
	}
	query := u.Query()
	wants := map[string]string{
		"clientId":      "ABC",
		"seq":           "1721111222333",
		"authorization": signBase64("abc", "ABC_1721111222333"),
		"redirectUrl":   "http://127.0.0.1:33998/callback?from=local",
		"grantType":     "authorization_code",
		"state":         "state value",
		"showQRCode":    "false",
	}
	for key, want := range wants {
		if got := query.Get(key); got != want {
			t.Errorf("query[%q] = %q, want %q", key, got, want)
		}
	}
	if got := query.Get("nonce"); len(got) != 8 {
		t.Errorf("nonce = %q, want 8 characters", got)
	}
}

func TestExchangeCodeSignsAndSendsExactBody(t *testing.T) {
	wantBody := `{"code":"code-value","redirectUrl":"http://callback/路径","grantType":"authorization_code"}`
	server := oauthServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/user/oauth/token" {
			t.Errorf("request = %s %s, want POST /v2/user/oauth/token", r.Method, r.URL.Path)
		}
		assertSignedHeaders(t, r, "app-id", "secret", wantBody)
		if got := string(body); got != wantBody {
			t.Errorf("body = %q, want %q", got, wantBody)
		}
		io.WriteString(w, `{"error":0,"msg":"","data":{"accessToken":"access","atExpiredTime":1721111222333,"refreshToken":"refresh","rtExpiredTime":1722222333444}}`)
	})

	client := testOAuthClient(server.URL, "http://callback/路径", time.Time{})
	got, err := client.ExchangeCode(context.Background(), "as", "code-value")
	if err != nil {
		t.Fatalf("ExchangeCode() error = %v", err)
	}
	if got.Region != "as" || got.AccessToken != "access" || got.RefreshToken != "refresh" {
		t.Fatalf("ExchangeCode() = %#v", got)
	}
	if want := time.UnixMilli(1_721_111_222_333); !got.AccessTokenExpiresAt.Equal(want) {
		t.Errorf("access expiry = %v, want %v", got.AccessTokenExpiresAt, want)
	}
	if want := time.UnixMilli(1_722_222_333_444); !got.RefreshTokenExpiresAt.Equal(want) {
		t.Errorf("refresh expiry = %v, want %v", got.RefreshTokenExpiresAt, want)
	}
}

func TestRefreshSignsExactBodyAndDerivesExpiries(t *testing.T) {
	wantBody := `{"rt":"old-refresh"}`
	server := oauthServer(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		if r.Method != http.MethodPost || r.URL.Path != "/v2/user/refresh" {
			t.Errorf("request = %s %s, want POST /v2/user/refresh", r.Method, r.URL.Path)
		}
		assertSignedHeaders(t, r, "app-id", "secret", wantBody)
		if got := string(body); got != wantBody {
			t.Errorf("body = %q, want %q", got, wantBody)
		}
		io.WriteString(w, `{"error":0,"msg":"","data":{"at":"new-access","rt":"new-refresh"}}`)
	})

	now := time.Date(2026, 7, 16, 2, 3, 4, 0, time.FixedZone("CST", 8*60*60))
	client := testOAuthClient(server.URL, "http://callback", now)
	got, err := client.Refresh(context.Background(), "eu", "old-refresh")
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if got.Region != "eu" || got.AccessToken != "new-access" || got.RefreshToken != "new-refresh" {
		t.Fatalf("Refresh() = %#v", got)
	}
	if want := now.Add(30 * 24 * time.Hour); !got.AccessTokenExpiresAt.Equal(want) {
		t.Errorf("access expiry = %v, want %v", got.AccessTokenExpiresAt, want)
	}
	if want := now.Add(60 * 24 * time.Hour); !got.RefreshTokenExpiresAt.Equal(want) {
		t.Errorf("refresh expiry = %v, want %v", got.RefreshTokenExpiresAt, want)
	}
}

func TestExchangeReturnsUpstreamErrorEnvelope(t *testing.T) {
	server := oauthServer(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":401,"msg":"invalid token","data":{}}`)
	})
	client := testOAuthClient(server.URL, "http://callback", time.Time{})

	_, err := client.ExchangeCode(context.Background(), "us", "bad-code")
	var upstream *UpstreamError
	if !errors.As(err, &upstream) {
		t.Fatalf("ExchangeCode() error = %T %v, want *UpstreamError", err, err)
	}
	if upstream.HTTPStatus != http.StatusUnauthorized || upstream.Code != 401 || upstream.Message != "invalid token" {
		t.Fatalf("upstream error = %#v", upstream)
	}
	if !IsTokenError(err) {
		t.Fatal("IsTokenError() = false, want true")
	}
}

func TestRefreshUsesReturnedMillisecondExpiries(t *testing.T) {
	server := oauthServer(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		io.WriteString(w, `{"error":0,"msg":"","data":{"at":"at","rt":"rt","atExpiredTime":1721111222333,"rtExpiredTime":1722222333444}}`)
	})
	client := testOAuthClient(server.URL, "http://callback", time.Unix(0, 0))
	got, err := client.Refresh(context.Background(), "cn", "rt")
	if err != nil {
		t.Fatalf("Refresh() error = %v", err)
	}
	if !got.AccessTokenExpiresAt.Equal(time.UnixMilli(1_721_111_222_333)) || !got.RefreshTokenExpiresAt.Equal(time.UnixMilli(1_722_222_333_444)) {
		t.Fatalf("Refresh() expiries = %v, %v", got.AccessTokenExpiresAt, got.RefreshTokenExpiresAt)
	}
}

func TestExchangeRejectsIncompleteSuccessData(t *testing.T) {
	server := oauthServer(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		io.WriteString(w, `{"error":0,"msg":"","data":{"accessToken":"access","refreshToken":""}}`)
	})
	if _, err := testOAuthClient(server.URL, "http://callback", time.Time{}).ExchangeCode(context.Background(), "as", "code"); err == nil {
		t.Fatal("ExchangeCode() error = nil, want incomplete success data error")
	}
}

func TestRefreshRejectsIncompleteSuccessData(t *testing.T) {
	server := oauthServer(t, func(w http.ResponseWriter, _ *http.Request, _ []byte) {
		io.WriteString(w, `{"error":0,"msg":"","data":{"at":"","rt":"refresh"}}`)
	})
	if _, err := testOAuthClient(server.URL, "http://callback", time.Time{}).Refresh(context.Background(), "as", "refresh"); err == nil {
		t.Fatal("Refresh() error = nil, want incomplete success data error")
	}
}

func oauthServer(t *testing.T, handler func(http.ResponseWriter, *http.Request, []byte)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		handler(w, r, body)
	}))
}

func assertSignedHeaders(t *testing.T, r *http.Request, appID, secret, body string) {
	t.Helper()
	if got, want := r.Header.Get("X-CK-Appid"), appID; got != want {
		t.Errorf("X-CK-Appid = %q, want %q", got, want)
	}
	if got, want := r.Header.Get("Authorization"), "Sign "+signBase64(secret, body); got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if got := r.Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func testOAuthClient(baseURL, callback string, now time.Time) *Client {
	return NewClient(Config{
		AppID:           "app-id",
		AppSecret:       "secret",
		CallbackURL:     callback,
		HTTPClient:      http.DefaultClient,
		Now:             func() time.Time { return now },
		RegionBaseURL:   func(string) (string, error) { return baseURL, nil },
		RequestInterval: time.Nanosecond,
	})
}

func decodeJSONBody(t *testing.T, body []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(body, target); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v", body, err)
	}
}
