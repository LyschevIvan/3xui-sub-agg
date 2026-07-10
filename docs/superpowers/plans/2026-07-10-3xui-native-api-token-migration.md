# 3x-ui Native API and Token-Only Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Перевести агрегатор на нативное API 3x-ui v3.4.2+ с Bearer-токенами, нативными client/inbound endpoints и штатными subscription links без cookie/password fallback.

**Architecture:** Новый token-only клиент в `internal/xui` разделяет transport, server, clients и inbounds API; агрегатор строит snapshot из paged clients + slim inbounds и получает ссылки через `subLinks`. SQLite хранит токены только в AES-GCM виде, web UI никогда не получает сохранённый секрет, а in-memory link cache обеспечивает singleflight, ограниченный параллелизм и stale-on-error.

**Tech Stack:** Go 1.25, стандартная библиотека `net/http`/`httptest`/`encoding/json`/`sync`, `modernc.org/sqlite`, существующий AES-256-GCM helper, HTML templates.

## Global Constraints

- Поддерживаемая панель: 3x-ui v3.4.2 и новее только в совместимой major-ветке 3; major 4 не принимается без отдельной проверки совместимости.
- Авторизация: только `Authorization: Bearer <token>`; `/login`, cookie jar и password fallback запрещены в итоговом runtime.
- Сохранение или замена непустого API-токена требует настроенного `master_key`; приложение без ключа продолжает запускаться.
- Существующие `username`/`password` колонки остаются в SQLite в этой миграции, но итоговый runtime их не читает и записывает в них пустые строки для новых серверов.
- Существующие клиенты с одинаковым `subId` не объединяются, не переименовываются и не удаляются автоматически.
- Paged clients API не содержит числовой client ID: канонизация использует ID только если он уже известен из detail response, иначе выбирает лексикографически минимальный email.
- Inbound update выполняется через свежий lossless JSON-документ и сохраняет неизвестные top-level и nested поля.
- Мутации автоматически не повторяются; один повтор разрешён только для GET при временной transport-ошибке.
- HTTP redirects панели не follow-ятся, чтобы Bearer-токен не мог уйти на другой origin.
- Кэш ссылок живёт только в памяти процесса; после рестарта он пуст до первой успешной загрузки.
- HTTP и `insecure_skip_verify` остаются разрешёнными, но web UI показывает постоянное предупреждение.
- Новые внешние Go-зависимости не добавляются.
- Каждый production change начинается с падающего теста; после каждой задачи выполняется локальный package test и отдельный commit.
- Перед каждым commit после локального package test обязательно выполняется `go test ./...`; commit создаётся только при полном PASS.

## File Map

**Create:**

- `internal/xui/api_transport.go` — Bearer transport, envelope decoding, retry policy and typed errors.
- `internal/xui/api_server.go` — status/version validation.
- `internal/xui/api_clients.go` — paged clients, detail, CRUD, attach/detach and subLinks.
- `internal/xui/api_inbounds.go` — slim list and lossless inbound documents.
- `internal/xui/api_*_test.go` — official-v3.4.2-shaped contract tests through `httptest`.
- `internal/aggregator/link_cache.go` — per `(server, subId, host)` in-memory stale/singleflight cache.
- `internal/aggregator/fetch.go` — native snapshot fetch/join logic.
- `internal/aggregator/link_cache_test.go`, `internal/aggregator/fetch_test.go` — cache and snapshot tests.
- `internal/webui/server_handlers.go` — token-only server form, validation and safe view model.
- `internal/webui/client_handlers.go` — canonical attach and exact-email detach operations.
- `internal/webui/inbound_handlers.go` — lossless edit/copy/delete operations.
- `internal/webui/test_helpers_test.go`, `server_handlers_test.go`, `client_handlers_test.go`, `inbound_handlers_test.go` — focused handler/domain tests.
- `internal/httpapi/http_test.go` — public subscription behavior with native links.

**Modify:**

- `internal/storage/storage.go` — nullable encrypted `api_token`, per-row credential state and token-only CRUD.
- `internal/secrets/secrets.go` — secret-neutral errors/comments.
- `internal/config/config.go`, `config.example.yaml`, `cmd/aggregator/main.go` — token-oriented `master_key` messaging.
- `internal/aggregator/aggregator.go` — token client cache, new snapshot models and cold subscription lookup.
- `internal/httpapi/http.go` — consume `SubscriptionLinks` instead of locally built `ClientEntry.Link`.
- `internal/webui/webui.go` — remove extracted server/client/inbound handlers and use native snapshot fields.
- `internal/webui/templates/server_form.html`, `dashboard.html` — token UI, panel version, transport warnings and email-free detach form.
- `README.md` — v3.4.2+, token creation/migration and multi-protocol subscription documentation.

**Delete after cutover:**

- `internal/xui/client.go` — username/password login client.
- `internal/xui/parse.go` — legacy JSON-string inbound parsing.
- `internal/link/vless.go` — local VLESS-only link builder.

## Scope Check

Storage, transport, inventory, mutations and UI cannot ship as independent product changes because token-only credentials make the legacy reader unusable and native client identity changes the mutation semantics. They therefore remain one implementation plan, while each task exposes a tested boundary and keeps the repository buildable before the final cutover.

## Requirements Traceability

| Design requirement | Implemented by |
|---|---|
| Bearer-only auth, no redirects/login/fallback, v3.4.2 major-3 gate | Tasks 2, 7, 10 |
| Encrypted token, startup without key, legacy columns untouched | Tasks 1, 6, 10 |
| Paged native clients, detail wire conversion, CRUD/attach/detach | Tasks 3, 8, 9 |
| Slim inventory and lossless inbound get/add/update/delete | Tasks 4, 5, 9 |
| Exact duplicate preservation and deterministic canonical record | Tasks 5, 8 |
| Native multi-protocol subLinks, host override, bounded stale cache | Tasks 3, 5, 7 |
| Partial-panel failure and cold-start subscription lookup | Tasks 5, 7 |
| Safe UI/status/error mapping and transport warnings | Tasks 2, 6, 7 |
| Non-destructive migration, rollback-aware copy and final cleanup | Tasks 8, 9, 10 |
| Contract, storage, race, HTTP and migration verification | Tasks 1-10 |

---

### Task 1: Add encrypted API-token storage without breaking legacy rows

**Files:**

- Modify: `internal/storage/storage.go:19-430`
- Modify: `internal/secrets/secrets.go:1-99`
- Create: `internal/storage/storage_test.go`

**Interfaces:**

- Produces: `storage.ErrMasterKeyRequired`, `storage.ErrPlaintextAPIToken`.
- Produces fields on `storage.Server`: `APIToken string`, `HasAPIToken bool`, `TokenError error`; `APIToken` помечается `json:"-"` и не попадает в renderable view models.
- Produces: `(*Store).CanStoreAPITokens() bool` for UI capability display.
- Keeps existing CRUD signatures for this task so the repository compiles; final token-only SQL cleanup is Task 10.

- [ ] **Step 1: Write failing migration and encryption tests**

Add tests with these assertions (use a `t.TempDir()` database and `secrets.New(key)`):

