package webui

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/LyschevIvan/3xui-sub-agg/internal/aggregator"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
)

func TestClientCardsJoinNativeGroupsAndInboundsReadOnly(t *testing.T) {
	recordID := 41
	snap := &aggregator.Snapshot{Servers: []aggregator.ServerSnapshot{
		{
			ID: 1, UserID: 9, Name: "alpha",
			Inbounds: []aggregator.InboundInfo{
				{ID: 1, Remark: "primary", Port: 443, Protocol: "vless", Network: "ws", Security: "tls", Enable: true},
				{ID: 2, Remark: "trojan", Port: 8443, Protocol: "trojan", Network: "grpc", Security: "reality", Enable: true},
				{ID: 3, Remark: "disabled member", Port: 9443, Protocol: "vless", Network: "tcp", Security: "reality", Enable: true},
				{ID: 4, Remark: "candidate", Port: 10443, Protocol: "vless", Network: "tcp", Security: "none", Enable: true},
				{ID: 5, Remark: "disabled inbound", Port: 11443, Protocol: "vless", Enable: false},
			},
			Groups: map[string]aggregator.ClientGroup{
				"shared": {
					SubID: "shared",
					Records: []aggregator.ClientRef{
						{RecordID: &recordID, Email: "active@example", SubID: "shared", Enabled: true, InboundIDs: []int{1, 2}},
						{Email: "duplicate@example", SubID: "shared", Enabled: false, InboundIDs: []int{1}},
						{Email: "disabled@example", SubID: "shared", Enabled: false, InboundIDs: []int{3}},
						{Email: "orphan@example", SubID: "shared", Enabled: true, InboundIDs: []int{99}},
					},
				},
			},
		},
		{
			ID: 2, UserID: 9, Name: "beta",
			Inbounds: []aggregator.InboundInfo{{ID: 20, Remark: "secondary", Port: 2443, Protocol: "vless", Network: "tcp", Security: "none", Enable: true}},
			Groups: map[string]aggregator.ClientGroup{
				"shared": {SubID: "shared", Records: []aggregator.ClientRef{{Email: "beta@example", SubID: "shared", Enabled: true, InboundIDs: []int{20}}}},
			},
		},
		{
			ID: 3, UserID: 10, Name: "foreign",
			Inbounds: []aggregator.InboundInfo{{ID: 30, Port: 3443, Protocol: "vless", Enable: true}},
			Groups:   map[string]aggregator.ClientGroup{"foreign": {SubID: "foreign", Records: []aggregator.ClientRef{{Email: "foreign", SubID: "foreign", Enabled: true, InboundIDs: []int{30}}}}},
		},
	}}

	cards := buildClientCards(snap, 9, "https://subs.example/u")
	if len(cards) != 1 {
		t.Fatalf("cards=%+v", cards)
	}
	card := cards[0]
	if card.Sub != "shared" || card.SubURL != "https://subs.example/u/shared" {
		t.Fatalf("card=%+v", card)
	}
	for _, email := range []string{"active@example", "beta@example", "disabled@example", "duplicate@example", "orphan@example"} {
		if !strings.Contains(card.Emails, email) {
			t.Fatalf("emails=%q missing %q", card.Emails, email)
		}
	}
	if len(card.Servers) != 2 || card.Servers[0].Name != "alpha" || card.Servers[1].Name != "beta" {
		t.Fatalf("servers=%+v", card.Servers)
	}
	if got := card.Servers[0].Inbounds; len(got) != 3 || got[0].Port != 443 || got[1].Port != 8443 || got[2].Port != 9443 {
		t.Fatalf("alpha logical rows=%+v", got)
	} else if !got[0].Enabled || !got[1].Enabled || got[2].Enabled {
		t.Fatalf("alpha enabled states=%+v", got)
	}
	if len(card.Candidates) != 1 || card.Candidates[0].InboundID != 4 {
		t.Fatalf("VLESS candidates/suppression=%+v", card.Candidates)
	}
}

func TestDashboardRendersControlledNativeStateAndNoMutationForms(t *testing.T) {
	app := newServerTestApp(t, "dashboard-task-7-key")
	server, err := app.store.CreateServer(&storage.Server{
		UserID: app.user.ID, Name: "node", APIURL: "https://panel.example", Path: "/", APIToken: "token",
	})
	if err != nil {
		t.Fatal(err)
	}
	rawSecret := "raw-panel-error-and-token"
	snapshot := app.handler.Agg.Snapshot()
	snapshot.Servers = []aggregator.ServerSnapshot{{
		ID: server.ID, UserID: app.user.ID, Name: server.Name, PublicHost: "public.example:443",
		PanelVersion: "3.4.2<script>" + rawSecret, State: aggregator.ServerTokenRejected,
		FetchedAt: time.Now(), AttemptedAt: time.Now(), SyncErr: errors.New(rawSecret),
		Inbounds: []aggregator.InboundInfo{
			{ID: 1, Remark: "member", Port: 443, Protocol: "vless", Enable: true},
			{ID: 2, Remark: "candidate", Port: 8443, Protocol: "vless", Enable: true},
		},
		Groups: map[string]aggregator.ClientGroup{
			"group": {SubID: "group", Records: []aggregator.ClientRef{{Email: "member@example", SubID: "group", Enabled: true, InboundIDs: []int{1}}}},
		},
	}}

	rr := app.request(t, http.MethodGet, "/dashboard", nil)
	body := rr.Body.String()
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rr.Code, body)
	}
	if strings.Contains(body, rawSecret) || strings.Contains(body, "3.4.2&lt;script&gt;") {
		t.Fatalf("raw state/version leaked: %s", body)
	}
	if !strings.Contains(body, "API-токен отклонён") || !strings.Contains(body, "управление будет доступно после обновления") {
		t.Fatalf("controlled labels missing: %s", body)
	}
	for _, action := range []string{"/dashboard/clients/inbound/add", "/dashboard/clients/inbound/remove"} {
		if strings.Contains(body, action) {
			t.Fatalf("legacy mutation remains reachable from native dashboard: %s", action)
		}
	}
}

func TestPopulateServerEditExtrasUsesControlledStateMessage(t *testing.T) {
	app := newServerTestApp(t, "server-edit-task-7-key")
	rawSecret := "raw-form-error-secret"
	form := serverFormData{}
	populateServerEditExtras(&form, &aggregator.Snapshot{Servers: []aggregator.ServerSnapshot{{
		ID: 7, UserID: app.user.ID, State: aggregator.ServerUnavailable, SyncErr: errors.New(rawSecret),
	}}}, 7, app.store, app.user.ID)
	if strings.Contains(form.InboundsErr, rawSecret) || form.InboundsErr != "Панель временно недоступна" {
		t.Fatalf("InboundsErr=%q", form.InboundsErr)
	}
}
