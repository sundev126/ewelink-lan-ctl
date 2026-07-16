package ewelink

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zm/ewelink-lan-ctl/internal/store"
)

const authorizationEndpoint = "https://c2ccdn.coolkit.cc/oauth/index.html"

type Config struct {
	AppID         string
	AppSecret     string
	CallbackURL   string
	HTTPClient    *http.Client
	Now           func() time.Time
	RegionBaseURL func(string) (string, error)
}

type Client struct {
	appID       string
	appSecret   string
	callbackURL string
	httpClient  *http.Client
	now         func() time.Time
	baseURL     func(string) (string, error)
	gate        *requestGate
}

type requestGate struct {
	token         chan struct{}
	mu            sync.Mutex
	lastRequestAt time.Time
	interval      time.Duration
	now           func() time.Time
}

type optionalMillis struct {
	present bool
	null    bool
	value   int64
}

func (m *optionalMillis) UnmarshalJSON(data []byte) error {
	m.present = true
	if bytes.Equal(data, []byte("null")) {
		m.null = true
		return nil
	}
	return json.Unmarshal(data, &m.value)
}

var productionRequestGate = newRequestGate(500*time.Millisecond, time.Now)

func NewClient(config Config) *Client {
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	baseURL := config.RegionBaseURL
	if baseURL == nil {
		baseURL = regionBaseURL
	}
	return &Client{
		appID:       config.AppID,
		appSecret:   config.AppSecret,
		callbackURL: config.CallbackURL,
		httpClient:  httpClient,
		now:         now,
		baseURL:     baseURL,
		gate:        productionRequestGate,
	}
}

