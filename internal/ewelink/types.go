package ewelink

import (
	"errors"
	"fmt"
)

type Device struct {
	DeviceID string `json:"device_id"`
	Name     string `json:"name"`
	State    string `json:"state"`
	Model    string `json:"model"`
	Online   bool   `json:"online"`
	UIID     int    `json:"uiid"`
}

type UpstreamError struct {
	HTTPStatus int
	Code       int
	Message    string
}

func (e *UpstreamError) Error() string {
	if e == nil {
		return "ewelink upstream error"
	}
	return fmt.Sprintf("ewelink upstream error: HTTP %d, code %d: %s", e.HTTPStatus, e.Code, e.Message)
}

func IsTokenError(err error) bool {
	var upstream *UpstreamError
	return errors.As(err, &upstream) && (upstream.Code == 401 || upstream.Code == 402)
}
