# Dashboard UX and Client Groups Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Перестроить кабинет вокруг серверов, VPN-пользователей и групп и сделать добавление нового сервера пошаговым сценарием с безопасным массовым копированием пользователей.

**Architecture:** Сохраняем `net/http`, embedded Go templates и SQLite. Локальные группы хранят ссылки на логических VPN-пользователей по `subId`; отдельный bulk-сервис агрегатора копирует/прикрепляет их к целевому VLESS inbound без удаления источников. UI получает отдельные read models и маршруты, но существующие POST endpoints и subscription URLs остаются совместимыми.

**Tech Stack:** Go 1.25, `net/http`, `html/template`, SQLite (`modernc.org/sqlite`), существующий native 3x-ui API client, vanilla CSS/JavaScript, Go `testing`.

**Implementation note:** Во время выполнения bulk-copy был сознательно сужен до безопасного первого релиза: новый target client получает прежний `subId`, новый UUID/email и корректный flow, но не наследует traffic/expiry payload исходной панели. Это соответствует утверждённому неразрушающему сценарию и исключает неоднозначный выбор источника при разных лимитах на нескольких серверах.

## Global Constraints

- Не выполнять `git commit`, `git push`, rebase, reset или другие операции с историей без явного разрешения пользователя; commit-шаги заменены diff-checkpoint'ами.
- Исходные подключения и записи на 3x-ui серверах никогда не удаляются массовой операцией.
- Массовое копирование принимает не более 500 уникальных непустых `subId`.
- Один VPN-пользователь может состоять в нескольких локальных группах.
- Существующие `/sub/...`, login/register/admin и текущие mutation endpoints сохраняют семантику.
- Технические ошибки, токены и сырые ответы панелей не выводятся в UI.
- Все POST routes проходят текущую CSRF-защиту и owner scope.

---

## File Map

- `internal/storage/storage.go` — миграция `client_groups`, `client_group_members`, `servers.onboarding_completed`; storage API групп и onboarding.
- `internal/storage/groups_test.go` — CRUD, нормализация, multi-group, owner scope и каскадное удаление.
- `internal/aggregator/bulk_copy.go` — bulk copy request/result и безопасная обработка каждого `subId`.
- `internal/aggregator/bulk_copy_test.go` — canonical source, target reuse, idempotency, partial failure, ownership и limit.
- `internal/webui/catalog.go` — единый read model серверов и VPN-пользователей из snapshot, используемый всеми страницами.
- `internal/webui/group_handlers.go` — CRUD групп и изменение состава.
- `internal/webui/onboarding_handlers.go` — preview/execute массового копирования и завершение мастера.
- `internal/webui/webui.go` — регистрация новых templates/routes и компактные page models.
- `internal/webui/server_handlers.go` — redirect нового сервера в onboarding и чтение onboarding state.
- `internal/webui/templates/base.html` — новый shell, tokens, navigation и responsive primitives.
- `internal/webui/templates/dashboard.html` — обзор.
- `internal/webui/templates/servers.html` — список серверов.
- `internal/webui/templates/clients.html` — пользователи, поиск, выбор и membership actions.
- `internal/webui/templates/groups.html` — группы и управление составом.
- `internal/webui/templates/server_form.html` — упрощённая форма, tabs/sections и onboarding card.
- `internal/webui/templates/server_onboarding.html` — шаги inbound/users и preview.
- `internal/webui/templates/admin.html`, `login.html`, `register.html` — адаптация к shell без изменения логики.
- `internal/webui/*_test.go` — route, CSRF, owner scope, rendering и regression assertions.

---

### Task 1: Persist Client Groups and Server Onboarding State

**Files:**
- Modify: `internal/storage/storage.go`
- Create: `internal/storage/groups_test.go`
- Modify: `internal/storage/storage_test.go`

**Interfaces:**
- Produces:
  - `type ClientGroup struct { ID, UserID int64; Name string; CreatedAt time.Time; Members []string }`
  - `CreateClientGroup(userID int64, name string) (*ClientGroup, error)`
  - `ListClientGroups(userID int64) ([]ClientGroup, error)`
  - `ClientGroupByID(userID, groupID int64) (*ClientGroup, error)`
  - `RenameClientGroup(userID, groupID int64, name string) error`
  - `DeleteClientGroup(userID, groupID int64) error`
  - `AddClientGroupMembers(userID, groupID int64, subIDs []string) error`
  - `RemoveClientGroupMember(userID, groupID int64, subID string) error`
  - `ClientGroupMemberships(userID int64) (map[string][]ClientGroup, error)`
  - `CompleteServerOnboarding(userID, serverID int64) error`