```go
func TestCreateServerEncryptsAPIToken(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "data.db"), secrets.New("master"))
	if err != nil { t.Fatal(err) }
	defer store.Close()
	user, err := store.CreateUser("owner", "hash", false)
	if err != nil { t.Fatal(err) }

	created, err := store.CreateServer(&Server{
		UserID: user.ID, Name: "node", APIURL: "https://panel", Path: "/",
		APIToken: "token-visible-only-here",
	})
	if err != nil { t.Fatal(err) }
	if !created.HasAPIToken || created.APIToken != "token-visible-only-here" { t.Fatalf("created=%+v", created) }

	var raw sql.NullString
	if err := store.db.QueryRow(`SELECT api_token FROM servers WHERE id=?`, created.ID).Scan(&raw); err != nil { t.Fatal(err) }
	if !raw.Valid || !secrets.IsEncrypted(raw.String) { t.Fatalf("raw token is not encrypted: %q", raw.String) }
	if strings.Contains(raw.String, "token-visible-only-here") { t.Fatal("plaintext token stored") }
}

func TestCreateServerRejectsTokenWithoutMasterKey(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "data.db"), secrets.New(""))
	if err != nil { t.Fatal(err) }
	defer store.Close()
	user, _ := store.CreateUser("owner", "hash", false)
	_, err = store.CreateServer(&Server{UserID: user.ID, Name: "node", APIURL: "https://panel", Path: "/", APIToken: "secret"})
	if !errors.Is(err, ErrMasterKeyRequired) { t.Fatalf("err=%v", err) }
}

func TestMigrateAddsNullableAPITokenToLegacyServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil { t.Fatal(err) }
	_, err = db.Exec(`CREATE TABLE servers (
		id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL, name TEXT NOT NULL,
		api_url TEXT NOT NULL, path TEXT NOT NULL, username TEXT NOT NULL, password TEXT NOT NULL,
		insecure_skip_verify INTEGER NOT NULL DEFAULT 0, host_override TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL
	)`)
	if err != nil { t.Fatal(err) }
	_, _ = db.Exec(`INSERT INTO servers(user_id,name,api_url,path,username,password,created_at) VALUES(1,'old','http://old','/','u','p',1)`)
	_ = db.Close()

	store, err := Open(path, secrets.New("master"))
	if err != nil { t.Fatal(err) }
	defer store.Close()
	var token sql.NullString
	if err := store.db.QueryRow(`SELECT api_token FROM servers WHERE id=1`).Scan(&token); err != nil { t.Fatal(err) }
	if token.Valid { t.Fatalf("legacy token must stay NULL, got %q", token.String) }
}

func TestBadTokenKeyDoesNotHideOtherServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	good, _ := Open(path, secrets.New("right"))
	user, _ := good.CreateUser("owner", "hash", false)
	_, _ = good.CreateServer(&Server{UserID: user.ID, Name: "encrypted", APIURL: "https://one", Path: "/", APIToken: "secret"})
	_, _ = good.CreateServer(&Server{UserID: user.ID, Name: "needs-token", APIURL: "https://two", Path: "/"})
	_ = good.Close()

	wrong, err := Open(path, secrets.New("wrong"))
	if err != nil { t.Fatal(err) }
	defer wrong.Close()
	rows, err := wrong.ListAllServers()
	if err != nil { t.Fatal(err) }
	if len(rows) != 2 || rows[0].TokenError == nil || rows[1].TokenError != nil { t.Fatalf("rows=%+v", rows) }
}
```

Add three storage cases in the same file: opening the legacy database twice remains idempotent; the raw legacy `username`/`password` bytes are identical before and after migration; a manually inserted plaintext `api_token` yields `TokenError` wrapping `ErrPlaintextAPIToken` without returning the plaintext. Add a delete case asserting the server row and ciphertext disappear together.

- [ ] **Step 2: Run the tests and verify the expected failure**

Run: `go test ./internal/storage -run 'Test(CreateServer|Migrate|BadToken)' -v`

Expected: FAIL because `Server.APIToken`, `HasAPIToken`, `TokenError` and token-column migration do not exist.

- [ ] **Step 3: Implement the idempotent column migration and strict token crypto**

Add these exact contracts and preserve the old password fields until Task 10:

```go
var (
	ErrMasterKeyRequired = errors.New("master_key is required to store API tokens")
	ErrPlaintextAPIToken = errors.New("stored API token is not encrypted")
)

type Server struct {
	ID, UserID           int64
	Name, APIURL, Path   string
	Username, Password   string // legacy DB compatibility; removed from runtime in Task 10
	APIToken             string `json:"-"`
	HasAPIToken          bool
	TokenError           error
	InsecureSkipVerify   bool
	HostOverride         string
	CreatedAt            time.Time
}

func (s *Store) encryptAPIToken(token string) (string, error) {
	if token == "" { return "", nil }
	if s.cipher == nil || !s.cipher.Enabled() { return "", ErrMasterKeyRequired }
	return s.cipher.Encrypt(token)
}

func (s *Store) CanStoreAPITokens() bool {
	return s.cipher != nil && s.cipher.Enabled()
}

func (s *Store) decodeAPIToken(stored sql.NullString, srv *Server) {
	srv.HasAPIToken = stored.Valid && stored.String != ""
	if !srv.HasAPIToken { return }
	if !secrets.IsEncrypted(stored.String) {
		srv.TokenError = ErrPlaintextAPIToken
		return
	}
	if s.cipher == nil || !s.cipher.Enabled() {
		srv.TokenError = ErrMasterKeyRequired
		return
	}
	plain, err := s.cipher.Decrypt(stored.String)
	if err != nil { srv.TokenError = fmt.Errorf("decrypt API token: %w", err); return }
	srv.APIToken = plain
}
```

In `migrate`, put `api_token TEXT` in the new-table DDL and run the schema statements plus the column check inside one transaction. Inspect `PRAGMA table_info(servers)` and execute `ALTER TABLE servers ADD COLUMN api_token TEXT` only when the column is absent. Extend `serverCols`, `scanServer`, `CreateServer` and `UpdateServer` with `sql.NullString`; encrypt only a non-empty incoming token. An update that receives the already-decrypted saved token must compare it with the current ciphertext and omit `api_token` from SQL when unchanged, preserving the exact ciphertext. `ListServersByUser` and `ListAllServers` must return rows even when `decodeAPIToken` records a per-row error.

Remove the `Open` call to `encryptLegacyPasswords`: this migration must preserve old username/password bytes for rollback. Until Task 10 removes the obsolete helpers, an unreadable legacy password is cleared only in memory and never makes server listing fail. When `APIToken` is non-empty, new inserts write `username=''` and `password=''`.

Change `internal/secrets/secrets.go` messages from “password” to “secret”; do not change the `enc:v1:` wire format.

- [ ] **Step 4: Run storage tests**

Run: `go test ./internal/storage ./internal/secrets -v`

Expected: PASS; raw `servers.api_token` is `enc:v1:*`, legacy rows contain SQL NULL, and a wrong key affects one returned server only.

- [ ] **Step 5: Commit the storage slice**

```bash
git add internal/storage/storage.go internal/storage/storage_test.go internal/secrets/secrets.go
git commit -m "feat: add encrypted 3x-ui API token storage"
```

### Task 2: Build the Bearer transport and panel-version gate

**Files:**

- Create: `internal/xui/api_transport.go`
- Create: `internal/xui/api_server.go`
- Create: `internal/xui/api_transport_test.go`
- Create: `internal/xui/api_server_test.go`

**Interfaces:**

- Produces: `xui.APIConfig`, `xui.APIClient`, `xui.NewAPI(APIConfig)`.
- Produces: `xui.Error`, `xui.ErrorKind`, `xui.IsKind`.
- Produces: `xui.ServerStatus`, `(*APIClient).Validate(context.Context)`.
- Does not modify the legacy `xui.Client` yet; callers cut over in Tasks 7-9.

- [ ] **Step 1: Write failing transport contract tests**

```go
func TestAPIClientAddsBearerHeadersAndNeverLogsIn(t *testing.T) {
	var paths []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer top-secret" { t.Errorf("Authorization=%q", got) }
		if got := r.Header.Get("X-Requested-With"); got != "XMLHttpRequest" { t.Errorf("X-Requested-With=%q", got) }
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"success":true,"msg":"","obj":{"panelVersion":"v3.4.2"}}`)
	}))
	defer ts.Close()

	c, err := NewAPI(APIConfig{BaseURL: ts.URL, PanelPath: "/secret/", Token: "top-secret", Timeout: time.Second})
	if err != nil { t.Fatal(err) }
	if _, err := c.Validate(context.Background()); err != nil { t.Fatal(err) }
	if want := []string{"/secret/panel/api/server/status"}; !slices.Equal(paths, want) { t.Fatalf("paths=%v want=%v", paths, want) }
	for _, p := range paths { if strings.Contains(p, "/login") { t.Fatalf("unexpected login path %q", p) } }
}

func TestAPIClientClassifiesAndRedactsErrors(t *testing.T) {
	for _, tc := range []struct{ name string; status int; body string; kind ErrorKind }{
		{"unauthorized", http.StatusUnauthorized, "", ErrorUnauthorized},
		{"masked unauthorized", http.StatusNotFound, "", ErrorUnauthorized},
		{"api error", http.StatusOK, `{"success":false,"msg":"bad top-secret"}`, ErrorAPI},
		{"decode", http.StatusOK, `<html>top-secret</html>`, ErrorDecode},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(tc.status); _, _ = io.WriteString(w, tc.body) }))
			defer ts.Close()
			c, _ := NewAPI(APIConfig{BaseURL: ts.URL, PanelPath: "/", Token: "top-secret", Timeout: time.Second})
			_, err := c.Validate(context.Background())
			if !IsKind(err, tc.kind) { t.Fatalf("err=%v", err) }
			if strings.Contains(err.Error(), "top-secret") { t.Fatalf("token leaked: %v", err) }
		})
	}
}
```

Also add a custom `RoundTripper` test proving a temporary GET error is attempted twice while a POST is attempted once.

- [ ] **Step 2: Run the transport tests and verify failure**

Run: `go test ./internal/xui -run 'TestAPIClient' -v`

Expected: FAIL because the token-only transport does not exist.

- [ ] **Step 3: Implement typed errors and the shared API transport**

Use these public contracts:

```go
type ErrorKind string
const (
	ErrorUnauthorized ErrorKind = "unauthorized"
	ErrorUnsupportedVersion ErrorKind = "unsupported_version"
	ErrorAPI ErrorKind = "api"
	ErrorTransport ErrorKind = "transport"
	ErrorDecode ErrorKind = "decode"
)

