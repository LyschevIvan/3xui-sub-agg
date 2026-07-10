package xui

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

type ErrorKind string

const (
	ErrorUnauthorized       ErrorKind = "unauthorized"
	ErrorUnsupportedVersion ErrorKind = "unsupported_version"
	ErrorAPI                ErrorKind = "api"
	ErrorTransport          ErrorKind = "transport"
	ErrorDecode             ErrorKind = "decode"
)

type Error struct {
	Kind       ErrorKind
	Op         string
	StatusCode int
	Message    string
	Err        error
}

func (e *Error) Error() string {
	msg := e.Message
	if msg == "" {
		msg = string(e.Kind)
	}
	if e.Op == "" {
		return msg
	}
	return e.Op + ": " + msg
}

func (e *Error) Unwrap() error { return e.Err }

func IsKind(err error, kind ErrorKind) bool {
	var typed *Error
	return errors.As(err, &typed) && typed.Kind == kind
}

type APIConfig struct {
	BaseURL            string
	PanelPath          string
	Token              string
	InsecureSkipVerify bool
	Timeout            time.Duration
	HTTPClient         *http.Client
}

type APIClient struct {
	transport *apiTransport
}

type apiTransport struct {
	baseURL    *url.URL
	token      string
	httpClient *http.Client
}

type apiEnvelope[T any] struct {
	Success *bool  `json:"success"`
	Msg     string `json:"msg"`
	Obj     T      `json:"obj"`
}

func NewAPI(cfg APIConfig) (*APIClient, error) {
	if cfg.Token == "" {
		return nil, &Error{
			Kind:    ErrorTransport,
			Op:      "new API",
			Message: "token is required",
		}
	}

	baseURL, err := url.Parse(cfg.BaseURL)
	if err != nil {
		return nil, &Error{
			Kind:    ErrorTransport,
			Op:      "new API",
			Message: "invalid base URL",
			Err:     err,
		}
	}
	baseURL.Scheme = strings.ToLower(baseURL.Scheme)
	if (baseURL.Scheme != "http" && baseURL.Scheme != "https") || baseURL.Host == "" {
		return nil, &Error{
			Kind:    ErrorTransport,
			Op:      "new API",
			Message: "base URL must use http or https",
		}
	}

	basePath := path.Join(
		"/",
		strings.Trim(baseURL.Path, "/"),
		strings.Trim(cfg.PanelPath, "/"),
	)
	if !strings.HasSuffix(basePath, "/panel/api") {
		basePath = path.Join(basePath, "panel/api")
	}
	baseURL.Path = strings.TrimRight(basePath, "/") + "/"
	baseURL.RawPath = ""
	baseURL.RawQuery = ""
	baseURL.Fragment = ""

	httpClient := apiHTTPClient(cfg)
	return &APIClient{transport: &apiTransport{
		baseURL:    baseURL,
		token:      cfg.Token,
		httpClient: httpClient,
	}}, nil
}

func apiHTTPClient(cfg APIConfig) *http.Client {
	var client *http.Client
	if cfg.HTTPClient != nil {
		cloned := *cfg.HTTPClient
		client = &cloned
		if cfg.Timeout > 0 {
			client.Timeout = cfg.Timeout
		}
	} else {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig = &tls.Config{
			InsecureSkipVerify: cfg.InsecureSkipVerify, //nolint:gosec // explicitly controlled by configuration
		}
		client = &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client
}

func doAPI[T any](
	ctx context.Context,
	t *apiTransport,
	method string,
	rel string,
	body any,
	host string,
) (T, error) {
	var zero T
	op := method + " " + strings.TrimLeft(rel, "/")

	var requestBody []byte
	if body != nil {
		var err error
		requestBody, err = json.Marshal(body)
		if err != nil {
			return zero, &Error{
				Kind:    ErrorDecode,
				Op:      op,
				Message: "encode request",
				Err:     err,
			}
		}
	}

	endpoint := *t.baseURL
	endpoint.Path += strings.TrimLeft(rel, "/")
	endpoint.RawPath = ""
	requestURL := endpoint.String()

	attempts := 1
	if method == http.MethodGet {
		attempts = 2
	}
	for attempt := 0; attempt < attempts; attempt++ {
		var reader io.Reader
		if requestBody != nil {
			reader = bytes.NewReader(requestBody)
		}
		req, err := http.NewRequestWithContext(ctx, method, requestURL, reader)
		if err != nil {
			return zero, &Error{
				Kind:    ErrorTransport,
				Op:      op,
				Message: "build request",
				Err:     err,
			}
		}
		req.Header.Set("Authorization", "Bearer "+t.token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("X-Requested-With", "XMLHttpRequest")
		if requestBody != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if host != "" {
			req.Host = host
		}

		resp, err := t.httpClient.Do(req)
		if err != nil {
			if attempt+1 < attempts && retryableNetworkError(err) {
				continue
			}
			return zero, &Error{
				Kind:    ErrorTransport,
				Op:      op,
				Message: "request failed",
				Err:     err,
			}
		}

		responseBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return zero, &Error{
				Kind:       ErrorTransport,
				Op:         op,
				StatusCode: resp.StatusCode,
				Message:    "read response",
				Err:        readErr,
			}
		}

		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusNotFound {
			return zero, &Error{
				Kind:       ErrorUnauthorized,
				Op:         op,
				StatusCode: resp.StatusCode,
				Message:    "unauthorized",
			}
		}
		if resp.StatusCode != http.StatusOK {
			return zero, &Error{
				Kind:       ErrorTransport,
				Op:         op,
				StatusCode: resp.StatusCode,
				Message:    responseMessage(fmt.Sprintf("http %d", resp.StatusCode), responseBody, t.token),
			}
		}

		var envelope apiEnvelope[T]
		if err := json.Unmarshal(responseBody, &envelope); err != nil {
			return zero, &Error{
				Kind:       ErrorDecode,
				Op:         op,
				StatusCode: resp.StatusCode,
				Message:    responseMessage("decode response", responseBody, t.token),
				Err:        err,
			}
		}
		if envelope.Success == nil {
			return zero, &Error{
				Kind:       ErrorDecode,
				Op:         op,
				StatusCode: resp.StatusCode,
				Message:    responseMessage("missing success field", responseBody, t.token),
			}
		}
		if !*envelope.Success {
			return zero, &Error{
				Kind:       ErrorAPI,
				Op:         op,
				StatusCode: resp.StatusCode,
				Message:    redact(envelope.Msg, t.token),
			}
		}
		return envelope.Obj, nil
	}

	return zero, &Error{Kind: ErrorTransport, Op: op, Message: "request failed"}
}

func retryableNetworkError(err error) bool {
	for current := err; current != nil; current = errors.Unwrap(current) {
		networkError, ok := current.(net.Error)
		if ok && (networkError.Timeout() || networkError.Temporary()) {
			return true
		}
	}
	return false
}

func responseMessage(prefix string, body []byte, token string) string {
	snippet := redact(string(body), token)
	const maxSnippetLength = 200
	if len(snippet) > maxSnippetLength {
		snippet = snippet[:maxSnippetLength] + "..."
	}
	if snippet == "" {
		return prefix
	}
	return prefix + ": " + snippet
}

func redact(value, token string) string {
	if token == "" {
		return value
	}
	return strings.ReplaceAll(value, token, "[REDACTED]")
}
