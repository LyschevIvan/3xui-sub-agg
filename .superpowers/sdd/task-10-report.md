# Task 10 report

Base: `ce1c6931245bef127e1348976cacb9c1eaaf2f7b`

## RED

Initial exact static regression command:

```text
rg -n 'cookiejar|inbounds/addClient|inbounds/updateClient|delClient|BuildVless|ParseClients|client_uuid|srv\.(Username|Password)' internal/xui internal/aggregator internal/httpapi internal/webui internal/storage
```

Exit code: 0. Matches were present in `internal/xui/client.go`, `internal/xui/parse.go`, `internal/aggregator/aggregator.go`, `internal/storage/storage.go`, `internal/xui/api_transport_test.go`, and a web form negative-assertion fixture. This proves the legacy cleanup gate initially failed.

Broader case-insensitive search classified initial matches as:

- legacy panel runtime: `internal/xui/client.go`, `internal/storage/storage.go`, `internal/aggregator/aggregator.go`;
- native protocol payload fields (not panel login credentials): client `password` fields in `internal/xui/api_clients*.go` and lossless inbound fixtures;
- application authentication (must remain): `internal/auth`, `internal/config`, `cmd/aggregator`, `/login` web handlers/templates, admin password documentation;
- compatibility/negative assertions: physical SQLite `username`/`password` columns and tests asserting panel credential fields are absent.

Before production cleanup, `TestTask10StorageSourceHasNoLegacyCredentialRuntime` was added. Its initial run failed because `storage.go` still contained `Username, Password`, `encryptLegacyPasswords`, `encryptPassword`, `decryptPassword`, and `legacyPasswordUnreadable`.

Focused RED command:

```text
go test ./internal/storage -run 'TestTask10' -count=1
```

Exit code: 1. Besides the five source-symbol failures, the behavioral test proved the old metadata path rewrote the physical password bytes by decrypting and re-encrypting them under the configured key.

## GREEN

### Runtime/storage cutover

- Deleted `internal/xui/client.go` and `internal/xui/parse.go`.
- Removed the legacy `Aggregator.XuiClient` constructor and every `storage.Server` panel username/password field and caller.
- Removed all legacy password encrypt/decrypt/migration helpers.
- `serverCols` and scan destinations no longer select physical username/password columns.
- The physical SQLite columns remain in schema creation and legacy databases. New inserts explicitly list them and write literal empty placeholders.
- Metadata updates omit username, password, and `api_token`; explicit token replacement updates only intended metadata plus `api_token`.

`internal/storage/task10_migration_test.go` proves with raw BLOB comparisons that:

- a populated legacy database opens and remains visible with nil, disabled, and configured ciphers;
- opening/listing never rewrites the legacy credential bytes;
- metadata and token updates preserve both physical credential byte strings;
- rejected save without a master key leaves the legacy bytes untouched;
- save with a valid key succeeds, remains encrypted at rest, and decrypts only through `APIToken`.

Existing storage tests continue to prove new inserts store explicit empty legacy placeholders, unchanged-token metadata edits preserve ciphertext, and no-key token creation/replacement fails atomically.

### Static cleanup

Final exact command:

```text
rg -n 'cookiejar|inbounds/addClient|inbounds/updateClient|delClient|BuildVless|ParseClients|client_uuid|srv\.(Username|Password)' internal/xui internal/aggregator internal/httpapi internal/webui internal/storage
```

Exit code: 1 with no output, the expected no-match result.

The final broader case-insensitive search has only these classified matches:

- application authentication: admin bootstrap config, bcrypt helpers, app `/login` routes/templates/tests, session-cookie documentation;
- native protocol data: `password` fields required for protocol-specific client payloads and lossless inbound round trips; these are not panel login credentials;
- physical-schema compatibility: SQLite `servers.username`/`servers.password` DDL, explicit empty insert placeholders, raw-byte migration tests, and documentation;
- negative assertions: tests that reject panel credential form fields or assert the token transport never requests a login path.

No 3x-ui panel login request or legacy embedded-client endpoint remains compiled.

### Documentation/deployment

`README.md`, `config.example.yaml`, and `docker-compose.yml` now document/enforce the minimum `3.4.2` / major `3` contract, dedicated named tokens, one-time plaintext display, master-key backup, requires-token legacy rows, no cookie/password fallback, native multi-protocol links, volatile in-memory stale fallback, TLS/HTTP/insecure-mode risks, full-admin blast radius, and token rotation/revocation. Compose passes `ADMIN_PASSWORD` and `XUISUBAGG_MASTER_KEY` from the host without literal values or defaults.

### Automated smoke

Focused legacy DB smoke:

```text
go test ./internal/storage -run 'TestTask10' -count=1
```

Exit code: 0.

Focused native HTTP smoke:

```text
go test ./internal/xui -run 'TestNativeAPISmokeCoversFinalOperationsAndNoLegacyRequests' -count=1
```

Exit code: 0. The local recorder exercised validation/check, client inventory sync, subscription links, attach, detach, inbound fetch, and inbound update. Every request carried the Bearer token; the exact recorded path list contained no panel login or legacy embedded-client endpoint.

A real 3x-ui `v3.4.2` instance and token are not available in this workspace, so external smoke was not claimed. `README.md` contains the exact manual checklist for a real panel: check, sync, decode subscription, attach/detach, edit a test inbound, inspect access logs, and rotate the token.

### Timing-test stabilization

The first full test run exposed an existing assertion race in `TestDeletionCancelsHeldDiscoveryAndPreventsLateCachePopulation`: its context observer closed a channel in a goroutine, while the assertion used a non-blocking `default`. The focused test reproduced once in 20 runs even though the resolver already observed cancellation. The test-only fix waits on the condition with a one-second timeout; no sleep and no production change were added.

```text
go test -race ./internal/aggregator -run TestDeletionCancelsHeldDiscoveryAndPreventsLateCachePopulation -count=50 -timeout=60s
```

Exit code: 0.

### Final verification

Fresh final commands after all code edits:

```text
gofmt -w cmd internal
go test ./... -count=1
go test -race ./... -count=1
go vet ./...
git diff --check
```

All exit codes: 0. Vet and diff check emitted no diagnostics.

### Changed/deleted files and self-review

- Deleted: `internal/xui/client.go`, `internal/xui/parse.go`.
- Runtime/storage: `internal/storage/storage.go`, `internal/aggregator/aggregator.go`.
- Migration/native smoke and adapted fixtures: `internal/storage/storage_test.go`, `internal/storage/task10_migration_test.go`, `internal/xui/native_smoke_test.go`, `internal/xui/api_transport_test.go`, `internal/aggregator/subscription_test.go`, `internal/webui/client_handlers_test.go`.
- Docs/deployment: `README.md`, `config.example.yaml`, `docker-compose.yml`.

Self-review against base confirmed the diff is limited to Task 10 cleanup, its tests, documentation, and the test-only cancellation assertion stabilization. No user-owned or unrelated files changed.

Unresolved external verification: real-panel `v3.4.2` smoke remains unavailable and must be run manually using the README checklist.