type Error struct {
	Kind ErrorKind
	Op string
	StatusCode int
	Message string
	Err error
}
func (e *Error) Error() string {
	msg := e.Message
	if msg == "" { msg = string(e.Kind) }
	if e.Op == "" { return msg }
	return e.Op + ": " + msg
}
func (e *Error) Unwrap() error { return e.Err }
func IsKind(err error, kind ErrorKind) bool { var e *Error; return errors.As(err, &e) && e.Kind == kind }

type APIConfig struct {
	BaseURL string
	PanelPath string
	Token string
	InsecureSkipVerify bool
	Timeout time.Duration
	HTTPClient *http.Client // optional test hook; nil builds the production client
}

type APIClient struct { transport *apiTransport }
func NewAPI(cfg APIConfig) (*APIClient, error)
```

`NewAPI` must reject an empty token without echoing it, parse only `http`/`https`, build a base path ending in `/panel/api/`, and configure `CheckRedirect` to return `http.ErrUseLastResponse`. Every request sets Bearer, `Accept: application/json`, `X-Requested-With: XMLHttpRequest`, and JSON content type when a body exists. Implement the generic helper as a package function because Go does not allow generic methods:

```go
type apiEnvelope[T any] struct { Success *bool `json:"success"`; Msg string `json:"msg"`; Obj T `json:"obj"` }
func doAPI[T any](ctx context.Context, t *apiTransport, method, rel string, body any, host string) (T, error)
```

Map 401 and 404 to `ErrorUnauthorized`; a missing `success` field is `ErrorDecode`; map HTTP 200 with `success:false` to `ErrorAPI`; replace every exact token occurrence in decoded messages and body snippets with `[REDACTED]`. Retry only a GET whose wrapped `net.Error` is temporary or timed out, with no sleep and at most two total attempts.

Add a redirect test whose fake status endpoint returns 302 to a second server; assert the second server receives zero requests and therefore never sees Authorization.

- [ ] **Step 4: Implement and test the version gate**

```go
const MinimumPanelVersion = "3.4.2"
type ServerStatus struct { PanelVersion string `json:"panelVersion"` }
type ServerAPI interface { Validate(context.Context) (ServerStatus, error) }

func (c *APIClient) Validate(ctx context.Context) (ServerStatus, error) {
	status, err := doAPI[ServerStatus](ctx, c.transport, http.MethodGet, "server/status", nil, "")
	if err != nil { return ServerStatus{}, err }
	if versionLess(status.PanelVersion, MinimumPanelVersion) {
		return ServerStatus{}, &Error{Kind: ErrorUnsupportedVersion, Op: "server status", Message: "3x-ui " + status.PanelVersion + " is older than " + MinimumPanelVersion}
	}
	return status, nil
}
```

`versionLess` must parse an optional `v`, require exactly numeric major/minor/patch components, require `major == 3`, and compare `(minor, patch)` against `(4, 2)`. Empty, malformed and other-major versions return `ErrorUnsupportedVersion` rather than being treated as “newer”.

Test `v3.4.2`, `3.4.10` and `v3.10.0` as accepted; test `v3.4.1`, `v2.9.9`, `v4.0.0`, an empty value and malformed text as `ErrorUnsupportedVersion`.

- [ ] **Step 5: Run transport and server tests**

Run: `go test ./internal/xui -run 'Test(APIClient|Validate|Version)' -v`

Expected: PASS with exactly one status request in the normal case and no `/login` request.

- [ ] **Step 6: Commit the transport slice**

```bash
git add internal/xui/api_transport.go internal/xui/api_transport_test.go internal/xui/api_server.go internal/xui/api_server_test.go
git commit -m "feat: add token-only 3x-ui API transport"
```

### Task 3: Implement the native clients API

**Files:**

- Create: `internal/xui/api_clients.go`
- Create: `internal/xui/api_clients_test.go`

**Interfaces:**

- Consumes: `APIClient`, `doAPI` and typed errors from Task 2.
- Produces: `ClientSummary`, `ClientPage`, `ClientPayload`, `ClientDetail`, `ClientAPI`.
- Produces methods: `ListClients`, `GetClient`, `AddClient`, `UpdateClient`, `DeleteClient`, `AttachClient`, `DetachClient`, `SubLinks`.

- [ ] **Step 1: Write failing paged-list and endpoint tests**

Use a single `httptest.Server` that records method, path, query, Host and decoded body. The first test must return two official-shaped pages:

```go
func newAPIServer(t *testing.T, handler func(http.ResponseWriter, *http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(handler))
}

func mustAPI(t *testing.T, baseURL string) *APIClient {
	t.Helper()
	c, err := NewAPI(APIConfig{BaseURL: baseURL, PanelPath: "/", Token: "test-token", Timeout: time.Second})
	if err != nil { t.Fatal(err) }
	return c
}

func TestListClientsReadsEveryPage(t *testing.T) {
	requests := 0
	ts := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if r.URL.Query().Get("pageSize") != "200" { t.Errorf("pageSize=%q", r.URL.Query().Get("pageSize")) }
		items := `[{"email":"a","subId":"sub","enable":true,"inboundIds":[1]}]`
		if page == 2 { items = `[{"email":"b","subId":"sub","enable":true,"inboundIds":[2]}]` }
		fmt.Fprintf(w, `{"success":true,"obj":{"items":%s,"total":2,"filtered":2,"page":%d,"pageSize":1}}`, items, page)
	})
	defer ts.Close()
	c := mustAPI(t, ts.URL)
	rows, err := c.ListClients(context.Background())
	if err != nil { t.Fatal(err) }
	if requests != 2 || len(rows) != 2 || rows[1].Email != "b" { t.Fatalf("requests=%d rows=%+v", requests, rows) }
}
```

Add table-driven cases with exact endpoints and bodies:

```text
GET  /panel/api/clients/get/{url.PathEscape(email)} -> ClientDetail
POST /panel/api/clients/add                     -> {client:{...},inboundIds:[1,2]}
POST /panel/api/clients/update/{url.PathEscape(email)} -> full ClientPayload
POST /panel/api/clients/del/{url.PathEscape(email)}    -> no body
POST /panel/api/clients/{url.PathEscape(email)}/attach -> {inboundIds:[2]}
POST /panel/api/clients/{url.PathEscape(email)}/detach -> {inboundIds:[1]}
GET  /panel/api/clients/subLinks/sub-id         -> Host override and []string response
```

Assert POST transport errors are never retried.

- [ ] **Step 2: Run clients tests and verify failure**

Run: `go test ./internal/xui -run 'Test(ListClients|ClientEndpoints|SubLinks)' -v`

Expected: FAIL because the client DTOs and methods do not exist.

- [ ] **Step 3: Add exact v3.4.2 DTOs and all client methods**

```go
type ClientSummary struct {
	RecordID *int `json:"id,omitempty"`
	Email string `json:"email"`
	SubID string `json:"subId"`
	Enable bool `json:"enable"`
	TotalGB int64 `json:"totalGB"`
	ExpiryTime int64 `json:"expiryTime"`
	LimitIP int `json:"limitIp"`
	Reset int `json:"reset"`
	Group string `json:"group,omitempty"`
	Comment string `json:"comment,omitempty"`
	InboundIDs []int `json:"inboundIds"`
	CreatedAt int64 `json:"createdAt"`
	UpdatedAt int64 `json:"updatedAt"`
}

type ClientPage struct {
	Items []ClientSummary `json:"items"`
	Total int `json:"total"`
	Filtered int `json:"filtered"`
	Page int `json:"page"`
	PageSize int `json:"pageSize"`
}

type ClientReverse struct { Tag string `json:"tag"` }
type ClientPayload struct {
	ID string `json:"id,omitempty"`
	Security string `json:"security"`
	Password string `json:"password,omitempty"`
	Flow string `json:"flow,omitempty"`
	Reverse *ClientReverse `json:"reverse,omitempty"`
	Auth string `json:"auth,omitempty"`
	PrivateKey string `json:"privateKey,omitempty"`
	PublicKey string `json:"publicKey,omitempty"`
	AllowedIPs []string `json:"allowedIPs,omitempty"`
	PreSharedKey string `json:"preSharedKey,omitempty"`
	KeepAlive int `json:"keepAlive,omitempty"`
	Email string `json:"email"`
	LimitIP int `json:"limitIp"`
	TotalGB int64 `json:"totalGB"`
	ExpiryTime int64 `json:"expiryTime"`
	Enable bool `json:"enable"`
	TgID int64 `json:"tgId"`
	SubID string `json:"subId"`
	Group string `json:"group,omitempty"`
	Comment string `json:"comment"`
	Reset int `json:"reset"`
	CreatedAt int64 `json:"created_at,omitempty"`
	UpdatedAt int64 `json:"updated_at,omitempty"`
}

