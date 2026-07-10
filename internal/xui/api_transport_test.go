package xui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAPIClientAddsBearerHeadersAndNeverLogsIn(t *testing.T) {
	var paths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if got := r.Method; got != http.MethodGet {
			t.Errorf("method=%q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer top-secret" {
			t.Errorf("Authorization=%q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept=%q", got)
		}
		if got := r.Header.Get("X-Requested-With"); got != "XMLHttpRequest" {
			t.Errorf("X-Requested-With=%q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "" {
			t.Errorf("Content-Type=%q for bodyless request", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"msg":"","obj":{"panelVersion":"v3.4.2"}}`)
	}))
	defer ts.Close()

	c, err := NewAPI(APIConfig{
		BaseURL:   ts.URL + "/",
		PanelPath: "/secret/",
		Token:     "top-secret",
		Timeout:   time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}

	if want := []string{"/secret/panel/api/server/status"}; !slices.Equal(paths, want) {
		t.Fatalf("paths=%v want=%v", paths, want)
	}
	for _, path := range paths {
		if strings.Contains(path, "/login") {
			t.Fatalf("unexpected login path %q", path)
		}
	}
}

func TestAPIClientRejectsEmptyTokenAndNonHTTPURLs(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		token   string
	}{
		{name: "empty token", baseURL: "https://panel.example", token: ""},
		{name: "relative URL", baseURL: "panel.example", token: "top-secret"},
		{name: "FTP URL", baseURL: "ftp://panel.example", token: "top-secret"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewAPI(APIConfig{BaseURL: tt.baseURL, Token: tt.token})
			if err == nil {
				t.Fatal("NewAPI() error=nil")
			}
			if strings.Contains(err.Error(), "top-secret") {
				t.Fatalf("token leaked: %v", err)
			}
		})
	}
}

func TestAPIClientNormalizesPanelPathDotSegments(t *testing.T) {
	var requestPath string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"msg":"","obj":{"panelVersion":"v3.4.2"}}`)
	}))
	defer ts.Close()

	c, err := NewAPI(APIConfig{
		BaseURL:   ts.URL,
		PanelPath: "..",
		Token:     "top-secret",
		Timeout:   time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requestPath != "/panel/api/server/status" {
		t.Fatalf("path=%q", requestPath)
	}
}

func TestAPIClientClassifiesAndRedactsErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		kind   ErrorKind
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, body: "top-secret", kind: ErrorUnauthorized},
		{name: "masked unauthorized", status: http.StatusNotFound, body: "top-secret", kind: ErrorUnauthorized},
		{name: "other HTTP status", status: http.StatusInternalServerError, body: "bad top-secret", kind: ErrorTransport},
		{name: "API error", status: http.StatusOK, body: `{"success":false,"msg":"bad top-secret and top-secret"}`, kind: ErrorAPI},
		{name: "decode", status: http.StatusOK, body: `<html>top-secret</html>`, kind: ErrorDecode},
		{name: "missing success", status: http.StatusOK, body: `{"msg":"top-secret","obj":{}}`, kind: ErrorDecode},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer ts.Close()

			c, err := NewAPI(APIConfig{
				BaseURL:   ts.URL,
				PanelPath: "/",
				Token:     "top-secret",
				Timeout:   time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Validate(context.Background())
			if !IsKind(err, tt.kind) {
				t.Fatalf("err=%v kind=%q", err, tt.kind)
			}
			if strings.Contains(err.Error(), "top-secret") {
				t.Fatalf("token leaked: %v", err)
			}
		})
	}
}

