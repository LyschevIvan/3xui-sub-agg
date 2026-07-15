package storage

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/LyschevIvan/3xui-sub-agg/internal/secrets"
)

func newGroupTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "data.db"), secrets.New("master"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func createGroupTestUser(t *testing.T, store *Store, login string) *User {
	t.Helper()
	user, err := store.CreateUser(login, "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	return user
}

func TestClientGroupsAreOwnerScopedAndManyToMany(t *testing.T) {
	store := newGroupTestStore(t)
	owner := createGroupTestUser(t, store, "owner")
	foreign := createGroupTestUser(t, store, "foreign")

	family, err := store.CreateClientGroup(owner.ID, "  Семья  ")
	if err != nil {
		t.Fatal(err)
	}
	friends, err := store.CreateClientGroup(owner.ID, "Друзья")
	if err != nil {
		t.Fatal(err)
	}
	if family.Name != "Семья" {
		t.Fatalf("normalized name = %q", family.Name)
	}
	if err := store.AddClientGroupMembers(owner.ID, family.ID, []string{"alice", "bob", "alice", ""}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddClientGroupMembers(owner.ID, friends.ID, []string{"alice"}); err != nil {
		t.Fatal(err)
	}

	memberships, err := store.ClientGroupMemberships(owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(memberships["alice"]); got != 2 {
		t.Fatalf("alice groups = %d", got)
	}
	if got := len(memberships["bob"]); got != 1 {
		t.Fatalf("bob groups = %d", got)
	}
	if _, ok := memberships[""]; ok {
		t.Fatal("blank subID was persisted")
	}
	groups, err := store.ListClientGroups(owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || len(groups[0].Members)+len(groups[1].Members) != 3 {
		t.Fatalf("groups = %+v", groups)
	}

	if _, err := store.ClientGroupByID(foreign.ID, family.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign read err = %v", err)
	}
	if err := store.AddClientGroupMembers(foreign.ID, family.ID, []string{"mallory"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign write err = %v", err)
	}
}

func TestClientGroupNameKeyRejectsCaseAndWhitespaceDuplicates(t *testing.T) {
	store := newGroupTestStore(t)
	owner := createGroupTestUser(t, store, "owner")
	if _, err := store.CreateClientGroup(owner.ID, "Моя   семья"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateClientGroup(owner.ID, "  МОЯ семья "); !errors.Is(err, ErrClientGroupExists) {
		t.Fatalf("duplicate err = %v", err)
	}
}

func TestClientGroupDeleteCascadesMembershipOnlyForOwner(t *testing.T) {
	store := newGroupTestStore(t)
	owner := createGroupTestUser(t, store, "owner")
	foreign := createGroupTestUser(t, store, "foreign")
	group, err := store.CreateClientGroup(owner.ID, "Семья")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddClientGroupMembers(owner.ID, group.ID, []string{"alice"}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteClientGroup(foreign.ID, group.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign delete err = %v", err)
	}
	if err := store.DeleteClientGroup(owner.ID, group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClientGroupByID(owner.ID, group.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted read err = %v", err)
	}
}

func TestNewServerStartsOnboardingAndCanCompleteIt(t *testing.T) {
	store := newGroupTestStore(t)
	owner := createGroupTestUser(t, store, "owner")
	server, err := store.CreateServer(&Server{
		UserID: owner.ID, Name: "FI", APIURL: "https://panel.example", Path: "/",
	})
	if err != nil {
		t.Fatal(err)
	}
	if server.OnboardingCompleted {
		t.Fatal("new server unexpectedly completed onboarding")
	}
	if err := store.CompleteServerOnboarding(owner.ID, server.ID); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.ServerByID(owner.ID, server.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.OnboardingCompleted {
		t.Fatal("onboarding completion was not persisted")
	}
}
