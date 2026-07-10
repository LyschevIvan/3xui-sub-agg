package xui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestInboundDocumentPreservesUnknownFields(t *testing.T) {
	raw := []byte(`{
		"id": 7,
		"remark": "old",
		"port": 443,
		"enable": true,
		"future": {"x": 1},
		"settings": {"clients": [], "futureSettings": {"mode": "keep"}},
		"streamSettings": {"network": "tcp", "security": "reality", "futureNested": 9},
		"sniffing": {"enabled": true, "futureSniffing": [1, 2]}
	}`)

	var doc InboundDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if err := doc.Set("remark", "new"); err != nil {
		t.Fatal(err)
	}
	if err := doc.Set("port", 8443); err != nil {
		t.Fatal(err)
	}

	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	assertInboundJSONEqual(t, encoded, `{
		"id": 7,
		"remark": "new",
		"port": 8443,
		"enable": true,
		"future": {"x": 1},
		"settings": {"clients": [], "futureSettings": {"mode": "keep"}},
		"streamSettings": {"network": "tcp", "security": "reality", "futureNested": 9},
		"sniffing": {"enabled": true, "futureSniffing": [1, 2]}
	}`)
}

func TestInboundDocumentNormalizesLegacyNestedJSONStrings(t *testing.T) {
	raw := []byte(`{
		"id": 7,
		"settings": "{\"clients\":[{\"email\":\"legacy@example.com\",\"security\":\"auto\",\"limitIp\":0,\"totalGB\":0,\"expiryTime\":0,\"enable\":true,\"tgId\":0,\"subId\":\"legacy-sub\",\"comment\":\"\",\"reset\":0}],\"futureSettings\":7}",
		"streamSettings": "{\"network\":\"ws\",\"security\":\"tls\",\"futureStream\":true}",
		"sniffing": "{\"enabled\":true,\"futureSniffing\":{\"x\":1}}"
	}`)

	var doc InboundDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	clients, err := doc.Clients()
	if err != nil {
		t.Fatal(err)
	}
	if len(clients) != 1 || clients[0].Email != "legacy@example.com" || clients[0].SubID != "legacy-sub" {
		t.Fatalf("clients=%+v", clients)
	}

	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"settings", "streamSettings", "sniffing"} {
		if _, ok := decoded[key].(map[string]any); !ok {
			t.Fatalf("%s encoded as %T, want JSON object: %s", key, decoded[key], encoded)
		}
	}
	assertInboundJSONEqual(t, encoded, `{
		"id": 7,
		"settings": {"clients":[{"email":"legacy@example.com","security":"auto","limitIp":0,"totalGB":0,"expiryTime":0,"enable":true,"tgId":0,"subId":"legacy-sub","comment":"","reset":0}],"futureSettings":7},
		"streamSettings": {"network":"ws","security":"tls","futureStream":true},
		"sniffing": {"enabled":true,"futureSniffing":{"x":1}}
	}`)
}

func TestInboundDocumentInvalidLegacyStringIsPreservedAndFailsOnlyRelevantAccessor(t *testing.T) {
	const invalidSettings = `{not-json credential-sensitive-value}`
	raw := []byte(fmt.Sprintf(
		`{"id":7,"remark":"still-readable","settings":%q,"streamSettings":{"network":"tcp"},"sniffing":{"enabled":true}}`,
		invalidSettings,
	))

	var doc InboundDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if id, err := doc.Int("id"); err != nil || id != 7 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	if remark, err := doc.String("remark"); err != nil || remark != "still-readable" {
		t.Fatalf("remark=%q err=%v", remark, err)
	}
	if _, err := doc.Clients(); err == nil {
		t.Fatal("Clients() error=nil")
	} else if strings.Contains(err.Error(), "credential-sensitive-value") || strings.Contains(err.Error(), invalidSettings) {
		t.Fatalf("Clients() leaked invalid settings: %v", err)
	}

	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["settings"] != invalidSettings {
		t.Fatalf("settings=%#v want byte-semantic string %q", decoded["settings"], invalidSettings)
	}
}

