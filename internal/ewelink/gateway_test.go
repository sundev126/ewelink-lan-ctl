package ewelink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zm/ewelink-lan-ctl/internal/store"
	"github.com/zm/ewelink-lan-ctl/internal/token"
)

type gatewayTokenProvider struct {
	accessResults []gatewayAccessResult
	refreshErr    error
	accessCalls   int
	refreshCalls  int
	contexts      []context.Context
}

type gatewayAccessResult struct {
	region string
	token  string
	err    error
}

func (p *gatewayTokenProvider) Access(ctx context.Context) (string, string, error) {
	p.contexts = append(p.contexts, ctx)
	p.accessCalls++
	result := p.accessResults[p.accessCalls-1]
	return result.region, result.token, result.err
}

func (p *gatewayTokenProvider) RefreshIfCurrent(ctx context.Context, _, _ string) error {
	p.contexts = append(p.contexts, ctx)
	p.refreshCalls++
	return p.refreshErr
}

type gatewayDeviceClient struct {
	listResults []gatewayListResult
	getResults  []gatewayGetResult
	setErrors   []error
	calls       []gatewayClientCall
}

type gatewayListResult struct {
	devices []Device
	err     error
}

type gatewayGetResult struct {
	device Device
	err    error
}

type gatewayClientCall struct {
	method   string
	ctx      context.Context
	region   string
	token    string
	deviceID string
	state    string
}

func (c *gatewayDeviceClient) ListDevices(ctx context.Context, region, token string) ([]Device, error) {
	c.calls = append(c.calls, gatewayClientCall{method: "list", ctx: ctx, region: region, token: token})
	result := c.listResults[len(c.calls)-1]
	return result.devices, result.err
}

func (c *gatewayDeviceClient) GetDevice(ctx context.Context, region, token, deviceID string) (Device, error) {
	c.calls = append(c.calls, gatewayClientCall{method: "get", ctx: ctx, region: region, token: token, deviceID: deviceID})
	result := c.getResults[len(c.calls)-1]
	return result.device, result.err
}

func (c *gatewayDeviceClient) SetSwitch(ctx context.Context, region, token, deviceID, state string) error {
	c.calls = append(c.calls, gatewayClientCall{method: "set", ctx: ctx, region: region, token: token, deviceID: deviceID, state: state})
	return c.setErrors[len(c.calls)-1]
}

func TestGatewaySuccessDoesNotRefresh(t *testing.T) {
	wantDevice := Device{DeviceID: "device-1", State: "on"}
	tests := []struct {
		name   string
		client *gatewayDeviceClient
		call   func(context.Context, *Gateway) error
	}{
		{
			name:   "list",
			client: &gatewayDeviceClient{listResults: []gatewayListResult{{devices: []Device{wantDevice}}}},
			call: func(ctx context.Context, gateway *Gateway) error {
				got, err := gateway.ListDevices(ctx)
				if !reflect.DeepEqual(got, []Device{wantDevice}) {
					t.Fatalf("ListDevices() = %#v, want device", got)
				}
				return err
			},
		},
		{
			name:   "get",
			client: &gatewayDeviceClient{getResults: []gatewayGetResult{{device: wantDevice}}},
			call: func(ctx context.Context, gateway *Gateway) error {
				got, err := gateway.GetDevice(ctx, "device-1")
				if got != wantDevice {
					t.Fatalf("GetDevice() = %#v, want %#v", got, wantDevice)
				}
				return err
			},
		},
		{
			name:   "set",
			client: &gatewayDeviceClient{setErrors: []error{nil}},
			call: func(ctx context.Context, gateway *Gateway) error {
				return gateway.SetSwitch(ctx, "device-1", "on")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), gatewayContextKey{}, tt.name)
			tokens := &gatewayTokenProvider{accessResults: []gatewayAccessResult{{region: "as", token: "old-token"}}}
			gateway := &Gateway{Tokens: tokens, Client: tt.client}
			if err := tt.call(ctx, gateway); err != nil {
				t.Fatalf("gateway call error = %v", err)
			}
			if tokens.accessCalls != 1 || tokens.refreshCalls != 0 {
				t.Fatalf("token calls = access %d, refresh %d; want 1, 0", tokens.accessCalls, tokens.refreshCalls)
			}
			if len(tt.client.calls) != 1 {
				t.Fatalf("client calls = %d, want 1", len(tt.client.calls))
			}
			assertGatewayClientCall(t, tt.client.calls[0], ctx, "as", "old-token")
		})
	}
}