func TestAPIClientNeverFollowsRedirects(t *testing.T) {
	for _, useInjectedClient := range []bool{false, true} {
		name := "production client"
		if useInjectedClient {
			name = "injected client"
		}
		t.Run(name, func(t *testing.T) {
			var targetRequests atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				targetRequests.Add(1)
				if got := r.Header.Get("Authorization"); got != "" {
					t.Errorf("redirect target received Authorization=%q", got)
				}
			}))
			defer target.Close()

			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+"/capture", http.StatusFound)
			}))
			defer source.Close()

			cfg := APIConfig{
				BaseURL: source.URL,
				Token:   "top-secret",
				Timeout: time.Second,
			}
			if useInjectedClient {
				cfg.HTTPClient = &http.Client{Timeout: time.Second}
			}
			c, err := NewAPI(cfg)
			if err != nil {
				t.Fatal(err)
			}
			_, err = c.Validate(context.Background())
			if !IsKind(err, ErrorTransport) {
				t.Fatalf("err=%v", err)
			}
			if got := targetRequests.Load(); got != 0 {
				t.Fatalf("redirect target requests=%d", got)
			}
		})
	}
}

func TestAPIClientRetriesOnlyTemporaryGETErrors(t *testing.T) {
	tests := []struct {
		name      string
		method    string
		temporary bool
		timeout   bool
		attempts  int
	}{
		{name: "temporary GET", method: http.MethodGet, temporary: true, attempts: 2},
		{name: "timed out GET", method: http.MethodGet, timeout: true, attempts: 2},
		{name: "permanent GET", method: http.MethodGet, attempts: 1},
		{name: "temporary POST", method: http.MethodPost, temporary: true, attempts: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var attempts int
			rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
				attempts++
				return nil, fmt.Errorf("wrapped network failure: %w", testNetError{
					temporary: tt.temporary,
					timeout:   tt.timeout,
				})
			})
			c, err := NewAPI(APIConfig{
				BaseURL:    "https://panel.example",
				Token:      "top-secret",
				HTTPClient: &http.Client{Transport: rt},
			})
			if err != nil {
				t.Fatal(err)
			}

			_, err = doAPI[ServerStatus](
				context.Background(),
				c.transport,
				tt.method,
				"server/status",
				map[string]string{"probe": "value"},
				"",
			)
			if !IsKind(err, ErrorTransport) {
				t.Fatalf("err=%v", err)
			}
			if attempts != tt.attempts {
				t.Fatalf("attempts=%d want=%d", attempts, tt.attempts)
			}
		})
	}
}

func TestAPIClientSetsJSONContentTypeWhenBodyExists(t *testing.T) {
	var contentType string
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		contentType = r.Header.Get("Content-Type")
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: io.NopCloser(strings.NewReader(
				`{"success":true,"msg":"","obj":{}}`,
			)),
		}, nil
	})
	c, err := NewAPI(APIConfig{
		BaseURL:    "https://panel.example",
		Token:      "top-secret",
		HTTPClient: &http.Client{Transport: rt},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = doAPI[struct{}](
		context.Background(),
		c.transport,
		http.MethodPost,
		"server/status",
		map[string]string{"probe": "value"},
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "application/json" {
		t.Fatalf("Content-Type=%q", contentType)
	}
}

func TestIsKindTraversesWrappedErrors(t *testing.T) {
	cause := errors.New("cause")
	typed := &Error{
		Kind:    ErrorUnauthorized,
		Op:      "server status",
		Message: "unauthorized",
		Err:     cause,
	}
	err := fmt.Errorf("outer: %w", typed)

	if !IsKind(err, ErrorUnauthorized) {
		t.Fatalf("IsKind(%v, %q)=false", err, ErrorUnauthorized)
	}
	if !errors.Is(err, cause) {
		t.Fatalf("errors.Is(%v, cause)=false", err)
	}
	if got, want := typed.Error(), "server status: unauthorized"; got != want {
		t.Fatalf("Error()=%q want=%q", got, want)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type testNetError struct {
	temporary bool
	timeout   bool
}

func (e testNetError) Error() string   { return "test network error" }
func (e testNetError) Temporary() bool { return e.temporary }
func (e testNetError) Timeout() bool   { return e.timeout }