func TestInboundDocumentCloneAndMutationsAreIsolated(t *testing.T) {
	raw := []byte(`{
		"remark":"source",
		"obsolete":true,
		"opaque":{"x":1},
		"settings":{
			"clients":[{"email":"source@example.com","security":"auto","limitIp":0,"totalGB":0,"expiryTime":0,"enable":true,"tgId":0,"subId":"source-sub","comment":"source","reset":0}],
			"decryption":"none",
			"futureSettings":{"keep":true}
		},
		"streamSettings":{"network":"tcp","security":"none"},
		"sniffing":{"enabled":false}
	}`)
	var source InboundDocument
	if err := json.Unmarshal(raw, &source); err != nil {
		t.Fatal(err)
	}

	rawClone := source.Clone()
	rawClone["opaque"][0] = '['
	if bytes.Equal(rawClone["opaque"], source["opaque"]) || source["opaque"][0] != '{' {
		t.Fatalf("Clone() shared raw bytes: source=%q clone=%q", source["opaque"], rawClone["opaque"])
	}

	clone := source.Clone()
	if err := clone.Set("remark", "clone"); err != nil {
		t.Fatal(err)
	}
	clone.Delete("obsolete")
	wantClients := []ClientPayload{{
		ID:       "clone-uuid",
		Security: "auto",
		Email:    "clone@example.com",
		Enable:   true,
		SubID:    "clone-sub",
		Comment:  "clone",
	}}
	if err := clone.SetClients(wantClients); err != nil {
		t.Fatal(err)
	}

	if remark, err := source.String("remark"); err != nil || remark != "source" {
		t.Fatalf("source remark=%q err=%v", remark, err)
	}
	if _, exists := source["obsolete"]; !exists {
		t.Fatal("Delete() on clone removed source field")
	}
	sourceClients, err := source.Clients()
	if err != nil {
		t.Fatal(err)
	}
	if len(sourceClients) != 1 || sourceClients[0].Email != "source@example.com" {
		t.Fatalf("source clients mutated: %+v", sourceClients)
	}
	cloneClients, err := clone.Clients()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cloneClients, wantClients) {
		t.Fatalf("clone clients=%+v want=%+v", cloneClients, wantClients)
	}

	encoded, err := json.Marshal(clone)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	settings, ok := decoded["settings"].(map[string]any)
	if !ok {
		t.Fatalf("settings=%T", decoded["settings"])
	}
	if settings["decryption"] != "none" || !reflect.DeepEqual(settings["futureSettings"], map[string]any{"keep": true}) {
		t.Fatalf("unrelated settings changed: %#v", settings)
	}
	if _, exists := decoded["obsolete"]; exists {
		t.Fatalf("clone still has deleted field: %#v", decoded)
	}
}

func TestInboundDocumentClientsRejectsNull(t *testing.T) {
	var doc InboundDocument
	if err := json.Unmarshal([]byte(`{"settings":{"clients":null}}`), &doc); err != nil {
		t.Fatal(err)
	}

	clients, err := doc.Clients()
	if err == nil {
		t.Fatalf("Clients()=%v, nil; want safe type error", clients)
	}
	if strings.Contains(err.Error(), `null`) {
		t.Fatalf("Clients() leaked raw value: %v", err)
	}
}

func TestInboundDocumentSetClientsNilEncodesEmptyArray(t *testing.T) {
	var doc InboundDocument
	if err := json.Unmarshal([]byte(`{
		"settings":{
			"clients":[{"email":"old@example.com"}],
			"decryption":"none",
			"future":{"keep":true}
		}
	}`), &doc); err != nil {
		t.Fatal(err)
	}

	if err := doc.SetClients(nil); err != nil {
		t.Fatal(err)
	}
	var settings map[string]json.RawMessage
	if err := json.Unmarshal(doc["settings"], &settings); err != nil {
		t.Fatal(err)
	}
	if got := string(settings["clients"]); got != `[]` {
		t.Fatalf("settings.clients=%s want=[]", got)
	}
	assertInboundJSONEqual(t, doc["settings"], `{
		"clients":[],
		"decryption":"none",
		"future":{"keep":true}
	}`)
}

