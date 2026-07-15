package webui

import (
	"net/http"
	"strings"
	"testing"
)

func TestLegacyClientsPageRedirectsToSubscriptions(t *testing.T) {
	app := newServerTestApp(t, "master")
	rr := app.request(t, http.MethodGet, "/dashboard/clients", nil)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/dashboard/subscriptions" {
		t.Fatalf("status=%d location=%q", rr.Code, rr.Header().Get("Location"))
	}
}

func TestSubscriptionsPageUsesSubscriptionTerminologyAndBatchAction(t *testing.T) {
	app := newServerTestApp(t, "master")
	installPlannerSnapshot(t, app)

	rr := app.request(t, http.MethodGet, "/dashboard/subscriptions", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"<h1>Подписки</h1>", "alice@example", "bob@example", "Подключить к inbound", "name=\"sub_id\" value=\"alice\""} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.Contains(body, "<h1>Пользователи</h1>") {
		t.Fatal("legacy terminology retained")
	}
}

func TestSubscriptionDetailShowsConnectionsGroupsAndLink(t *testing.T) {
	app := newServerTestApp(t, "master")
	installPlannerSnapshot(t, app)
	group, err := app.store.CreateClientGroup(app.user.ID, "Семья")
	if err != nil {
		t.Fatal(err)
	}
	if err := app.store.AddClientGroupMembers(app.user.ID, group.ID, []string{"alice"}); err != nil {
		t.Fatal(err)
	}

	rr := app.request(t, http.MethodGet, "/dashboard/subscriptions/view?sub_id=alice", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"alice@example", "Подключения", "Группы", "Семья", "Копировать ссылку", "Подключить к inbound"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q", want)
		}
	}
}
