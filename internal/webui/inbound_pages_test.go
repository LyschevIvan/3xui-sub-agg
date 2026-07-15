package webui

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestInboundsPageListsOwnedInboundWithPermanentConnectAction(t *testing.T) {
	app := newServerTestApp(t, "master")
	server := installPlannerSnapshot(t, app)

	rr := app.request(t, http.MethodGet, "/dashboard/inbounds", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"Inbound'ы", server.Name, "main", "fi.example:443", "1 подписка", "Подключить"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
}

func TestInboundDetailShowsConnectionsAndPrefilledPlanner(t *testing.T) {
	app := newServerTestApp(t, "master")
	server := installPlannerSnapshot(t, app)

	target := "/dashboard/inbounds/view?server_id=" + strconv.FormatInt(server.ID, 10) + "&inbound_id=77"
	rr := app.request(t, http.MethodGet, target, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"main", "alice", "Подключить подписки",
		"target_server_id=" + strconv.FormatInt(server.ID, 10) + "&amp;target_inbound_id=77",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
}
