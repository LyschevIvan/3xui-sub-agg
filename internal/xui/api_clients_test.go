package xui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newAPIServer(t *testing.T, handler func(http.ResponseWriter, *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(handler))
}

func mustAPI(t *testing.T, baseURL string) *APIClient {
	t.Helper()
	c, err := NewAPI(APIConfig{
		BaseURL:   baseURL,
		PanelPath: "/",
		Token:     "test-token",
		Timeout:   time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestListClientsUsesPagedNativeEndpoint(t *testing.T) {
	ts := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/panel/api/clients/list/paged" {
			http.Error(w, "wrong endpoint", http.StatusNotFound)
			return
		}
		if got := r.URL.Query().Get("pageSize"); got != strconv.Itoa(clientPageSize) {
			t.Fatalf("pageSize=%q want=%d", got, clientPageSize)
		}
		_, _ = io.WriteString(w, `{"success":true,"msg":"","obj":{"items":[],"total":0,"filtered":0,"page":1,"pageSize":200}}`)
	})
	defer ts.Close()

	if _, err := mustAPI(t, ts.URL).ListClients(context.Background()); err != nil {
		t.Fatalf("ListClients() error = %v", err)
	}
}

func TestListClientsReadsEveryPage(t *testing.T) {
	requests := 0
	ts := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet {
			t.Errorf("method=%q", r.Method)
		}
		if r.URL.EscapedPath() != "/panel/api/clients/list/paged" {
			t.Errorf("path=%q", r.URL.EscapedPath())
		}
		if r.URL.RawQuery != "page="+strconv.Itoa(requests)+"&pageSize=200" {
			t.Errorf("query=%q", r.URL.RawQuery)
		}

		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		items := `[{"id":11,"email":"a","subId":"sub-a","enable":true,"totalGB":100,"expiryTime":200,"limitIp":1,"reset":0,"group":"group-a","comment":"comment-a","inboundIds":[1],"createdAt":300,"updatedAt":400}]`
		if page == 2 {
			items = `[{"id":12,"email":"b","subId":"sub-b","enable":true,"totalGB":101,"expiryTime":201,"limitIp":2,"reset":1,"group":"group-b","comment":"comment-b","inboundIds":[2],"createdAt":301,"updatedAt":401}]`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"success":true,"msg":"","obj":{"items":%s,"total":2,"filtered":2,"page":%d,"pageSize":1}}`, items, page)
	})
	defer ts.Close()

	rows, err := mustAPI(t, ts.URL).ListClients(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(rows) != 2 || rows[1].Email != "b" {
		t.Fatalf("requests=%d rows=%+v", requests, rows)
	}
	if rows[0].RecordID == nil || *rows[0].RecordID != 11 {
		t.Fatalf("first RecordID=%v", rows[0].RecordID)
	}
}

func TestListClientsPreservesDuplicatesFromMixedPage(t *testing.T) {
	requests := 0
	ts := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		items := `[{"id":11,"email":"a","subId":"sub-a","enable":true,"totalGB":100,"expiryTime":200,"limitIp":1,"reset":0,"group":"group-a","comment":"first a","inboundIds":[1],"createdAt":300,"updatedAt":400}]`
		if page == 2 {
			items = `[{"id":12,"email":"a","subId":"sub-a-duplicate","enable":true,"totalGB":101,"expiryTime":201,"limitIp":2,"reset":1,"group":"group-a2","comment":"second a","inboundIds":[2],"createdAt":301,"updatedAt":401},{"id":13,"email":"b","subId":"sub-b","enable":false,"totalGB":102,"expiryTime":202,"limitIp":3,"reset":2,"group":"group-b","comment":"b","inboundIds":[3],"createdAt":302,"updatedAt":402}]`
		} else if page > 2 {
			items = `[]`
		}
		_, _ = fmt.Fprintf(w, `{"success":true,"msg":"","obj":{"items":%s,"total":3,"filtered":3,"page":%d,"pageSize":200}}`, items, page)
	})
	defer ts.Close()

	rows, err := mustAPI(t, ts.URL).ListClients(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests=%d want=2", requests)
	}
	if len(rows) != 3 {
		t.Fatalf("rows=%+v", rows)
	}
	wantEmails := []string{"a", "a", "b"}
	wantRecordIDs := []int{11, 12, 13}
	for i, row := range rows {
		if row.Email != wantEmails[i] || row.RecordID == nil || *row.RecordID != wantRecordIDs[i] {
			t.Fatalf("row %d=%+v want email=%q id=%d", i, row, wantEmails[i], wantRecordIDs[i])
		}
	}
}

func TestListClientsStopsOnEmptyPage(t *testing.T) {
	requests := 0
	ts := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		items := `[{"id":11,"email":"a","subId":"sub-a","enable":true,"totalGB":100,"expiryTime":200,"limitIp":1,"reset":0,"group":"","comment":"","inboundIds":[1],"createdAt":300,"updatedAt":400}]`
		if requests == 2 {
			items = `[]`
		}
		_, _ = fmt.Fprintf(w, `{"success":true,"msg":"","obj":{"items":%s,"total":10,"filtered":10,"page":%d,"pageSize":200}}`, items, requests)
	})
	defer ts.Close()

	rows, err := mustAPI(t, ts.URL).ListClients(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(rows) != 1 {
		t.Fatalf("requests=%d rows=%+v", requests, rows)
	}
}

func TestListClientsStopsWhenPageAddsNoNewEmail(t *testing.T) {
	requests := 0
	ts := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = fmt.Fprintf(w, `{"success":true,"msg":"","obj":{"items":[{"id":11,"email":"same","subId":"sub-a","enable":true,"totalGB":100,"expiryTime":200,"limitIp":1,"reset":0,"group":"","comment":"","inboundIds":[1],"createdAt":300,"updatedAt":400}],"total":10,"filtered":10,"page":%d,"pageSize":200}}`, requests)
	})
	defer ts.Close()

	rows, err := mustAPI(t, ts.URL).ListClients(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if requests != 2 || len(rows) != 1 {
		t.Fatalf("requests=%d rows=%+v", requests, rows)
	}
}

func TestListClientsStopsAtSafetyCap(t *testing.T) {
	const wantMaxPages = 1000
	requests := 0
	ts := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests > wantMaxPages {
			_, _ = io.WriteString(w, `{"success":false,"msg":"guard leaked user-1000@example.com sub-1000 1001","obj":null}`)
			return
		}
		_, _ = fmt.Fprintf(
			w,
			`{"success":true,"msg":"","obj":{"items":[{"id":%d,"email":"user-%d@example.com","subId":"sub-%d","enable":true,"totalGB":100,"expiryTime":200,"limitIp":1,"reset":0,"group":"","comment":"","inboundIds":[1],"createdAt":300,"updatedAt":400}],"total":%d,"filtered":%d,"page":%d,"pageSize":200}}`,
			requests,
			requests,
			requests,
			requests+1,
			requests+1,
			requests,
		)
	})
	defer ts.Close()

	rows, err := mustAPI(t, ts.URL).ListClients(context.Background())
	if !IsKind(err, ErrorAPI) {
		t.Fatalf("err=%v", err)
	}
	if rows != nil {
		t.Fatalf("rows=%d want nil on safety error", len(rows))
	}
	if requests != wantMaxPages {
		t.Fatalf("requests=%d want=%d", requests, wantMaxPages)
	}
	for _, sensitive := range []string{"user-", "sub-", "1001", "200000"} {
		if strings.Contains(err.Error(), sensitive) {
			t.Fatalf("error leaked %q: %v", sensitive, err)
		}
	}
}

func TestListClientsReturnsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := 0
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		if requests == 2 {
			cancel()
			return nil, ctx.Err()
		}
		return testHTTPResponse(http.StatusOK, io.NopCloser(strings.NewReader(
			`{"success":true,"msg":"","obj":{"items":[{"id":11,"email":"a","subId":"sub-a","enable":true,"totalGB":100,"expiryTime":200,"limitIp":1,"reset":0,"group":"","comment":"","inboundIds":[1],"createdAt":300,"updatedAt":400}],"total":2,"filtered":2,"page":1,"pageSize":200}}`,
		))), nil
	})
	c, err := NewAPI(APIConfig{
		BaseURL:    "https://panel.example",
		Token:      "test-token",
		HTTPClient: &http.Client{Transport: rt},
	})
	if err != nil {
		t.Fatal(err)
	}

	rows, err := c.ListClients(ctx)
	if !IsKind(err, ErrorTransport) || !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v canceled=%t", err, errors.Is(err, context.Canceled))
	}
	if rows != nil || requests != 2 {
		t.Fatalf("rows=%v requests=%d", rows, requests)
	}
}

type recordedClientRequest struct {
	Method   string
	Path     string
	RawQuery string
	Host     string
	Body     any
}

func TestClientEndpointsUseOfficialWireShape(t *testing.T) {
	const email = "user/name?tag#x@example.com"
	var requests []recordedClientRequest
	detailResponse := `{
		"success": true,
		"msg": "",
		"obj": {
			"client": {
				"id": 17,
				"email": "user/name?tag#x@example.com",
				"subId": "subscription-id",
				"uuid": "uuid-value",
				"password": "password-value",
				"auth": "auth-value",
				"flow": "xtls-rprx-vision",
				"security": "auto",
				"reverse": {"tag": "reverse-tag"},
				"privateKey": "private-key",
				"publicKey": "public-key",
				"allowedIPs": "10.0.0.1/32,fd00::/8",
				"preSharedKey": "pre-shared-key",
				"keepAlive": 25,
				"limitIp": 3,
				"totalGB": 987654321,
				"expiryTime": 1900000000000,
				"enable": true,
				"tgId": 123456789,
				"group": "premium",
				"comment": "full official fixture",
				"reset": 2,
				"createdAt": 1700000000000,
				"updatedAt": 1800000000000
			},
			"inboundIds": [1, 2],
			"externalLinks": [{"id": 9, "remark": "external", "url": "https://external.example/sub"}],
			"usedTraffic": 321
		}
	}`

	ts := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		var body any
		if len(bodyBytes) > 0 {
			if err := json.Unmarshal(bodyBytes, &body); err != nil {
				t.Errorf("decode request body: %v", err)
			}
		}
		requests = append(requests, recordedClientRequest{
			Method:   r.Method,
			Path:     r.URL.EscapedPath(),
			RawQuery: r.URL.RawQuery,
			Host:     r.Host,
			Body:     body,
		})

		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, detailResponse)
			return
		}
		_, _ = io.WriteString(w, `{"success":true,"msg":"","obj":null}`)
	})
	defer ts.Close()

	c := mustAPI(t, ts.URL)
	detail, err := c.GetClient(context.Background(), email)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Client.RecordID != 17 || detail.Client.UUID != "uuid-value" || detail.UsedTraffic != 321 {
		t.Fatalf("detail=%+v", detail)
	}
	if !slices.Equal(detail.InboundIDs, []int{1, 2}) || len(detail.ExternalLinks) != 1 {
		t.Fatalf("detail associations=%+v", detail)
	}
	var externalLink any
	if err := json.Unmarshal(detail.ExternalLinks[0], &externalLink); err != nil {
		t.Fatal(err)
	}
	assertDecodedJSON(t, externalLink, `{"id":9,"remark":"external","url":"https://external.example/sub"}`)
	payload, err := detail.Client.Payload()
	if err != nil {
		t.Fatal(err)
	}
	wantPayload := ClientPayload{
		ID:           "uuid-value",
		Security:     "auto",
		Password:     "password-value",
		Flow:         "xtls-rprx-vision",
		Reverse:      &ClientReverse{Tag: "reverse-tag"},
		Auth:         "auth-value",
		PrivateKey:   "private-key",
		PublicKey:    "public-key",
		AllowedIPs:   []string{"10.0.0.1/32", "fd00::/8"},
		PreSharedKey: "pre-shared-key",
		KeepAlive:    25,
		Email:        email,
		LimitIP:      3,
		TotalGB:      987654321,
		ExpiryTime:   1900000000000,
		Enable:       true,
		TgID:         123456789,
		SubID:        "subscription-id",
		Group:        "premium",
		Comment:      "full official fixture",
		Reset:        2,
		CreatedAt:    1700000000000,
		UpdatedAt:    1800000000000,
	}
	if !reflect.DeepEqual(payload, wantPayload) {
		t.Fatalf("payload=%+v want=%+v", payload, wantPayload)
	}

	if err := c.AddClient(context.Background(), payload, []int{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := c.UpdateClient(context.Background(), email, payload); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteClient(context.Background(), email); err != nil {
		t.Fatal(err)
	}
	if err := c.AttachClient(context.Background(), email, []int{2}); err != nil {
		t.Fatal(err)
	}
	if err := c.DetachClient(context.Background(), email, []int{1}); err != nil {
		t.Fatal(err)
	}

	host := strings.TrimPrefix(ts.URL, "http://")
	escapedEmail := url.PathEscape(email)
	want := []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodGet, path: "/panel/api/clients/get/" + escapedEmail},
		{
			method: http.MethodPost,
			path:   "/panel/api/clients/add",
			body: `{
				"client": {
					"id":"uuid-value","security":"auto","password":"password-value","flow":"xtls-rprx-vision",
					"reverse":{"tag":"reverse-tag"},"auth":"auth-value","privateKey":"private-key","publicKey":"public-key",
					"allowedIPs":["10.0.0.1/32","fd00::/8"],"preSharedKey":"pre-shared-key","keepAlive":25,
					"email":"user/name?tag#x@example.com","limitIp":3,"totalGB":987654321,"expiryTime":1900000000000,
					"enable":true,"tgId":123456789,"subId":"subscription-id","group":"premium","comment":"full official fixture",
					"reset":2,"created_at":1700000000000,"updated_at":1800000000000
				},
				"inboundIds":[1,2]
			}`,
		},
		{
			method: http.MethodPost,
			path:   "/panel/api/clients/update/" + escapedEmail,
			body: `{
				"id":"uuid-value","security":"auto","password":"password-value","flow":"xtls-rprx-vision",
				"reverse":{"tag":"reverse-tag"},"auth":"auth-value","privateKey":"private-key","publicKey":"public-key",
				"allowedIPs":["10.0.0.1/32","fd00::/8"],"preSharedKey":"pre-shared-key","keepAlive":25,
				"email":"user/name?tag#x@example.com","limitIp":3,"totalGB":987654321,"expiryTime":1900000000000,
				"enable":true,"tgId":123456789,"subId":"subscription-id","group":"premium","comment":"full official fixture",
				"reset":2,"created_at":1700000000000,"updated_at":1800000000000
			}`,
		},
		{method: http.MethodPost, path: "/panel/api/clients/del/" + escapedEmail},
		{method: http.MethodPost, path: "/panel/api/clients/" + escapedEmail + "/attach", body: `{"inboundIds":[2]}`},
		{method: http.MethodPost, path: "/panel/api/clients/" + escapedEmail + "/detach", body: `{"inboundIds":[1]}`},
	}

	if len(requests) != len(want) {
		t.Fatalf("requests=%d want=%d: %+v", len(requests), len(want), requests)
	}
	for i, expected := range want {
		got := requests[i]
		if got.Method != expected.method || got.Path != expected.path || got.RawQuery != "" || got.Host != host {
			t.Errorf("request %d = %+v, want method=%q path=%q query=%q host=%q", i, got, expected.method, expected.path, "", host)
		}
		assertDecodedJSON(t, got.Body, expected.body)
	}
}

func TestClientEndpointsSerializeMandatoryZeroValues(t *testing.T) {
	var body any
	ts := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		_, _ = io.WriteString(w, `{"success":true,"msg":"","obj":null}`)
	})
	defer ts.Close()

	payload := ClientPayload{Email: "zero@example.com"}
	if err := mustAPI(t, ts.URL).AddClient(context.Background(), payload, []int{}); err != nil {
		t.Fatal(err)
	}
	assertDecodedJSON(t, body, `{
		"client": {
			"security":"",
			"email":"zero@example.com",
			"limitIp":0,
			"totalGB":0,
			"expiryTime":0,
			"enable":false,
			"tgId":0,
			"subId":"",
			"comment":"",
			"reset":0
		},
		"inboundIds":[]
	}`)
}

func TestClientEndpointsSanitizeUpstreamErrors(t *testing.T) {
	const (
		email        = "secret/user?tag#fragment@example.com"
		subID        = "secret/sub?id#fragment"
		password     = "password-sensitive-value"
		auth         = "auth-sensitive-value"
		privateKey   = "private-key-sensitive-value"
		publicKey    = "public-key-sensitive-value"
		preSharedKey = "pre-shared-key-sensitive-value"
	)
	payload := ClientPayload{
		ID:           "uuid-sensitive-value",
		Security:     "auto",
		Password:     password,
		Auth:         auth,
		PrivateKey:   privateKey,
		PublicKey:    publicKey,
		PreSharedKey: preSharedKey,
		Email:        email,
		Enable:       true,
		SubID:        subID,
		Comment:      "comment-sensitive-value",
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		op   string
		call func(*APIClient) error
	}{
		{name: "list", op: "list clients", call: func(c *APIClient) error {
			_, err := c.ListClients(context.Background())
			return err
		}},
		{name: "get", op: "get client", call: func(c *APIClient) error {
			_, err := c.GetClient(context.Background(), email)
			return err
		}},
		{name: "add", op: "add client", call: func(c *APIClient) error {
			return c.AddClient(context.Background(), payload, []int{1, 2})
		}},
		{name: "update", op: "update client", call: func(c *APIClient) error {
			return c.UpdateClient(context.Background(), email, payload)
		}},
		{name: "delete", op: "delete client", call: func(c *APIClient) error {
			return c.DeleteClient(context.Background(), email)
		}},
		{name: "attach", op: "attach client", call: func(c *APIClient) error {
			return c.AttachClient(context.Background(), email, []int{2})
		}},
		{name: "detach", op: "detach client", call: func(c *APIClient) error {
			return c.DetachClient(context.Background(), email, []int{1})
		}},
		{name: "subscription links", op: "subscription links", call: func(c *APIClient) error {
			_, err := c.SubLinks(context.Background(), subID, "subscriptions.example")
			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requestURI, requestBody string
			ts := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
				body, readErr := io.ReadAll(r.Body)
				if readErr != nil {
					t.Errorf("read body: %v", readErr)
				}
				requestURI = r.URL.RequestURI()
				requestBody = string(body)
				message := strings.Join([]string{
					"email=" + email,
					"escapedEmail=" + url.PathEscape(email),
					"subID=" + subID,
					"escapedSubID=" + url.PathEscape(subID),
					"password=" + password,
					"auth=" + auth,
					"privateKey=" + privateKey,
					"publicKey=" + publicKey,
					"preSharedKey=" + preSharedKey,
					"payload=" + string(payloadJSON),
					"requestBody=" + requestBody,
					"requestURL=" + requestURI,
					"token=test-token",
				}, " ")
				w.Header().Set("Content-Type", "application/json")
				if encodeErr := json.NewEncoder(w).Encode(map[string]any{
					"success": false,
					"msg":     message,
					"obj":     nil,
				}); encodeErr != nil {
					t.Errorf("encode response: %v", encodeErr)
				}
			})
			defer ts.Close()

			err := tt.call(mustAPI(t, ts.URL))
			if !IsKind(err, ErrorAPI) {
				t.Fatalf("err=%v", err)
			}
			var typed *Error
			if !errors.As(err, &typed) {
				t.Fatalf("err=%v", err)
			}
			if typed.Kind != ErrorAPI || typed.StatusCode != http.StatusOK || typed.Op != tt.op || typed.Message != "request failed" {
				t.Fatalf("typed error=%+v", typed)
			}

			for _, sensitive := range []string{
				email,
				url.PathEscape(email),
				subID,
				url.PathEscape(subID),
				password,
				auth,
				privateKey,
				publicKey,
				preSharedKey,
				string(payloadJSON),
				requestBody,
				requestURI,
				"test-token",
			} {
				if sensitive != "" && strings.Contains(err.Error(), sensitive) {
					t.Fatalf("error leaked %q: %v", sensitive, err)
				}
			}
		})
	}
}

func TestClientEndpointsDoNotRetryPOSTTransportErrors(t *testing.T) {
	payload := ClientPayload{
		ID:       "uuid-value",
		Security: "auto",
		Email:    "user@example.com",
		Enable:   true,
		SubID:    "subscription-id",
		Comment:  "comment",
	}
	tests := []struct {
		name string
		call func(*APIClient) error
	}{
		{name: "add", call: func(c *APIClient) error { return c.AddClient(context.Background(), payload, []int{1}) }},
		{name: "update", call: func(c *APIClient) error { return c.UpdateClient(context.Background(), payload.Email, payload) }},
		{name: "delete", call: func(c *APIClient) error { return c.DeleteClient(context.Background(), payload.Email) }},
		{name: "attach", call: func(c *APIClient) error { return c.AttachClient(context.Background(), payload.Email, []int{1}) }},
		{name: "detach", call: func(c *APIClient) error { return c.DetachClient(context.Background(), payload.Email, []int{1}) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempts := 0
			rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
				attempts++
				return nil, testNetError{temporary: true}
			})
			c, err := NewAPI(APIConfig{
				BaseURL:    "https://panel.example",
				Token:      "test-token",
				HTTPClient: &http.Client{Transport: rt},
			})
			if err != nil {
				t.Fatal(err)
			}

			if err := tt.call(c); !IsKind(err, ErrorTransport) {
				t.Fatalf("err=%v", err)
			}
			if attempts != 1 {
				t.Fatalf("attempts=%d want=1", attempts)
			}
		})
	}
}

func TestClientEndpointsDoNotRetryPOSTResponseBodyErrors(t *testing.T) {
	body := &readErrorCloser{err: testNetError{temporary: true}}
	attempts := 0
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return testHTTPResponse(http.StatusOK, body), nil
	})
	c, err := NewAPI(APIConfig{
		BaseURL:    "https://panel.example",
		Token:      "test-token",
		HTTPClient: &http.Client{Transport: rt},
	})
	if err != nil {
		t.Fatal(err)
	}

	err = c.AddClient(context.Background(), ClientPayload{Email: "user@example.com"}, []int{1})
	if !IsKind(err, ErrorTransport) {
		t.Fatalf("err=%v", err)
	}
	if attempts != 1 || !body.closed {
		t.Fatalf("attempts=%d body closed=%t", attempts, body.closed)
	}
}

func TestSubLinksUsesEscapedSubIDAndEffectiveHost(t *testing.T) {
	const (
		subID         = "sub/id?tag#fragment"
		effectiveHost = "subscriptions.example:8443"
	)
	wantLinks := []string{"vmess://one", "", "vmess://one", "vless://two"}
	requests := 0
	ts := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet {
			t.Errorf("method=%q", r.Method)
		}
		if got, want := r.URL.EscapedPath(), "/panel/api/clients/subLinks/"+url.PathEscape(subID); got != want {
			t.Errorf("path=%q want=%q", got, want)
		}
		if r.URL.RawQuery != "" {
			t.Errorf("query=%q", r.URL.RawQuery)
		}
		if r.Host != effectiveHost {
			t.Errorf("Host=%q want=%q", r.Host, effectiveHost)
		}
		if got := r.Header.Get("X-Forwarded-Host"); got != "" {
			t.Errorf("X-Forwarded-Host=%q", got)
		}
		_, _ = io.WriteString(w, `{"success":true,"msg":"","obj":["vmess://one","","vmess://one","vless://two"]}`)
	})
	defer ts.Close()

	links, err := mustAPI(t, ts.URL).SubLinks(context.Background(), subID, effectiveHost)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || !slices.Equal(links, wantLinks) {
		t.Fatalf("requests=%d links=%q want=%q", requests, links, wantLinks)
	}
}

func TestSubLinksRedactsCapabilityFromErrors(t *testing.T) {
	const subID = "secret/sub?id#fragment"
	ts := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(
			w,
			`{"success":false,"msg":%q,"obj":null}`,
			"failed for "+subID+" using test-token",
		)
	})
	defer ts.Close()

	_, err := mustAPI(t, ts.URL).SubLinks(context.Background(), subID, "subscriptions.example")
	if !IsKind(err, ErrorAPI) {
		t.Fatalf("err=%v", err)
	}
	var typed *Error
	if !errors.As(err, &typed) || typed.StatusCode != http.StatusOK {
		t.Fatalf("typed error=%+v", typed)
	}
	for _, secret := range []string{subID, url.PathEscape(subID), "test-token"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked %q: %v", secret, err)
		}
	}

	cause := errors.New("transport failure for " + subID)
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, cause
	})
	c, newErr := NewAPI(APIConfig{
		BaseURL:    "https://panel.example",
		Token:      "test-token",
		HTTPClient: &http.Client{Transport: rt},
	})
	if newErr != nil {
		t.Fatal(newErr)
	}
	_, err = c.SubLinks(context.Background(), subID, "subscriptions.example")
	if !IsKind(err, ErrorTransport) || !errors.Is(err, cause) {
		t.Fatalf("transport err=%v cause preserved=%t", err, errors.Is(err, cause))
	}
	if strings.Contains(err.Error(), subID) || strings.Contains(err.Error(), url.PathEscape(subID)) {
		t.Fatalf("transport error leaked subID: %v", err)
	}
}

func assertDecodedJSON(t *testing.T, got any, expectedJSON string) {
	t.Helper()
	if expectedJSON == "" {
		if got != nil {
			t.Errorf("body=%v want no body", got)
		}
		return
	}

	var want any
	if err := json.Unmarshal([]byte(expectedJSON), &want); err != nil {
		t.Fatalf("decode expected JSON: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("body=%v want=%v", got, want)
	}
}
