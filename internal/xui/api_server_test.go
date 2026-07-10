package xui

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateAcceptsSupportedPanelVersions(t *testing.T) {
	for _, version := range []string{"v3.4.2", "3.4.10", "v3.10.0"} {
		t.Run(version, func(t *testing.T) {
			var requests int
			ts := panelVersionServer(t, version, &requests)
			defer ts.Close()

			c, err := NewAPI(APIConfig{
				BaseURL: ts.URL,
				Token:   "top-secret",
				Timeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			status, err := c.Validate(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if status.PanelVersion != version {
				t.Fatalf("PanelVersion=%q want=%q", status.PanelVersion, version)
			}
			if requests != 1 {
				t.Fatalf("requests=%d want=1", requests)
			}
		})
	}
}

func TestValidateRejectsUnsupportedPanelVersions(t *testing.T) {
	versions := []string{
		"v3.4.1",
		"v2.9.9",
		"v4.0.0",
		"",
		"malformed",
		"3.4",
		"3.4.2.1",
		"3.4.beta",
		"V3.4.2",
		" 3.4.2",
		"top-secret",
	}

	for _, version := range versions {
		name := version
		if name == "" {
			name = "empty"
		}
		t.Run(name, func(t *testing.T) {
			var requests int
			ts := panelVersionServer(t, version, &requests)
			defer ts.Close()

			c, err := NewAPI(APIConfig{
				BaseURL: ts.URL,
				Token:   "top-secret",
				Timeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Validate(context.Background())
			if !IsKind(err, ErrorUnsupportedVersion) {
				t.Fatalf("Validate() err=%v", err)
			}
			if strings.Contains(err.Error(), "top-secret") {
				t.Fatalf("token leaked: %v", err)
			}
			if requests != 1 {
				t.Fatalf("requests=%d want=1", requests)
			}
		})
	}
}

func panelVersionServer(t *testing.T, version string, requests *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*requests++
		if r.URL.Path != "/panel/api/server/status" {
			t.Errorf("path=%q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(
			`{"success":true,"msg":"","obj":{"panelVersion":%q}}`,
			version,
		))
	}))
}