func TestInboundDocumentSetClientsFailureIsAtomic(t *testing.T) {
	tests := []struct {
		name     string
		settings json.RawMessage
	}{
		{name: "wrong type", settings: json.RawMessage(`"credential-sensitive-value"`)},
		{name: "invalid raw JSON", settings: json.RawMessage(`{"clients":[],"future":`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := InboundDocument{
				"remark":   json.RawMessage(`"source"`),
				"settings": append(json.RawMessage(nil), tt.settings...),
				"future":   json.RawMessage(`{"keep":true}`),
			}
			before := doc.Clone()

			err := doc.SetClients([]ClientPayload{{Email: "new@example.com"}})
			if err == nil {
				t.Fatal("SetClients() error=nil")
			}
			if strings.Contains(err.Error(), "credential-sensitive-value") || strings.Contains(err.Error(), string(tt.settings)) {
				t.Fatalf("SetClients() leaked settings: %v", err)
			}
			if !reflect.DeepEqual(doc, before) {
				t.Fatalf("failed SetClients() mutated document\n got: %#v\nwant: %#v", doc, before)
			}
		})
	}
}

func TestInboundDocumentRejectsTopLevelNull(t *testing.T) {
	doc := InboundDocument{"existing": json.RawMessage(`true`)}
	before := doc.Clone()

	err := json.Unmarshal([]byte(`null`), &doc)
	if err == nil {
		t.Fatalf("Unmarshal(null) doc=%v error=nil", doc)
	}
	if strings.Contains(err.Error(), `null`) {
		t.Fatalf("Unmarshal(null) leaked raw input: %v", err)
	}
	if !reflect.DeepEqual(doc, before) {
		t.Fatalf("failed Unmarshal(null) mutated document: got=%v want=%v", doc, before)
	}
}

func TestInboundDocumentTypedAccessorsReturnSafeErrors(t *testing.T) {
	const sensitive = "credential-sensitive-value"
	var doc InboundDocument
	if err := json.Unmarshal([]byte(`{
		"id":7,
		"remark":"hello",
		"enable":true,
		"wrongInt":"credential-sensitive-value",
		"wrongString":{"secret":"credential-sensitive-value"},
		"wrongBool":["credential-sensitive-value"]
	}`), &doc); err != nil {
		t.Fatal(err)
	}
	if got, err := doc.Int("id"); err != nil || got != 7 {
		t.Fatalf("Int(id)=%d, %v", got, err)
	}
	if got, err := doc.String("remark"); err != nil || got != "hello" {
		t.Fatalf("String(remark)=%q, %v", got, err)
	}
	if got, err := doc.Bool("enable"); err != nil || !got {
		t.Fatalf("Bool(enable)=%t, %v", got, err)
	}

	tests := []struct {
		name string
		call func() error
	}{
		{name: "missing int", call: func() error { _, err := doc.Int("missing"); return err }},
		{name: "wrong int", call: func() error { _, err := doc.Int("wrongInt"); return err }},
		{name: "null int", call: func() error {
			copy := doc.Clone()
			if err := copy.Set("nullInt", nil); err != nil {
				return err
			}
			_, err := copy.Int("nullInt")
			return err
		}},
		{name: "missing string", call: func() error { _, err := doc.String("missing"); return err }},
		{name: "wrong string", call: func() error { _, err := doc.String("wrongString"); return err }},
		{name: "missing bool", call: func() error { _, err := doc.Bool("missing"); return err }},
		{name: "wrong bool", call: func() error { _, err := doc.Bool("wrongBool"); return err }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.call()
			if err == nil {
				t.Fatal("error=nil")
			}
			if strings.Contains(err.Error(), sensitive) {
				t.Fatalf("error leaked raw field value: %v", err)
			}
		})
	}
}

func TestInboundDocumentIntUsesGoJSONIntegerSemantics(t *testing.T) {
	doc := InboundDocument{
		"fractional": json.RawMessage(`7.5`),
		"overflow":   json.RawMessage(`9223372036854775808`),
		"exponent":   json.RawMessage(`1e2`),
	}
	tests := []struct {
		key string
		raw string
	}{
		{key: "fractional", raw: `7.5`},
		{key: "overflow", raw: `9223372036854775808`},
		// encoding/json decodes directly into int; exponent notation is not
		// accepted by that integer decoder even when its value is integral.
		{key: "exponent", raw: `1e2`},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			value, err := doc.Int(tt.key)
			if err == nil {
				t.Fatalf("Int(%q)=%d, nil; want integer decoding error", tt.key, value)
			}
			if strings.Contains(err.Error(), tt.raw) {
				t.Fatalf("Int(%q) leaked raw numeric value: %v", tt.key, err)
			}
		})
	}
}

