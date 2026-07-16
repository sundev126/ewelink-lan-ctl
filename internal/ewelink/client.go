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
		Switch   any             `json:"switch"`
		Switches json.RawMessage `json:"switches"`
	} `json:"params"`
	Extra struct {
		UIID  int    `json:"uiid"`
		Model string `json:"model"`
	} `json:"extra"`
}

type switchMode uint8

const (
	switchModeScalar switchMode = iota + 1
	switchModeOutletZero
)

type switchEntry struct {
	Outlet *int `json:"outlet"`
	Switch any  `json:"switch"`
}

func (c *Client) ListDevices(ctx context.Context, region, token string) ([]Device, error) {
	families, err := c.listFamilies(ctx, region, token)
	if err != nil {
		return nil, err
	}
	var devices []Device
	seen := make(map[string]struct{})
	for _, family := range families {
		items, err := c.listThingsForFamily(ctx, region, token, family.ID)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			device, _, ok := switchDescriptor(item)
			if !ok {
				continue
			}
			if _, ok := seen[device.DeviceID]; ok {
				continue
			}
			seen[device.DeviceID] = struct{}{}
			devices = append(devices, device)
		}
	}
	return devices, nil
}

func (c *Client) listThingsForFamily(ctx context.Context, region, token, familyID string) ([]thing, error) {
	var items []thing
	beginIndex := -9999999
	fetched := 0
	for {
		query := url.Values{
			"num":        {strconv.Itoa(devicePageSize)},
			"beginIndex": {strconv.Itoa(beginIndex)},
			"familyid":   {familyID},
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
		items = append(items, page.ThingList...)
		fetched += len(page.ThingList)
		beginIndex = nextIndex
		if fetched >= page.Total {
			break
		}
	}
	return items, nil
}

func (c *Client) GetDevice(ctx context.Context, region, token, deviceID string) (Device, error) {
	_, device, _, err := c.getThing(ctx, region, token, deviceID)
	return device, err
}

func (c *Client) getThing(ctx context.Context, region, token, deviceID string) (thing, Device, switchMode, error) {
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
		return thing{}, Device{}, 0, fmt.Errorf("encode specified thing request: %w", err)
	}
	var response struct {
		ThingList []thing `json:"thingList"`
	}
	if err := c.bearerRequest(ctx, region, token, http.MethodPost, "/v2/device/thing", body, &response); err != nil {
		return thing{}, Device{}, 0, fmt.Errorf("get ewelink device: %w", err)
	}
	matchedUnsupported := false
	for _, item := range response.ThingList {
		if item.ItemData.DeviceID != deviceID {
			continue
		}
		device, mode, ok := switchDescriptor(item)
		if !ok {
			matchedUnsupported = true
			continue
		}
		return item, device, mode, nil
	}
	if matchedUnsupported {
		return thing{}, Device{}, 0, fmt.Errorf("%w: %q", ErrUnsupportedDevice, deviceID)
	}
	return thing{}, Device{}, 0, fmt.Errorf("%w: %q", ErrDeviceNotFound, deviceID)
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
	device, _, ok := switchDescriptor(item)
	return device, ok
}

func switchDescriptor(item thing) (Device, switchMode, bool) {
	if item.ItemType != 1 && item.ItemType != 2 {
		return Device{}, 0, false
	}
	state, ok := item.ItemData.Params.Switch.(string)
	mode := switchModeScalar
	if !ok || (state != "on" && state != "off") {
		var entries []switchEntry
		if len(item.ItemData.Params.Switches) == 0 || json.Unmarshal(item.ItemData.Params.Switches, &entries) != nil || entries == nil {
			return Device{}, 0, false
		}
		found := false
		for _, entry := range entries {
			if entry.Outlet == nil {
				return Device{}, 0, false
			}
			entryState, valid := entry.Switch.(string)
			if !valid || (entryState != "on" && entryState != "off") {
				return Device{}, 0, false
			}
			if *entry.Outlet != 0 {
				continue
			}
			if found {
				return Device{}, 0, false
			}
			found = true
			state = entryState
		}
		if !found {
			return Device{}, 0, false
		}
		mode = switchModeOutletZero
	}
	return Device{
		DeviceID: item.ItemData.DeviceID,
		Name:     item.ItemData.Name,
		State:    state,
		Model:    item.ItemData.Extra.Model,
		Online:   item.ItemData.Online,
		UIID:     item.ItemData.Extra.UIID,
	}, mode, true
}
