package ewelink

import (
	"context"
	"encoding/json"
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
		switch requests.Add(1) {
		case 1:
			if r.Method != http.MethodGet || r.URL.Path != "/v2/family" {
				t.Errorf("first request = %s %s, want GET /v2/family", r.Method, r.URL.String())
			}
			io.WriteString(w, `{"error":0,"data":{"familyList":[{"id":"family"}]}}`)
		case 2:
			assertThingListQuery(t, r, "family", "-9999999")
			if got := r.URL.Query().Get("beginIndex"); got != "-9999999" {
				t.Errorf("first beginIndex = %q, want -9999999", got)
			}
			io.WriteString(w, `{"error":0,"msg":"","data":{"total":5,"thingList":[`+
				`{"itemType":1,"index":-5,"itemData":{"deviceid":"owned","name":"Owned","online":true,"params":{"switch":"on"},"extra":{"uiid":1,"model":"BASIC"}}},`+
				`{"itemType":2,"index":17,"itemData":{"deviceid":"shared","name":"Shared","online":false,"params":{"switch":"off"},"extra":{"uiid":5,"model":"S20"}}},`+
				`{"itemType":3,"index":42,"itemData":{"id":"group","name":"Group","params":{"switch":"on"}}}`+
				`]}}`)
		case 3:
			assertThingListQuery(t, r, "family", "42")
			if got := r.URL.Query().Get("beginIndex"); got != "42" {
				t.Errorf("second beginIndex = %q, want 42", got)
			}
			io.WriteString(w, `{"error":0,"msg":"","data":{"total":5,"thingList":[`+
				`{"itemType":1,"index":50,"itemData":{"deviceid":"multi","name":"Multi","online":true,"params":{"switches":[{"switch":"on","outlet":0}]},"extra":{"uiid":2,"model":"DUAL"}}},`+
				`{"itemType":1,"index":60,"itemData":{"deviceid":"bad-state","name":"Bad","online":true,"params":{"switch":"toggle"},"extra":{"uiid":3,"model":"ODD"}}}`+
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
		{DeviceID: "multi", Name: "Multi", Online: true, State: "on", UIID: 2, Model: "DUAL"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListDevices() = %#v, want %#v", got, want)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("request count = %d, want 3", got)
	}
}

func TestClientListDevicesAcrossFamilies(t *testing.T) {
	t.Run("paginates each family and deduplicates devices", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assertBearerHeaders(t, r)
			switch requests.Add(1) {
			case 1:
				if r.Method != http.MethodGet || r.URL.Path != "/v2/family" {
					t.Fatalf("first request = %s %s, want GET /v2/family", r.Method, r.URL.String())
				}
				io.WriteString(w, `{"error":0,"data":{"familyList":[{"id":"family-a"},{"id":"family-b"}]}}`)
			case 2:
				assertThingListQuery(t, r, "family-a", "-9999999")
				io.WriteString(w, `{"error":0,"data":{"total":2,"thingList":[`+
					`{"itemType":1,"index":10,"itemData":{"deviceid":"scalar","name":"Scalar","online":true,"params":{"switch":"off"},"extra":{"uiid":1,"model":"BASIC"}}}`+
					`]}}`)
			case 3:
				assertThingListQuery(t, r, "family-a", "10")
				io.WriteString(w, `{"error":0,"data":{"total":2,"thingList":[`+
					`{"itemType":1,"index":20,"itemData":{"deviceid":"duplicate","name":"First","online":true,"params":{"switch":"on"},"extra":{"uiid":2,"model":"FIRST"}}}`+
					`]}}`)
			case 4:
				assertThingListQuery(t, r, "family-b", "-9999999")
				io.WriteString(w, `{"error":0,"data":{"total":2,"thingList":[`+
					`{"itemType":2,"index":7,"itemData":{"deviceid":"duplicate","name":"Second","online":false,"params":{"switch":"off"},"extra":{"uiid":3,"model":"SECOND"}}},`+
					`{"itemType":1,"index":8,"itemData":{"deviceid":"array","name":"Array","online":true,"params":{"switches":[{"outlet":0,"switch":"on"}]},"extra":{"uiid":138,"model":"ARRAY"}}}`+
					`]}}`)
			default:
				t.Fatalf("unexpected request %d: %s", requests.Load(), r.URL.String())
			}
		}))
		defer server.Close()

		got, err := testBearerClient(server.URL, time.Nanosecond).ListDevices(context.Background(), "as", "access-token")
		if err != nil {
			t.Fatalf("ListDevices() error = %v", err)
		}
		want := []Device{
			{DeviceID: "scalar", Name: "Scalar", Online: true, State: "off", UIID: 1, Model: "BASIC"},
			{DeviceID: "duplicate", Name: "First", Online: true, State: "on", UIID: 2, Model: "FIRST"},
			{DeviceID: "array", Name: "Array", Online: true, State: "on", UIID: 138, Model: "ARRAY"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ListDevices() = %#v, want %#v", got, want)
		}
		if got := requests.Load(); got != 4 {
			t.Fatalf("request count = %d, want 4", got)
		}
	})

	t.Run("fails when one family thing request fails", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch requests.Add(1) {
			case 1:
				io.WriteString(w, `{"error":0,"data":{"familyList":[{"id":"family-a"},{"id":"family-b"}]}}`)
			case 2:
				assertThingListQuery(t, r, "family-a", "-9999999")
				io.WriteString(w, `{"error":0,"data":{"total":0,"thingList":[]}}`)
			case 3:
				assertThingListQuery(t, r, "family-b", "-9999999")
				io.WriteString(w, `{"error":503,"msg":"unavailable","data":{}}`)
			default:
				t.Fatalf("unexpected request %d", requests.Load())
			}
		}))
		defer server.Close()

		if _, err := testBearerClient(server.URL, time.Nanosecond).ListDevices(context.Background(), "as", "access-token"); err == nil {
			t.Fatal("ListDevices() error = nil, want family thing error")
		}
	})

	t.Run("fails when a family pagination cursor does not advance", func(t *testing.T) {
		var requests atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if requests.Add(1) == 1 {
				io.WriteString(w, `{"error":0,"data":{"familyList":[{"id":"family-a"}]}}`)
				return
			}
			io.WriteString(w, `{"error":0,"data":{"total":100,"thingList":[{"itemType":3,"index":10,"itemData":{}}]}}`)
		}))
		defer server.Close()

		if _, err := testBearerClient(server.URL, time.Nanosecond).ListDevices(context.Background(), "as", "access-token"); err == nil {
			t.Fatal("ListDevices() error = nil, want repeated pagination cursor error")
		}
		if got := requests.Load(); got != 3 {
			t.Fatalf("request count = %d, want 3", got)
		}
	})
}