func TestInboundDocumentClientsErrorsDoNotLeakRawSettings(t *testing.T) {
	const sensitive = "clients-sensitive-value"
	tests := []string{
		`{}`,
		`{"settings":{"future":1}}`,
		`{"settings":{"clients":"clients-sensitive-value"}}`,
		`{"settings":"clients-sensitive-value"}`,
		`{"settings":["clients-sensitive-value"]}`,
	}
	for _, raw := range tests {
		var doc InboundDocument
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			t.Fatal(err)
		}
		_, err := doc.Clients()
		if err == nil {
			t.Fatalf("Clients() error=nil for %s", raw)
		}
		if strings.Contains(err.Error(), sensitive) || strings.Contains(err.Error(), raw) {
			t.Fatalf("Clients() leaked raw settings for %s: %v", raw, err)
		}
	}
}

func TestInboundSummaryNetworkSecuritySupportsNestedAndLegacySettings(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		network  string
		security string
	}{
		{name: "nested", raw: `{"network":"ws","security":"tls","future":1}`, network: "ws", security: "tls"},
		{name: "legacy string", raw: `"{\"network\":\"grpc\",\"security\":\"reality\",\"future\":1}"`, network: "grpc", security: "reality"},
		{name: "network default", raw: `{"security":"tls"}`, network: "tcp", security: "tls"},
		{name: "security default", raw: `{"network":"kcp"}`, network: "kcp", security: "none"},
		{name: "null defaults", raw: `null`, network: "tcp", security: "none"},
		{name: "invalid legacy defaults", raw: `"{credential-sensitive-value}"`, network: "tcp", security: "none"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary := InboundSummary{StreamSettings: json.RawMessage(tt.raw)}
			network, security := summary.NetworkSecurity()
			if network != tt.network || security != tt.security {
				t.Fatalf("NetworkSecurity()=(%q,%q) want=(%q,%q)", network, security, tt.network, tt.security)
			}
		})
	}
}

type recordedInboundRequest struct {
	method string
	path   string
	body   []byte
}

