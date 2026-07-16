package ewelink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientListDevicesPaginatesAndFiltersUnsupportedThings(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearerHeaders(t, r)
		if r.Method != http.MethodGet || r.URL.Path != "/v2/device/thing" || r.URL.Query().Get("num") != "30" {
			t.Errorf("request = %s %s, want GET /v2/device/thing?num=30", r.Method, r.URL.String())
		}
		switch requests.Add(1) {
		case 1:
			if got := r.URL.Query().Get("beginIndex"); got != "-9999999" {
				t.Errorf("first beginIndex = %q, want -9999999", got)
			}
			io.WriteString(w, `{"error":0,"msg":"","data":{"total":5,"thingList":[`+
				`{"itemType":1,"index":-5,"itemData":{"deviceid":"owned","name":"Owned","online":true,"params":{"switch":"on"},"extra":{"uiid":1,"model":"BASIC"}}},`+
				`{"itemType":2,"index":17,"itemData":{"deviceid":"shared","name":"Shared","online":false,"params":{"switch":"off"},"extra":{"uiid":5,"model":"S20"}}},`+
				`{"itemType":3,"index":42,"itemData":{"id":"group","name":"Group","params":{"switch":"on"}}}`+
				`]}}`)
		case 2:
			if got := r.URL.Query().Get("beginIndex"); got != "42" {
				t.Errorf("second beginIndex = %q, want 42", got)
			}
			io.WriteString(w, `{"error":0,"msg":"","data":{"total":5,"thingList":[`+
				`{"itemType":1,"itemData":{"deviceid":"multi","name":"Multi","online":true,"params":{"switches":[{"switch":"on","outlet":0}]},"extra":{"uiid":2,"model":"DUAL"}}},`+
				`{"itemType":1,"itemData":{"deviceid":"bad-state","name":"Bad","online":true,"params":{"switch":"toggle"},"extra":{"uiid":3,"model":"ODD"}}}`+
				`]}}`)
		default:
			t.Errorf("unexpected request %d", requests.Load())
		}
	}))
	defer server.Close()

	client := testBearerClient(server.URL, time.Nanosecond)
	got, err := client.ListDevices(context.Background(), "as", "access-token")
	if err != nil {
		t.Fatalf("ListDevices() error = %v", err)
	}
	want := []Device{
		{DeviceID: "owned", Name: "Owned", Online: true, State: "on", UIID: 1, Model: "BASIC"},
		{DeviceID: "shared", Name: "Shared", Online: false, State: "off", UIID: 5, Model: "S20"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListDevices() = %#v, want %#v", got, want)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("request count = %d, want 2", got)
	}
}

func TestClientListDevicesStopsOnEmptyPage(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			io.WriteString(w, `{"error":0,"msg":"","data":{"total":10,"thingList":[]}}`)
			return
		}
		t.Error("ListDevices issued a request after an empty page")
	}))
	defer server.Close()

	got, err := testBearerClient(server.URL, time.Nanosecond).ListDevices(context.Background(), "cn", "access-token")
	if err != nil || len(got) != 0 {
		t.Fatalf("ListDevices() = %#v, %v; want empty, nil", got, err)
	}
}

func TestClientGetDeviceUsesSpecifiedThing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertBearerHeaders(t, r)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v2/device/thing" {
			t.Errorf("request = %s %s, want POST /v2/device/thing", r.Method, r.URL.Path)
		}
		if got, want := string(body), `{"thingList":[{"itemType":1,"id":"device-1"},{"itemType":2,"id":"device-1"}]}`; got != want {
			t.Errorf("body = %q, want %q", got, want)
		}
		io.WriteString(w, `{"error":0,"msg":"","data":{"thingList":[{"itemType":1,"itemData":{"deviceid":"device-1","name":"Plug","online":true,"params":{"switch":"off"},"extra":{"uiid":6,"model":"MINI"}}}]}}`)
	}))
	defer server.Close()

	got, err := testBearerClient(server.URL, time.Nanosecond).GetDevice(context.Background(), "eu", "access-token", "device-1")
	if err != nil {
		t.Fatalf("GetDevice() error = %v", err)
	}
	want := Device{DeviceID: "device-1", Name: "Plug", Online: true, State: "off", UIID: 6, Model: "MINI"}
	if got != want {
		t.Fatalf("GetDevice() = %#v, want %#v", got, want)
	}
}

