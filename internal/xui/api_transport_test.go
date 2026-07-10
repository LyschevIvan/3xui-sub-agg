package xui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
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

func TestAPIClientParsesRelativeQuery(t *testing.T) {
	var requests int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got, want := r.URL.EscapedPath(), "/panel/api/clients/list"; got != want {
			t.Errorf("path=%q want=%q", got, want)
		}
		if got, want := r.URL.RawQuery, "page=1&pageSize=200"; got != want {
			t.Errorf("query=%q want=%q", got, want)
		}
		if got, want := r.URL.Query().Get("page"), "1"; got != want {
			t.Errorf("page=%q want=%q", got, want)
		}
		if got, want := r.URL.Query().Get("pageSize"), "200"; got != want {
			t.Errorf("pageSize=%q want=%q", got, want)
		}
		_, _ = io.WriteString(w, `{"success":true,"msg":"","obj":{}}`)
	}))
	defer ts.Close()

	c, err := NewAPI(APIConfig{
		BaseURL: ts.URL,
		Token:   "top-secret",
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doAPI[struct{}](
		context.Background(),
		c.transport,
		http.MethodGet,
		"clients/list?page=1&pageSize=200",
		nil,
		"",
	); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("requests=%d want=1", requests)
	}
}

func TestAPIClientPreservesEscapedRelativePath(t *testing.T) {
	tests := []struct {
		name    string
		segment string
	}{
		{name: "reserved characters", segment: "user/name?tag#fragment%done"},
		{name: "single dot", segment: "."},
		{name: "double dot", segment: ".."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			escapedSegment := url.PathEscape(tt.segment)
			var requests int
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				wantPath := "/panel/api/clients/get/" + escapedSegment
				if got := r.URL.EscapedPath(); got != wantPath {
					t.Errorf("escaped path=%q want=%q", got, wantPath)
				}
				if got := strings.TrimPrefix(r.URL.Path, "/panel/api/clients/get/"); got != tt.segment {
					t.Errorf("decoded segment=%q want=%q", got, tt.segment)
				}
				if r.URL.RawQuery != "" {
					t.Errorf("query=%q want empty", r.URL.RawQuery)
				}
				_, _ = io.WriteString(w, `{"success":true,"msg":"","obj":{}}`)
			}))
			defer ts.Close()

			c, err := NewAPI(APIConfig{
				BaseURL: ts.URL,
				Token:   "top-secret",
				Timeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := doAPI[struct{}](
				context.Background(),
				c.transport,
				http.MethodGet,
				"clients/get/"+escapedSegment,
				nil,
				"",
			); err != nil {
				t.Fatal(err)
			}
			if requests != 1 {
				t.Fatalf("requests=%d want=1", requests)
			}
		})
	}
}

func TestAPIClientRejectsAbsoluteAPIReferences(t *testing.T) {
	for _, rel := range []string{
		"https://attacker.example/clients/list",
		"//attacker.example/clients/list",
		"/clients/list",
		"///clients/list",
	} {
		t.Run(rel, func(t *testing.T) {
			requests := 0
			rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
				requests++
				return testHTTPResponse(http.StatusOK, io.NopCloser(strings.NewReader(
					`{"success":true,"msg":"","obj":{}}`,
				))), nil
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
				http.MethodGet,
				rel,
				nil,
				"",
			)
			if !IsKind(err, ErrorTransport) {
				t.Fatalf("err=%v want transport error", err)
			}
			if requests != 0 {
				t.Fatalf("requests=%d want=0", requests)
			}
		})
	}
}

func TestAPIClientOperationNeverContainsRelativeTarget(t *testing.T) {
	const secret = "secret/path?id#fragment"
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return testHTTPResponse(http.StatusOK, io.NopCloser(strings.NewReader(
			`{"success":false,"msg":"request failed","obj":null}`,
		))), nil
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
		http.MethodGet,
		"clients/get/"+url.PathEscape(secret),
		nil,
		"",
	)
	if !IsKind(err, ErrorAPI) {
		t.Fatalf("err=%v", err)
	}
	var typed *Error
	if !errors.As(err, &typed) {
		t.Fatalf("err=%v", err)
	}
	if typed.Op != "3x-ui GET" {
		t.Fatalf("Op=%q want=%q", typed.Op, "3x-ui GET")
	}
	for _, value := range []string{secret, url.PathEscape(secret)} {
		if strings.Contains(err.Error(), value) {
			t.Fatalf("error leaked relative target %q: %v", value, err)
		}
	}
}