func TestInboundEndpointsUseOfficialWireShapeAndReturnResponseDocuments(t *testing.T) {
	var requests []recordedInboundRequest
	ts := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		requests = append(requests, recordedInboundRequest{
			method: r.Method,
			path:   r.URL.EscapedPath(),
			body:   append([]byte(nil), body...),
		})

		obj := `null`
		switch r.URL.Path {
		case "/panel/api/inbounds/list/slim":
			obj = `[{"id":7,"remark":"source","enable":true,"port":443,"protocol":"vless","streamSettings":"{\"network\":\"ws\",\"security\":\"tls\"}"}]`
		case "/panel/api/inbounds/get/7":
			obj = `{"id":7,"remark":"source","future":{"keep":true},"settings":"{\"clients\":[],\"decryption\":\"none\"}","streamSettings":"{\"network\":\"ws\",\"security\":\"tls\",\"futureStream\":9}","sniffing":"{\"enabled\":true,\"futureSniffing\":1}"}`
		case "/panel/api/inbounds/add":
			obj = `{"id":9,"remark":"created","createdFuture":true,"settings":{"clients":[]},"streamSettings":{"network":"ws","security":"tls"},"sniffing":{"enabled":true}}`
		case "/panel/api/inbounds/update/7":
			obj = `{"id":7,"remark":"updated","updatedFuture":{"x":1},"settings":{"clients":[]},"streamSettings":{"network":"ws","security":"tls"},"sniffing":{"enabled":true}}`
		case "/panel/api/inbounds/del/7":
			obj = `null`
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"success":true,"msg":"","obj":%s}`, obj)
	})
	defer ts.Close()

	c := mustAPI(t, ts.URL)
	ctx := context.Background()
	summaries, err := c.ListSlimInbounds(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].ID != 7 || summaries[0].Protocol != "vless" {
		t.Fatalf("summaries=%+v", summaries)
	}
	if network, security := summaries[0].NetworkSecurity(); network != "ws" || security != "tls" {
		t.Fatalf("summary NetworkSecurity()=(%q,%q)", network, security)
	}

	doc, err := c.GetInbound(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	created, err := c.AddInbound(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := c.UpdateInbound(ctx, 7, doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteInbound(ctx, 7); err != nil {
		t.Fatal(err)
	}

	if id, err := created.Int("id"); err != nil || id != 9 {
		t.Fatalf("created id=%d err=%v", id, err)
	}
	if remark, err := created.String("remark"); err != nil || remark != "created" {
		t.Fatalf("created remark=%q err=%v", remark, err)
	}
	if id, err := updated.Int("id"); err != nil || id != 7 {
		t.Fatalf("updated id=%d err=%v", id, err)
	}
	if _, exists := updated["updatedFuture"]; !exists {
		t.Fatalf("updated response lost unknown field: %s", mustMarshalInbound(t, updated))
	}

	wantCalls := []string{
		"GET /panel/api/inbounds/list/slim",
		"GET /panel/api/inbounds/get/7",
		"POST /panel/api/inbounds/add",
		"POST /panel/api/inbounds/update/7",
		"POST /panel/api/inbounds/del/7",
	}
	gotCalls := make([]string, len(requests))
	for i, request := range requests {
		gotCalls[i] = request.method + " " + request.path
	}
	if !slices.Equal(gotCalls, wantCalls) {
		t.Fatalf("calls=%v want=%v", gotCalls, wantCalls)
	}
	for _, i := range []int{0, 1, 4} {
		if len(requests[i].body) != 0 {
			t.Errorf("request %d body=%q want empty", i, requests[i].body)
		}
	}
	wantBody := mustMarshalInbound(t, doc)
	for _, i := range []int{2, 3} {
		assertInboundJSONEqual(t, requests[i].body, string(wantBody))
		var decoded map[string]any
		if err := json.Unmarshal(requests[i].body, &decoded); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"settings", "streamSettings", "sniffing"} {
			if _, ok := decoded[key].(map[string]any); !ok {
				t.Errorf("request %d field %s encoded as %T, want JSON object: %s", i, key, decoded[key], requests[i].body)
			}
		}
	}
}

func TestInboundEndpointsSanitizeUpstreamErrors(t *testing.T) {
	const (
		secretRemark   = "remark-sensitive-value"
		secretPassword = "password-sensitive-value"
		rawSecret      = "raw-response-sensitive-value"
	)
	var doc InboundDocument
	if err := json.Unmarshal([]byte(`{
		"id":7,
		"remark":"remark-sensitive-value",
		"settings":{"clients":[{"email":"secret@example.com","security":"auto","password":"password-sensitive-value","limitIp":0,"totalGB":0,"expiryTime":0,"enable":true,"tgId":0,"subId":"secret-sub","comment":"secret-comment","reset":0}]},
		"streamSettings":{"network":"tcp","security":"none"},
		"sniffing":{"enabled":true}
	}`), &doc); err != nil {
		t.Fatal(err)
	}
	docJSON := string(mustMarshalInbound(t, doc))

	tests := []struct {
		name string
		op   string
		call func(*APIClient) error
	}{
		{name: "list", op: "list inbounds", call: func(c *APIClient) error {
			_, err := c.ListSlimInbounds(context.Background())
			return err
		}},
		{name: "get", op: "get inbound", call: func(c *APIClient) error {
			_, err := c.GetInbound(context.Background(), 7)
			return err
		}},
		{name: "add", op: "add inbound", call: func(c *APIClient) error {
			_, err := c.AddInbound(context.Background(), doc)
			return err
		}},
		{name: "update", op: "update inbound", call: func(c *APIClient) error {
			_, err := c.UpdateInbound(context.Background(), 7, doc)
			return err
		}},
		{name: "delete", op: "delete inbound", call: func(c *APIClient) error {
			return c.DeleteInbound(context.Background(), 7)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tsURL string
			ts := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read request body: %v", err)
				}
				message := strings.Join([]string{
					secretRemark,
					secretPassword,
					rawSecret,
					string(body),
					r.URL.RequestURI(),
					tsURL,
					"test-token",
				}, " | ")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"success": false,
					"msg":     message,
					"obj":     nil,
				})
			})
			defer ts.Close()
			tsURL = ts.URL

			err := tt.call(mustAPI(t, ts.URL))
			if !IsKind(err, ErrorAPI) {
				t.Fatalf("err=%v", err)
			}
			var typed *Error
			if !errors.As(err, &typed) {
				t.Fatalf("typed error missing: %v", err)
			}
			if typed.Kind != ErrorAPI || typed.StatusCode != http.StatusOK || typed.Op != tt.op || typed.Message != "request failed" {
				t.Fatalf("typed error=%+v", typed)
			}
			for _, sensitive := range []string{
				secretRemark,
				secretPassword,
				rawSecret,
				docJSON,
				ts.URL,
				"test-token",
			} {
				if strings.Contains(err.Error(), sensitive) {
					t.Fatalf("error leaked %q: %v", sensitive, err)
				}
			}
		})
	}
}

func TestInboundEndpointsPreserveDecodeCauseWithoutLeakingBody(t *testing.T) {
	const rawSecret = "raw-response-sensitive-value"
	ts := newAPIServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"obj":{"remark":"raw-response-sensitive-value"`)
	})
	defer ts.Close()

	_, err := mustAPI(t, ts.URL).GetInbound(context.Background(), 7)
	if !IsKind(err, ErrorDecode) {
		t.Fatalf("err=%v", err)
	}
	var typed *Error
	if !errors.As(err, &typed) || typed.StatusCode != http.StatusOK || typed.Op != "get inbound" {
		t.Fatalf("typed error=%+v", typed)
	}
	var syntaxError *json.SyntaxError
	if !errors.As(err, &syntaxError) {
		t.Fatalf("decode cause not retained: %v", err)
	}
	if strings.Contains(err.Error(), rawSecret) || strings.Contains(err.Error(), ts.URL) {
		t.Fatalf("decode error leaked response body or URL: %v", err)
	}
}

