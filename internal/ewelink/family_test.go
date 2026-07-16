package ewelink

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestClientListFamilies(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     []family
		wantErr  bool
	}{
		{
			name:     "deduplicates valid families in first-seen order",
			response: `{"error":0,"data":{"familyList":[{"id":"family-a"},{"id":"family-a"},{"id":"family-b"}]}}`,
			want:     []family{{ID: "family-a"}, {ID: "family-b"}},
		},
		{
			name:     "accepts an empty family list",
			response: `{"error":0,"data":{"familyList":[]}}`,
			want:     []family{},
		},
		{
			name:     "rejects a missing family list",
			response: `{"error":0,"data":{}}`,
			wantErr:  true,
		},
		{
			name:     "rejects a null family list",
			response: `{"error":0,"data":{"familyList":null}}`,
			wantErr:  true,
		},
		{
			name:     "rejects an empty family ID",
			response: `{"error":0,"data":{"familyList":[{"id":""}]}}`,
			wantErr:  true,
		},
		{
			name:     "wraps an upstream error",
			response: `{"error":401,"msg":"rejected","data":{}}`,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assertBearerHeaders(t, r)
				if r.Method != http.MethodGet || r.URL.Path != "/v2/family" {
					t.Errorf("request = %s %s, want GET /v2/family", r.Method, r.URL.Path)
				}
				_, _ = io.WriteString(w, tt.response)
			}))
			defer server.Close()

			got, err := testBearerClient(server.URL, time.Nanosecond).listFamilies(context.Background(), "as", "access-token")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("listFamilies() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("listFamilies() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("listFamilies() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
