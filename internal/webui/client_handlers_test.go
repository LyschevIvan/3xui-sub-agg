package webui

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/LyschevIvan/3xui-sub-agg/internal/aggregator"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
)

type nativeMutationPanel struct {
	mu          sync.Mutex
	detachPaths []string
	attachPaths []string
	failDetach  string
	secret      string
	clientItems string
}

func (p *nativeMutationPanel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path
	switch {
	case path == "/panel/api/server/status":
		_, _ = io.WriteString(w, `{"success":true,"obj":{"panelVersion":"v3.4.2"}}`)
	case path == "/panel/api/clients/list":
		p.mu.Lock()
		items := p.clientItems
		p.mu.Unlock()
		if items == "" {
			items = `{"id":2,"email":"a@x","subId":"group","enable":true,"inboundIds":[9]},` +
				`{"id":7,"email":"b@x","subId":"group","enable":true,"inboundIds":[9]},` +
				`{"id":8,"email":"free@x","subId":"free","enable":true,"inboundIds":[]}`
		}
		_, _ = io.WriteString(w, `{"success":true,"obj":{"items":[`+items+
			`],"total":3,"filtered":3,"page":1,"pageSize":200}}`)
	case path == "/panel/api/inbounds/list/slim":
		_, _ = io.WriteString(w, `{"success":true,"obj":[{"id":9,"remark":"main","enable":true,"port":443,"protocol":"vless","streamSettings":{"network":"tcp","security":"reality"}}]}`)
	case strings.Contains(path, "/panel/api/clients/subLinks/"):
		_, _ = io.WriteString(w, `{"success":true,"obj":[]}`)
	case strings.HasSuffix(path, "/attach"):
		p.mu.Lock()
		p.attachPaths = append(p.attachPaths, path)
		p.mu.Unlock()
		_, _ = io.WriteString(w, `{"success":true,"obj":{}}`)
	case strings.HasSuffix(path, "/detach"):
		p.mu.Lock()
		p.detachPaths = append(p.detachPaths, path)
		fail := strings.Contains(path, "/"+p.failDetach+"/")
		secret := p.secret
		p.mu.Unlock()
		if fail {
			_, _ = fmt.Fprintf(w, `{"success":false,"msg":%q}`, secret)
			return
		}
		_, _ = io.WriteString(w, `{"success":true,"obj":{}}`)
	default:
		http.Error(w, `{"success":false,"msg":"unexpected"}`, http.StatusNotFound)
	}
}

func newClientMutationTestApp(t *testing.T, panel *nativeMutationPanel) (*serverTestApp, storage.Server, func()) {
	t.Helper()
	server := httptest.NewServer(panel)
	app := newServerTestApp(t, "client-handler-master-key")
	stored, err := app.store.CreateServer(&storage.Server{
		UserID: app.user.ID, Name: "node", APIURL: server.URL, Path: "/", APIToken: "native-token",
	})
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	return app, *stored, server.Close
}

func TestClientCardsRenderSemanticAttachDetachFormsWithoutSecrets(t *testing.T) {
	app := newServerTestApp(t, "client-form-master-key")
	stored, err := app.store.CreateServer(&storage.Server{
		UserID: app.user.ID, Name: "node", APIURL: "https://panel.example", Path: "/secret/", APIToken: "token-not-in-form",
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := app.handler.Agg.Snapshot()
	snapshot.Servers = []aggregator.ServerSnapshot{{
		ID: stored.ID, UserID: app.user.ID, Name: stored.Name,
		Inbounds: []aggregator.InboundInfo{
			{ID: 9, Remark: "attached", Port: 443, Protocol: "vless", Enable: true},
			{ID: 10, Remark: "candidate", Port: 8443, Protocol: "vless", Enable: true},
		},
		Groups: map[string]aggregator.ClientGroup{
			"group": {SubID: "group", Records: []aggregator.ClientRef{
				{Email: "a@x", SubID: "group", Enabled: true, InboundIDs: []int{9}},
				{Email: "b@x", SubID: "group", Enabled: true, InboundIDs: []int{9}},
			}},
		},
	}}

	rr := app.request(t, http.MethodGet, "/dashboard", nil)
	body := rr.Body.String()
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rr.Code, body)
	}
	for _, action := range []string{"/dashboard/clients/inbound/add", "/dashboard/clients/inbound/remove"} {
		if strings.Count(body, `action="`+action+`"`) != 1 {
			t.Fatalf("action %q count=%d body=%s", action, strings.Count(body, `action="`+action+`"`), body)
		}
	}
	for _, field := range []string{`name="sub_id" value="group"`, `name="server_id" value="` + strconv.FormatInt(stored.ID, 10) + `"`} {
		if strings.Count(body, field) != 2 {
			t.Fatalf("semantic field %q count=%d", field, strings.Count(body, field))
		}
	}
	for _, forbidden := range []string{
		`name="uuid"`, `name="client_` + `uuid"`, `name="email"`, `name="api_token"`, `name="username"`, `name="password"`, "token-not-in-form",
	} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("forbidden form data %q in body=%s", forbidden, body)
		}
	}
}