func TestInboundDocumentEndpointsRejectMissingAndNullResponseObjects(t *testing.T) {
	const (
		documentSecret = "document-sensitive-value"
		responseSecret = "response-sensitive-value"
	)
	var requestDoc InboundDocument
	if err := json.Unmarshal([]byte(`{
		"id":7,
		"remark":"document-sensitive-value",
		"settings":{"clients":[]},
		"streamSettings":{},
		"sniffing":{}
	}`), &requestDoc); err != nil {
		t.Fatal(err)
	}
	documentJSON := string(mustMarshalInbound(t, requestDoc))

	operations := []struct {
		name string
		op   string
		call func(*APIClient) (InboundDocument, error)
	}{
		{name: "get", op: "get inbound", call: func(c *APIClient) (InboundDocument, error) {
			return c.GetInbound(context.Background(), 7)
		}},
		{name: "add", op: "add inbound", call: func(c *APIClient) (InboundDocument, error) {
			return c.AddInbound(context.Background(), requestDoc)
		}},
		{name: "update", op: "update inbound", call: func(c *APIClient) (InboundDocument, error) {
			return c.UpdateInbound(context.Background(), 7, requestDoc)
		}},
	}
	responses := []struct {
		name    string
		objJSON string
	}{
		{name: "missing obj"},
		{name: "null obj", objJSON: `,"obj":null`},
	}

	for _, operation := range operations {
		for _, response := range responses {
			t.Run(operation.name+"/"+response.name, func(t *testing.T) {
				var serverURL, rawResponse string
				ts := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
					requestBody, err := io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("read request body: %v", err)
					}
					message := strings.Join([]string{
						documentSecret,
						documentJSON,
						string(requestBody),
						responseSecret,
						serverURL,
						r.URL.RequestURI(),
						"test-token",
					}, " | ")
					rawResponse = fmt.Sprintf(
						`{"success":true,"msg":%q,"future":%q%s}`,
						message,
						responseSecret,
						response.objJSON,
					)
					_, _ = io.WriteString(w, rawResponse)
				})
				defer ts.Close()
				serverURL = ts.URL

				document, err := operation.call(mustAPI(t, ts.URL))
				if err == nil {
					t.Fatalf("document=%v error=nil", document)
				}
				if document != nil {
					t.Fatalf("document=%v want=nil on decode error", document)
				}
				if !IsKind(err, ErrorDecode) {
					t.Fatalf("err=%v want decode error", err)
				}
				var typed *Error
				if !errors.As(err, &typed) {
					t.Fatalf("typed error missing: %v", err)
				}
				if typed.Kind != ErrorDecode || typed.StatusCode != http.StatusOK || typed.Op != operation.op || typed.Message != "request failed" {
					t.Fatalf("typed error=%+v", typed)
				}
				if errors.Unwrap(err) == nil {
					t.Fatalf("decode cause missing: %+v", typed)
				}
				for _, sensitive := range []string{
					documentSecret,
					documentJSON,
					responseSecret,
					rawResponse,
					ts.URL,
					"test-token",
				} {
					if sensitive != "" && strings.Contains(err.Error(), sensitive) {
						t.Fatalf("error leaked %q: %v", sensitive, err)
					}
				}
			})
		}
	}
}

