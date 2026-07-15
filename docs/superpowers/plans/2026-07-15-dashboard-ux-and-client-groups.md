# Inbound, Subscription, and Connection UX Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (- [ ]) syntax for tracking.

**Goal:** Перестроить кабинет вокруг постоянных разделов «Серверы», «Inbound'ы», «Подписки» и «Группы» и дать из каждого релевантного объекта один общий сценарий безопасного подключения подписок к inbound.

**Architecture:** Сохраняется серверный Go/html/template интерфейс. Новый catalog read model один раз объединяет snapshot 3x-ui по владельцу и используется всеми каталогами и detail pages; отдельный connection planner выбирает аудиторию, строит свежий preview и вызывает существующий идемпотентный bulk-copy. Onboarding только предзаполняет этот planner и больше не содержит уникальной операции.

**Tech Stack:** Go 1.25, net/http, html/template, SQLite (modernc.org/sqlite), существующий native 3x-ui API client, vanilla CSS/JavaScript, Go testing.

## Global Constraints

- Не выполнять git commit, git push, rebase, reset или другие операции с историей без отдельного явного разрешения пользователя.
- Основной термин для логического объекта по subId — «Подписка»; «Пользователь» не используется как название раздела.
- Глобальная навигация: «Обзор», «Серверы», «Inbound'ы», «Подписки», «Группы».
- Исходные подключения не удаляются; массовое действие называется «Подключить».
- Массовая операция принимает не более 500 уникальных непустых subId.
- Все POST-маршруты используют текущую CSRF-защиту и owner scope.
- Preview не является доверенной авторизацией: apply повторно проверяет владельца, сервер, inbound и каждый subId.
- Существующие /sub/..., login/register/admin и безопасные mutation endpoints сохраняют семантику.
- Токены, URL панели, сырые ответы API и внутренние ошибки не выводятся пользователю.
- SPA, frontend framework и автоматическая синхронизация групп не добавляются.

---

## File Map