func TestClientInboundAddUsesNativeAttachAndDeterministicRedirect(t *testing.T) {
	panel := &nativeMutationPanel{}
	app, server, closePanel := newClientMutationTestApp(t, panel)
	defer closePanel()

	rr := app.request(t, http.MethodPost, "/dashboard/clients/inbound/add", url.Values{
		"sub_id": {"free"}, "server_id": {strconv.FormatInt(server.ID, 10)}, "inbound_id": {"9"},
	})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/dashboard#add-free" {
		t.Fatalf("status=%d location=%q body=%q", rr.Code, rr.Header().Get("Location"), rr.Body.String())
	}
	panel.mu.Lock()
	paths := append([]string(nil), panel.attachPaths...)
	panel.mu.Unlock()
	if len(paths) != 1 || !strings.Contains(paths[0], "/free@x/attach") {
		t.Fatalf("attach paths=%v", paths)
	}
}

func TestClientInboundRemoveReportsSafePartialCompletion(t *testing.T) {
	const secret = "Bearer token-secret raw-upstream-body"
	panel := &nativeMutationPanel{failDetach: "a@x", secret: secret}
	app, server, closePanel := newClientMutationTestApp(t, panel)
	defer closePanel()

	rr := app.request(t, http.MethodPost, "/dashboard/clients/inbound/remove", url.Values{
		"sub_id": {"group"}, "server_id": {strconv.FormatInt(server.ID, 10)}, "inbound_id": {"9"},
	})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/dashboard#card-group" {
		t.Fatalf("status=%d location=%q body=%q", rr.Code, rr.Header().Get("Location"), rr.Body.String())
	}
	message := responseFlash(t, rr)
	if !strings.Contains(message, "частично") || !strings.Contains(message, "1 из 2") || strings.Contains(message, secret) || strings.Contains(message, "token-secret") {
		t.Fatalf("unsafe/unhelpful flash=%q", message)
	}
	panel.mu.Lock()
	paths := append([]string(nil), panel.detachPaths...)
	panel.mu.Unlock()
	if len(paths) != 2 || !strings.Contains(paths[0], "/a@x/detach") || !strings.Contains(paths[1], "/b@x/detach") {
		t.Fatalf("detach paths=%v", paths)
	}
}

func TestClientInboundPreservesExactSubID(t *testing.T) {
	panel := &nativeMutationPanel{clientItems: `{"id":2,"email":"spaced@x","subId":" group ","enable":true,"inboundIds":[9]}`}
	app, server, closePanel := newClientMutationTestApp(t, panel)
	defer closePanel()

	rr := app.request(t, http.MethodPost, "/dashboard/clients/inbound/remove", url.Values{
		"sub_id": {" group "}, "server_id": {strconv.FormatInt(server.ID, 10)}, "inbound_id": {"9"},
	})
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
	}
	panel.mu.Lock()
	paths := append([]string(nil), panel.detachPaths...)
	panel.mu.Unlock()
	if len(paths) != 1 || !strings.Contains(paths[0], "/spaced@x/detach") {
		t.Fatalf("exact subID detach paths=%v", paths)
	}
}

func TestClientInboundMutationEnforcesOwnershipAndCSRF(t *testing.T) {
	panel := &nativeMutationPanel{}
	app, server, closePanel := newClientMutationTestApp(t, panel)
	defer closePanel()
	foreign, err := app.store.CreateUser("other-owner", "unused", false)
	if err != nil {
		t.Fatal(err)
	}
	foreignServer, err := app.store.CreateServer(&storage.Server{
		UserID: foreign.ID, Name: "foreign", APIURL: server.APIURL, Path: "/", APIToken: "foreign-token",
	})
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"sub_id": {"group"}, "server_id": {strconv.FormatInt(foreignServer.ID, 10)}, "inbound_id": {"9"}}
	if rr := app.request(t, http.MethodPost, "/dashboard/clients/inbound/remove", form); rr.Code != http.StatusNotFound {
		t.Fatalf("foreign status=%d body=%q", rr.Code, rr.Body.String())
	}

	form.Set("server_id", strconv.FormatInt(server.ID, 10))
	form.Del("csrf_token") // app.request adds it to the caller-owned values.
	req := httptest.NewRequest(http.MethodPost, "/dashboard/clients/inbound/remove", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range app.cookies {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	app.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d body=%q", rr.Code, rr.Body.String())
	}
}

func responseFlash(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	for _, cookie := range rr.Result().Cookies() {
		if cookie.Name != flashCookie {
			continue
		}
		raw, err := base64.URLEncoding.DecodeString(cookie.Value)
		if err != nil {
			t.Fatal(err)
		}
		parts := strings.SplitN(string(raw), "|", 2)
		if len(parts) != 2 {
			t.Fatalf("flash=%q", raw)
		}
		return parts[1]
	}
	t.Fatal("flash cookie missing")
	return ""
}
