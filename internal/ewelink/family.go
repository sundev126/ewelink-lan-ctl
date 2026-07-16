package ewelink

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

type family struct {
	ID string `json:"id"`
}

func (c *Client) listFamilies(ctx context.Context, region, token string) ([]family, error) {
	var response struct {
		FamilyList json.RawMessage `json:"familyList"`
	}
	if err := c.bearerRequest(ctx, region, token, http.MethodGet, "/v2/family", nil, &response); err != nil {
		return nil, fmt.Errorf("list ewelink families: %w", err)
	}
	if len(response.FamilyList) == 0 {
		return nil, fmt.Errorf("list ewelink families: %w", fmt.Errorf("missing familyList"))
	}
	if bytes.Equal(bytes.TrimSpace(response.FamilyList), []byte("null")) {
		return nil, fmt.Errorf("list ewelink families: %w", fmt.Errorf("familyList is null"))
	}

	var parsed []family
	if err := json.Unmarshal(response.FamilyList, &parsed); err != nil {
		return nil, fmt.Errorf("list ewelink families: %w", err)
	}
	result := make([]family, 0, len(parsed))
	seen := make(map[string]struct{}, len(parsed))
	for _, item := range parsed {
		if item.ID == "" {
			return nil, fmt.Errorf("list ewelink families: %w", fmt.Errorf("family ID is empty"))
		}
		if _, ok := seen[item.ID]; ok {
			continue
		}
		seen[item.ID] = struct{}{}
		result = append(result, item)
	}
	return result, nil
}
