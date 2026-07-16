package ewelink

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

const devicePageSize = 30

type thing struct {
	ItemType int       `json:"itemType"`
	Index    int       `json:"index"`
	ItemData thingData `json:"itemData"`
}

type thingData struct {
	DeviceID string `json:"deviceid"`
	Name     string `json:"name"`
	Online   bool   `json:"online"`
	Params   struct {
		Switch any `json:"switch"`
	} `json:"params"`
	Extra struct {
		UIID  int    `json:"uiid"`
		Model string `json:"model"`
	} `json:"extra"`
}

func (c *Client) ListDevices(ctx context.Context, region, token string) ([]Device, error) {
	var devices []Device
	beginIndex := -9999999
	fetched := 0
	for {
		query := url.Values{
			"num":        {strconv.Itoa(devicePageSize)},
			"beginIndex": {strconv.Itoa(beginIndex)},
		}
		var page struct {
			ThingList []thing `json:"thingList"`
			Total     int     `json:"total"`
		}
		if err := c.bearerRequest(ctx, region, token, http.MethodGet, "/v2/device/thing?"+query.Encode(), nil, &page); err != nil {
			return nil, fmt.Errorf("list ewelink devices: %w", err)
		}
		if len(page.ThingList) == 0 {
			break
		}
		nextIndex := page.ThingList[len(page.ThingList)-1].Index
		if nextIndex <= beginIndex {
			return nil, fmt.Errorf("list ewelink devices: pagination cursor did not advance from %d to %d", beginIndex, nextIndex)
		}
		for _, item := range page.ThingList {
			if device, ok := supportedDevice(item); ok {
				devices = append(devices, device)
			}
		}
		fetched += len(page.ThingList)
		beginIndex = nextIndex
		if fetched >= page.Total {
			break
		}
	}
	return devices, nil
}

func (c *Client) GetDevice(ctx context.Context, region, token, deviceID string) (Device, error) {
	body, err := json.Marshal(struct {
		ThingList []struct {
			ItemType int    `json:"itemType"`
			ID       string `json:"id"`
		} `json:"thingList"`
	}{ThingList: []struct {
		ItemType int    `json:"itemType"`
		ID       string `json:"id"`
	}{{ItemType: 1, ID: deviceID}, {ItemType: 2, ID: deviceID}}})
	if err != nil {
		return Device{}, fmt.Errorf("encode specified thing request: %w", err)
	}
	var response struct {
		ThingList []thing `json:"thingList"`
	}
	if err := c.bearerRequest(ctx, region, token, http.MethodPost, "/v2/device/thing", body, &response); err != nil {
		return Device{}, fmt.Errorf("get ewelink device: %w", err)
	}
	matchedUnsupported := false
	for _, item := range response.ThingList {
		if item.ItemData.DeviceID != deviceID {
			continue
		}
		device, ok := supportedDevice(item)
		if !ok {
			matchedUnsupported = true
			continue
		}
		return device, nil
	}
	if matchedUnsupported {
		return Device{}, fmt.Errorf("ewelink device %q does not expose a scalar on/off switch", deviceID)
	}
	return Device{}, fmt.Errorf("get ewelink device %q: matching thing not returned", deviceID)
}

func (c *Client) SetSwitch(ctx context.Context, region, token, deviceID, state string) error {
	if state != "on" && state != "off" {
		return fmt.Errorf("invalid switch state %q: must be on or off", state)
	}
	body, err := json.Marshal(struct {
		Type   int    `json:"type"`
		ID     string `json:"id"`
		Params struct {
			Switch string `json:"switch"`
		} `json:"params"`
	}{Type: 1, ID: deviceID, Params: struct {
		Switch string `json:"switch"`
	}{Switch: state}})
	if err != nil {
		return fmt.Errorf("encode switch control: %w", err)
	}
	if err := c.bearerRequest(ctx, region, token, http.MethodPost, "/v2/device/thing/status", body, nil); err != nil {
		return fmt.Errorf("set ewelink switch: %w", err)
	}
	return nil
}

func (c *Client) bearerRequest(ctx context.Context, region, token, method, path string, body []byte, target any) error {
	return c.do(ctx, region, method, path, body, "Bearer "+token, target)
}

func supportedDevice(item thing) (Device, bool) {
	if item.ItemType != 1 && item.ItemType != 2 {
		return Device{}, false
	}
	state, ok := item.ItemData.Params.Switch.(string)
	if !ok || (state != "on" && state != "off") {
		return Device{}, false
	}
	return Device{
		DeviceID: item.ItemData.DeviceID,
		Name:     item.ItemData.Name,
		State:    state,
		Model:    item.ItemData.Extra.Model,
		Online:   item.ItemData.Online,
		UIID:     item.ItemData.Extra.UIID,
	}, true
}