func TestGatewayRefreshesAfterTokenErrorAndRetriesOnce(t *testing.T) {
	for _, code := range []int{401, 402} {
		for _, method := range []string{"list", "get", "set"} {
			t.Run(fmt.Sprintf("%s/%d", method, code), func(t *testing.T) {
				ctx := context.WithValue(context.Background(), gatewayContextKey{}, method)
				tokenErr := fmt.Errorf("raw client operation: %w", &UpstreamError{HTTPStatus: 200, Code: code, Message: "expired"})
				tokens := &gatewayTokenProvider{accessResults: []gatewayAccessResult{
					{region: "eu", token: "expired-token"},
					{region: "us", token: "fresh-token"},
				}}
				client := &gatewayDeviceClient{}
				gateway := &Gateway{Tokens: tokens, Client: client}
				var err error
				switch method {
				case "list":
					want := []Device{{DeviceID: "listed"}}
					client.listResults = []gatewayListResult{{err: tokenErr}, {devices: want}}
					var got []Device
					got, err = gateway.ListDevices(ctx)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("ListDevices() = %#v, want %#v", got, want)
					}
				case "get":
					want := Device{DeviceID: "device-1", State: "off"}
					client.getResults = []gatewayGetResult{{err: tokenErr}, {device: want}}
					var got Device
					got, err = gateway.GetDevice(ctx, "device-1")
					if got != want {
						t.Fatalf("GetDevice() = %#v, want %#v", got, want)
					}
				case "set":
					client.setErrors = []error{tokenErr, nil}
					err = gateway.SetSwitch(ctx, "device-1", "off")
				}
				if err != nil {
					t.Fatalf("gateway call error = %v", err)
				}
				if tokens.accessCalls != 2 || tokens.refreshCalls != 1 {
					t.Fatalf("token calls = access %d, refresh %d; want 2, 1", tokens.accessCalls, tokens.refreshCalls)
				}
				if len(client.calls) != 2 {
					t.Fatalf("client calls = %d, want 2", len(client.calls))
				}
				assertGatewayClientCall(t, client.calls[0], ctx, "eu", "expired-token")
				assertGatewayClientCall(t, client.calls[1], ctx, "us", "fresh-token")
				for i, callCtx := range tokens.contexts {
					if callCtx != ctx {
						t.Fatalf("token context %d was not preserved", i)
					}
				}
			})
		}
	}
}