func TestClientGetDeviceSupportsSharedThingAndValidatesIdentity(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"error":0,"msg":"","data":{"thingList":[{"itemType":2,"itemData":{"deviceid":"other","params":{"switch":"on"}}},{"itemType":2,"itemData":{"deviceid":"shared","name":"Shared","online":true,"params":{"switch":"off"},"extra":{"uiid":5,"model":"S20"}}}]}}`)
	}))
	defer server.Close()

	got, err := testBearerClient(server.URL, time.Nanosecond).GetDevice(context.Background(), "as", "access-token", "shared")
	if err != nil {
		t.Fatalf("GetDevice() error = %v", err)
	}
	if got.DeviceID != "shared" || got.State != "off" {
		t.Fatalf("GetDevice() = %#v, want matching shared device", got)
	}
}

func TestClientSetSwitchSendsControlWithoutStatusRead(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assertBearerHeaders(t, r)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v2/device/thing/status" {
			t.Errorf("request = %s %s, want POST /v2/device/thing/status", r.Method, r.URL.Path)
		}
		if got, want := string(body), `{"type":1,"id":"device-1","params":{"switch":"on"}}`; got != want {
			t.Errorf("body = %q, want %q", got, want)
		}
		io.WriteString(w, `{"error":0,"msg":"","data":{}}`)
	}))
	defer server.Close()

	if err := testBearerClient(server.URL, time.Nanosecond).SetSwitch(context.Background(), "us", "access-token", "device-1", "on"); err != nil {
		t.Fatalf("SetSwitch() error = %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("request count = %d, want exactly 1", got)
	}
}

func TestClientSetSwitchRejectsInvalidStateLocally(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()

	err := testBearerClient(server.URL, time.Nanosecond).SetSwitch(context.Background(), "us", "access-token", "device-1", "toggle")
	if err == nil {
		t.Fatal("SetSwitch(toggle) error = nil, want error")
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("request count = %d, want 0", got)
	}
}

func TestClientErrorEnvelopeCodes(t *testing.T) {
	for _, code := range []int{30022, 412} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"error":%d,"msg":"rejected","data":{}}`, code)
			}))
			defer server.Close()
			err := testBearerClient(server.URL, time.Nanosecond).SetSwitch(context.Background(), "as", "token", "id", "off")
			var upstream *UpstreamError
			if !errors.As(err, &upstream) || upstream.Code != code || upstream.HTTPStatus != http.StatusOK {
				t.Fatalf("SetSwitch() error = %T %#v, want UpstreamError code %d", err, err, code)
			}
		})
	}
}

func TestClientErrorHTTPStatusWithoutJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gateway unavailable", http.StatusBadGateway)
	}))
	defer server.Close()

	_, err := testBearerClient(server.URL, time.Nanosecond).ListDevices(context.Background(), "eu", "token")
	var upstream *UpstreamError
	if !errors.As(err, &upstream) || upstream.HTTPStatus != http.StatusBadGateway {
		t.Fatalf("ListDevices() error = %T %v, want UpstreamError HTTP 502", err, err)
	}
}

func TestClientErrorMalformedSuccessJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"error":0,"data":`)
	}))
	defer server.Close()

	_, err := testBearerClient(server.URL, time.Nanosecond).GetDevice(context.Background(), "cn", "token", "id")
	if err == nil {
		t.Fatal("GetDevice() error = nil, want malformed JSON error")
	}
}

func TestClientLimitWaitHonorsCancellation(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		io.WriteString(w, `{"error":0,"msg":"","data":{}}`)
	}))
	defer server.Close()

	client := testBearerClient(server.URL, 250*time.Millisecond)
	if err := client.SetSwitch(context.Background(), "as", "token", "id", "on"); err != nil {
		t.Fatalf("first SetSwitch() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := client.SetSwitch(ctx, "as", "token", "id", "off")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second SetSwitch() error = %v, want context deadline exceeded", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("request count = %d, want 1", got)
	}
}

func TestClientLimitDefaultGateIsProcessWide(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		io.WriteString(w, `{"error":0,"msg":"","data":{}}`)
	}))
	defer server.Close()
	newProductionClient := func() *Client {
		return NewClient(Config{
			AppID:         "app-id",
			HTTPClient:    http.DefaultClient,
			RegionBaseURL: func(string) (string, error) { return server.URL, nil },
		})
	}
	if err := newProductionClient().SetSwitch(context.Background(), "as", "token", "id", "on"); err != nil {
		t.Fatalf("first SetSwitch() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := newProductionClient().SetSwitch(ctx, "as", "token", "id", "off")
	if !errors.Is(err, context.DeadlineExceeded) || requests.Load() != 1 {
		t.Fatalf("second SetSwitch() = %v, requests = %d; want deadline, 1 request", err, requests.Load())
	}
}

func assertBearerHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	if got, want := r.Header.Get("Authorization"), "Bearer access-token"; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if got, want := r.Header.Get("X-CK-Appid"), "app-id"; got != want {
		t.Errorf("X-CK-Appid = %q, want %q", got, want)
	}
}

func testBearerClient(baseURL string, interval time.Duration) *Client {
	return NewClient(Config{
		AppID:           "app-id",
		AppSecret:       "secret",
		CallbackURL:     "http://callback",
		HTTPClient:      http.DefaultClient,
		RegionBaseURL:   func(string) (string, error) { return baseURL, nil },
		RequestInterval: interval,
	})
}
