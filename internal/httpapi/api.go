package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/zm/ewelink-lan-ctl/internal/ewelink"
	"github.com/zm/ewelink-lan-ctl/internal/store"
	"github.com/zm/ewelink-lan-ctl/internal/token"
)

const (
	oauthStateLifetime = 10 * time.Minute
	maxOAuthStates     = 1024
	maxSwitchBodyBytes = 4 << 10
)

var deviceIDPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,128}$`)

type Gateway interface {
	ListDevices(context.Context) ([]ewelink.Device, error)
	GetDevice(context.Context, string) (ewelink.Device, error)
	SetSwitch(context.Context, string, string) error
}

type OAuthClient interface {
	AuthorizationURL(string) (string, error)
	ExchangeCode(context.Context, string, string) (store.Credentials, error)
}

type TokenManager interface {
	Set(store.Credentials) error
	Health() bool
}

type Config struct {
	Gateway Gateway
	OAuth   OAuthClient
	Tokens  TokenManager
	Now     func() time.Time
	Logger  *slog.Logger
}

type api struct {
	gateway Gateway
	oauth   OAuthClient
	tokens  TokenManager
	now     func() time.Time
	logger  *slog.Logger

	stateMu sync.Mutex
	states  map[string]time.Time
}

func New(config Config) http.Handler {
	now := config.Now
	if now == nil {
		now = time.Now
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	a := &api{gateway: config.Gateway, oauth: config.OAuth, tokens: config.Tokens, now: now, logger: logger, states: make(map[string]time.Time)}
	mux := http.NewServeMux()
	methodNotAllowed := func(allow string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Allow", allow)
			writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		}
	}
	exactMethod := func(method string, handler http.HandlerFunc) http.HandlerFunc {
		fallback := methodNotAllowed(method)
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != method {
				fallback(w, r)
				return
			}
			handler(w, r)
		}
	}
	mux.HandleFunc("GET /healthz", exactMethod(http.MethodGet, a.health))
	mux.HandleFunc("GET /oauth/start", exactMethod(http.MethodGet, a.oauthStart))
	mux.HandleFunc("GET /callback", exactMethod(http.MethodGet, a.callback))
	mux.HandleFunc("GET /api/v1/devices", exactMethod(http.MethodGet, a.devices))
	mux.HandleFunc("GET /api/v1/devices/{device_id}/status", exactMethod(http.MethodGet, a.status))
	mux.HandleFunc("PUT /api/v1/devices/{device_id}/switch", exactMethod(http.MethodPut, a.setSwitch))
	getOnly := methodNotAllowed(http.MethodGet)
	mux.HandleFunc("/healthz", getOnly)
	mux.HandleFunc("/oauth/start", getOnly)
	mux.HandleFunc("/callback", getOnly)
	mux.HandleFunc("/api/v1/devices", getOnly)
	mux.HandleFunc("/api/v1/devices/{device_id}/status", getOnly)
	mux.HandleFunc("/api/v1/devices/{device_id}/switch", methodNotAllowed(http.MethodPut))
	notFound := func(w http.ResponseWriter, _ *http.Request) {
		writeAPIError(w, http.StatusNotFound, "not_found", "not found")
	}
	mux.HandleFunc("/api/v1", notFound)
	mux.HandleFunc("/api/v1/", notFound)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if malformedDevicePath(r.URL.Path) {
			writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid request")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func malformedDevicePath(path string) bool {
	const prefix = "/api/v1/devices/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	for _, segment := range strings.Split(strings.TrimPrefix(path, prefix), "/") {
		if segment == "" || segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func (a *api) health(w http.ResponseWriter, _ *http.Request) {
	ready := a.tokens != nil && a.tokens.Health()
	writeJSON(w, http.StatusOK, struct {
		Status     string `json:"status"`
		OAuthReady bool   `json:"oauth_ready"`
	}{Status: "ok", OAuthReady: ready})
}

func (a *api) devices(w http.ResponseWriter, r *http.Request) {
	if a.gateway == nil {
		a.writeOperationError(w, errors.New("gateway unavailable"))
		return
	}
	devices, err := a.gateway.ListDevices(r.Context())
	if err != nil {
		a.writeOperationError(w, err)
		return
	}
	output := make([]deviceResponse, len(devices))
	for index, device := range devices {
		output[index] = deviceResponse{DeviceID: device.DeviceID, Name: device.Name, Online: device.Online, State: device.State, UIID: device.UIID, Model: device.Model}
	}
	writeJSON(w, http.StatusOK, struct {
		Devices []deviceResponse `json:"devices"`
	}{Devices: output})
}

type deviceResponse struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	Online   bool   `json:"online"`
	State    string `json:"state"`
	UIID     int    `json:"uiid"`
	Model    string `json:"model"`
}

func (a *api) status(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("device_id")
	if !validDeviceID(id) {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid request")
		return
	}
	if a.gateway == nil {
		a.writeOperationError(w, errors.New("gateway unavailable"))
		return
	}
	device, err := a.gateway.GetDevice(r.Context(), id)
	if err != nil {
		a.writeOperationError(w, err)
		return
	}
	if !device.Online {
		writeAPIError(w, http.StatusBadGateway, "device_offline", "device is offline")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		DeviceID string `json:"device_id"`
		Online   bool   `json:"online"`
		State    string `json:"state"`
	}{DeviceID: device.DeviceID, Online: device.Online, State: device.State})
}

func (a *api) setSwitch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("device_id")
	if !validDeviceID(id) {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid request")
		return
	}
	var input struct {
		State string `json:"state"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSwitchBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || requireEOF(decoder) != nil || (input.State != "on" && input.State != "off") {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid request")
		return
	}
	if a.gateway == nil {
		a.writeOperationError(w, errors.New("gateway unavailable"))
		return
	}
	if err := a.gateway.SetSwitch(r.Context(), id, input.State); err != nil {
		a.writeOperationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		Success  bool   `json:"success"`
		DeviceID string `json:"device_id"`
		State    string `json:"state"`
	}{Success: true, DeviceID: id, State: input.State})
}

