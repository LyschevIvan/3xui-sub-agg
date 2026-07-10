package xui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNativeAPISmokeCoversFinalOperationsAndNoLegacyRequests(t *testing.T) {
	const token = "task-10-native-token"
	var mu sync.Mutex
	var paths []string
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("Authorization=%q", got)
		}
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()

		var obj any = struct{}{}
		switch r.URL.Path {
		case "/panel/api/server/status":
			obj = ServerStatus{PanelVersion: "3.4.2"}
		case "/panel/api/clients/list":
			obj = ClientPage{Items: []ClientSummary{{RecordID: intPointerForSmoke(1), Email: "group@example", SubID: "group", Enable: true, InboundIDs: []int{7}}}, Total: 1, Filtered: 1, Page: 1, PageSize: 200}
		case "/panel/api/clients/subLinks/group":
			obj = []string{"vless://native-link"}
		case "/panel/api/clients/group@example/attach", "/panel/api/clients/group@example/detach":
			if r.Method != http.MethodPost {
				t.Errorf("%s method=%s", r.URL.Path, r.Method)
			}
		case "/panel/api/inbounds/get/7":
			obj = json.RawMessage(`{"id":7,"remark":"before","port":443,"enable":true,"protocol":"vless","settings":{"clients":[]},"streamSettings":{"network":"tcp","security":"reality"},"sniffing":{}}`)
		case "/panel/api/inbounds/update/7":
			if r.Method != http.MethodPost {
				t.Errorf("%s method=%s", r.URL.Path, r.Method)
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read update: %v", err)
			}
			obj = json.RawMessage(body)
		default:
			http.Error(w, "unexpected native API path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "msg": "", "obj": obj})
	}))
	defer panel.Close()

	client, err := NewAPI(APIConfig{BaseURL: panel.URL, Token: token, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := client.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	clients, err := client.ListClients(ctx)
	if err != nil || len(clients) != 1 {
		t.Fatalf("sync clients=%+v err=%v", clients, err)
	}
	links, err := client.SubLinks(ctx, "group", "public.example")
	if err != nil || !slices.Equal(links, []string{"vless://native-link"}) {
		t.Fatalf("subscription links=%v err=%v", links, err)
	}
	if err := client.AttachClient(ctx, "group@example", []int{7}); err != nil {
		t.Fatal(err)
	}
	if err := client.DetachClient(ctx, "group@example", []int{7}); err != nil {
		t.Fatal(err)
	}
	document, err := client.GetInbound(ctx, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := document.Set("remark", "after"); err != nil {
		t.Fatal(err)
	}
	updated, err := client.UpdateInbound(ctx, 7, document)
	if err != nil {
		t.Fatal(err)
	}
	if remark, err := updated.String("remark"); err != nil || remark != "after" {
		t.Fatalf("updated remark=%q err=%v", remark, err)
	}

	mu.Lock()
	gotPaths := slices.Clone(paths)
	mu.Unlock()
	wantPaths := []string{
		"/panel/api/server/status",
		"/panel/api/clients/list",
		"/panel/api/clients/subLinks/group",
		"/panel/api/clients/group@example/attach",
		"/panel/api/clients/group@example/detach",
		"/panel/api/inbounds/get/7",
		"/panel/api/inbounds/update/7",
	}
	if !slices.Equal(gotPaths, wantPaths) {
		t.Fatalf("paths=%v want=%v", gotPaths, wantPaths)
	}
	for _, requestPath := range gotPaths {
		for _, forbidden := range []string{
			"/lo" + "gin",
			"inbounds/" + "addClient",
			"inbounds/" + "updateClient",
			"/del" + "Client/",
		} {
			if strings.Contains(requestPath, forbidden) {
				t.Fatal(fmt.Sprintf("legacy path requested: %s", requestPath))
			}
		}
	}
}

func intPointerForSmoke(value int) *int { return &value }