- [ ] **Step 1: Add failing migration and group behavior tests**

```go
func TestClientGroupsAreOwnerScopedAndManyToMany(t *testing.T) {
    store, err := Open(filepath.Join(t.TempDir(), "data.db"), secrets.New("master"))
    if err != nil { t.Fatal(err) }
    t.Cleanup(func() { _ = store.Close() })
    owner, err := store.CreateUser("owner", "hash", false)
    if err != nil { t.Fatal(err) }
    foreign, err := store.CreateUser("foreign", "hash", false)
    if err != nil { t.Fatal(err) }
    family, err := store.CreateClientGroup(owner.ID, "  Семья  ")
    if err != nil { t.Fatal(err) }
    friends, err := store.CreateClientGroup(owner.ID, "Друзья")
    if err != nil { t.Fatal(err) }
    if err := store.AddClientGroupMembers(owner.ID, family.ID, []string{"alice", "bob", "alice"}); err != nil { t.Fatal(err) }
    if err := store.AddClientGroupMembers(owner.ID, friends.ID, []string{"alice"}); err != nil { t.Fatal(err) }
    memberships, err := store.ClientGroupMemberships(owner.ID)
    if err != nil { t.Fatal(err) }
    if got := len(memberships["alice"]); got != 2 { t.Fatalf("alice groups = %d", got) }
    if _, err := store.ClientGroupByID(foreign.ID, family.ID); !errors.Is(err, ErrNotFound) { t.Fatalf("foreign read = %v", err) }
}

func TestClientGroupNameKeyRejectsCaseAndWhitespaceDuplicates(t *testing.T) {
    store, err := Open(filepath.Join(t.TempDir(), "data.db"), secrets.New("master"))
    if err != nil { t.Fatal(err) }
    t.Cleanup(func() { _ = store.Close() })
    owner, err := store.CreateUser("owner", "hash", false)
    if err != nil { t.Fatal(err) }
    if _, err := store.CreateClientGroup(owner.ID, "Семья"); err != nil { t.Fatal(err) }
    if _, err := store.CreateClientGroup(owner.ID, "  СЕМЬЯ "); err == nil { t.Fatal("expected duplicate") }
}
```

- [ ] **Step 2: Run storage tests and confirm schema/API failures**

Run: `go test ./internal/storage -run 'TestClientGroup|TestMigration' -count=1`

Expected: FAIL because group methods and new columns do not exist.

- [ ] **Step 3: Add schema migration, normalization and transactional owner-scoped methods**

Use `strings.Fields`, `strings.Join(..., " ")`, `strings.ToLower` and `utf8.RuneCountInString` for a 1–64 rune name. Add `name_key` unique per owner. Resolve group ownership inside the same transaction before member writes:

```go
func normalizeClientGroupName(raw string) (string, string, error) {
    name := strings.Join(strings.Fields(raw), " ")
    if n := utf8.RuneCountInString(name); n < 1 || n > 64 {
        return "", "", ErrInvalidClientGroup
    }
    return name, strings.ToLower(name), nil
}
```

During migration, detect `servers.onboarding_completed` via the existing `PRAGMA table_info` pattern, add it when absent, then set all pre-existing rows to `1`. `CreateServer` explicitly inserts `0`; server scanning includes the boolean.

- [ ] **Step 4: Run and pass storage tests**

Run: `go test ./internal/storage -count=1`

Expected: PASS.

- [ ] **Step 5: Diff checkpoint**

Run: `git diff --check -- internal/storage`

Expected: no output. Do not commit.

---

### Task 2: Build a Shared VPN-User Catalog

**Files:**
- Create: `internal/webui/catalog.go`
- Create: `internal/webui/catalog_test.go`
- Modify: `internal/webui/webui.go`

