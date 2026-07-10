package storage

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LyschevIvan/3xui-sub-agg/internal/secrets"
)

func TestCreateServerEncryptsAPIToken(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "data.db"), secrets.New("master"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, err := store.CreateUser("owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}

	created, err := store.CreateServer(&Server{
		UserID: user.ID, Name: "node", APIURL: "https://panel", Path: "/",
		APIToken: "token-visible-only-here",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created.HasAPIToken || created.APIToken != "token-visible-only-here" {
		t.Fatalf("created=%+v", created)
	}

	var raw sql.NullString
	if err := store.db.QueryRow(`SELECT api_token FROM servers WHERE id=?`, created.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !raw.Valid || !secrets.IsEncrypted(raw.String) {
		t.Fatalf("raw token is not encrypted: %q", raw.String)
	}
	if strings.Contains(raw.String, "token-visible-only-here") {
		t.Fatal("plaintext token stored")
	}

	var username, password string
	if err := store.db.QueryRow(`SELECT username, password FROM servers WHERE id=?`, created.ID).Scan(&username, &password); err != nil {
		t.Fatal(err)
	}
	if username != "" || password != "" {
		t.Fatalf("token server stored legacy credentials: username=%q password=%q", username, password)
	}
}

func TestCreateServerRejectsTokenWithoutMasterKey(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "data.db"), secrets.New(""))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, err := store.CreateUser("owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}

	_, err = store.CreateServer(&Server{
		UserID: user.ID, Name: "node", APIURL: "https://panel", Path: "/", APIToken: "secret",
	})
	if !errors.Is(err, ErrMasterKeyRequired) {
		t.Fatalf("err=%v", err)
	}
}

func TestMigrateAddsNullableAPITokenToLegacyServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	createLegacyDatabase(t, path, "u", "p")

	store, err := Open(path, secrets.New("master"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var token sql.NullString
	if err := store.db.QueryRow(`SELECT api_token FROM servers WHERE id=1`).Scan(&token); err != nil {
		t.Fatal(err)
	}
	if token.Valid {
		t.Fatalf("legacy token must stay NULL, got %q", token.String)
	}
}

func TestBadTokenKeyDoesNotHideOtherServers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	good, err := Open(path, secrets.New("right"))
	if err != nil {
		t.Fatal(err)
	}
	user, err := good.CreateUser("owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := good.CreateServer(&Server{
		UserID: user.ID, Name: "encrypted", APIURL: "https://one", Path: "/", APIToken: "secret",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := good.CreateServer(&Server{
		UserID: user.ID, Name: "needs-token", APIURL: "https://two", Path: "/",
	}); err != nil {
		t.Fatal(err)
	}
	if err := good.Close(); err != nil {
		t.Fatal(err)
	}

	wrong, err := Open(path, secrets.New("wrong"))
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	rows, err := wrong.ListAllServers()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].TokenError == nil || rows[1].TokenError != nil {
		t.Fatalf("rows=%+v", rows)
	}
}

func TestMigrateLegacyServersIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	createLegacyDatabase(t, path, "legacy-user", "legacy-password")

	for i := 0; i < 2; i++ {
		store, err := Open(path, secrets.New("master"))
		if err != nil {
			t.Fatalf("open %d: %v", i+1, err)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("close %d: %v", i+1, err)
		}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`PRAGMA table_info(servers)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var tokenColumns int
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "api_token" {
			tokenColumns++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if tokenColumns != 1 {
		t.Fatalf("api_token columns=%d", tokenColumns)
	}
}

func TestMigratePreservesLegacyCredentialBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	username := "legacy-user\x00raw"
	password := "legacy-password:\xff"
	createLegacyDatabase(t, path, username, password)

	beforeUser, beforePassword := readRawLegacyCredentials(t, path)
	store, err := Open(path, secrets.New("master"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	afterUser, afterPassword := readRawLegacyCredentials(t, path)

	if !bytes.Equal(beforeUser, afterUser) {
		t.Fatalf("username bytes changed: before=%x after=%x", beforeUser, afterUser)
	}
	if !bytes.Equal(beforePassword, afterPassword) {
		t.Fatalf("password bytes changed: before=%x after=%x", beforePassword, afterPassword)
	}
}

func TestPlaintextAPITokenIsRejectedPerRowWithoutLeaking(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "data.db"), secrets.New("master"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, err := store.CreateUser("owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	const plaintext = "must-never-be-returned"
	if _, err := store.db.Exec(
		`INSERT INTO servers (user_id, name, api_url, path, username, password, api_token, created_at)
		 VALUES (?, 'node', 'https://panel', '/', '', '', ?, 1)`,
		user.ID, plaintext,
	); err != nil {
		t.Fatal(err)
	}

	rows, err := store.ListAllServers()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows=%+v", rows)
	}
	if !rows[0].HasAPIToken || !errors.Is(rows[0].TokenError, ErrPlaintextAPIToken) {
		t.Fatalf("server=%+v", rows[0])
	}
	if rows[0].APIToken != "" || strings.Contains(rows[0].APIToken, plaintext) {
		t.Fatalf("plaintext token returned: %q", rows[0].APIToken)
	}
}

func TestDeleteServerRemovesRowAndCiphertext(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "data.db"), secrets.New("master"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, err := store.CreateUser("owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateServer(&Server{
		UserID: user.ID, Name: "node", APIURL: "https://panel", Path: "/", APIToken: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	var ciphertext string
	if err := store.db.QueryRow(`SELECT api_token FROM servers WHERE id=?`, created.ID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}

	if err := store.DeleteServer(user.ID, created.ID); err != nil {
		t.Fatal(err)
	}
	var rowCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM servers WHERE id=?`, created.ID).Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 0 {
		t.Fatalf("server row remains: count=%d", rowCount)
	}
	var ciphertextCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM servers WHERE api_token=?`, ciphertext).Scan(&ciphertextCount); err != nil {
		t.Fatal(err)
	}
	if ciphertextCount != 0 {
		t.Fatalf("ciphertext remains: count=%d", ciphertextCount)
	}
}

func TestUpdateServerPreservesUnchangedAPITokenCiphertext(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "data.db"), secrets.New("master"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, err := store.CreateUser("owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateServer(&Server{
		UserID: user.ID, Name: "node", APIURL: "https://panel", Path: "/", APIToken: "secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	var before string
	if err := store.db.QueryRow(`SELECT api_token FROM servers WHERE id=?`, created.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.ServerByID(user.ID, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Name = "renamed"
	if err := store.UpdateServer(loaded); err != nil {
		t.Fatal(err)
	}
	var name, after string
	if err := store.db.QueryRow(`SELECT name, api_token FROM servers WHERE id=?`, created.ID).Scan(&name, &after); err != nil {
		t.Fatal(err)
	}
	if name != "renamed" {
		t.Fatalf("name=%q", name)
	}
	if after != before {
		t.Fatalf("unchanged token was re-encrypted: before=%q after=%q", before, after)
	}
}

func TestUpdateServerEncryptsChangedAPIToken(t *testing.T) {
	cipher := secrets.New("master")
	store, err := Open(filepath.Join(t.TempDir(), "data.db"), cipher)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, err := store.CreateUser("owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateServer(&Server{
		UserID: user.ID, Name: "node", APIURL: "https://panel", Path: "/", APIToken: "old-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	var before string
	if err := store.db.QueryRow(`SELECT api_token FROM servers WHERE id=?`, created.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}

	created.APIToken = "new-secret"
	if err := store.UpdateServer(created); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := store.db.QueryRow(`SELECT api_token FROM servers WHERE id=?`, created.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after == before || !secrets.IsEncrypted(after) {
		t.Fatalf("replacement ciphertext=%q previous=%q", after, before)
	}
	plain, err := cipher.Decrypt(after)
	if err != nil {
		t.Fatal(err)
	}
	if plain != "new-secret" {
		t.Fatalf("decrypted token=%q", plain)
	}
}

func TestUpdateServerPreservesUnreadableLegacyPassword(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cipher *secrets.Cipher
	}{
		{name: "wrong key", cipher: secrets.New("wrong")},
		{name: "no cipher", cipher: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "data.db")
			good, err := Open(path, secrets.New("right"))
			if err != nil {
				t.Fatal(err)
			}
			user, err := good.CreateUser("owner", "hash", false)
			if err != nil {
				t.Fatal(err)
			}
			created, err := good.CreateServer(&Server{
				UserID: user.ID, Name: "legacy", APIURL: "https://panel", Path: "/",
				Username: "legacy-user", Password: "recoverable-password",
			})
			if err != nil {
				t.Fatal(err)
			}
			var before []byte
			if err := good.db.QueryRow(
				`SELECT CAST(password AS BLOB) FROM servers WHERE id=?`, created.ID,
			).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if !secrets.IsEncrypted(string(before)) {
				t.Fatalf("legacy password is not encrypted: %q", before)
			}
			if err := good.Close(); err != nil {
				t.Fatal(err)
			}

			unreadable, err := Open(path, tc.cipher)
			if err != nil {
				t.Fatal(err)
			}
			defer unreadable.Close()
			loaded, err := unreadable.ServerByID(user.ID, created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if loaded.Password != "" {
				t.Fatalf("unreadable password exposed: %q", loaded.Password)
			}
			loaded.Name = "renamed"
			if err := unreadable.UpdateServer(loaded); err != nil {
				t.Fatal(err)
			}
			var after []byte
			if err := unreadable.db.QueryRow(
				`SELECT CAST(password AS BLOB) FROM servers WHERE id=?`, created.ID,
			).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("legacy password bytes changed: before=%x after=%x", before, after)
			}
			if err := unreadable.Close(); err != nil {
				t.Fatal(err)
			}

			recoveredStore, err := Open(path, secrets.New("right"))
			if err != nil {
				t.Fatal(err)
			}
			defer recoveredStore.Close()
			recovered, err := recoveredStore.ServerByID(user.ID, created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.Name != "renamed" || recovered.Password != "recoverable-password" {
				t.Fatalf("recovered=%+v", recovered)
			}
		})
	}
}

func TestUpdateServerRejectsTokenReplacementWithoutMasterKey(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cipher *secrets.Cipher
	}{
		{name: "empty cipher", cipher: secrets.New("")},
		{name: "nil cipher", cipher: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "data.db")
			good, err := Open(path, secrets.New("right"))
			if err != nil {
				t.Fatal(err)
			}
			user, err := good.CreateUser("owner", "hash", false)
			if err != nil {
				t.Fatal(err)
			}
			created, err := good.CreateServer(&Server{
				UserID: user.ID, Name: "node", APIURL: "https://panel", Path: "/", APIToken: "original-secret",
			})
			if err != nil {
				t.Fatal(err)
			}
			var before string
			if err := good.db.QueryRow(`SELECT api_token FROM servers WHERE id=?`, created.ID).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err := good.Close(); err != nil {
				t.Fatal(err)
			}

			withoutKey, err := Open(path, tc.cipher)
			if err != nil {
				t.Fatal(err)
			}
			defer withoutKey.Close()
			loaded, err := withoutKey.ServerByID(user.ID, created.ID)
			if err != nil {
				t.Fatal(err)
			}
			loaded.Name = "must-not-persist"
			loaded.APIToken = "replacement-secret"
			if err := withoutKey.UpdateServer(loaded); !errors.Is(err, ErrMasterKeyRequired) {
				t.Fatalf("err=%v", err)
			}
			var name, after string
			if err := withoutKey.db.QueryRow(
				`SELECT name, api_token FROM servers WHERE id=?`, created.ID,
			).Scan(&name, &after); err != nil {
				t.Fatal(err)
			}
			if name != "node" || after != before {
				t.Fatalf("row changed after rejected replacement: name=%q before=%q after=%q", name, before, after)
			}
		})
	}
}

func TestUnreadableLegacyPasswordDoesNotHideServer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	good, err := Open(path, secrets.New("right"))
	if err != nil {
		t.Fatal(err)
	}
	user, err := good.CreateUser("owner", "hash", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := good.CreateServer(&Server{
		UserID: user.ID, Name: "legacy", APIURL: "https://panel", Path: "/",
		Username: "legacy-user", Password: "legacy-password",
	}); err != nil {
		t.Fatal(err)
	}
	if err := good.Close(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		cipher *secrets.Cipher
	}{
		{name: "wrong key", cipher: secrets.New("wrong")},
		{name: "no cipher", cipher: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := Open(path, tc.cipher)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			rows, err := store.ListAllServers()
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Password != "" {
				t.Fatalf("rows=%+v", rows)
			}
		})
	}
}

func TestCanStoreAPITokensRequiresMasterKey(t *testing.T) {
	for _, tc := range []struct {
		name      string
		masterKey string
		want      bool
	}{
		{name: "configured", masterKey: "master", want: true},
		{name: "empty", masterKey: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "data.db"), secrets.New(tc.masterKey))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if got := store.CanStoreAPITokens(); got != tc.want {
				t.Fatalf("CanStoreAPITokens()=%v want %v", got, tc.want)
			}
		})
	}
}

func TestServerJSONOmitsAPIToken(t *testing.T) {
	raw, err := json.Marshal(Server{APIToken: "must-not-leak"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "must-not-leak") || strings.Contains(string(raw), `"APIToken":`) {
		t.Fatalf("APIToken leaked in JSON: %s", raw)
	}
}

func createLegacyDatabase(t *testing.T, path, username, password string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE servers (
		id INTEGER PRIMARY KEY AUTOINCREMENT, user_id INTEGER NOT NULL, name TEXT NOT NULL,
		api_url TEXT NOT NULL, path TEXT NOT NULL, username TEXT NOT NULL, password TEXT NOT NULL,
		insecure_skip_verify INTEGER NOT NULL DEFAULT 0, host_override TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO servers(user_id,name,api_url,path,username,password,created_at)
		 VALUES(1,'old','http://old','/',?,?,1)`,
		username, password,
	); err != nil {
		t.Fatal(err)
	}
}

func readRawLegacyCredentials(t *testing.T, path string) ([]byte, []byte) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var username, password []byte
	if err := db.QueryRow(
		`SELECT CAST(username AS BLOB), CAST(password AS BLOB) FROM servers WHERE id=1`,
	).Scan(&username, &password); err != nil {
		t.Fatal(err)
	}
	return username, password
}