func TestGatewaySetSwitchRetriesWholeProtocolAwareOperation(t *testing.T) {
	tests := []struct {
		name       string
		firstError string
		retryFails bool
		wantCode   int
		wantPaths  []string
		wantTokens []string
	}{
		{
			name:       "metadata read 401 then succeeds",
			firstError: "read",
			wantPaths:  []string{"/v2/device/thing", "/v2/device/thing", "/v2/device/thing/status"},
			wantTokens: []string{"Bearer expired-token", "Bearer fresh-token", "Bearer fresh-token"},
		},
		{
			name:       "status write 402 then re-reads and succeeds",
			firstError: "write",
			wantPaths:  []string{"/v2/device/thing", "/v2/device/thing/status", "/v2/device/thing", "/v2/device/thing/status"},
			wantTokens: []string{"Bearer expired-token", "Bearer expired-token", "Bearer fresh-token", "Bearer fresh-token"},
		},
		{
			name:       "metadata read second token error is returned",
			firstError: "read",
			retryFails: true,
			wantCode:   402,
			wantPaths:  []string{"/v2/device/thing", "/v2/device/thing"},
			wantTokens: []string{"Bearer expired-token", "Bearer fresh-token"},
		},
		{
			name:       "status write second token error is returned after re-read",
			firstError: "write",
			retryFails: true,
			wantCode:   401,
			wantPaths:  []string{"/v2/device/thing", "/v2/device/thing/status", "/v2/device/thing", "/v2/device/thing/status"},
			wantTokens: []string{"Bearer expired-token", "Bearer expired-token", "Bearer fresh-token", "Bearer fresh-token"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var paths []string
			var authorizations []string
			readCalls := 0
			writeCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				authorizations = append(authorizations, r.Header.Get("Authorization"))
				switch r.URL.Path {
				case "/v2/device/thing":
					readCalls++
					if tt.firstError == "read" && readCalls == 1 {
						io.WriteString(w, `{"error":401,"msg":"expired","data":{}}`)
						return
					}
					if tt.firstError == "read" && tt.retryFails && readCalls == 2 {
						io.WriteString(w, `{"error":402,"msg":"invalid","data":{}}`)
						return
					}
					io.WriteString(w, `{"error":0,"data":{"thingList":[{"itemType":1,"itemData":{"deviceid":"device-1","params":{"switches":[{"outlet":0,"switch":"off"}]}}}]}}`)
				case "/v2/device/thing/status":
					writeCalls++
					if tt.firstError == "write" && writeCalls == 1 {
						io.WriteString(w, `{"error":402,"msg":"expired","data":{}}`)
						return
					}
					if tt.firstError == "write" && tt.retryFails && writeCalls == 2 {
						io.WriteString(w, `{"error":401,"msg":"invalid","data":{}}`)
						return
					}
					io.WriteString(w, `{"error":0,"data":{}}`)
				default:
					t.Fatalf("unexpected request path %q", r.URL.Path)
				}
			}))
			defer server.Close()

			tokens := &gatewayTokenProvider{accessResults: []gatewayAccessResult{
				{region: "as", token: "expired-token"},
				{region: "as", token: "fresh-token"},
			}}
			gateway := &Gateway{Tokens: tokens, Client: testBearerClient(server.URL, time.Nanosecond)}
			err := gateway.SetSwitch(context.Background(), "device-1", "on")
			if tt.retryFails {
				var upstream *UpstreamError
				if !errors.As(err, &upstream) || upstream.Code != tt.wantCode {
					t.Fatalf("SetSwitch() error = %T %v, want second token error code %d", err, err, tt.wantCode)
				}
			} else if err != nil {
				t.Fatalf("SetSwitch() error = %v", err)
			}
			if tokens.accessCalls != 2 || tokens.refreshCalls != 1 {
				t.Fatalf("token calls = access %d, refresh %d; want 2, 1", tokens.accessCalls, tokens.refreshCalls)
			}
			if !reflect.DeepEqual(paths, tt.wantPaths) {
				t.Fatalf("request paths = %#v, want %#v", paths, tt.wantPaths)
			}
			if !reflect.DeepEqual(authorizations, tt.wantTokens) {
				t.Fatalf("request tokens = %#v, want %#v", authorizations, tt.wantTokens)
			}
		})
	}
}

func TestGatewaySecondTokenFailureIsReturnedWithoutAnotherRetry(t *testing.T) {
	firstErr := &UpstreamError{HTTPStatus: 200, Code: 401, Message: "expired"}
	secondErr := &UpstreamError{HTTPStatus: 200, Code: 402, Message: "invalid"}
	tokens := &gatewayTokenProvider{accessResults: []gatewayAccessResult{{region: "as", token: "old"}, {region: "eu", token: "new"}}}
	client := &gatewayDeviceClient{getResults: []gatewayGetResult{{err: firstErr}, {err: secondErr}}}

	_, err := (&Gateway{Tokens: tokens, Client: client}).GetDevice(context.Background(), "device-1")
	if err != secondErr {
		t.Fatalf("GetDevice() error = %v, want exact second token error", err)
	}
	if tokens.accessCalls != 2 || tokens.refreshCalls != 1 || len(client.calls) != 2 {
		t.Fatalf("calls = access %d, refresh %d, client %d; want 2, 1, 2", tokens.accessCalls, tokens.refreshCalls, len(client.calls))
	}
}

type lateTokenFailureClient struct {
	oldCalls       atomic.Int32
	secondOldReady chan struct{}
	releaseSecond  chan struct{}
}

func (c *lateTokenFailureClient) ListDevices(_ context.Context, _, accessToken string) ([]Device, error) {
	if accessToken == "fresh-token" {
		return []Device{{DeviceID: "device-1"}}, nil
	}
	if c.oldCalls.Add(1) == 2 {
		close(c.secondOldReady)
		<-c.releaseSecond
	} else {
		<-c.secondOldReady
	}
	return nil, &UpstreamError{Code: 401}
}

func (*lateTokenFailureClient) GetDevice(context.Context, string, string, string) (Device, error) {
	panic("unexpected GetDevice")
}

func (*lateTokenFailureClient) SetSwitch(context.Context, string, string, string, string) error {
	panic("unexpected SetSwitch")
}