**Interfaces:**
- Consumes: `Store.ListClientGroups`, `Store.ClientGroupMemberships`, `Aggregator.Snapshot`.
- Produces:
  - `type clientCatalog struct { Servers []serverRow; Clients []clientRow; Groups []groupRow; HealthyServers, ProblemServers int }`
  - `func buildClientCatalog(snap *aggregator.Snapshot, userID int64, subBase string, groups []storage.ClientGroup, memberships map[string][]storage.ClientGroup) clientCatalog`
  - `func (h *Handler) loadClientCatalog(userID int64, subBase string) (clientCatalog, error)`
  - `func uniqueCatalogSubIDs(c clientCatalog) []string`

- [ ] **Step 1: Add failing catalog tests for dedupe and membership**

```go
func TestBuildClientCatalogJoinsSameSubIDAcrossServersAndGroups(t *testing.T) {
    group := storage.ClientGroup{ID: 3, UserID: 7, Name: "Семья", Members: []string{"alice"}}
    snap := &aggregator.Snapshot{Servers: []aggregator.ServerSnapshot{
        {ID: 10, UserID: 7, Name: "DE", Groups: map[string]aggregator.ClientGroup{"alice": {SubID: "alice", Records: []aggregator.ClientRef{{Email: "alice-de", SubID: "alice", InboundIDs: []int{1}}}}}},
        {ID: 11, UserID: 7, Name: "FI", Groups: map[string]aggregator.ClientGroup{"alice": {SubID: "alice", Records: []aggregator.ClientRef{{Email: "alice-fi", SubID: "alice", InboundIDs: []int{2}}}}}},
    }}
    catalog := buildClientCatalog(snap, 7, "https://sub.test/sub/prefix", []storage.ClientGroup{group}, map[string][]storage.ClientGroup{"alice": {group}})
    if len(catalog.Clients) != 1 { t.Fatalf("clients = %d", len(catalog.Clients)) }
    if got := catalog.Clients[0].SubID; got != "alice" { t.Fatalf("subID = %q", got) }
    if len(catalog.Clients[0].Groups) != 1 { t.Fatalf("groups = %d", len(catalog.Clients[0].Groups)) }
}
```

- [ ] **Step 2: Verify failure**

Run: `go test ./internal/webui -run TestBuildClientCatalog -count=1`

Expected: FAIL because `buildClientCatalog` is undefined.

- [ ] **Step 3: Extract current `buildClientCards` accumulation into focused read models**

Keep exact `subId` matching, VLESS candidate rules, server state mapping and deterministic sorting. Add group membership after snapshot aggregation; never synthesize a VPN-user from a group-only missing member on the main clients list, but expose the missing member on the group row.

- [ ] **Step 4: Run catalog and existing dashboard tests**

Run: `go test ./internal/webui -run 'TestBuildClientCatalog|TestDashboard' -count=1`

Expected: PASS.

- [ ] **Step 5: Diff checkpoint**

Run: `git diff --check -- internal/webui/catalog.go internal/webui/catalog_test.go internal/webui/webui.go`

Expected: no output. Do not commit.

---

### Task 3: Add Group Web Workflows

**Files:**
- Create: `internal/webui/group_handlers.go`
- Create: `internal/webui/group_handlers_test.go`
- Create: `internal/webui/templates/groups.html`
- Create: `internal/webui/templates/clients.html`
- Modify: `internal/webui/webui.go`

**Interfaces:**
- Consumes: Task 1 storage API and Task 2 catalog.
- Produces routes:
  - `GET /dashboard/clients`
  - `GET /dashboard/groups`
  - `POST /dashboard/groups/new`
  - `POST /dashboard/groups/{id}/rename`
  - `POST /dashboard/groups/{id}/delete`
  - `POST /dashboard/groups/{id}/members/add`
  - `POST /dashboard/groups/{id}/members/remove`

- [ ] **Step 1: Write failing route tests**

