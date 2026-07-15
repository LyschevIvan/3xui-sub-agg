package webui

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestGroupsPageCreatesAndRendersGroup(t *testing.T) {
	app := newServerTestApp(t, "master")
	created := app.request(t, http.MethodPost, "/dashboard/groups/new", url.Values{"name": {"Семья"}})
	if created.Code != http.StatusSeeOther {
		t.Fatalf("create status = %d body=%s", created.Code, created.Body.String())
	}

	page := app.request(t, http.MethodGet, "/dashboard/groups", nil)
	if page.Code != http.StatusOK {
		t.Fatalf("page status = %d body=%s", page.Code, page.Body.String())
	}
	for _, want := range []string{"Группы", "Семья", "Создать группу"} {
		if !strings.Contains(page.Body.String(), want) {
			t.Fatalf("page missing %q: %s", want, page.Body.String())
		}
	}
}

func TestGroupMemberActionsAreOwnerScoped(t *testing.T) {
	app := newServerTestApp(t, "master")
	own, err := app.store.CreateClientGroup(app.user.ID, "Семья")
	if err != nil {
		t.Fatal(err)
	}
	foreignUser, err := app.store.CreateUser("foreign", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := app.store.CreateClientGroup(foreignUser.ID, "Чужая")
	if err != nil {
		t.Fatal(err)
	}

	add := app.request(t, http.MethodPost,
		"/dashboard/groups/"+strconv.FormatInt(own.ID, 10)+"/members/add",
		url.Values{"sub_id": {"alice", "bob"}},
	)
	if add.Code != http.StatusSeeOther {
		t.Fatalf("add status = %d body=%s", add.Code, add.Body.String())
	}
	loaded, err := app.store.ClientGroupByID(app.user.ID, own.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Members) != 2 {
		t.Fatalf("members = %v", loaded.Members)
	}

	foreignAdd := app.request(t, http.MethodPost,
		"/dashboard/groups/"+strconv.FormatInt(foreign.ID, 10)+"/members/add",
		url.Values{"sub_id": {"mallory"}},
	)
	if foreignAdd.Code != http.StatusNotFound {
		t.Fatalf("foreign status = %d body=%s", foreignAdd.Code, foreignAdd.Body.String())
	}
}

func TestClientsPageOffersGroupAssignment(t *testing.T) {
	app := newServerTestApp(t, "master")
	if _, err := app.store.CreateClientGroup(app.user.ID, "Семья"); err != nil {
		t.Fatal(err)
	}
	page := app.request(t, http.MethodGet, "/dashboard/clients", nil)
	if page.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", page.Code, page.Body.String())
	}
	for _, want := range []string{"Пользователи", "Добавить в группу", "Семья"} {
		if !strings.Contains(page.Body.String(), want) {
			t.Fatalf("page missing %q: %s", want, page.Body.String())
		}
	}
}