func TestGatewayLateOldTokenFailureDoesNotRefreshAgain(t *testing.T) {
	now := time.Date(2026, time.July, 16, 12, 0, 0, 0, time.UTC)
	old := store.Credentials{
		Region: "us", AccessToken: "old-token", RefreshToken: "refresh-token",
		AccessTokenExpiresAt: now.Add(time.Hour), RefreshTokenExpiresAt: now.Add(24 * time.Hour),
	}
	fresh := old
	fresh.AccessToken = "fresh-token"
	fresh.AccessTokenExpiresAt = now.Add(2 * time.Hour)
	var refreshCalls atomic.Int32
	manager := token.New(nil, func(store.Credentials) error { return nil }, func(context.Context, store.Credentials) (store.Credentials, error) {
		refreshCalls.Add(1)
		return fresh, nil
	}, func() time.Time { return now }, 7*24*time.Hour)
	if err := manager.Set(old); err != nil {
		t.Fatal(err)
	}
	client := &lateTokenFailureClient{secondOldReady: make(chan struct{}), releaseSecond: make(chan struct{})}
	gateway := &Gateway{Tokens: manager, Client: client}

	start := make(chan struct{})
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for range 2 {
		go func() {
			ready.Done()
			<-start
			_, err := gateway.ListDevices(context.Background())
			results <- err
		}()
	}
	ready.Wait()
	close(start)
	<-client.secondOldReady
	select {
	case err := <-results:
		if err != nil {
			t.Fatalf("first ListDevices() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("first request did not finish refresh and retry")
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls before late response = %d, want 1", got)
	}
	close(client.releaseSecond)
	select {
	case err := <-results:
		if err != nil {
			t.Fatalf("second ListDevices() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("late-response request did not finish")
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

func TestGatewayRefreshOAuthRequiredIsPropagated(t *testing.T) {
	oauthRequired := errors.New("oauth authorization required")
	tokens := &gatewayTokenProvider{
		accessResults: []gatewayAccessResult{{region: "as", token: "expired"}},
		refreshErr:    oauthRequired,
	}
	client := &gatewayDeviceClient{setErrors: []error{&UpstreamError{Code: 401}}}

	err := (&Gateway{Tokens: tokens, Client: client}).SetSwitch(context.Background(), "device-1", "on")
	if err != oauthRequired {
		t.Fatalf("SetSwitch() error = %v, want exact OAuth-required error", err)
	}
	if tokens.accessCalls != 1 || tokens.refreshCalls != 1 || len(client.calls) != 1 {
		t.Fatalf("calls = access %d, refresh %d, client %d; want 1, 1, 1", tokens.accessCalls, tokens.refreshCalls, len(client.calls))
	}
}

func TestGatewayFreshAccessOAuthRequiredIsPropagated(t *testing.T) {
	oauthRequired := errors.New("oauth authorization required")
	tokens := &gatewayTokenProvider{
		accessResults: []gatewayAccessResult{{region: "as", token: "expired"}, {err: oauthRequired}},
	}
	client := &gatewayDeviceClient{listResults: []gatewayListResult{{err: &UpstreamError{Code: 402}}}}

	_, err := (&Gateway{Tokens: tokens, Client: client}).ListDevices(context.Background())
	if err != oauthRequired {
		t.Fatalf("ListDevices() error = %v, want exact OAuth-required error", err)
	}
	if tokens.accessCalls != 2 || tokens.refreshCalls != 1 || len(client.calls) != 1 {
		t.Fatalf("calls = access %d, refresh %d, client %d; want 2, 1, 1", tokens.accessCalls, tokens.refreshCalls, len(client.calls))
	}
}

func TestGatewayNonTokenFailureDoesNotRefresh(t *testing.T) {
	wantErr := errors.New("upstream unavailable")
	tokens := &gatewayTokenProvider{accessResults: []gatewayAccessResult{{region: "as", token: "token"}}}
	client := &gatewayDeviceClient{listResults: []gatewayListResult{{err: wantErr}}}

	_, err := (&Gateway{Tokens: tokens, Client: client}).ListDevices(context.Background())
	if err != wantErr {
		t.Fatalf("ListDevices() error = %v, want exact non-token error", err)
	}
	if tokens.accessCalls != 1 || tokens.refreshCalls != 0 || len(client.calls) != 1 {
		t.Fatalf("calls = access %d, refresh %d, client %d; want 1, 0, 1", tokens.accessCalls, tokens.refreshCalls, len(client.calls))
	}
}

type gatewayContextKey struct{}

func assertGatewayClientCall(t *testing.T, call gatewayClientCall, ctx context.Context, region, token string) {
	t.Helper()
	if call.ctx != ctx || call.region != region || call.token != token {
		t.Fatalf("client call = context %v, region %q, token %q; want original context, %q, %q", call.ctx == ctx, call.region, call.token, region, token)
	}
}