```go
func TestGroupMemberActionsRequireOwnerAndCSRF(t *testing.T) {
    app := newServerTestApp(t, "master")
    own, _ := app.store.CreateClientGroup(app.user.ID, "Семья")
    foreignUser, err := app.store.CreateUser("foreign", "hash", false)
    if err != nil { t.Fatal(err) }
    foreign, _ := app.store.CreateClientGroup(foreignUser.ID, "Чужая")
    ownResponse := app.request(t, http.MethodPost, "/dashboard/groups/"+strconv.FormatInt(own.ID, 10)+"/members/add", url.Values{"sub_id": {"alice"}})
    if ownResponse.Code != http.StatusSeeOther { t.Fatalf("own status = %d", ownResponse.Code) }
    foreignResponse := app.request(t, http.MethodPost, "/dashboard/groups/"+strconv.FormatInt(foreign.ID, 10)+"/members/add", url.Values{"sub_id": {"alice"}})
    if foreignResponse.Code != http.StatusNotFound { t.Fatalf("foreign status = %d", foreignResponse.Code) }
}
```

Also assert group deletion copy says it does not delete 3x-ui connections and client selection posts repeated `sub_id` fields.

- [ ] **Step 2: Verify tests fail**

Run: `go test ./internal/webui -run 'TestGroup|TestClientsPage' -count=1`

Expected: FAIL with 404/undefined template.

- [ ] **Step 3: Implement a single owner-scoped path parser and handlers**

Use POST-redirect-GET and existing flash cookies. Validate at least one non-empty `sub_id`; dedupe before storage. Map duplicate group names to a safe message «Группа с таким названием уже есть».

- [ ] **Step 4: Implement clients and groups templates**

Clients page provides search filtering in local JavaScript, row checkboxes, subscription copy, group badges and one bulk add form. Groups page provides create/rename/delete and member lists; missing members show «не найден на серверах».

- [ ] **Step 5: Run web tests**

Run: `go test ./internal/webui -run 'TestGroup|TestClientsPage' -count=1`

Expected: PASS.

- [ ] **Step 6: Diff checkpoint**

Run: `git diff --check -- internal/webui`

Expected: no output. Do not commit.

---

### Task 4: Implement Idempotent Bulk Copy in the Aggregator

**Files:**
- Create: `internal/aggregator/bulk_copy.go`
- Create: `internal/aggregator/bulk_copy_test.go`
- Modify: `internal/aggregator/inbound_mutations.go`

**Interfaces:**
- Produces:

```go
type BulkCopyStatus string
const (
    BulkCopyAdded BulkCopyStatus = "added"
    BulkCopyAlreadyAttached BulkCopyStatus = "already_attached"
    BulkCopyFailed BulkCopyStatus = "failed"
)
type BulkCopyItem struct { SubID string; Status BulkCopyStatus }
type BulkCopyResult struct { Items []BulkCopyItem; Added, AlreadyAttached, Failed int }
func (a *Aggregator) CopyGroupsToInbound(ctx context.Context, userID, targetServerID int64, targetInboundID int, subIDs []string) (BulkCopyResult, error)
```

- [ ] **Step 1: Add failing tests for reuse, copy and partial failure**

```go
func TestCopyGroupsToInboundPreservesSourcesAndReturnsPartialResult(t *testing.T) {
    a, sourcePanel, targetPanel, userID, targetID := setupBulkCopy(t)
    targetPanel.failSubID = "bob"
    result, err := a.CopyGroupsToInbound(context.Background(), userID, targetID, 77, []string{"alice", "bob", "alice"})
    if err != nil { t.Fatal(err) }
    if result.Added != 1 || result.Failed != 1 { t.Fatalf("result = %+v", result) }
    if sourcePanel.mutationCount() != 0 { t.Fatal("source was mutated") }
    if got := targetPanel.added[0].SubID; got != "alice" { t.Fatalf("subID = %q", got) }
}
```

Add separate tests for target-existing attach, already attached, non-VLESS target, foreign server, 501 unique `subId`, deterministic source and one final refresh.

- [ ] **Step 2: Verify failures**

Run: `go test ./internal/aggregator -run TestCopyGroupsToInbound -count=1`

Expected: FAIL because the method does not exist.

- [ ] **Step 3: Implement request normalization and target validation**

Trim only for rejecting blank input; preserve exact non-empty `subId`, dedupe and sort. Return `errInvalidMutation` when unique count is 0 or above 500. Resolve target with `ownedMutationClient`, fetch exact target document, require enabled VLESS and derive target network/security once.

