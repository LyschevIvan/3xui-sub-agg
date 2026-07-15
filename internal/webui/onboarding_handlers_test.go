package webui

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/LyschevIvan/3xui-sub-agg/internal/aggregator"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
)

func TestServerOnboardingRendersAllAndGroupScopes(t *testing.T) {
	app := newServerTestApp(t, "master")
	target, err := app.store.CreateServer(&storage.Server{UserID: app.user.ID, Name: "FI", APIURL: "https://fi.example", Path: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.store.CreateClientGroup(app.user.ID, "Семья"); err != nil {
		t.Fatal(err)
	}
	snapshot := app.handler.Agg.Snapshot()
	snapshot.Servers = []aggregator.ServerSnapshot{{
		ID: target.ID, UserID: app.user.ID, Name: target.Name, State: aggregator.ServerOK,
		Inbounds: []aggregator.InboundInfo{{ID: 9, Remark: "main", Port: 443, Protocol: "vless", Enable: true}},
		Groups:   map[string]aggregator.ClientGroup{"alice": {SubID: "alice"}},
	}}

	page := app.request(t, http.MethodGet, "/dashboard/onboarding/"+strconv.FormatInt(target.ID, 10), nil)
	if page.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", page.Code, page.Body.String())
	}
	for _, want := range []string{"Настройка сервера FI", "Все пользователи", "Выбранные группы", "Семья", "Исходные подключения сохранятся"} {
		if !strings.Contains(page.Body.String(), want) {
			t.Fatalf("missing %q: %s", want, page.Body.String())
		}
	}
}

func TestServerOnboardingCompleteIsOwnerScoped(t *testing.T) {
	app := newServerTestApp(t, "master")
	target, err := app.store.CreateServer(&storage.Server{UserID: app.user.ID, Name: "FI", APIURL: "https://fi.example", Path: "/"})
	if err != nil {
		t.Fatal(err)
	}
	response := app.request(t, http.MethodPost,
		"/dashboard/onboarding/"+strconv.FormatInt(target.ID, 10)+"/complete", url.Values{},
	)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	loaded, err := app.store.ServerByID(app.user.ID, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.OnboardingCompleted {
		t.Fatal("onboarding was not completed")
	}
}
