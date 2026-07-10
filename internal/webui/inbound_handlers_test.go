package webui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
)

type nativeInboundPanel struct {
	mu          sync.Mutex
	paths       []string
	bodies      map[string][]byte
	failUpdate  bool
	secret      string
	clientAdded bool
}

func (p *nativeInboundPanel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	p.paths = append(p.paths, r.Method+" "+r.URL.Path)
	if p.bodies == nil {
		p.bodies = make(map[string][]byte)
	}
	p.bodies[r.Method+" "+r.URL.Path] = body
	failUpdate, secret, clientAdded := p.failUpdate, p.secret, p.clientAdded
	p.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/panel/api/server/status":
		_, _ = io.WriteString(w, `{"success":true,"obj":{"panelVersion":"3.4.2"}}`)
	case r.URL.Path == "/panel/api/inbounds/get/9":
		_, _ = io.WriteString(w, `{"success":true,"obj":{"id":9,"up":1,"down":2,"remark":"source","port":443,"enable":true,"tag":"old","protocol":"vless","unknownTop":{"keep":true},"settings":{"clients":[{"id":"old","security":"auto","email":"source-client","limitIp":2,"totalGB":100,"expiryTime":200,"enable":true,"subId":"group","comment":"keep"}],"unknownSetting":7},"streamSettings":{"network":"tcp","security":"reality","unknownStream":8},"sniffing":{"enabled":true,"unknownSniff":9}}}`)
	case r.URL.Path == "/panel/api/inbounds/update/9":
		if failUpdate {
			_, _ = fmt.Fprintf(w, `{"success":false,"msg":%q}`, secret)
			return
		}
		_, _ = io.WriteString(w, `{"success":true,"obj":{"id":9,"protocol":"vless"}}`)
	case r.URL.Path == "/panel/api/inbounds/add":
		var doc map[string]any
		_ = json.Unmarshal(body, &doc)
		doc["id"] = float64(77)
		encoded, _ := json.Marshal(doc)
		_, _ = fmt.Fprintf(w, `{"success":true,"obj":%s}`, encoded)
	case r.URL.Path == "/panel/api/inbounds/del/9" || r.URL.Path == "/panel/api/inbounds/del/77":
		_, _ = io.WriteString(w, `{"success":true,"obj":{}}`)
	case r.URL.Path == "/panel/api/clients/list":
		items := `[{"id":2,"email":"source-client","subId":"group","enable":true,"inboundIds":[9]}]`
		if clientAdded {
			items = `[{"id":2,"email":"source-client","subId":"group","enable":true,"inboundIds":[9]},{"id":3,"email":"group-copy","subId":"group","enable":true,"inboundIds":[77]}]`
		}
		_, _ = fmt.Fprintf(w, `{"success":true,"obj":{"items":%s,"total":1,"filtered":1,"page":1,"pageSize":200}}`, items)
	case r.URL.Path == "/panel/api/clients/get/source-client":
		_, _ = io.WriteString(w, `{"success":true,"obj":{"client":{"id":2,"email":"source-client","subId":"group","uuid":"old","security":"auto","limitIp":2,"totalGB":100,"expiryTime":200,"enable":true,"comment":"keep"},"inboundIds":[9]}}`)
	case r.URL.Path == "/panel/api/clients/source-client/attach":
		_, _ = io.WriteString(w, `{"success":true,"obj":{}}`)
	case r.URL.Path == "/panel/api/clients/add":
		p.mu.Lock()
		p.clientAdded = true
		p.mu.Unlock()
		_, _ = io.WriteString(w, `{"success":true,"obj":{}}`)
	case r.URL.Path == "/panel/api/inbounds/list/slim":
		_, _ = io.WriteString(w, `{"success":true,"obj":[{"id":9,"remark":"source","enable":true,"port":443,"protocol":"vless","streamSettings":{"network":"tcp","security":"reality"}}]}`)
	case strings.HasPrefix(r.URL.Path, "/panel/api/clients/subLinks/"):
		_, _ = io.WriteString(w, `{"success":true,"obj":[]}`)
	default:
		http.Error(w, `{"success":false,"msg":"unexpected"}`, http.StatusNotFound)
	}
}

func newInboundHandlerApp(t *testing.T, panel *nativeInboundPanel) (*serverTestApp, storage.Server, func()) {
	t.Helper()
	ts := httptest.NewServer(panel)
	app := newServerTestApp(t, "inbound-handler-key")
	server, err := app.store.CreateServer(&storage.Server{UserID: app.user.ID, Name: "node", APIURL: ts.URL, Path: "/", APIToken: "native-token"})
	if err != nil {
		ts.Close()
		t.Fatal(err)
	}
	return app, *server, ts.Close
}