- [ ] **Step 4: Implement deterministic source selection and per-item mutation**

List owned servers ordered by ID. Cache each fresh client inventory once. For a missing target record, choose the first exact source summary after `canonicalAttachClient`, fetch its detail and payload, then reuse the payload normalization from `copyGroupToInbound`. Hold the existing `(targetServerID, subId)` gate through the fresh target check and POST. Do not call any detach/delete API.

- [ ] **Step 5: Return safe per-item statuses and refresh once**

Context cancellation or target generation rotation stops the outer operation and returns the accumulated result plus the safe control error. Ordinary per-user inventory/request failures append `failed` and continue. Call `a.refresh(ctx)` once in a defer after target validation.

- [ ] **Step 6: Run aggregator tests**

Run: `go test ./internal/aggregator -run 'TestCopyGroupsToInbound|TestCopyInbound|TestAttachGroup' -count=1`

Expected: PASS.

- [ ] **Step 7: Diff checkpoint**

Run: `git diff --check -- internal/aggregator`

Expected: no output. Do not commit.

---

### Task 5: Add Bulk Preview and Server Onboarding Routes

**Files:**
- Create: `internal/webui/onboarding_handlers.go`
- Create: `internal/webui/onboarding_handlers_test.go`
- Create: `internal/webui/templates/server_onboarding.html`
- Modify: `internal/webui/webui.go`
- Modify: `internal/webui/server_handlers.go`

**Interfaces:**
- Consumes: `CopyGroupsToInbound`, catalog and group storage.
- Produces routes:
  - `GET /dashboard/servers/{id}/onboarding`
  - `POST /dashboard/servers/{id}/onboarding/copy-inbound`
  - `POST /dashboard/servers/{id}/onboarding/copy-users`
  - `POST /dashboard/servers/{id}/onboarding/complete`
- Produces pure selection helper: `func selectedGroupSubIDs(userID int64, groupIDs []int64, groups []storage.ClientGroup, discoverable map[string]struct{}) ([]string, error)`.

- [ ] **Step 1: Add failing wizard tests**

```go
func TestOnboardingAllAndGroupsBuildUniqueOwnedSubIDs(t *testing.T) {
    discoverable := map[string]struct{}{"alice": {}, "bob": {}, "carol": {}}
    groups := []storage.ClientGroup{{ID: 5, UserID: 7, Name: "Семья", Members: []string{"alice", "bob"}}}
    got, err := selectedGroupSubIDs(7, []int64{5, 5}, groups, discoverable)
    if err != nil { t.Fatal(err) }
    if !slices.Equal(got, []string{"alice", "bob"}) { t.Fatalf("subIDs = %v", got) }
}
```

Also test foreign group/server IDs, disabled/non-VLESS target, already attached counts, completion persistence and safe partial result copy.

- [ ] **Step 2: Verify failures**

Run: `go test ./internal/webui -run TestOnboarding -count=1`

Expected: FAIL because routes/models are absent.

- [ ] **Step 3: Implement path parsing, preview and execution**

`scope=all` uses `uniqueCatalogSubIDs`. `scope=groups` loads every requested group through owner-scoped storage and unions members, then intersects with currently discoverable catalog users. Preview computes exact target membership from snapshot; execute delegates to aggregator and maps only status counts to flash text.

- [ ] **Step 4: Redirect new servers into onboarding**

After successful `CreateServer`, redirect to `/dashboard/servers/{id}/onboarding`. Existing servers stay completed after migration. «Пропустить» and a fully successful copy mark onboarding complete; partial failure keeps it open for retry.

- [ ] **Step 5: Build the three-step template**

Render progress, source/target inbound choices, all/groups controls, preview counts and explicit «Исходные подключения сохранятся». Disable submit with vanilla JS and change copy to «Добавляем…».

- [ ] **Step 6: Run wizard and server tests**

Run: `go test ./internal/webui -run 'TestOnboarding|TestServerNew|TestServerEdit' -count=1`

Expected: PASS.

- [ ] **Step 7: Diff checkpoint**

Run: `git diff --check -- internal/webui`

Expected: no output. Do not commit.

---

### Task 6: Replace the Overloaded Dashboard with the New Shell and Pages

