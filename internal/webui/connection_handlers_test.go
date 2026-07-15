package webui

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/LyschevIvan/3xui-sub-agg/internal/aggregator"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
)

func installPlannerSnapshot(t *testing.T, app *serverTestApp) *storage.Server {
	t.Helper()
	server, err := app.store.CreateServer(&storage.Server{
		UserID: app.user.ID, Name: "FI", APIURL: "https://fi.example", Path: "/",
	})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := app.handler.Agg.Snapshot()
	snapshot.Servers = []aggregator.ServerSnapshot{{
		ID: server.ID, UserID: app.user.ID, Name: server.Name, PublicHost: "fi.example",
		State: aggregator.ServerOK,
		Inbounds: []aggregator.InboundInfo{{
			ID: 77, Remark: "main", Port: 443, Protocol: "vless", Network: "tcp", Security: "reality", Enable: true,
		}},
		Groups: map[string]aggregator.ClientGroup{
			"alice": {
				SubID: "alice",
				Records: []aggregator.ClientRef{{
					Email: "alice@example", SubID: "alice", Enabled: true, InboundIDs: []int{77},
				}},
			},
			"bob": {
				SubID:   "bob",
				Records: []aggregator.ClientRef{{Email: "bob@example", SubID: "bob", Enabled: true}},
			},
		},
	}}
	return server
}

func TestConnectionPlannerRendersPermanentScopesAndPrefilledTarget(t *testing.T) {
	app := newServerTestApp(t, "master")
	server := installPlannerSnapshot(t, app)
	if _, err := app.store.CreateClientGroup(app.user.ID, "Семья"); err != nil {
		t.Fatal(err)
	}

	target := "/dashboard/connections/new?target_server_id=" + strconv.FormatInt(server.ID, 10) + "&target_inbound_id=77"
	rr := app.request(t, http.MethodGet, target, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"Подключить подписки", "Все подписки", "Выбранные группы",
		"Выбранные подписки", "main", "Исходные подключения сохранятся",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
}

func TestBuildConnectionPreviewUnionsGroupsAndSplitsTargetState(t *testing.T) {
	app := newServerTestApp(t, "master")
	server := installPlannerSnapshot(t, app)
	family, err := app.store.CreateClientGroup(app.user.ID, "Семья")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.AddClientGroupMembers(app.user.ID, family.ID, []string{"alice", "bob", "offline"}); err != nil {
		t.Fatal(err)
	}

	preview, err := app.handler.buildConnectionPreview(context.Background(), app.user.ID, connectionSelection{
		Scope:           connectionScopeGroups,
		GroupIDs:        []int64{family.ID},
		TargetServerID:  server.ID,
		TargetInboundID: 77,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(preview.AlreadyAttached, ",") != "alice" {
		t.Fatalf("already attached = %v", preview.AlreadyAttached)
	}
	if strings.Join(preview.ToAdd, ",") != "bob" {
		t.Fatalf("to add = %v", preview.ToAdd)
	}
	if strings.Join(preview.Unavailable, ",") != "offline" {
		t.Fatalf("unavailable = %v", preview.Unavailable)
	}
}