func TestInboundMutationEndpointsDoNotRetryTransportErrorsAndPreserveCause(t *testing.T) {
	var doc InboundDocument
	if err := json.Unmarshal([]byte(`{"id":7,"remark":"remark-sensitive-value","settings":{"clients":[]},"streamSettings":{},"sniffing":{}}`), &doc); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		op   string
		call func(*APIClient) error
	}{
		{name: "add", op: "add inbound", call: func(c *APIClient) error { _, err := c.AddInbound(context.Background(), doc); return err }},
		{name: "update", op: "update inbound", call: func(c *APIClient) error { _, err := c.UpdateInbound(context.Background(), 7, doc); return err }},
		{name: "delete", op: "delete inbound", call: func(c *APIClient) error { return c.DeleteInbound(context.Background(), 7) }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cause := testNetError{temporary: true}
			attempts := 0
			rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
				attempts++
				return nil, cause
			})
			c, err := NewAPI(APIConfig{
				BaseURL:    "https://panel-sensitive.example/path-sensitive",
				Token:      "test-token",
				HTTPClient: &http.Client{Transport: rt},
			})
			if err != nil {
				t.Fatal(err)
			}

			err = tt.call(c)
			if !IsKind(err, ErrorTransport) || !errors.Is(err, cause) {
				t.Fatalf("err=%v cause retained=%t", err, errors.Is(err, cause))
			}
			var typed *Error
			if !errors.As(err, &typed) || typed.Op != tt.op || typed.Message != "request failed" {
				t.Fatalf("typed error=%+v", typed)
			}
			if attempts != 1 {
				t.Fatalf("attempts=%d want=1", attempts)
			}
			for _, sensitive := range []string{"remark-sensitive-value", "panel-sensitive.example", "path-sensitive", "test-token"} {
				if strings.Contains(err.Error(), sensitive) {
					t.Fatalf("error leaked %q: %v", sensitive, err)
				}
			}
		})
	}
}

func TestInboundMutationEndpointDoesNotRetryTemporaryResponseBodyReadError(t *testing.T) {
	cause := testNetError{temporary: true}
	body := &readErrorCloser{err: cause}
	attempts := 0
	rt := roundTripFunc(func(*http.Request) (*http.Response, error) {
		attempts++
		return testHTTPResponse(http.StatusOK, body), nil
	})
	c, err := NewAPI(APIConfig{
		BaseURL:    "https://panel-sensitive.example/path-sensitive",
		Token:      "test-token",
		HTTPClient: &http.Client{Transport: rt},
	})
	if err != nil {
		t.Fatal(err)
	}
	var doc InboundDocument
	if err := json.Unmarshal([]byte(`{
		"remark":"document-sensitive-value",
		"settings":{"clients":[]},
		"streamSettings":{},
		"sniffing":{}
	}`), &doc); err != nil {
		t.Fatal(err)
	}

	_, err = c.AddInbound(context.Background(), doc)
	if !IsKind(err, ErrorTransport) || !errors.Is(err, cause) {
		t.Fatalf("err=%v cause retained=%t", err, errors.Is(err, cause))
	}
	if attempts != 1 || !body.closed {
		t.Fatalf("attempts=%d body.closed=%t want=1,true", attempts, body.closed)
	}
	for _, sensitive := range []string{"document-sensitive-value", "panel-sensitive.example", "path-sensitive", "test-token"} {
		if strings.Contains(err.Error(), sensitive) {
			t.Fatalf("error leaked %q: %v", sensitive, err)
		}
	}
}

func assertInboundJSONEqual(t *testing.T, got []byte, want string) {
	t.Helper()
	var gotValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("decode got JSON %q: %v", got, err)
	}
	var wantValue any
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("decode wanted JSON %q: %v", want, err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("JSON mismatch\n got: %s\nwant: %s", got, want)
	}
}

func mustMarshalInbound(t *testing.T, doc InboundDocument) []byte {
	t.Helper()
	encoded, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