**Files:**
- Modify: `internal/webui/templates/base.html`
- Modify: `internal/webui/templates/dashboard.html`
- Create: `internal/webui/templates/servers.html`
- Modify: `internal/webui/templates/server_form.html`
- Modify: `internal/webui/templates/admin.html`
- Modify: `internal/webui/templates/login.html`
- Modify: `internal/webui/templates/register.html`
- Modify: `internal/webui/webui.go`
- Modify: `internal/webui/dashboard_test.go`

**Interfaces:**
- Consumes: catalog and all prior routes.
- Produces separate Overview/Servers/Clients/Groups navigation and responsive shell.

- [ ] **Step 1: Update rendering tests before templates**

Assert `/dashboard` contains «Всё работает» or «Нужно внимание», metric links and one primary «Добавить сервер»; assert it no longer renders every client mutation form. Assert `/dashboard/servers` and `/dashboard/clients` render their data and correct active nav item.

- [ ] **Step 2: Verify template assertions fail**

Run: `go test ./internal/webui -run 'TestDashboard|TestServersPage|TestClientsPage' -count=1`

Expected: FAIL on missing copy/routes.

- [ ] **Step 3: Implement semantic shell and tokens**

Move inline page-specific CSS out of templates into shared classes in `base.html`. Add skip link, visible `:focus-visible`, labelled account/theme controls, sidebar desktop navigation and wrapping mobile navigation. Keep current pre-render theme bootstrap.

- [ ] **Step 4: Implement focused overview and servers pages**

Overview renders only status summary, metrics, up to three attention/last servers and a small recent-user list. Servers renders complete status cards/rows with accessible status text and «Открыть» actions.

- [ ] **Step 5: Simplify server form and detail hierarchy**

Place `webBasePath`, public host and TLS bypass in `<details>Дополнительные настройки</details>`. Separate connection, inbound and danger-zone sections. Replace icon-only mutation buttons with labelled controls on narrow and desktop layouts.

- [ ] **Step 6: Adapt auth/admin templates to primitives**

Keep handlers unchanged. Replace inline style attributes with shared `.auth-card`, `.page-header`, `.data-list`, `.danger-zone` and form primitives.

- [ ] **Step 7: Run web package tests**

Run: `go test ./internal/webui -count=1`

Expected: PASS.

- [ ] **Step 8: Diff checkpoint**

Run: `git diff --check -- internal/webui`

Expected: no output. Do not commit.

---

### Task 7: Full Regression, Race and Visual Verification

**Files:**
- Modify only files required by failures discovered in this task.
- Update: `README.md` only if route descriptions or onboarding instructions are user-visible and stale.

**Interfaces:**
- Consumes all prior tasks.
- Produces a verified working tree ready for user review.

- [ ] **Step 1: Run formatter**

Run: `gofmt -w internal/storage/storage.go internal/storage/groups_test.go internal/aggregator/bulk_copy.go internal/aggregator/bulk_copy_test.go internal/webui/*.go`

Expected: exit 0.

- [ ] **Step 2: Run all tests**

Run: `go test ./... -count=1`

Expected: PASS for every package.

- [ ] **Step 3: Run race-sensitive packages**

Run: `go test -race ./internal/aggregator ./internal/webui -count=1`

Expected: PASS and no race reports.

- [ ] **Step 4: Run static and diff checks**

Run: `go vet ./...`

Expected: exit 0.

Run: `git diff --check`

Expected: no output.

- [ ] **Step 5: Visually verify desktop and mobile**

Start the app with a temporary SQLite database and fake/test panel fixture, then inspect at widths 1280, 768, 390 and 320 px. Confirm no horizontal page scroll, controls remain at least 40 px high, focus is visible, onboarding preview and partial-result states fit, and theme toggle works without flash.

- [ ] **Step 6: Update documentation and rerun regression**

If README route/user-flow descriptions changed, document `/dashboard/servers`, `/dashboard/clients`, `/dashboard/groups` and the non-destructive onboarding copy. Then rerun `go test ./... -count=1` with expected PASS.

- [ ] **Step 7: Final working-tree checkpoint**

Run: `git status --short && git diff --stat`

Expected: only intentional source, tests and docs plus pre-existing user-owned untracked files. Do not stage or commit unless the user explicitly asks.