type ClientRecord struct {
	RecordID int `json:"id"`
	Email string `json:"email"`
	SubID string `json:"subId"`
	UUID string `json:"uuid"`
	Password string `json:"password"`
	Auth string `json:"auth"`
	Flow string `json:"flow"`
	Security string `json:"security"`
	Reverse json.RawMessage `json:"reverse"`
	PrivateKey string `json:"privateKey"`
	PublicKey string `json:"publicKey"`
	AllowedIPs string `json:"allowedIPs"`
	PreSharedKey string `json:"preSharedKey"`
	KeepAlive int `json:"keepAlive"`
	LimitIP int `json:"limitIp"`
	TotalGB int64 `json:"totalGB"`
	ExpiryTime int64 `json:"expiryTime"`
	Enable bool `json:"enable"`
	TgID int64 `json:"tgId"`
	Group string `json:"group"`
	Comment string `json:"comment"`
	Reset int `json:"reset"`
	CreatedAt int64 `json:"createdAt"`
	UpdatedAt int64 `json:"updatedAt"`
}

type ClientDetail struct {
	Client ClientRecord `json:"client"`
	InboundIDs []int `json:"inboundIds"`
	ExternalLinks []json.RawMessage `json:"externalLinks"`
	UsedTraffic int64 `json:"usedTraffic"`
}

func (r ClientRecord) Payload() (ClientPayload, error)

type ClientAPI interface {
	ListClients(context.Context) ([]ClientSummary, error)
	GetClient(context.Context, string) (ClientDetail, error)
	AddClient(context.Context, ClientPayload, []int) error
	UpdateClient(context.Context, string, ClientPayload) error
	DeleteClient(context.Context, string) error
	AttachClient(context.Context, string, []int) error
	DetachClient(context.Context, string, []int) error
	SubLinks(context.Context, string, string) ([]string, error)
}
```

`ListClients` uses `pageSize=200`, increments pages, and stops when `len(all) >= Filtered`, a page is empty, or a page adds no new email. All email/subId path parameters use `url.PathEscape`. `ClientRecord.Payload` maps numeric `RecordID` nowhere, maps `UUID` to payload `ID`, splits comma-separated `AllowedIPs`, converts `Reverse` to `*ClientReverse`, and uses the mutation timestamp tags. `SubLinks` passes the supplied effective host to `doAPI`; `doAPI` assigns it to `req.Host` rather than adding a forwarded-host header.

- [ ] **Step 4: Run clients API tests**

Run: `go test ./internal/xui -run 'Test(ListClients|ClientEndpoints|SubLinks)' -v`

Expected: PASS; recorded paths, bodies and Host match the table exactly.

- [ ] **Step 5: Commit the client API slice**

```bash
git add internal/xui/api_clients.go internal/xui/api_clients_test.go
git commit -m "feat: add native 3x-ui clients API"
```

### Task 4: Implement lossless native inbound API

**Files:**

- Create: `internal/xui/api_inbounds.go`
- Create: `internal/xui/api_inbounds_test.go`

**Interfaces:**

- Consumes: `APIClient` and `doAPI` from Task 2.
- Produces: `InboundSummary`, `InboundDocument`, `InboundAPI`.
- Produces lossless accessors: `Clone`, `Int`, `String`, `Bool`, `Set`, `Delete`, `Clients`, `SetClients`.

- [ ] **Step 1: Write failing nested/lossless JSON tests**

```go
func TestInboundDocumentPreservesUnknownFields(t *testing.T) {
	raw := []byte(`{"id":7,"remark":"old","port":443,"enable":true,"future":{"x":1},"settings":{"clients":[]},"streamSettings":{"network":"tcp","security":"reality","futureNested":9},"sniffing":{"enabled":true}}`)
	var doc InboundDocument
	if err := json.Unmarshal(raw, &doc); err != nil { t.Fatal(err) }
	if err := doc.Set("remark", "new"); err != nil { t.Fatal(err) }
	if err := doc.Set("port", 8443); err != nil { t.Fatal(err) }
	encoded, err := json.Marshal(doc)
	if err != nil { t.Fatal(err) }
	for _, want := range []string{`"future":{"x":1}`, `"futureNested":9`, `"settings":{"clients":[]}`, `"remark":"new"`} {
		if !bytes.Contains(encoded, []byte(want)) { t.Fatalf("missing %s in %s", want, encoded) }
	}
}

func TestInboundEndpointsUseSlimGetAndReturnCreatedObject(t *testing.T) {
	var calls []string
	ts := newAPIServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		obj := `null`
		switch r.URL.Path {
		case "/panel/api/inbounds/list/slim": obj = `[]`
		case "/panel/api/inbounds/get/7": obj = `{"id":7,"settings":{},"streamSettings":{},"sniffing":{}}`
		case "/panel/api/inbounds/add": obj = `{"id":9,"settings":{},"streamSettings":{},"sniffing":{}}`
		case "/panel/api/inbounds/update/7": obj = `{"id":7,"settings":{},"streamSettings":{},"sniffing":{}}`
		}
		fmt.Fprintf(w, `{"success":true,"obj":%s}`, obj)
	})
	defer ts.Close()
	c := mustAPI(t, ts.URL)
	ctx := context.Background()
	_, _ = c.ListSlimInbounds(ctx)
	doc, _ := c.GetInbound(ctx, 7)
	created, _ := c.AddInbound(ctx, doc)
	_, _ = c.UpdateInbound(ctx, 7, doc)
	_ = c.DeleteInbound(ctx, 7)
	if id, _ := created.Int("id"); id != 9 { t.Fatalf("created id=%d", id) }
	want := []string{"GET /panel/api/inbounds/list/slim", "GET /panel/api/inbounds/get/7", "POST /panel/api/inbounds/add", "POST /panel/api/inbounds/update/7", "POST /panel/api/inbounds/del/7"}
	if !slices.Equal(calls, want) { t.Fatalf("calls=%v want=%v", calls, want) }
}
```

Add a fixture where `settings`, `streamSettings` and `sniffing` are legacy JSON strings; normalize them into `json.RawMessage` objects while retaining valid content.

- [ ] **Step 2: Run inbound tests and verify failure**

Run: `go test ./internal/xui -run 'TestInbound' -v`

Expected: FAIL because `InboundDocument` and native endpoints do not exist.

- [ ] **Step 3: Implement the lossless document and API**

```go
type InboundDocument map[string]json.RawMessage

func (d *InboundDocument) UnmarshalJSON(data []byte) error
func (d InboundDocument) MarshalJSON() ([]byte, error)
func (d InboundDocument) Clone() InboundDocument
func (d InboundDocument) Int(key string) (int, error)
func (d InboundDocument) String(key string) (string, error)
func (d InboundDocument) Bool(key string) (bool, error)
func (d InboundDocument) Set(key string, value any) error
func (d InboundDocument) Delete(key string) { delete(d, key) }
func (d InboundDocument) Clients() ([]ClientPayload, error)
func (d InboundDocument) SetClients(clients []ClientPayload) error

type InboundSummary struct {
	ID int `json:"id"`
	Remark string `json:"remark"`
	Enable bool `json:"enable"`
	Port int `json:"port"`
	Protocol string `json:"protocol"`
	StreamSettings json.RawMessage `json:"streamSettings"`
}

func (i InboundSummary) NetworkSecurity() (network, security string) {
	var stream struct { Network string `json:"network"`; Security string `json:"security"` }
	_ = json.Unmarshal(normalizeRawJSON(i.StreamSettings), &stream)
	if stream.Network == "" { stream.Network = "tcp" }
	if stream.Security == "" { stream.Security = "none" }
	return stream.Network, stream.Security
}

type InboundAPI interface {
	ListSlimInbounds(context.Context) ([]InboundSummary, error)
	GetInbound(context.Context, int) (InboundDocument, error)
	AddInbound(context.Context, InboundDocument) (InboundDocument, error)
	UpdateInbound(context.Context, int, InboundDocument) (InboundDocument, error)
	DeleteInbound(context.Context, int) error
}