func validDeviceID(id string) bool { return deviceIDPattern.MatchString(id) }

func requireEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func (a *api) writeOperationError(w http.ResponseWriter, err error) {
	status, code, message := classifyOperationError(err)
	a.logger.Warn("request failed", "category", code)
	writeAPIError(w, status, code, message)
}

func classifyOperationError(err error) (int, string, string) {
	if errors.Is(err, token.ErrOAuthRequired) {
		return http.StatusServiceUnavailable, "oauth_required", "OAuth authorization required"
	}
	if errors.Is(err, token.ErrRefreshUnavailable) {
		return http.StatusServiceUnavailable, "oauth_unavailable", "OAuth credentials temporarily unavailable"
	}
	if errors.Is(err, ewelink.ErrDeviceNotFound) {
		return http.StatusNotFound, "device_not_found", "device not found"
	}
	if errors.Is(err, ewelink.ErrUnsupportedDevice) {
		return http.StatusConflict, "unsupported_device", "device does not support a single switch"
	}
	if errors.Is(err, ewelink.ErrDeviceOffline) {
		return http.StatusBadGateway, "device_offline", "device is offline"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return http.StatusServiceUnavailable, "ewelink_unavailable", "eWeLink service unavailable"
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return http.StatusServiceUnavailable, "ewelink_unavailable", "eWeLink service unavailable"
	}
	var upstream *ewelink.UpstreamError
	if errors.As(err, &upstream) {
		switch {
		case upstream.HTTPStatus == http.StatusNotFound:
			return http.StatusNotFound, "device_not_found", "device not found"
		case upstream.HTTPStatus == http.StatusConflict:
			return http.StatusConflict, "unsupported_device", "device does not support a single switch"
		case upstream.HTTPStatus >= 500:
			return http.StatusServiceUnavailable, "ewelink_unavailable", "eWeLink service unavailable"
		case upstream.Code == http.StatusNotFound:
			return http.StatusNotFound, "device_not_found", "device not found"
		case upstream.Code == http.StatusConflict:
			return http.StatusConflict, "unsupported_device", "device does not support a single switch"
		case upstream.Code >= 500:
			return http.StatusServiceUnavailable, "ewelink_unavailable", "eWeLink service unavailable"
		}
		return http.StatusBadGateway, "ewelink_rejected", "eWeLink rejected request"
	}
	return http.StatusBadGateway, "ewelink_rejected", "eWeLink rejected request"
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{Error: struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (a *api) oauthStart(w http.ResponseWriter, r *http.Request) {
	if a.oauth == nil {
		a.logger.Error("OAuth start failed", "category", "internal_error")
		writeAPIError(w, http.StatusInternalServerError, "internal_error", "could not start authorization")
		return
	}
	now := a.now()
	state, err := randomState()
	if err != nil {
		a.logger.Error("OAuth start failed", "category", "random_state")
		writeAPIError(w, http.StatusInternalServerError, "internal_error", "could not start authorization")
		return
	}
	authorizationURL, err := a.oauth.AuthorizationURL(state)
	if err != nil {
		a.logger.Error("OAuth start failed", "category", "authorization_url")
		writeAPIError(w, http.StatusInternalServerError, "internal_error", "could not start authorization")
		return
	}
	a.stateMu.Lock()
	a.removeExpiredStatesLocked(now)
	if len(a.states) >= maxOAuthStates {
		a.stateMu.Unlock()
		a.logger.Warn("OAuth start unavailable", "category", "state_capacity")
		writeAPIError(w, http.StatusServiceUnavailable, "oauth_unavailable", "authorization temporarily unavailable")
		return
	}
	a.states[state] = now.Add(oauthStateLifetime)
	a.stateMu.Unlock()
	http.Redirect(w, r, authorizationURL, http.StatusFound)
}

func (a *api) callback(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	code, codeOK := oneQueryValue(query["code"])
	region, regionOK := oneQueryValue(query["region"])
	state, stateOK := oneQueryValue(query["state"])
	if !codeOK || !regionOK || !stateOK || !store.ValidRegion(region) || !a.consumeState(state) {
		writeAPIError(w, http.StatusBadRequest, "invalid_request", "invalid OAuth callback")
		return
	}
	if a.oauth == nil || a.tokens == nil {
		a.logger.Error("OAuth callback failed", "category", "internal_error")
		writeAPIError(w, http.StatusInternalServerError, "internal_error", "authorization failed")
		return
	}
	credentials, err := a.oauth.ExchangeCode(r.Context(), region, code)
	if err != nil {
		status, errorCode, message := classifyOAuthExchangeError(err)
		a.logger.Warn("OAuth callback failed", "category", errorCode)
		writeAPIError(w, status, errorCode, message)
		return
	}
	if err := a.tokens.Set(credentials); err != nil {
		a.logger.Error("OAuth callback failed", "category", "credential_persistence")
		writeAPIError(w, http.StatusInternalServerError, "internal_error", "could not save authorization")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "<!doctype html><html><body>Authorization complete. You may close this window.</body></html>\n")
}

func oneQueryValue(values []string) (string, bool) {
	if len(values) != 1 || values[0] == "" {
		return "", false
	}
	return values[0], true
}

func (a *api) consumeState(state string) bool {
	now := a.now()
	a.stateMu.Lock()
	defer a.stateMu.Unlock()
	expiresAt, exists := a.states[state]
	delete(a.states, state)
	a.removeExpiredStatesLocked(now)
	return exists && now.Before(expiresAt)
}

func (a *api) removeExpiredStatesLocked(now time.Time) {
	for state, expiresAt := range a.states {
		if !now.Before(expiresAt) {
			delete(a.states, state)
		}
	}
}

func classifyOAuthExchangeError(err error) (int, string, string) {
	status, code, _ := classifyOperationError(err)
	if status == http.StatusServiceUnavailable {
		return status, code, "eWeLink service unavailable"
	}
	return http.StatusBadGateway, "ewelink_rejected", "eWeLink rejected authorization"
}

func randomState() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}