func assertThingListQuery(t *testing.T, r *http.Request, familyID, beginIndex string) {
	t.Helper()
	if r.Method != http.MethodGet || r.URL.Path != "/v2/device/thing" {
		t.Fatalf("request = %s %s, want GET /v2/device/thing", r.Method, r.URL.String())
	}
	query := r.URL.Query()
	if got := query.Get("familyid"); got != familyID {
		t.Errorf("familyid = %q, want %q", got, familyID)
	}
	if got := query.Get("num"); got != "30" {
		t.Errorf("num = %q, want 30", got)
	}
	if got := query.Get("beginIndex"); got != beginIndex {
		t.Errorf("beginIndex = %q, want %q", got, beginIndex)
	}
}

func TestClientListDevicesStopsOnEmptyPage(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			io.WriteString(w, `{"error":0,"data":{"familyList":[{"id":"family"}]}}`)
			return
		}
		if requests.Load() == 2 {
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

func TestClientListDevicesRejectsRepeatedPaginationCursor(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		request := requests.Add(1)
		if request == 1 {
			io.WriteString(w, `{"error":0,"data":{"familyList":[{"id":"family"}]}}`)
			return
		}
		if request > 4 {
			t.Fatal("ListDevices did not stop after repeated cursor")
		}
		io.WriteString(w, `{"error":0,"msg":"","data":{"total":100,"thingList":[{"itemType":3,"index":10,"itemData":{}}]}}`)
	}))
	defer server.Close()

	_, err := testBearerClient(server.URL, time.Nanosecond).ListDevices(context.Background(), "as", "access-token")
	if err == nil {
		t.Fatal("ListDevices() error = nil, want repeated pagination cursor error")
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("request count = %d, want 3", got)
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

func TestSwitchDescriptorFormats(t *testing.T) {
	tests := []struct {
		name       string
		paramsJSON string
		wantState  string
		wantMode   switchMode
		wantOK     bool
	}{
		{"valid outlet zero", `{"switches":[{"outlet":0,"switch":"on"}]}`, "on", switchModeOutletZero, true},
		{"only another outlet", `{"switches":[{"outlet":1,"switch":"on"}]}`, "", 0, false},
		{"duplicate outlet zero", `{"switches":[{"outlet":0,"switch":"on"},{"outlet":0,"switch":"off"}]}`, "", 0, false},
		{"missing outlet", `{"switches":[{"switch":"on"}]}`, "", 0, false},
		{"null entry before valid outlet zero", `{"switches":[null,{"outlet":0,"switch":"on"}]}`, "", 0, false},
		{"missing outlet before valid outlet zero", `{"switches":[{"switch":"off"},{"outlet":0,"switch":"on"}]}`, "", 0, false},
		{"invalid nonzero outlet state before valid outlet zero", `{"switches":[{"outlet":1,"switch":"toggle"},{"outlet":0,"switch":"on"}]}`, "", 0, false},
		{"missing nonzero outlet state before valid outlet zero", `{"switches":[{"outlet":1},{"outlet":0,"switch":"on"}]}`, "", 0, false},
		{"invalid outlet type before valid outlet zero", `{"switches":[{"outlet":"1","switch":"off"},{"outlet":0,"switch":"on"}]}`, "", 0, false},
		{"valid nonzero outlet before valid outlet zero", `{"switches":[{"outlet":1,"switch":"off"},{"outlet":0,"switch":"on"}]}`, "on", switchModeOutletZero, true},
		{"invalid outlet zero state", `{"switches":[{"outlet":0,"switch":"toggle"}]}`, "", 0, false},
		{"switches is object", `{"switches":{"outlet":0,"switch":"on"}}`, "", 0, false},
		{"switches is null", `{"switches":null}`, "", 0, false},
		{"scalar takes precedence", `{"switch":"off","switches":[{"outlet":0,"switch":"on"}]}`, "off", switchModeScalar, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var item thing
			item.ItemType = 1
			item.ItemData.DeviceID = "device"
			if err := json.Unmarshal([]byte(tt.paramsJSON), &item.ItemData.Params); err != nil {
				t.Fatal(err)
			}
			device, mode, ok := switchDescriptor(item)
			if ok != tt.wantOK || mode != tt.wantMode || device.State != tt.wantState {
				t.Fatalf("switchDescriptor() = (%#v, %v, %v), want state %q, mode %v, ok %v", device, mode, ok, tt.wantState, tt.wantMode, tt.wantOK)
			}
		})
	}
}