type PanelAPI interface { ServerAPI; ClientAPI; InboundAPI }
```

`UnmarshalJSON` first decodes a `map[string]json.RawMessage`, then runs `normalizeRawJSON` for exactly `settings`, `streamSettings` and `sniffing`; `MarshalJSON` emits the map without re-encoding valid nested values as strings. `normalizeRawJSON` accepts an object/array/null or a JSON-encoded string and returns nested JSON bytes when the decoded string is valid JSON. An invalid legacy string stays byte-semantically preserved as a string and causes only the relevant typed accessor to fail. `Clients` reads `settings.clients`; `SetClients` changes only that nested key and preserves every other settings key. Add and update return the API response object so copy code can obtain the new ID.

- [ ] **Step 4: Run all xui contract tests**

Run: `go test ./internal/xui -v`

Expected: PASS; nested objects remain objects and unknown fields survive update round-trips.

- [ ] **Step 5: Commit the inbound API slice**

```bash
git add internal/xui/api_inbounds.go internal/xui/api_inbounds_test.go
git commit -m "feat: add lossless native inbound API"
```

### Task 5: Add native grouping, bounded link cache and snapshot fetcher

**Files:**

- Create: `internal/aggregator/groups.go`
- Create: `internal/aggregator/groups_test.go`
- Create: `internal/aggregator/link_cache.go`
- Create: `internal/aggregator/link_cache_test.go`
- Create: `internal/aggregator/fetch.go`
- Create: `internal/aggregator/fetch_test.go`
- Modify: `internal/aggregator/aggregator.go:19-108` (types only; runtime wiring remains Task 7)

**Interfaces:**

- Consumes: `xui.ServerAPI`, `xui.ClientAPI`, `xui.InboundAPI` from Tasks 2-4.
- Produces: `ClientRef`, `ClientGroup`, `InboundInfo`, `ServerState`, revised `ServerSnapshot`.
- Produces: `linkCache.Get`, `GetOrFetch`, `Refresh`, `PruneServer`.
- Produces: `nativeFetcher.Fetch(context.Context, storage.Server, fetchAPI)`.

- [ ] **Step 1: Write failing grouping tests**

```go
func TestGroupClientsPreservesDuplicateRecords(t *testing.T) {
	rows := []xui.ClientSummary{
		{Email: "z@example", SubID: "same", Enable: true, InboundIDs: []int{2, 1, 1}},
		{Email: "a@example", SubID: "same", Enable: false, InboundIDs: []int{1}},
	}
	groups := groupClients(rows)
	g := groups["same"]
	if len(g.Records) != 2 { t.Fatalf("records=%+v", g.Records) }
	if !slices.Equal(g.Records[0].InboundIDs, []int{1}) { t.Fatalf("ids=%v", g.Records[0].InboundIDs) }
	if !slices.Equal(g.Records[1].InboundIDs, []int{1, 2}) { t.Fatalf("ids=%v", g.Records[1].InboundIDs) }
}

func TestCanonicalClientUsesIDThenEmail(t *testing.T) {
	id2, id7 := 2, 7
	for _, tc := range []struct{ name string; rows []ClientRef; email string }{
		{"lowest id", []ClientRef{{RecordID:&id7, Email:"a"}, {RecordID:&id2, Email:"z"}}, "z"},
		{"email fallback", []ClientRef{{Email:"z"}, {Email:"a"}}, "a"},
		{"id tie", []ClientRef{{RecordID:&id2, Email:"z"}, {RecordID:&id2, Email:"a"}}, "a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := canonicalClient(tc.rows)
			if !ok || got.Email != tc.email { t.Fatalf("got=%+v ok=%v", got, ok) }
		})
	}
}
```

- [ ] **Step 2: Run grouping tests and verify failure**

Run: `go test ./internal/aggregator -run 'Test(GroupClients|CanonicalClient)' -v`

Expected: FAIL because native client groups do not exist.

- [ ] **Step 3: Implement immutable group/snapshot models**

```go
type ClientRef struct {
	RecordID *int
	Email string
	SubID string
	Enabled bool
	InboundIDs []int
}

type ClientGroup struct {
	SubID string
	Records []ClientRef
}

type InboundInfo struct {
	ID int
	Remark string
	Port int
	Protocol string
	Network string
	Security string
	Enable bool
}

type ServerState string
const (
	ServerOK ServerState = "ok"
	ServerTokenRequired ServerState = "token_required"
	ServerTokenRejected ServerState = "token_rejected"
	ServerUnsupportedVersion ServerState = "unsupported_version"
	ServerUnavailable ServerState = "panel_unavailable"
	ServerConfigurationError ServerState = "configuration_error"
	ServerDegraded ServerState = "degraded"
)

type ServerSnapshot struct {
	ID int64
	UserID int64
	Name string
	PublicHost string
	PanelVersion string
	Inbounds []InboundInfo
	Groups map[string]ClientGroup
	Entries []ClientEntry // transitional read compatibility; removed in Task 7
	State ServerState
	FetchedAt time.Time
	AttemptedAt time.Time
	SyncErr error // internal only; web UI maps State and never renders Error()
	Err error // transitional template compatibility; removed with safe state rendering in Task 7
}
```

`groupClients` must compare `SubID` exactly, discard only empty `SubID`, sort/deduplicate inbound IDs, preserve disabled and orphan records, and sort records by email. Never merge records into one client. A group is active for subscription-link fetching only when at least one enabled exact record is attached to an enabled inbound; disabled/orphan membership remains in `Groups` for safe UI/candidate decisions but does not trigger subLinks.

Task 5 adds `Groups`/state beside the old `Entries`/`Err` fields so every intermediate commit compiles. Task 7 switches all readers and removes the two transitional fields.

- [ ] **Step 4: Write failing cache concurrency/stale tests**

```go
func TestLinkCacheCoalescesAndPreservesStale(t *testing.T) {
	cache := newLinkCache(2)
	key := linkKey{ServerID: 1, SubID: "sub", EffectiveHost: "public.example:443"}
	var calls atomic.Int32
	fetch := func(context.Context) ([]string, error) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		return []string{"vless://b", "vless://a", "vless://a", ""}, nil
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() { defer wg.Done(); value, err := cache.GetOrFetch(context.Background(), key, fetch); if err != nil || len(value.Links) != 2 { t.Errorf("value=%+v err=%v", value, err) } }()
	}
	wg.Wait()
	if calls.Load() != 1 { t.Fatalf("calls=%d", calls.Load()) }

	value, err := cache.Refresh(context.Background(), key, func(context.Context) ([]string, error) { return nil, errors.New("offline") })
	if err == nil || strings.Join(value.Links, ",") != "vless://a,vless://b" { t.Fatalf("value=%+v err=%v", value, err) }
}

```

In the same test file add four table/coordination tests with exact assertions: a successful empty `Refresh` makes the next `Get` return `len(Links)==0`; ten distinct blocked fetches with cache limit two never make an atomic `maxRunning` exceed two; two keys differing only by `EffectiveHost` invoke the fetcher twice; `PruneServer(1, keep)` removes only omitted keys for server 1 and leaves every server-2 key intact.

- [ ] **Step 5: Implement cache with one global slot limit**

```go
type linkKey struct { ServerID int64; SubID, EffectiveHost string }
type linkValue struct { Links []string; FetchedAt time.Time }
type linkFlight struct { done chan struct{}; value linkValue; err error }
type linkCache struct {
	mu sync.RWMutex
	values map[linkKey]linkValue
	flights map[linkKey]*linkFlight
	slots chan struct{}
}