func signBase64(key, message string) string {
	mac := hmac.New(sha256.New, []byte(key))
	_, _ = mac.Write([]byte(message))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

func regionBaseURL(region string) (string, error) {
	switch region {
	case "cn":
		return "https://cn-apia.coolkit.cn", nil
	case "as":
		return "https://as-apia.coolkit.cc", nil
	case "us":
		return "https://us-apia.coolkit.cc", nil
	case "eu":
		return "https://eu-apia.coolkit.cc", nil
	default:
		return "", fmt.Errorf("unsupported ewelink region %q", region)
	}
}

func (c *Client) AuthorizationURL(state string) (string, error) {
	seq := strconv.FormatInt(c.now().UnixMilli(), 10)
	nonce, err := randomNonce()
	if err != nil {
		return "", fmt.Errorf("generate oauth nonce: %w", err)
	}
	query := url.Values{
		"clientId":      {c.appID},
		"seq":           {seq},
		"authorization": {signBase64(c.appSecret, c.appID+"_"+seq)},
		"redirectUrl":   {c.callbackURL},
		"grantType":     {"authorization_code"},
		"state":         {state},
		"nonce":         {nonce},
		"showQRCode":    {"false"},
	}
	return authorizationEndpoint + "?" + query.Encode(), nil
}

func (c *Client) ExchangeCode(ctx context.Context, region, code string) (store.Credentials, error) {
	body, err := json.Marshal(struct {
		Code        string `json:"code"`
		RedirectURL string `json:"redirectUrl"`
		GrantType   string `json:"grantType"`
	}{Code: code, RedirectURL: c.callbackURL, GrantType: "authorization_code"})
	if err != nil {
		return store.Credentials{}, fmt.Errorf("encode oauth exchange: %w", err)
	}
	var response struct {
		AccessToken           string `json:"accessToken"`
		AccessTokenExpiresAt  int64  `json:"atExpiredTime"`
		RefreshToken          string `json:"refreshToken"`
		RefreshTokenExpiresAt int64  `json:"rtExpiredTime"`
	}
	if err := c.signedPost(ctx, region, "/v2/user/oauth/token", body, &response); err != nil {
		return store.Credentials{}, fmt.Errorf("exchange oauth code: %w", err)
	}
	if response.AccessToken == "" || response.RefreshToken == "" || response.AccessTokenExpiresAt <= 0 || response.RefreshTokenExpiresAt <= 0 {
		return store.Credentials{}, fmt.Errorf("exchange oauth code: incomplete credential data")
	}
	return store.Credentials{
		Region:                region,
		AccessToken:           response.AccessToken,
		AccessTokenExpiresAt:  time.UnixMilli(response.AccessTokenExpiresAt).UTC(),
		RefreshToken:          response.RefreshToken,
		RefreshTokenExpiresAt: time.UnixMilli(response.RefreshTokenExpiresAt).UTC(),
	}, nil
}

func (c *Client) Refresh(ctx context.Context, region, refreshToken string) (store.Credentials, error) {
	body, err := json.Marshal(struct {
		RefreshToken string `json:"rt"`
	}{RefreshToken: refreshToken})
	if err != nil {
		return store.Credentials{}, fmt.Errorf("encode token refresh: %w", err)
	}
	var response struct {
		AccessToken           string         `json:"at"`
		RefreshToken          string         `json:"rt"`
		AccessTokenExpiresAt  optionalMillis `json:"atExpiredTime"`
		RefreshTokenExpiresAt optionalMillis `json:"rtExpiredTime"`
	}
	if err := c.signedPost(ctx, region, "/v2/user/refresh", body, &response); err != nil {
		return store.Credentials{}, fmt.Errorf("refresh oauth token: %w", err)
	}
	if response.AccessToken == "" || response.RefreshToken == "" ||
		invalidOptionalExpiry(response.AccessTokenExpiresAt) || invalidOptionalExpiry(response.RefreshTokenExpiresAt) {
		return store.Credentials{}, fmt.Errorf("refresh oauth token: incomplete credential data")
	}
	now := c.now()
	accessExpiry := now.Add(30 * 24 * time.Hour)
	if response.AccessTokenExpiresAt.present {
		accessExpiry = time.UnixMilli(response.AccessTokenExpiresAt.value)
	}
	refreshExpiry := now.Add(60 * 24 * time.Hour)
	if response.RefreshTokenExpiresAt.present {
		refreshExpiry = time.UnixMilli(response.RefreshTokenExpiresAt.value)
	}
	return store.Credentials{
		Region:                region,
		AccessToken:           response.AccessToken,
		AccessTokenExpiresAt:  accessExpiry.UTC(),
		RefreshToken:          response.RefreshToken,
		RefreshTokenExpiresAt: refreshExpiry.UTC(),
	}, nil
}

func invalidOptionalExpiry(expiry optionalMillis) bool {
	return expiry.present && (expiry.null || expiry.value <= 0)
}

func (c *Client) signedPost(ctx context.Context, region, path string, body []byte, target any) error {
	return c.do(ctx, region, http.MethodPost, path, body, "Sign "+signBase64(c.appSecret, string(body)), target)
}

func (c *Client) do(ctx context.Context, region, method, path string, body []byte, authorization string, target any) error {
	baseURL, err := c.baseURL(region)
	if err != nil {
		return err
	}
	if err := c.gate.wait(ctx); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create ewelink request: %w", err)
	}
	req.Header.Set("Authorization", authorization)
	req.Header.Set("X-CK-Appid", c.appID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("send ewelink request: %w", err)
	}
	defer response.Body.Close()

	var envelope struct {
		Error json.RawMessage `json:"error"`
		Msg   string          `json:"msg"`
		Data  json.RawMessage `json:"data"`
	}
	decoder := json.NewDecoder(response.Body)
	if err := decoder.Decode(&envelope); err != nil {
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return &UpstreamError{HTTPStatus: response.StatusCode, Message: response.Status}
		}
		return fmt.Errorf("decode ewelink response: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode ewelink response: unexpected trailing JSON value")
		}
		return fmt.Errorf("decode ewelink response trailing data: %w", err)
	}
	if len(envelope.Error) == 0 || bytes.Equal(envelope.Error, []byte("null")) {
		return fmt.Errorf("decode ewelink response: missing numeric error field")
	}
	var errorCode int
	if err := json.Unmarshal(envelope.Error, &errorCode); err != nil {
		return fmt.Errorf("decode ewelink response error field: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 || errorCode != 0 {
		message := envelope.Msg
		if message == "" {
			message = response.Status

		}
		return &UpstreamError{HTTPStatus: response.StatusCode, Code: errorCode, Message: message}
	}
	if target != nil {
		if err := json.Unmarshal(envelope.Data, target); err != nil {
			return fmt.Errorf("decode ewelink response data: %w", err)
		}
	}
	return nil
}

func newRequestGate(interval time.Duration, now func() time.Time) *requestGate {
	token := make(chan struct{}, 1)
	token <- struct{}{}
	return &requestGate{token: token, interval: interval, now: now}
}

func (g *requestGate) wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.token:
	}
	defer func() { g.token <- struct{}{} }()

	g.mu.Lock()
	wait := g.lastRequestAt.Add(g.interval).Sub(g.now())
	g.mu.Unlock()
	if wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	g.mu.Lock()
	g.lastRequestAt = g.now()
	g.mu.Unlock()
	return nil
}

func randomNonce() (string, error) {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	data := make([]byte, 8)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	for index := range data {
		data[index] = alphabet[int(data[index])%len(alphabet)]
	}
	return string(data), nil
}