func TestClientGetDeviceSupportsOwnedAndSharedSwitchFormats(t *testing.T) {
	tests := []struct {
		name       string
		itemType   int
		paramsJSON string
		wantState  string
	}{
		{"owned outlet zero", 1, `{"switches":[{"outlet":0,"switch":"on"}]}`, "on"},
		{"shared outlet zero", 2, `{"switches":[{"outlet":0,"switch":"off"}]}`, "off"},
		{"scalar takes precedence", 1, `{"switch":"off","switches":[{"outlet":0,"switch":"on"}]}`, "off"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `{"error":0,"data":{"thingList":[{"itemType":%d,"itemData":{"deviceid":"device","name":"Switch","params":%s}}]}}`, tt.itemType, tt.paramsJSON)
			}))
			defer server.Close()

			got, err := testBearerClient(server.URL, time.Nanosecond).GetDevice(context.Background(), "as", "token", "device")
			if err != nil {
				t.Fatalf("GetDevice() error = %v", err)
			}
			if got.State != tt.wantState {
				t.Fatalf("GetDevice().State = %q, want %q", got.State, tt.wantState)
			}
		})
	}
}

func TestClientGetThingReturnsRawThingDescriptorAndMode(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(body), `{"thingList":[{"itemType":1,"id":"device"},{"itemType":2,"id":"device"}]}`; got != want {
			t.Errorf("body = %q, want %q", got, want)
		}
		io.WriteString(w, `{"error":0,"data":{"thingList":[`+
			`{"itemType":1,"itemData":{"deviceid":"device","params":{"switches":[]}}},`+
			`{"itemType":2,"itemData":{"deviceid":"device","name":"Shared","params":{"switches":[{"outlet":0,"switch":"on"}]}}}`+
			`]}}`)
	}))
	defer server.Close()

	item, device, mode, err := testBearerClient(server.URL, time.Nanosecond).getThing(context.Background(), "as", "token", "device")
	if err != nil {
		t.Fatalf("getThing() error = %v", err)
	}
	if item.ItemType != 2 || device.DeviceID != "device" || device.State != "on" || mode != switchModeOutletZero {
		t.Fatalf("getThing() = (%#v, %#v, %v), want shared outlet-zero device", item, device, mode)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("request count = %d, want 1", got)
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

func TestClientGetDeviceScansPastUnsupportedMatchingCandidate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"error":0,"msg":"","data":{"thingList":[`+
			`{"itemType":1,"itemData":{"deviceid":"device","params":{"switches":[{"switch":"on"}]} }},`+
			`{"itemType":2,"itemData":{"deviceid":"device","name":"Shared","online":true,"params":{"switch":"on"},"extra":{"uiid":5,"model":"S20"}}}`+
			`]}}`)
	}))
	defer server.Close()

	got, err := testBearerClient(server.URL, time.Nanosecond).GetDevice(context.Background(), "as", "access-token", "device")
	if err != nil {
		t.Fatalf("GetDevice() error = %v", err)
	}
	if got.DeviceID != "device" || got.State != "on" || got.UIID != 5 {
		t.Fatalf("GetDevice() = %#v, want supported matching candidate", got)
	}
}

func TestClientGetDeviceReturnsTypedLocalErrors(t *testing.T) {
	tests := []struct {
		name, response string
		want           error
	}{
		{"not found", `{"error":0,"data":{"thingList":[]}}`, ErrDeviceNotFound},
		{"unsupported", `{"error":0,"data":{"thingList":[{"itemType":1,"itemData":{"deviceid":"device","params":{"switches":[]}}}]}}`, ErrUnsupportedDevice},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tt.response) }))
			defer server.Close()
			_, err := testBearerClient(server.URL, time.Nanosecond).GetDevice(context.Background(), "us", "token", "device")
			if !errors.Is(err, tt.want) {
				t.Fatalf("GetDevice() error = %v, want errors.Is(_, %v)", err, tt.want)
			}
		})
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

func TestClientErrorRequiresNumericEnvelopeErrorOnRead(t *testing.T) {
	for _, response := range []string{`{}`, `{"error":null,"data":{"thingList":[]}}`} {
		t.Run(response, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, response)
			}))
			defer server.Close()

			if _, err := testBearerClient(server.URL, time.Nanosecond).ListDevices(context.Background(), "as", "token"); err == nil {
				t.Fatal("ListDevices() error = nil, want invalid envelope error")
			}
		})
	}
}

func TestClientErrorRequiresNumericEnvelopeErrorOnWrite(t *testing.T) {
	for _, response := range []string{`{}`, `{"error":null,"data":{}}`} {
		t.Run(response, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				io.WriteString(w, response)
			}))
			defer server.Close()

			if err := testBearerClient(server.URL, time.Nanosecond).SetSwitch(context.Background(), "as", "token", "id", "on"); err == nil {
				t.Fatal("SetSwitch() error = nil, want invalid envelope error")
			}
		})
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

func TestClientLimitProductionConstructorUsesSharedGate(t *testing.T) {
	client := NewClient(Config{})
	if client.gate != productionRequestGate {
		t.Fatal("NewClient() did not use process-wide production request gate")
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
	client := NewClient(Config{
		AppID:         "app-id",
		AppSecret:     "secret",
		CallbackURL:   "http://callback",
		HTTPClient:    http.DefaultClient,
		RegionBaseURL: func(string) (string, error) { return baseURL, nil },
	})
	client.gate = newRequestGate(interval, time.Now)
	return client
}