type fetchLinks func(context.Context) ([]string, error)
func newLinkCache(limit int) *linkCache
func (c *linkCache) Get(key linkKey) (linkValue, bool)
func (c *linkCache) GetOrFetch(ctx context.Context, key linkKey, fetch fetchLinks) (linkValue, error)
func (c *linkCache) Refresh(ctx context.Context, key linkKey, fetch fetchLinks) (linkValue, error)
func (c *linkCache) PruneServer(serverID int64, keep map[linkKey]struct{})
```

The flight leader acquires `slots`, waiting callers select on `flight.done` and their own context. Normalize successful links by trimming empty strings, sorting, and removing exact duplicates. A failed refresh returns the prior value plus the new error without overwriting it; a successful empty result replaces stale data.

- [ ] **Step 6: Write and implement the native fetcher tests**

Define a narrow fakeable interface:

```go
type fetchAPI interface {
	Validate(context.Context) (xui.ServerStatus, error)
	ListClients(context.Context) ([]xui.ClientSummary, error)
	ListSlimInbounds(context.Context) ([]xui.InboundSummary, error)
	SubLinks(context.Context, string, string) ([]string, error)
}
```

Test these exact behaviors:

- `TestNativeFetcherJoinsClientMembershipsToSlimInbounds` — keeps all protocols and every exact record.
- `TestNativeFetcherFetchesOneSubLinksPerActiveSubID` — duplicate records produce one upstream call.
- `TestNativeFetcherKeepsOtherLinksWhenOneSubIDFails` — state is `degraded`, successful groups remain.
- `TestNativeFetcherUsesStaleLinkOnRefreshError` — stale value remains in result.
- `TestNativeFetcherDoesNotPruneAfterInventoryFailure` — no cache deletion after client/inbound failure.
- `TestNativeFetcherPrunesRemovedGroupsAfterCompleteInventory` — obsolete keys for this server are removed.

Implement:

```go
type nativeFetcher struct { links *linkCache; workers int }
func (f *nativeFetcher) Fetch(ctx context.Context, srv storage.Server, api fetchAPI, effectiveHost string) (ServerSnapshot, error)
```

Discovery errors return a non-nil error so the caller can retain the previous whole snapshot. SubId work is sent through a fixed `workers`-sized job channel rather than spawning one goroutine per client. Per-subId link errors are joined into `SyncErr`, set `State=ServerDegraded`, and do not discard other results.

- [ ] **Step 7: Run aggregator unit/race tests**

Run: `go test -race ./internal/aggregator -run 'Test(GroupClients|CanonicalClient|LinkCache|NativeFetcher)' -v`

Expected: PASS; the concurrent-miss test reports one fetch and the max-concurrency test never exceeds the configured limit.

- [ ] **Step 8: Commit the snapshot foundation**

```bash
git add internal/aggregator/aggregator.go internal/aggregator/groups.go internal/aggregator/groups_test.go internal/aggregator/link_cache.go internal/aggregator/link_cache_test.go internal/aggregator/fetch.go internal/aggregator/fetch_test.go
git commit -m "feat: add native client snapshots and link cache"
```

### Task 6: Replace the server form with safe token validation and persistence

**Files:**

- Create: `internal/webui/server_handlers.go`
- Create: `internal/webui/test_helpers_test.go`
- Create: `internal/webui/server_handlers_test.go`
- Modify: `internal/webui/webui.go:30-63,83-103,1059-1277`
- Modify: `internal/webui/templates/server_form.html:1-133`
- Modify: `internal/webui/templates/base.html` (warning style only)
- Modify: `internal/config/config.go:26-30`
- Modify: `config.example.yaml:23-28`
- Modify: `cmd/aggregator/main.go:33-36`

**Interfaces:**

- Consumes: encrypted token fields from Task 1 and `xui.APIClient.Validate` from Task 2.
- Produces: `serverConnectionChecker`, sanitized `serverFormData`, `parseServerForm` with a separate token return.
- Produces route: authenticated/CSRF-protected `POST /dashboard/servers/check`.

- [ ] **Step 1: Write failing parser/rendering tests**

```go
func TestParseServerFormReturnsTokenSeparately(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(url.Values{
		"name":{"node"}, "api_url":{"https://panel.example"}, "path":{"/admin/"}, "api_token":{"submitted-secret"},
	}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	form, token, err := parseServerForm(r)
	if err != nil { t.Fatal(err) }
	if token != "submitted-secret" || form.Name != "node" { t.Fatalf("form=%+v token=%q", form, token) }
	if strings.Contains(fmt.Sprintf("%+v", form), token) { t.Fatal("renderable form contains token") }
}

```

Add `TestServerFormNeverRendersStoredOrSubmittedToken`: render both GET edit and a validation-error POST through the mounted mux, require the text “токен сохранён”, and assert the bodies contain neither secret value nor `name="username"`/`name="password"`. Add table cases rejecting non-HTTP(S) schemes and URLs without a host.

- [ ] **Step 2: Run parser/template tests and verify failure**

Run: `go test ./internal/webui -run 'Test(ParseServerForm|ServerForm)' -v`

Expected: FAIL because the existing form requires username/password and puts username in its view model.

- [ ] **Step 3: Extract the server handlers and define safe view/check contracts**

```go
type serverConnectionChecker interface {
	Check(context.Context, storage.Server, string) (xui.ServerStatus, error)
}

type xuiConnectionChecker struct { timeout time.Duration }
func (c xuiConnectionChecker) Check(ctx context.Context, srv storage.Server, token string) (xui.ServerStatus, error) {
	client, err := xui.NewAPI(xui.APIConfig{
		BaseURL: srv.APIURL, PanelPath: srv.Path, Token: token,
		InsecureSkipVerify: srv.InsecureSkipVerify, Timeout: c.timeout,
	})
	if err != nil { return xui.ServerStatus{}, err }
	return client.Validate(ctx)
}

type serverFormData struct {
	ID int64
	Name, APIURL, Path, HostOverride string
	InsecureSkipVerify bool
	HasAPIToken bool
	CanStoreAPIToken bool
	UsesHTTP bool
	PanelVersion string
	Inbounds []serverEditInbound
	OtherServers []serverOption
	InboundsErr string
}

func parseServerForm(r *http.Request) (serverFormData, string, error)
```

Add `checker serverConnectionChecker` to `Handler`; production `New` supplies `xuiConnectionChecker{timeout: cfg.RequestTimeout}`. Tests replace it with a fake. The submitted token exists only as the second return value/local variable and is never copied into `pageData`.

- [ ] **Step 4: Write failing check/create/edit flow tests**

Through the mounted mux with a real session and CSRF cookie, test:

- check with a submitted token succeeds without `master_key` and returns only `ok/panel_version`;
- edit-check with blank token uses the stored token after ownership verification;
- a foreign server ID is 404;
- unauthorized, unsupported version and transport failures map to fixed safe messages;
- a token string embedded in a fake error never appears in HTML/JSON;
- create validates first, stores second, and inserts nothing on validation failure;
- create/save without `master_key` inserts nothing;
- edit with blank token and unchanged API URL/path/TLS settings retains the exact existing ciphertext;
- changing API URL, panel path or TLS verification validates the saved token against the candidate connection before saving;
- failed replacement preserves metadata and ciphertext;
- successful replacement triggers the aggregator.

Use this JSON shape only:

```go
type connectionCheckResponse struct {
	OK bool `json:"ok"`
	PanelVersion string `json:"panel_version,omitempty"`
	Code string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}
```

- [ ] **Step 5: Implement check-before-save server flows**

Mount `/dashboard/servers/check` through both `RequireUser` and the existing CSRF validation. For create, require a non-empty token, run `checker.Check`, then call storage. For edit, a blank input means keep the stored token. Changes limited to name/host override do not require the panel to be reachable; changing API URL, panel path or TLS verification validates the saved token against the candidate connection, and a non-empty replacement always validates before any DB update. Never repopulate the input after an error.

Template requirements:

```html
<label>API-токен {{if .Form.ID}}(оставьте пустым, чтобы не менять){{end}}</label>
<input type="password" name="api_token" autocomplete="new-password" {{if not .Form.ID}}required{{end}}>
{{if .Form.HasAPIToken}}<span class="badge ok">токен сохранён</span>{{end}}
<p class="muted">Создайте отдельный именованный токен в настройках 3x-ui. Токен имеет административные права.</p>
{{if .Form.UsesHTTP}}<p class="warning">Токен передаётся по незашифрованному HTTP.</p>{{end}}
{{if .Form.InsecureSkipVerify}}<p class="warning">Проверка TLS-сертификата отключена.</p>{{end}}
```

Give the form `id="server-form"`, include hidden `server_id` in edit mode, and add a `type="button"` “Проверить” control. Its script sends `new FormData(form)` to `/dashboard/servers/check`, keeps credentials same-origin, and writes only `panel_version` or the fixed safe `message` via `textContent`:

```html
<button type="button" id="check-connection" class="secondary">Проверить подключение</button>
<span id="check-result" class="muted" aria-live="polite"></span>
<script>
document.getElementById('check-connection').addEventListener('click', async function () {
  const result = document.getElementById('check-result');
  try {
    const response = await fetch('/dashboard/servers/check', {
      method: 'POST', body: new FormData(document.getElementById('server-form')),
      credentials: 'same-origin', headers: {'Accept': 'application/json'}
    });
    const data = await response.json();
    result.textContent = data.ok ? '3x-ui ' + data.panel_version : data.message;
  } catch (_) {
    result.textContent = 'Не удалось проверить подключение';
  }
});
</script>
```

- [ ] **Step 6: Update startup/config wording without making the key startup-fatal**

`config.Load` still accepts an empty `master_key`. `main` logs a warning (“API tokens cannot be saved until master_key is configured”) instead of exiting. `config.example.yaml` explains that existing servers remain visible without the key, while new/replacement tokens cannot be persisted.

- [ ] **Step 7: Run web/storage tests**

Run: `go test -race ./internal/storage ./internal/webui -run 'Test.*(Token|Server|ParseServerForm)' -v`

Expected: PASS; no response body contains either a stored or submitted token.

- [ ] **Step 8: Commit the token UI slice**

```bash
git add internal/webui/server_handlers.go internal/webui/server_handlers_test.go internal/webui/test_helpers_test.go internal/webui/webui.go internal/webui/templates/server_form.html internal/webui/templates/base.html internal/config/config.go config.example.yaml cmd/aggregator/main.go
git commit -m "feat: add token-only 3x-ui server setup"
```

### Task 7: Wire native inventory and subscription resolution into runtime

**Files:**

- Modify: `internal/aggregator/aggregator.go:94-350`
- Create: `internal/aggregator/subscription.go`
- Create: `internal/aggregator/subscription_test.go`
- Modify: `internal/httpapi/http.go:15-97`
- Create: `internal/httpapi/http_test.go`
- Modify: `internal/webui/webui.go:356-610`
- Modify: `internal/webui/templates/dashboard.html:10-113`
- Delete: `internal/link/vless.go`

**Interfaces:**

- Consumes: token storage, `xui.NewAPI`, native fetcher and link cache.
- Produces: `Aggregator.ResolveSubscription(context.Context, userID, subID)`.
- Produces: safe `ServerState` rendering; raw `SyncErr.Error()` is never passed to templates.

- [ ] **Step 1: Write failing native refresh tests**

Use an injected factory:

```go
type panelFactory func(storage.Server) (xui.PanelAPI, error)
```

Test:

- every connection is built from `APIToken`, path and TLS settings, never username/password;
- a missing token creates `ServerTokenRequired` without calling the factory;
- a token decrypt error creates `ServerConfigurationError` without blocking other panels;
- unauthorized/version/transport errors map to `ServerTokenRejected`, `ServerUnsupportedVersion`, `ServerUnavailable`;
- one panel failure retains only that panel’s previous successful inventory;
- endpoint/path/token/host changes purge that server’s link-cache keys;
- refresh no longer calls local VLESS parsing/building.

- [ ] **Step 2: Implement token client caching and native refresh wiring**

```go
type serverClient struct {
	srv storage.Server
	api xui.PanelAPI
	host string
}

func sameConnection(a, b storage.Server) bool {
	return a.APIURL == b.APIURL && a.Path == b.Path &&
		a.APIToken == b.APIToken && a.InsecureSkipVerify == b.InsecureSkipVerify &&
		a.HostOverride == b.HostOverride
}
```

`clientFor` returns a typed missing/configuration error before constructing an API client. `refresh` uses `nativeFetcher.Fetch`; only inventory failure calls whole-server fallback. Store `AttemptedAt` on every attempt and preserve `FetchedAt` from the last successful inventory. Remove the import/call to `internal/link`.

Effective Host keeps an explicitly configured port: parse URL-form overrides with `u.Host`, not `u.Hostname`; a plain override is trimmed but otherwise preserved.

- [ ] **Step 3: Write failing resolver/HTTP tests**

```go
var (
	ErrSubscriptionNotFound = errors.New("subscription not found")
	ErrSubscriptionUnavailable = errors.New("subscription temporarily unavailable")
)

type SubscriptionResult struct { Links []string; Partial bool }
func (a *Aggregator) ResolveSubscription(ctx context.Context, userID int64, subID string) (SubscriptionResult, error)
```

Test cached lookup makes zero API calls; a cold miss fetches panels whose complete inventory contains the exact group; a request arriving before initial inventory performs bounded `ListClients` discovery on owned uninitialized panels; one panel failure still returns other links with `Partial=true`; all relevant panels failing returns unavailable; a group absent from every completed/discovered inventory returns not found; concurrent HTTP cold misses coalesce; final links are globally sorted and exact-deduplicated.

- [ ] **Step 4: Implement resolver and HTTP status mapping**

`ResolveSubscription` first locates exact groups in the immutable snapshot. For an owned server without a completed inventory, it performs bounded fresh client discovery before deciding that the group is absent; completed inventories are not re-probed. Cache hits return immediately. Cache misses invoke bounded `GetOrFetch` per relevant server. Return links when at least one panel succeeds, even with partial failure. Map confirmed absence to 404 and discovery/link unavailability to 503; never expose panel errors.

Change `httpapi.Server` to depend on a small interface:

```go
type subscriptionSource interface {
	ResolveSubscription(context.Context, int64, string) (aggregator.SubscriptionResult, error)
}
```

The handler base64-encodes the sorted `result.Links`, one per line. A known group whose panels successfully return only empty lists is 404. Keep existing profile headers and rate limiting.

- [ ] **Step 5: Update read-only dashboard cards and safe states**

Build cards from `ServerSnapshot.Groups` joined to `Inbounds`, not `ClientEntry.Link`. Membership uses all exact records, including disabled ones, to suppress duplicate candidates; display rows may still mark disabled state. Add panel version and fixed labels for `ServerState`. Never assign `SyncErr.Error()` to `serverRow` or `InboundsErr`.

Keep assignment candidates VLESS-only in this migration; native links for existing VMess/Trojan/Shadowsocks/etc. are still aggregated and returned.

- [ ] **Step 6: Run runtime read-path tests**

Run: `go test -race ./internal/aggregator ./internal/httpapi ./internal/webui -run 'Test.*(Refresh|Subscription|ClientCards|Dashboard)' -v`

Expected: PASS; one failing panel does not suppress another panel’s links.

- [ ] **Step 7: Commit native read-path wiring**

```bash
git add internal/aggregator/aggregator.go internal/aggregator/subscription.go internal/aggregator/subscription_test.go internal/httpapi/http.go internal/httpapi/http_test.go internal/webui/webui.go internal/webui/templates/dashboard.html internal/link/vless.go
git commit -m "feat: serve subscriptions from native 3x-ui links"
```

### Task 8: Replace embedded-client add/delete with native group attach/detach

**Files:**

- Create: `internal/aggregator/mutations.go`
- Create: `internal/aggregator/mutations_test.go`
- Create: `internal/webui/client_handlers.go`
- Create: `internal/webui/client_handlers_test.go`
- Modify: `internal/webui/webui.go:366-401,634-790`
- Modify: `internal/webui/templates/dashboard.html:54-105`

**Interfaces:**

- Consumes: native client API and exact groups.
- Produces: `Aggregator.AttachGroup`, `Aggregator.DetachGroup`, `MutationResult`.
- Web UI passes only `(userID, serverID, subID, inboundID)` and never receives panel credentials.

- [ ] **Step 1: Write failing canonical attach/detach tests**

```go
type MutationResult struct { Attempted, Succeeded int; Noop bool }
```

Add three concrete fake-API tests: `TestAttachGroupHydratesIDsAndUsesLowestNativeID` returns summaries `z@x`/`a@x` with nil IDs and detail IDs 2/7, then asserts the only attach call is `z@x -> [9]`; `TestAttachGroupCreatesVLESSClientWhenTargetHasNoRecord` returns no target record plus a VLESS Reality/TCP inbound and asserts one add call with a new UUID, exact subId, unique email, `Enable=true`, `flow=xtls-rprx-vision`, `[9]`; `TestDetachGroupAttemptsEveryExactAttachedEmail` makes detach of `a@x` fail and asserts `b@x` is still attempted while unrelated `c@x` is untouched. Also test: any exact record already attached makes attach a no-op; paged rows with genuinely absent IDs fall back to email; non-VLESS implicit creation is rejected; a mutation transport error produces exactly one POST; every operation checks server ownership.

- [ ] **Step 2: Run mutation tests and verify failure**

Run: `go test ./internal/aggregator -run 'Test(AttachGroup|DetachGroup)' -v`

Expected: FAIL because the mutation façade does not exist.

- [ ] **Step 3: Implement fresh-inventory mutation orchestration**

```go
func (a *Aggregator) AttachGroup(ctx context.Context, userID, serverID int64, subID string, inboundID int) (MutationResult, error)
func (a *Aggregator) DetachGroup(ctx context.Context, userID, serverID int64, subID string, inboundID int) (MutationResult, error)
```

Both methods load an owned server and call fresh `ListClients` rather than trusting the snapshot. For attach, return no-op if any exact record already includes the inbound. Hydrate missing numeric IDs through `GetClient`; a detail failure aborts before mutation. Select the minimum positive ID with email tie-break, or lexicographic email only when detail succeeded but IDs are absent. If no target record exists, create a new record only for a VLESS inbound; use `crypto/rand` UUID v4. Build the email base from exact `subId` plus the inbound remark (or `inbound-<id>`), replace characters outside `[A-Za-z0-9._-]` with `-`, and append `-2`, `-3`, … until it is absent from the fresh target list. Set flow only for Reality/TCP. For detach, sort exact attached emails and attempt every one, returning a joined safe partial error plus counts.

After success, no-op or partial completion, trigger a targeted/current refresh before returning. Never retry a mutation.

- [ ] **Step 4: Replace web handlers and remove UUID from forms**

Move `clientInboundAdd` and `clientInboundRemove` into `client_handlers.go`. Replace the hidden fields with:

```html
<input type="hidden" name="sub_id" value="{{$card.Sub}}">
<input type="hidden" name="server_id" value="{{.ServerID}}">
<input type="hidden" name="inbound_id" value="{{.InboundID}}">
```

Remove `inboundView.ClientUUID`, `defaultClientEmail` and the old web-layer UUID generator after their logic is owned by the aggregator. Handler tests assert ownership, CSRF, no UUID field, one displayed row for duplicate records, and safe partial-error messaging.

- [ ] **Step 5: Run mutation/UI tests**

Run: `go test -race ./internal/aggregator ./internal/webui -run 'Test(AttachGroup|DetachGroup|ClientInbound|ClientCards)' -v`

Expected: PASS; removing one displayed group/inbound detaches every exact legacy duplicate on that inbound.

- [ ] **Step 6: Commit client mutation cutover**

```bash
git add internal/aggregator/mutations.go internal/aggregator/mutations_test.go internal/webui/client_handlers.go internal/webui/client_handlers_test.go internal/webui/webui.go internal/webui/templates/dashboard.html
git commit -m "feat: use native client attach and detach"
```

### Task 9: Replace inbound edit/copy/delete with lossless native operations

**Files:**

- Create: `internal/aggregator/inbound_mutations.go`
- Create: `internal/aggregator/inbound_mutations_test.go`
- Create: `internal/webui/inbound_handlers.go`
- Create: `internal/webui/inbound_handlers_test.go`
- Modify: `internal/webui/webui.go:792-1057`
- Modify: `internal/webui/templates/server_form.html:43-121`

**Interfaces:**

- Consumes: `InboundDocument` and native client mutation primitives.
- Produces: `EditInbound`, `CopyInbound`, `DeleteInbound` façade methods.
- Preserves current UI scope: inbound management/copy is VLESS-only; subscription aggregation remains protocol-neutral.

- [ ] **Step 1: Write failing lossless edit tests**

```go
type InboundPatch struct { Remark string; Port int; Enable bool }
```

In `TestEditInboundFetchesFreshAndPreservesUnknownFields`, make fake `GetInbound` return a document containing unknown top-level and nested fields; deep-compare the document passed to `UpdateInbound` and allow differences only in remark, port, enable and `tag="inbound-<port>"`. Assert `GetInbound(id)` is called once, `ListSlimInbounds` is not used for edit, and a failed POST is not retried.

- [ ] **Step 2: Write failing copy/rollback tests**

```go
type CopyInboundRequest struct {
	UserID int64
	SourceServerID int64
	SourceInboundID int
	TargetServerID int64
	Remark string
	Port int
}
```

`TestCopyInboundCreatesEmptyDocumentThenAttachesOrAddsClients` uses two source VLESS clients, one target-existing subId and one missing subId; assert `AddInbound` receives `settings.clients=[]` with unknown settings intact, then one attach and one add with new UUID/email but preserved limits/expiry/comment. `TestCopyInboundRollsBackCreatedClientsAndInbound` fails the second client mutation; assert every client created by this operation is deleted, then the created inbound is deleted, and the returned error joins primary and compensation failures. Also test same-panel copy reuses existing native records, same-server same-port is rejected before mutation, and no pre-existing source/target client is renamed or deleted.

- [ ] **Step 3: Implement lossless inbound façade**

```go
func (a *Aggregator) EditInbound(ctx context.Context, userID, serverID int64, inboundID int, patch InboundPatch) error
func (a *Aggregator) CopyInbound(ctx context.Context, req CopyInboundRequest) (int, error)
func (a *Aggregator) DeleteInbound(ctx context.Context, userID, serverID int64, inboundID int) error
```

Edit performs fresh get → four allowed field mutations → update. Copy clones the document, resets/deletes `id`, `up`, `down`, `clientStats`, sets remark/port/tag, extracts full source `ClientPayload` values, sets `settings.clients=[]`, then creates the inbound. Use the created object’s numeric ID. For each unique exact `subId`, attach a hydrated canonical target record if present; otherwise create a new VLESS record derived from the source payload with new UUID/email and cleared timestamps. Track newly created emails for compensation. Delete performs one native POST and triggers refresh.

- [ ] **Step 4: Replace web inbound handlers and correct copy/delete text**

Move the three handlers to `inbound_handlers.go`; they parse/authorize form input and call only the aggregator façade. Remove `cloneInbound` and direct `xui.RawInbound` manipulation from web UI.

Change copy help to say that existing target client records are attached and missing VLESS records are created. Change delete confirmation from “all clients will be erased” to “the inbound and its attachments will be removed; shared client records may remain in 3x-ui”.

- [ ] **Step 5: Run inbound mutation/UI tests**

Run: `go test -race ./internal/aggregator ./internal/webui -run 'Test(EditInbound|CopyInbound|DeleteInbound|InboundHandler)' -v`

Expected: PASS; unknown JSON is preserved and compensation is attempted exactly once.

- [ ] **Step 6: Commit inbound mutation cutover**

```bash
git add internal/aggregator/inbound_mutations.go internal/aggregator/inbound_mutations_test.go internal/webui/inbound_handlers.go internal/webui/inbound_handlers_test.go internal/webui/webui.go internal/webui/templates/server_form.html
git commit -m "feat: use lossless native inbound operations"
```

### Task 10: Remove the cookie/password path, finish docs and verify the migration

**Files:**

- Delete: `internal/xui/client.go`
- Delete: `internal/xui/parse.go`
- Modify: `internal/storage/storage.go`
- Modify: `internal/webui/webui.go` imports/remaining extracted blocks
- Modify: `README.md`
- Modify: `config.example.yaml`
- Modify: `docker-compose.yml`
- Test: all new `*_test.go` files from Tasks 1-9

**Interfaces:**

- Consumes: every native API and façade from prior tasks.
- Final state: no panel username/password or cookie code is reachable; legacy DB columns remain untouched and unused.

- [ ] **Step 1: Write the final static-regression check**

Run before cleanup:

```bash
rg -n 'cookiejar|inbounds/addClient|inbounds/updateClient|delClient|BuildVless|ParseClients|client_uuid|srv\.(Username|Password)' internal/xui internal/aggregator internal/httpapi internal/webui internal/storage
```

Expected: matches in legacy xui/storage/web code, proving cleanup is still required.

- [ ] **Step 2: Delete legacy xui/link parsing and stop reading legacy credentials**

Delete the two xui files. Remove `Username` and `Password` from `storage.Server`, `serverCols` and `scanServer`; remove `encryptLegacyPasswords`, `encryptPassword`, `decryptPassword` and their `Open` call. Keep the physical columns. New inserts use explicit empty placeholders:

```sql
INSERT INTO servers (
    user_id, name, api_url, path, username, password,
    api_token, insecure_skip_verify, host_override, created_at
) VALUES (?, ?, ?, ?, '', '', ?, ?, ?, ?)
```

Metadata-only updates omit `username`, `password` and `api_token`; token replacements update `api_token` explicitly. Never backfill, decrypt, encrypt, clear or drop old credential bytes.

- [ ] **Step 3: Update deployment and user documentation**

Document:

- minimum panel version 3.4.2 and compatible major 3 releases;
- creation of a dedicated named API token in each 3x-ui panel;
- token plaintext is shown once by the panel and must be pasted into this app;
- `master_key` is required to save tokens and must be backed up;
- upgraded servers show “requires token” until configured;
- no password/cookie fallback;
- native multi-protocol links, in-memory stale behavior and loss after process restart;
- HTTP/TLS warnings and full-admin token risk.

Add `ADMIN_PASSWORD` and `XUISUBAGG_MASTER_KEY` under `docker-compose.yml` service `environment`, sourced from the host, without literal secrets.

- [ ] **Step 4: Run static checks again**

Run:

```bash
rg -n 'cookiejar|inbounds/addClient|inbounds/updateClient|delClient|BuildVless|ParseClients|client_uuid|srv\.(Username|Password)' internal/xui internal/aggregator internal/httpapi internal/webui internal/storage
```

Expected: no matches. The application’s own `/login` route and user password hashing remain and are outside this panel-credential check.

- [ ] **Step 5: Run formatting and the full verification suite**

Run:

```bash
gofmt -w cmd internal
go test ./...
go test -race ./...
go vet ./...
git diff --check
```

Expected: all test commands PASS, vet emits no diagnostics, and `git diff --check` emits nothing.

- [ ] **Step 6: Perform the migration smoke test**

Using a temporary copy of a legacy SQLite database, start the binary once with no `master_key`, then with a valid key. Verify: startup succeeds both times; old servers remain visible; old username/password bytes are unchanged; a token cannot be saved without the key; with the key, save/check/sync/subscription/attach/detach/inbound edit all work against 3x-ui v3.4.2; panel access logs contain no request to `/login`.

- [ ] **Step 7: Commit the final cutover**

```bash
git add internal/xui internal/aggregator internal/httpapi internal/storage internal/webui README.md config.example.yaml docker-compose.yml cmd/aggregator/main.go
git commit -m "feat: complete native 3x-ui token migration"
```