func TestAPIClientIgnoresInjectedCookieJar(t *testing.T) {
	var cookieHeaders []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookieHeaders = append(cookieHeaders, r.Header.Get("Cookie"))
		http.SetCookie(w, &http.Cookie{Name: "response_session", Value: "stored", Path: "/"})
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"msg":"","obj":{"panelVersion":"v3.4.2"}}`)
	}))
	defer ts.Close()

	serverURL, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	jar.SetCookies(serverURL, []*http.Cookie{{Name: "legacy_session", Value: "cookie-secret"}})
	injected := &http.Client{Jar: jar, Timeout: time.Second}

	c, err := NewAPI(APIConfig{
		BaseURL:    ts.URL,
		Token:      "top-secret",
		HTTPClient: injected,
	})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := c.Validate(context.Background()); err != nil {
			t.Fatal(err)
		}
	}

	if want := []string{"", ""}; !slices.Equal(cookieHeaders, want) {
		t.Fatalf("Cookie headers=%q want=%q", cookieHeaders, want)
	}
	if injected.Jar != jar {
		t.Fatal("caller-owned HTTP client was mutated")
	}
	for _, cookie := range jar.Cookies(serverURL) {
		if cookie.Name == "response_session" {
			t.Fatalf("response cookie persisted in caller jar: %v", cookie)
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

func TestAPIClientClassifiesUnauthorizedBeforeReadingBody(t *testing.T) {
	for _, statusCode := range []int{http.StatusUnauthorized, http.StatusNotFound} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			body := &readErrorCloser{err: fmt.Errorf("body read: %w", testNetError{temporary: true})}
			rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
				return testHTTPResponse(statusCode, body), nil
			})
			c, err := NewAPI(APIConfig{
				BaseURL:    "https://panel.example",
				Token:      "top-secret",
				HTTPClient: &http.Client{Transport: rt},
			})
			if err != nil {
				t.Fatal(err)
			}

			_, err = c.Validate(context.Background())
			if !IsKind(err, ErrorUnauthorized) {
				t.Fatalf("err=%v", err)
			}
			if body.reads != 0 {
				t.Fatalf("body reads=%d want=0", body.reads)
			}
			if !body.closed {
				t.Fatal("response body was not closed")
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

func TestAPIClientRetriesTemporaryGETBodyReadErrors(t *testing.T) {
	tests := []struct {
		name      string
		temporary bool
		timeout   bool
	}{
		{name: "temporary", temporary: true},
		{name: "timeout", timeout: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			firstBody := &readErrorCloser{err: fmt.Errorf("body read: %w", testNetError{
				temporary: tt.temporary,
				timeout:   tt.timeout,
			})}
			secondBody := &trackingReadCloser{Reader: strings.NewReader(
				`{"success":true,"msg":"","obj":{"panelVersion":"v3.4.2"}}`,
			)}
			attempts := 0
			rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
				attempts++
				if attempts == 1 {
					return testHTTPResponse(http.StatusOK, firstBody), nil
				}
				return testHTTPResponse(http.StatusOK, secondBody), nil
			})
			c, err := NewAPI(APIConfig{
				BaseURL:    "https://panel.example",
				Token:      "top-secret",
				HTTPClient: &http.Client{Transport: rt},
			})
			if err != nil {
				t.Fatal(err)
			}

			status, err := c.Validate(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if status.PanelVersion != "v3.4.2" {
				t.Fatalf("PanelVersion=%q", status.PanelVersion)
			}
			if attempts != 2 {
				t.Fatalf("attempts=%d want=2", attempts)
			}
			if !firstBody.closed || !secondBody.closed {
				t.Fatalf("body closed states: first=%t second=%t", firstBody.closed, secondBody.closed)
			}
		})
	}
}

func TestAPIClientDoesNotRetryPOSTBodyReadErrors(t *testing.T) {
	body := &readErrorCloser{err: fmt.Errorf("body read: %w", testNetError{temporary: true})}
	attempts := 0
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return testHTTPResponse(http.StatusOK, body), nil
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
	if !IsKind(err, ErrorTransport) {
		t.Fatalf("err=%v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts=%d want=1", attempts)
	}
	if !body.closed {
		t.Fatal("response body was not closed")
	}
}

func TestAPIClientDoesNotRetryPermanentGETBodyReadErrors(t *testing.T) {
	body := &readErrorCloser{err: fmt.Errorf("body read: %w", testNetError{})}
	attempts := 0
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return testHTTPResponse(http.StatusOK, body), nil
	})
	c, err := NewAPI(APIConfig{
		BaseURL:    "https://panel.example",
		Token:      "top-secret",
		HTTPClient: &http.Client{Transport: rt},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.Validate(context.Background())
	if !IsKind(err, ErrorTransport) {
		t.Fatalf("err=%v", err)
	}
	if attempts != 1 {
		t.Fatalf("attempts=%d want=1", attempts)
	}
	if !body.closed {
		t.Fatal("response body was not closed")
	}
}

func TestAPIClientRetriesJoinedTemporaryTransportError(t *testing.T) {
	attempts := 0
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, fmt.Errorf("wrapped: %w", errors.Join(
				testNetError{},
				testNetError{temporary: true},
			))
		}
		return testHTTPResponse(http.StatusOK, io.NopCloser(strings.NewReader(
			`{"success":true,"msg":"","obj":{"panelVersion":"v3.4.2"}}`,
		))), nil
	})
	c, err := NewAPI(APIConfig{
		BaseURL:    "https://panel.example",
		Token:      "top-secret",
		HTTPClient: &http.Client{Transport: rt},
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := c.Validate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts=%d want=2", attempts)
	}
}

func TestAPIClientReplaysGETBodyAfterTransportRetry(t *testing.T) {
	var requestBodies [][]byte
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		requestBodies = append(requestBodies, body)
		if len(requestBodies) == 1 {
			return nil, testNetError{temporary: true}
		}
		if !bytes.Equal(body, requestBodies[0]) {
			return nil, fmt.Errorf("replayed body=%q want=%q", body, requestBodies[0])
		}
		return testHTTPResponse(http.StatusOK, io.NopCloser(strings.NewReader(
			`{"success":true,"msg":"","obj":{}}`,
		))), nil
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
		http.MethodGet,
		"server/status",
		map[string]string{"probe": "value"},
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte(`{"probe":"value"}`)
	if len(requestBodies) != 2 {
		t.Fatalf("request bodies=%d want=2", len(requestBodies))
	}
	for attempt, body := range requestBodies {
		if !bytes.Equal(body, want) {
			t.Fatalf("attempt %d body=%q want=%q", attempt+1, body, want)
		}
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

func TestAPIClientBoundsResponseBodies(t *testing.T) {
	const responseSizeLimit int64 = 16 << 20
	tests := []struct {
		name    string
		size    int64
		prefix  string
		wantErr bool
	}{
		{
			name:   "at boundary",
			size:   responseSizeLimit,
			prefix: `{"success":true,"msg":"","obj":{"panelVersion":"v3.4.2"}}`,
		},
		{
			name:    "over boundary",
			size:    responseSizeLimit + 4096,
			prefix:  `{"success":true,"msg":"top-secret","obj":{"panelVersion":"v3.4.2"}}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := &countingReader{Reader: paddedJSONReader(tt.prefix, tt.size)}
			body := &trackingReadCloser{Reader: source}
			rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
				return testHTTPResponse(http.StatusOK, body), nil
			})
			c, err := NewAPI(APIConfig{
				BaseURL:    "https://panel.example",
				Token:      "top-secret",
				HTTPClient: &http.Client{Transport: rt},
			})
			if err != nil {
				t.Fatal(err)
			}

			_, err = c.Validate(context.Background())
			if !tt.wantErr {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if !IsKind(err, ErrorTransport) {
					t.Fatalf("err=%v", err)
				}
				var typed *Error
				if !errors.As(err, &typed) || typed.Message != "response too large" {
					t.Fatalf("err=%v", err)
				}
				if strings.Contains(err.Error(), "top-secret") {
					t.Fatalf("token leaked: %v", err)
				}
			}
			if !body.closed {
				t.Fatal("response body was not closed")
			}
			if tt.wantErr && source.bytesRead != responseSizeLimit+1 {
				t.Fatalf("response bytes read=%d want=%d", source.bytesRead, responseSizeLimit+1)
			}
		})
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

type trackingReadCloser struct {
	io.Reader
	closed bool
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

type readErrorCloser struct {
	err    error
	reads  int
	closed bool
}

func (r *readErrorCloser) Read([]byte) (int, error) {
	r.reads++
	return 0, r.err
}

func (r *readErrorCloser) Close() error {
	r.closed = true
	return nil
}

type repeatedByteReader byte

func (r repeatedByteReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(r)
	}
	return len(p), nil
}

type countingReader struct {
	io.Reader
	bytesRead int64
}

func (r *countingReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.bytesRead += int64(n)
	return n, err
}

func paddedJSONReader(prefix string, size int64) io.Reader {
	padding := size - int64(len(prefix))
	if padding < 0 {
		panic("JSON prefix exceeds requested response size")
	}
	return io.MultiReader(
		strings.NewReader(prefix),
		io.LimitReader(repeatedByteReader(' '), padding),
	)
}

func testHTTPResponse(statusCode int, body io.ReadCloser) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Header:     make(http.Header),
		Body:       body,
	}
}