- Create: internal/webui/catalog.go — owner-scoped read model серверов, inbound'ов, подписок и подключений.
- Create: internal/webui/catalog_test.go — дедупликация subId, ownership, counters и stale-state.
- Create: internal/webui/connection_handlers.go — selection, preview, apply и единый planner handler.
- Create: internal/webui/connection_handlers_test.go — все точки входа, preview, CSRF/ownership и apply.
- Create: internal/webui/inbound_pages.go — каталог и detail page inbound.
- Create: internal/webui/inbound_pages_test.go — список, detail, target prefill и чужие объекты.
- Create: internal/webui/subscription_pages.go — каталог, detail и legacy redirect.
- Create: internal/webui/subscription_pages_test.go — фильтры, selection payload и detail.
- Modify: internal/webui/group_handlers.go — detail page группы и planner entry point.
- Modify: internal/webui/onboarding_handlers.go — переход в общий planner вместо отдельного copy-users.
- Modify: internal/webui/server_handlers.go — постоянное действие подключения со страницы сервера.
- Modify: internal/webui/webui.go — routes, templates и pageData.
- Create: internal/webui/templates/inbounds.html
- Create: internal/webui/templates/inbound_detail.html
- Create: internal/webui/templates/subscriptions.html
- Create: internal/webui/templates/subscription_detail.html
- Create: internal/webui/templates/group_detail.html
- Create: internal/webui/templates/connections.html
- Modify: internal/webui/templates/base.html
- Modify: internal/webui/templates/dashboard.html
- Modify: internal/webui/templates/servers.html
- Modify: internal/webui/templates/server_form.html
- Modify: internal/webui/templates/server_onboarding.html
- Modify: internal/webui/templates/groups.html
- Remove after route migration: internal/webui/templates/clients.html
- Modify: internal/webui/*_test.go — navigation and regression assertions.
- Modify: README.md — current routes and permanent connection workflow.

---

### Task 1: Extract an Owner-Scoped Catalog Read Model

**Files:**
- Create: internal/webui/catalog.go
- Create: internal/webui/catalog_test.go
- Modify: internal/webui/webui.go

**Interfaces:**
- Consumes: Aggregator.Snapshot, Store.ListServersByUser, Store.ClientGroupMemberships.
- Produces:
  - type catalog struct { Servers []serverRow; Inbounds []catalogInbound; Subscriptions []catalogSubscription }
  - func buildCatalog(snap *aggregator.Snapshot, userID int64, subBase string, memberships map[string][]storage.ClientGroup) catalog
  - func (c catalog) subscription(subID string) (catalogSubscription, bool)
  - func (c catalog) inbound(serverID int64, inboundID int) (catalogInbound, bool)
  - func (h *Handler) loadCatalog(r *http.Request, userID int64) (catalog, error)

- [ ] **Step 1: Write the failing catalog test**

~~~go
func TestBuildCatalogDeduplicatesSubscriptionAndScopesOwner(t *testing.T) {
    snap := &aggregator.Snapshot{Servers: []aggregator.ServerSnapshot{
        {ID: 1, UserID: 7, Name: "DE", Inbounds: []aggregator.InboundInfo{{ID: 10, Remark: "main", Protocol: "vless", Enable: true}},
            Groups: map[string]aggregator.ClientGroup{"alice": {SubID: "alice", Records: []aggregator.ClientRef{{Email: "alice-de", SubID: "alice", InboundIDs: []int{10}}}}}},
        {ID: 2, UserID: 7, Name: "FI", Groups: map[string]aggregator.ClientGroup{"alice": {SubID: "alice", Records: []aggregator.ClientRef{{Email: "alice-fi", SubID: "alice"}}}},
        {ID: 3, UserID: 8, Name: "Foreign", Groups: map[string]aggregator.ClientGroup{"mallory": {SubID: "mallory"}}},
    }}
    got := buildCatalog(snap, 7, "https://sub.example/sub/token", nil)
    if len(got.Subscriptions) != 1 || got.Subscriptions[0].SubID != "alice" {
        t.Fatalf("subscriptions = %+v", got.Subscriptions)
    }
    if len(got.Inbounds) != 1 || got.Inbounds[0].ServerID != 1 {
        t.Fatalf("inbounds = %+v", got.Inbounds)
    }
}
~~~

- [ ] **Step 2: Run the test and verify RED**

Run: go test ./internal/webui -run TestBuildCatalogDeduplicatesSubscriptionAndScopesOwner -count=1

Expected: FAIL because buildCatalog is undefined.

- [ ] **Step 3: Implement the minimal catalog types and deterministic builder**

~~~go
type catalogConnection struct {
    ServerID int64
    ServerName string
    InboundID int
    InboundName string
    Endpoint string
    Enabled bool
}

type catalogSubscription struct {
    SubID string
    Emails []string
    SubURL string
    Connections []catalogConnection
    Groups []storage.ClientGroup
}

type catalogInbound struct {
    ServerID int64
    ServerName string
    InboundID int
    Remark string
    Protocol string
    Network string
    Security string
    Port int
    Enabled bool
    State aggregator.ServerState
    SubscriptionCount int
}

type catalog struct {
    Servers []serverRow
    Inbounds []catalogInbound
    Subscriptions []catalogSubscription
}
~~~

Move the exact subId accumulation rules from buildClientCards into buildCatalog. Sort servers by name/ID, inbound'ы by server/name/ID, subscriptions by primary email/subId, emails and connections deterministically. Ignore every ServerSnapshot whose UserID differs from userID.

- [ ] **Step 4: Add tests for membership counters and stale/unavailable servers**

~~~go
func TestBuildCatalogCountsExactInboundMemberships(t *testing.T) {
    snap := &aggregator.Snapshot{Servers: []aggregator.ServerSnapshot{{
        ID: 1, UserID: 7, Name: "DE",
        Inbounds: []aggregator.InboundInfo{{ID: 10, Remark: "main", Protocol: "vless", Enable: true}},
        Groups: map[string]aggregator.ClientGroup{"alice": {
            SubID: "alice",
            Records: []aggregator.ClientRef{{Email: "alice", SubID: "alice", InboundIDs: []int{10}}},
        }},
    }}}
    got := buildCatalog(snap, 7, "", nil)
    if got.Inbounds[0].SubscriptionCount != 1 {
        t.Fatalf("count = %d", got.Inbounds[0].SubscriptionCount)
    }
    if len(got.Subscriptions[0].Connections) != 1 {
        t.Fatalf("connections = %+v", got.Subscriptions[0].Connections)
    }
}
~~~

- [ ] **Step 5: Run catalog and existing dashboard tests**

Run: go test ./internal/webui -run 'TestBuildCatalog|TestClientCards|TestDashboard' -count=1

Expected: PASS.

- [ ] **Step 6: Diff checkpoint**

Run: git diff --check -- internal/webui/catalog.go internal/webui/catalog_test.go internal/webui/webui.go

Expected: no output.

---

### Task 2: Build the Connection Selection and Preview Domain

**Files:**
- Create: internal/webui/connection_handlers.go
- Create: internal/webui/connection_handlers_test.go

**Interfaces:**
- Consumes: Handler.Agg.RefreshNow, Handler.Agg.CopyGroupsToInbound, Store.ClientGroupByID, buildCatalog.
- Produces:

~~~go
type connectionScope string
const (
    connectionAll connectionScope = "all"
    connectionGroups connectionScope = "groups"
    connectionSubscriptions connectionScope = "subscriptions"
)

type connectionSelection struct {
    Scope connectionScope
    GroupIDs []int64
    SubIDs []string
    TargetServerID int64
    TargetInboundID int
}

type connectionPreview struct {
    Selection connectionSelection
    Target catalogInbound
    Selected []string
    ToAdd []string
    AlreadyAttached []string
    Unavailable []string
}

func (h *Handler) buildConnectionPreview(ctx context.Context, userID int64, selection connectionSelection) (connectionPreview, error)
func installConnectionFixture(t *testing.T, app *serverTestApp, subIDs []string, attached string) *storage.Server
~~~

- [ ] **Step 1: Write failing tests for all three audiences**

~~~go
func TestConnectionPreviewUnionsGroupsAndDeduplicatesMembers(t *testing.T) {
    app := newServerTestApp(t, "master")
    target := installConnectionFixture(t, app, []string{"alice", "bob"}, "alice")
    family, _ := app.store.CreateClientGroup(app.user.ID, "Семья")
    friends, _ := app.store.CreateClientGroup(app.user.ID, "Друзья")
    _ = app.store.AddClientGroupMembers(app.user.ID, family.ID, []string{"alice", "bob"})
    _ = app.store.AddClientGroupMembers(app.user.ID, friends.ID, []string{"alice"})

    got, err := app.handler.buildConnectionPreview(context.Background(), app.user.ID, connectionSelection{
        Scope: connectionGroups, GroupIDs: []int64{family.ID, friends.ID},
        TargetServerID: target.ID, TargetInboundID: 77,
    })
    if err != nil { t.Fatal(err) }
    if !slices.Equal(got.AlreadyAttached, []string{"alice"}) || !slices.Equal(got.ToAdd, []string{"bob"}) {
        t.Fatalf("preview = %+v", got)
    }
}
~~~

The shared test fixture creates the owned server and publishes a deterministic snapshot:

~~~go
func installConnectionFixture(t *testing.T, app *serverTestApp, subIDs []string, attached string) *storage.Server {
    t.Helper()
    server, err := app.store.CreateServer(&storage.Server{
        UserID: app.user.ID, Name: "FI", APIURL: "https://fi.example", Path: "/",
    })
    if err != nil { t.Fatal(err) }
    groups := make(map[string]aggregator.ClientGroup, len(subIDs))
    for _, subID := range subIDs {
        record := aggregator.ClientRef{Email: subID, SubID: subID, Enabled: true}
        if subID == attached { record.InboundIDs = []int{77} }
        groups[subID] = aggregator.ClientGroup{SubID: subID, Records: []aggregator.ClientRef{record}}
    }
    snapshot := app.handler.Agg.Snapshot()
    snapshot.Servers = []aggregator.ServerSnapshot{{
        ID: server.ID, UserID: app.user.ID, Name: server.Name, State: aggregator.ServerOK,
        Inbounds: []aggregator.InboundInfo{{ID: 77, Remark: "main", Protocol: "vless", Enable: true, Port: 443}},
        Groups: groups,
    }}
    return server
}
~~~

For pure preview tests, inject the catalog built from this snapshot before the refresh boundary. Handler integration tests use the existing fake native panel factory so RefreshNow reproduces the same state.

Add separate tests for scope=all, explicit repeated sub_id, empty group, foreign group, foreign server, disabled inbound and non-VLESS inbound.

- [ ] **Step 2: Run preview tests and verify RED**

Run: go test ./internal/webui -run TestConnectionPreview -count=1

Expected: FAIL because connectionSelection and buildConnectionPreview are undefined.

- [ ] **Step 3: Implement exact audience resolution**

~~~go
func normalizeSelectedSubIDs(values []string) []string {
    seen := make(map[string]struct{}, len(values))
    for _, value := range values {
        if value != "" { seen[value] = struct{}{} }
    }
    out := make([]string, 0, len(seen))
    for value := range seen { out = append(out, value) }
    sort.Strings(out)
    return out
}
~~~

For all, use every catalog subscription. For groups, load every ID through ClientGroupByID(userID, id), then union Members. For subscriptions, use submitted sub_id values. Intersect every audience with discoverable catalog subscriptions; keep non-discoverable IDs in Unavailable.

- [ ] **Step 4: Implement fresh target validation and preview split**

Call h.Agg.RefreshNow(ctx), rebuild the catalog, require an owner-scoped enabled VLESS target, collect its exact connected subId set, and split sorted IDs into ToAdd, AlreadyAttached and Unavailable. Reject zero selected IDs and more than 500 unique IDs.

- [ ] **Step 5: Run domain and aggregator bulk tests**

Run: go test ./internal/webui -run TestConnectionPreview -count=1

Expected: PASS.

Run: go test ./internal/aggregator -run TestCopyGroupsToInbound -count=1

Expected: PASS.

- [ ] **Step 6: Diff checkpoint**

Run: git diff --check -- internal/webui/connection_handlers.go internal/webui/connection_handlers_test.go

Expected: no output.

---

### Task 3: Add the Shared Connection Planner Page and Apply Route

**Files:**
- Modify: internal/webui/connection_handlers.go
- Modify: internal/webui/connection_handlers_test.go
- Create: internal/webui/templates/connections.html
- Modify: internal/webui/webui.go

**Interfaces:**
- Produces routes:
  - GET /dashboard/connections/new
  - POST /dashboard/connections/preview
  - POST /dashboard/connections/apply

- [ ] **Step 1: Write failing route tests**

~~~go
func TestConnectionPlannerPrefillsInboundAndRendersAllScopes(t *testing.T) {
    app := newServerTestApp(t, "master")
    target := installConnectionFixture(t, app, []string{"alice", "bob"}, "")
    rr := app.request(t, http.MethodGet,
        "/dashboard/connections/new?target_server_id="+strconv.FormatInt(target.ID, 10)+"&target_inbound_id=77", nil)
    if rr.Code != http.StatusOK { t.Fatalf("status = %d", rr.Code) }
    body := rr.Body.String()
    for _, want := range []string{"Все подписки", "Выбранные группы", "Выбранные подписки", "Исходные подключения сохранятся"} {
        if !strings.Contains(body, want) { t.Fatalf("missing %q", want) }
    }
}
~~~

Add tests proving POST without CSRF is rejected, foreign target returns 404/safe redirect, preview renders exact counts, apply calls bulk copy once, partial result keeps retry context, and successful apply redirects to inbound detail.

- [ ] **Step 2: Run route tests and verify RED**

Run: go test ./internal/webui -run 'TestConnectionPlanner|TestConnectionApply' -count=1

Expected: FAIL with 404 because routes are not mounted.

- [ ] **Step 3: Mount handlers with existing auth and CSRF middleware**

~~~go
mux.HandleFunc("/dashboard/connections/new", h.Auth.RequireUser(h.connectionNew))
mux.HandleFunc("/dashboard/connections/preview", h.Auth.RequireUser(h.connectionPreviewPost))
mux.HandleFunc("/dashboard/connections/apply", h.Auth.RequireUser(h.connectionApply))
~~~

GET only renders choices and prefill. Both POST handlers reject non-POST methods. The template includes csrf_token in preview and apply forms.

- [ ] **Step 4: Render the four-stage server-side flow**

connections.html contains one page, not a modal: audience controls, target controls, preview summary, explicit safe note, and an apply button whose label is «Подключить N подписок». Persist selection as repeated hidden group_id and sub_id inputs after preview.

~~~html
<button type="submit" {{if not .Connections.CanApply}}disabled{{end}}>
  Подключить {{len .Connections.Preview.ToAdd}} подписок
</button>
~~~

- [ ] **Step 5: Implement apply through the existing idempotent bulk operation**

~~~go
result, err := h.Agg.CopyGroupsToInbound(
    r.Context(), user.ID, preview.Target.ServerID, preview.Target.InboundID, preview.ToAdd,
)
~~~

Never send AlreadyAttached or Unavailable to the mutation. Show Added, AlreadyAttached and Failed counts; use safe copy for errors. A zero ToAdd preview cannot reach CopyGroupsToInbound.

- [ ] **Step 6: Run planner tests**

Run: go test ./internal/webui -run 'TestConnectionPlanner|TestConnectionPreview|TestConnectionApply' -count=1

Expected: PASS.

- [ ] **Step 7: Diff checkpoint**

Run: git diff --check -- internal/webui/connection_handlers.go internal/webui/connection_handlers_test.go internal/webui/templates/connections.html internal/webui/webui.go

Expected: no output.

---

### Task 4: Add the Global Inbound Catalog and Inbound Detail Page

**Files:**
- Create: internal/webui/inbound_pages.go
- Create: internal/webui/inbound_pages_test.go
- Create: internal/webui/templates/inbounds.html
- Create: internal/webui/templates/inbound_detail.html
- Modify: internal/webui/webui.go

**Interfaces:**
- Produces routes:
  - GET /dashboard/inbounds
  - GET /dashboard/inbounds/view?server_id=&inbound_id=

- [ ] **Step 1: Write failing rendering and ownership tests**

~~~go
func TestInboundsPageListsEveryOwnedInboundWithConnectAction(t *testing.T) {
    app := newServerTestApp(t, "master")
    target := installConnectionFixture(t, app, []string{"alice", "bob"}, "alice")
    rr := app.request(t, http.MethodGet, "/dashboard/inbounds", nil)
    if rr.Code != http.StatusOK { t.Fatalf("status = %d", rr.Code) }
    for _, want := range []string{"Inbound'ы", "main", target.Name, "Подключить", "1 подписка"} {
        if !strings.Contains(rr.Body.String(), want) { t.Fatalf("missing %q", want) }
    }
}
~~~

Add a detail test that verifies subscriptions, endpoint, status, tabs, a prefilled planner link, and 404 for an inbound owned by another user.

- [ ] **Step 2: Run tests and verify RED**

Run: go test ./internal/webui -run TestInboundPage -count=1

Expected: FAIL with 404.

- [ ] **Step 3: Implement GET handlers from the shared catalog**

Parse server_id with strconv.ParseInt and inbound_id with strconv.Atoi. Use catalog.inbound; do not query a raw snapshot without owner filtering. Return 404 for missing/foreign objects.

- [ ] **Step 4: Build searchable/filterable templates**

inbounds.html renders server, protocol, endpoint, network/security, status, count and a visible connect link:

~~~html
<a class="btn" href="/dashboard/connections/new?target_server_id={{.ServerID}}&target_inbound_id={{.InboundID}}">
  Подключить
</a>
~~~

Use GET query filters server_id, protocol, state and q on the server side so filtered URLs are shareable. On mobile, each row becomes a labelled block without horizontal scroll.

- [ ] **Step 5: Reuse existing edit/delete forms on the detail page**

The detail page posts to current /dashboard/inbounds/edit and /dashboard/inbounds/delete endpoints with server_id and inbound_id. Delete remains in a separate danger zone. Subscription removal posts to the existing detach endpoint.

- [ ] **Step 6: Run inbound and mutation regression tests**

Run: go test ./internal/webui -run 'TestInboundPage|TestInboundEdit|TestInboundDelete|TestClientInboundRemove' -count=1

Expected: PASS.

- [ ] **Step 7: Diff checkpoint**

Run: git diff --check -- internal/webui/inbound_pages.go internal/webui/inbound_pages_test.go internal/webui/templates/inbounds.html internal/webui/templates/inbound_detail.html

Expected: no output.

---

### Task 5: Replace «Пользователи» with Subscription Catalog and Detail Pages

**Files:**
- Create: internal/webui/subscription_pages.go
- Create: internal/webui/subscription_pages_test.go
- Create: internal/webui/templates/subscriptions.html
- Create: internal/webui/templates/subscription_detail.html
- Modify: internal/webui/group_handlers.go
- Modify: internal/webui/webui.go
- Remove: internal/webui/templates/clients.html

**Interfaces:**
- Produces routes:
  - GET /dashboard/subscriptions
  - GET /dashboard/subscriptions/view?sub_id=
  - GET /dashboard/clients -> 303 /dashboard/subscriptions

- [ ] **Step 1: Write failing terminology, redirect and batch selection tests**

~~~go
func TestLegacyClientsRedirectsToSubscriptions(t *testing.T) {
    app := newServerTestApp(t, "master")
    rr := app.request(t, http.MethodGet, "/dashboard/clients", nil)
    if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/dashboard/subscriptions" {
        t.Fatalf("status=%d location=%q", rr.Code, rr.Header().Get("Location"))
    }
}

func TestSubscriptionsPageUsesSubscriptionTerminology(t *testing.T) {
    app := newServerTestApp(t, "master")
    installConnectionFixture(t, app, []string{"alice"}, "")
    rr := app.request(t, http.MethodGet, "/dashboard/subscriptions", nil)
    if !strings.Contains(rr.Body.String(), "Подписки") || strings.Contains(rr.Body.String(), "<h1>Пользователи</h1>") {
        t.Fatalf("body = %s", rr.Body.String())
    }
}
~~~

Add assertions for repeated selected sub_id values in the planner form, group badges, subscription URL copy and detail connection rows.

- [ ] **Step 2: Run tests and verify RED**

Run: go test ./internal/webui -run 'TestLegacyClients|TestSubscriptionsPage|TestSubscriptionDetail' -count=1

Expected: FAIL because routes/templates are absent and /dashboard/clients still renders the old page.

- [ ] **Step 3: Implement catalog/detail handlers**

Use loadCatalog for both pages. Filters q, server_id, inbound_id and group_id are server-side. Detail resolves the exact raw sub_id query value through catalog.subscription and returns 404 when it is not discoverable for the current owner.

- [ ] **Step 4: Build the compact selectable subscription table**

Each checkbox belongs to one GET form targeting /dashboard/connections/new with scope=subscriptions and repeated sub_id values. JavaScript only reveals the contextual action bar and updates «Выбрано: N»; the form works without JavaScript.

~~~html
<input type="checkbox" name="sub_id" value="{{.SubID}}" form="subscription-selection">
<button type="submit" name="scope" value="subscriptions">Подключить к inbound</button>
~~~

The second batch action posts the same selected sub_id fields to the chosen group membership endpoint.

- [ ] **Step 5: Build subscription detail tabs**

Render full subId, copyable subscription URL, Connections, Groups and Native records sections. «Подключить к inbound» links to /dashboard/connections/new?scope=subscriptions&sub_id=... using url.QueryEscape generated in the view model.

- [ ] **Step 6: Run subscription, group and legacy regression tests**

Run: go test ./internal/webui -run 'TestLegacyClients|TestSubscriptionsPage|TestSubscriptionDetail|TestGroup|TestClientInbound' -count=1

Expected: PASS.

- [ ] **Step 7: Diff checkpoint**

Run: git diff --check -- internal/webui/subscription_pages.go internal/webui/subscription_pages_test.go internal/webui/group_handlers.go internal/webui/templates

Expected: no output.

---

### Task 6: Make Groups, Servers, and Onboarding Permanent Planner Entry Points

**Files:**
- Modify: internal/webui/group_handlers.go
- Modify: internal/webui/group_handlers_test.go
- Create: internal/webui/templates/group_detail.html
- Modify: internal/webui/templates/groups.html
- Modify: internal/webui/server_handlers.go
- Modify: internal/webui/server_handlers_test.go
- Modify: internal/webui/templates/server_form.html
- Modify: internal/webui/onboarding_handlers.go
- Modify: internal/webui/onboarding_handlers_test.go
- Modify: internal/webui/templates/server_onboarding.html
- Modify: internal/webui/webui.go

**Interfaces:**
- Adds GET /dashboard/groups/{id}.
- Existing POST /dashboard/groups/{id}/... routes remain owner-scoped.

- [ ] **Step 1: Write failing discoverability tests**

~~~go
func TestGroupDetailHasMemberManagementAndConnectAction(t *testing.T) {
    app := newServerTestApp(t, "master")
    group, _ := app.store.CreateClientGroup(app.user.ID, "Семья")
    _ = app.store.AddClientGroupMembers(app.user.ID, group.ID, []string{"alice"})
    rr := app.request(t, http.MethodGet, "/dashboard/groups/"+strconv.FormatInt(group.ID, 10), nil)
    for _, want := range []string{"Семья", "alice", "Добавить подписки", "Удалить из группы", "Подключить группу"} {
        if !strings.Contains(rr.Body.String(), want) { t.Fatalf("missing %q", want) }
    }
}
~~~

Add tests that server detail always contains «Подключить подписки», completed onboarding still leaves the action available, and onboarding step 3 links to /dashboard/connections/new with target_server_id.

- [ ] **Step 2: Run tests and verify RED**

Run: go test ./internal/webui -run 'TestGroupDetail|TestServer.*Connect|TestOnboarding.*Planner' -count=1

Expected: FAIL on 404 or missing copy.

- [ ] **Step 3: Split group GET detail from POST action parsing**

For /dashboard/groups/{id}, GET loads ClientGroupByID and the shared subscription catalog. For paths with an action suffix, require POST and keep existing owner-scoped storage calls. POST redirects back to the detail page instead of the group catalog.

- [ ] **Step 4: Add permanent group and server planner links**

~~~html
<a class="btn" href="/dashboard/connections/new?scope=groups&group_id={{.ID}}">Подключить группу</a>
~~~

Server detail uses target_server_id only; the planner chooses an inbound when the server has several, and preselects the only active VLESS inbound when there is exactly one.

- [ ] **Step 5: Remove onboarding-only copy-users behavior**

Keep copy-inbound and complete routes. Replace the Step 3 form with a link to the common planner:

~~~html
<a class="btn" href="/dashboard/connections/new?target_server_id={{.Server.ID}}">Добавить подписки</a>
~~~

The legacy onboarding /copy-users POST may return 303 to the common planner for one compatibility release; it must not maintain a second audience-selection implementation.

- [ ] **Step 6: Run focused and full web tests**

Run: go test ./internal/webui -run 'TestGroup|TestServer|TestOnboarding|TestConnection' -count=1

Expected: PASS.

- [ ] **Step 7: Diff checkpoint**

Run: git diff --check -- internal/webui

Expected: no output.

---

### Task 7: Update Global Navigation, Overview, Responsive Tables, and Documentation

**Files:**
- Modify: internal/webui/templates/base.html
- Modify: internal/webui/templates/dashboard.html
- Modify: internal/webui/templates/servers.html
- Modify: internal/webui/dashboard_test.go
- Modify: internal/webui/webui.go
- Modify: README.md

**Interfaces:**
- Consumes all prior routes and catalog counters.
- Produces the five-section shell and current user documentation.

- [ ] **Step 1: Write failing navigation and overview assertions**

~~~go
func TestAuthenticatedNavigationContainsFiveObjectSections(t *testing.T) {
    app := newServerTestApp(t, "master")
    rr := app.request(t, http.MethodGet, "/dashboard", nil)
    body := rr.Body.String()
    for _, want := range []string{"Обзор", "Серверы", "Inbound'ы", "Подписки", "Группы"} {
        if !strings.Contains(body, want) { t.Fatalf("missing %q", want) }
    }
    if strings.Contains(body, `href="/dashboard/clients"`) { t.Fatal("legacy clients nav retained") }
}
~~~

Add per-page active Section assertions and overview metrics for active inbound count.

- [ ] **Step 2: Run assertions and verify RED**

Run: go test ./internal/webui -run 'TestAuthenticatedNavigation|TestDashboard' -count=1

Expected: FAIL because Inbound'ы/Подписки navigation and counters are incomplete.

- [ ] **Step 3: Implement five-section navigation and shared table primitives**

Use Section values dashboard, servers, inbounds, subscriptions, groups. Add .resource-table, .resource-row, .resource-toolbar, .batch-bar and labelled mobile layout. Maintain visible focus, 40px minimum controls and status text in addition to color.

- [ ] **Step 4: Simplify overview and server directory**

Overview renders server health, active inbound count, subscription count and group count plus only two primary quick actions. Server rows show status, version, inbound/subscription counters and «Открыть»; they do not duplicate every inbound mutation.

- [ ] **Step 5: Update README routes and workflow**

Document /dashboard/inbounds, /dashboard/subscriptions and /dashboard/connections/new. State that groups are saved audiences, connection is explicit and repeatable, and changing membership does not automatically mutate servers.

- [ ] **Step 6: Run web tests and diff checks**

Run: go test ./internal/webui -count=1

Expected: PASS.

Run: git diff --check -- internal/webui README.md docs/superpowers

Expected: no output.

---

### Task 8: Full Regression and Visual Verification

**Files:**
- Modify only files required by concrete failures.

**Interfaces:**
- Produces a verified working tree; does not stage or commit.

- [ ] **Step 1: Format changed Go files**

Run: gofmt -w internal/webui/catalog.go internal/webui/catalog_test.go internal/webui/connection_handlers.go internal/webui/connection_handlers_test.go internal/webui/inbound_pages.go internal/webui/inbound_pages_test.go internal/webui/subscription_pages.go internal/webui/subscription_pages_test.go internal/webui/group_handlers.go internal/webui/onboarding_handlers.go internal/webui/server_handlers.go internal/webui/webui.go

Expected: exit 0.

- [ ] **Step 2: Run the full test suite**

Run: go test ./... -count=1

Expected: PASS for every package.

- [ ] **Step 3: Run race and static checks**

Run: go test -race ./internal/aggregator ./internal/webui -count=1

Expected: PASS with no race report.

Run: go vet ./...

Expected: exit 0.

- [ ] **Step 4: Build the application**

Run: go build -o /tmp/3xui-sub-agg-ux-verify ./cmd/aggregator

Expected: exit 0.

- [ ] **Step 5: Verify the complete task flow in a browser**

At desktop 1280px and mobile 390px:

1. Open Inbound'ы and locate a target without entering a server first.
2. Open «Подключить», select a group, verify counts, apply, and confirm source connections remain visible.
3. Repeat the action and confirm the UI reports already connected without duplicates.
4. Open Подписки, select several rows, use both batch actions.
5. Open a group, add/remove one member, then open its prefilled planner.
6. Open an existing completed server and confirm «Подключить подписки» is visible.
7. Confirm no horizontal page scroll, no console errors, keyboard focus is visible and all five navigation sections remain reachable.

- [ ] **Step 6: Run final checks after visual fixes**

Run: go test ./... -count=1

Expected: PASS.

Run: git diff --check

Expected: no output.

Run: git status --short

Expected: only intentional source/tests/docs plus the pre-existing user-owned .DS_Store, .claude/, AGENTS.md and CLAUDE.md. Do not stage or commit.