func TestInboundHandlerEditUsesSemanticNativeFacade(t *testing.T) {
	panel := &nativeInboundPanel{}
	app, server, closePanel := newInboundHandlerApp(t, panel)
	defer closePanel()
	rr := app.request(t, http.MethodPost, "/dashboard/inbounds/edit", url.Values{
		"server_id": {strconv.FormatInt(server.ID, 10)}, "inbound_id": {"9"}, "new_remark": {"new"}, "new_port": {"8443"}, "enable": {"1"},
	})
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/dashboard/servers/"+strconv.FormatInt(server.ID, 10)+"#inbounds" {
		t.Fatalf("status=%d location=%q body=%q", rr.Code, rr.Header().Get("Location"), rr.Body.String())
	}
	panel.mu.Lock()
	paths := append([]string(nil), panel.paths...)
	body := append([]byte(nil), panel.bodies["POST /panel/api/inbounds/update/9"]...)
	panel.mu.Unlock()
	if strings.Count(strings.Join(paths, "\n"), "GET /panel/api/inbounds/get/9") != 1 || strings.Count(strings.Join(paths, "\n"), "POST /panel/api/inbounds/update/9") != 1 {
		t.Fatalf("paths=%v", paths)
	}
	if strings.Contains(string(body), "native-token") || !strings.Contains(string(body), `"unknownTop"`) || !strings.Contains(string(body), `"unknownSniff"`) {
		t.Fatalf("unsafe/lossy body=%s", body)
	}
}

func TestInboundHandlerCopyAndDeleteUseExactSemanticIDs(t *testing.T) {
	panel := &nativeInboundPanel{}
	app, server, closePanel := newInboundHandlerApp(t, panel)
	defer closePanel()
	copyResponse := app.request(t, http.MethodPost, "/dashboard/inbounds/copy", url.Values{
		"source_server_id": {strconv.FormatInt(server.ID, 10)}, "source_inbound_id": {"9"}, "target_server_id": {strconv.FormatInt(server.ID, 10)}, "new_remark": {"copy"}, "new_port": {"8443"},
	})
	if copyResponse.Code != http.StatusSeeOther || copyResponse.Header().Get("Location") != "/dashboard/servers/"+strconv.FormatInt(server.ID, 10)+"#inbounds" {
		t.Fatalf("copy status=%d location=%q flash=%q", copyResponse.Code, copyResponse.Header().Get("Location"), responseFlash(t, copyResponse))
	}
	deleteResponse := app.request(t, http.MethodPost, "/dashboard/inbounds/delete", url.Values{"server_id": {strconv.FormatInt(server.ID, 10)}, "inbound_id": {"9"}})
	if deleteResponse.Code != http.StatusSeeOther {
		t.Fatalf("delete status=%d body=%q", deleteResponse.Code, deleteResponse.Body.String())
	}
	panel.mu.Lock()
	paths := strings.Join(panel.paths, "\n")
	panel.mu.Unlock()
	for _, path := range []string{"POST /panel/api/inbounds/add", "POST /panel/api/inbounds/del/9"} {
		if strings.Count(paths, path) != 1 {
			t.Fatalf("path %q count wrong in %s", path, paths)
		}
	}
}

func TestInboundHandlerEnforcesBothOwnershipCSRFAndSafeErrors(t *testing.T) {
	const secret = "Bearer native-token raw-upstream-body"
	panel := &nativeInboundPanel{failUpdate: true, secret: secret}
	app, server, closePanel := newInboundHandlerApp(t, panel)
	defer closePanel()
	foreign, _ := app.store.CreateUser("foreign-copy", "unused", false)
	foreignServer, _ := app.store.CreateServer(&storage.Server{UserID: foreign.ID, Name: "foreign", APIURL: server.APIURL, Path: "/", APIToken: "foreign-token"})
	foreignCopy := app.request(t, http.MethodPost, "/dashboard/inbounds/copy", url.Values{"source_server_id": {strconv.FormatInt(server.ID, 10)}, "source_inbound_id": {"9"}, "target_server_id": {strconv.FormatInt(foreignServer.ID, 10)}, "new_remark": {"copy"}, "new_port": {"8443"}})
	if foreignCopy.Code != http.StatusNotFound {
		t.Fatalf("foreign status=%d flash=%q", foreignCopy.Code, responseFlash(t, foreignCopy))
	}
	errResponse := app.request(t, http.MethodPost, "/dashboard/inbounds/edit", url.Values{"server_id": {strconv.FormatInt(server.ID, 10)}, "inbound_id": {"9"}, "new_remark": {"new"}, "new_port": {"8443"}})
	message := responseFlash(t, errResponse)
	if !strings.Contains(message, "Не удалось") || strings.Contains(message, secret) || strings.Contains(message, "native-token") {
		t.Fatalf("unsafe flash=%q", message)
	}

	form := url.Values{"server_id": {strconv.FormatInt(server.ID, 10)}, "inbound_id": {"9"}}
	req := httptest.NewRequest(http.MethodPost, "/dashboard/inbounds/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range app.cookies {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	app.mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d", rr.Code)
	}
}

func TestInboundHandlerTemplateExplainsCopyAndDeleteSemantics(t *testing.T) {
	raw, err := tmplFS.ReadFile("templates/server_form.html")
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, exact := range []string{
		"существующие записи клиентов на целевом сервере подключаются к новому inbound, а отсутствующие VLESS-записи создаются",
		"Общие записи клиентов могут остаться в 3x-ui",
	} {
		if !strings.Contains(body, exact) {
			t.Fatalf("missing help text %q", exact)
		}
	}
	for _, obsolete := range []string{"Все клиенты в нём будут стёрты", "email перепишется"} {
		if strings.Contains(body, obsolete) {
			t.Fatalf("obsolete help text remains: %q", obsolete)
		}
	}
}
