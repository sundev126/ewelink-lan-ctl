package ewelink

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
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

func (p *gatewayTokenProvider) ForceRefresh(ctx context.Context) error {
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
