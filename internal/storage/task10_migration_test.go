package storage

import (
	"bytes"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LyschevIvan/3xui-sub-agg/internal/secrets"
)

func TestTask10StorageSourceHasNoLegacyCredentialRuntime(t *testing.T) {
	raw, err := os.ReadFile("storage.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"Username, Password",
		"encryptLegacyPasswords",
		"encryptPassword",
		"decryptPassword",
		"legacyPasswordUnreadable",
	} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("legacy panel credential runtime remains: %q", forbidden)
		}
	}
}

func TestTask10LegacyCredentialBytesStayOpaqueAcrossOpenModes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	createLegacyDatabase(t, path, "legacy-user\x00raw", "legacy-password:\xff")
	wantUser, wantPassword := readRawLegacyCredentials(t, path)

	for _, tc := range []struct {
		name   string
		cipher *secrets.Cipher
	}{
		{name: "no cipher", cipher: nil},
		{name: "disabled cipher", cipher: secrets.New("")},
		{name: "configured cipher", cipher: secrets.New("task-10-master-key")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := Open(path, tc.cipher)
			if err != nil {
				t.Fatal(err)
			}
			servers, err := store.ListAllServers()
			if err != nil {
				_ = store.Close()
				t.Fatal(err)
			}
			if len(servers) != 1 || servers[0].Name != "old" {
				_ = store.Close()
				t.Fatalf("legacy servers=%+v", servers)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			gotUser, gotPassword := readRawLegacyCredentials(t, path)
			if !bytes.Equal(gotUser, wantUser) || !bytes.Equal(gotPassword, wantPassword) {
				t.Fatalf("legacy credential bytes changed: username=%x/%x password=%x/%x", gotUser, wantUser, gotPassword, wantPassword)
			}
		})
	}
}

func TestTask10MetadataAndTokenUpdatesNeverTouchLegacyCredentialBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	createLegacyDatabase(t, path, "legacy-user\x00raw", "legacy-password:\xff")
	store, err := Open(path, secrets.New("task-10-master-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	server, err := store.ServerByID(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	server.APIToken = "first-token"
	if err := store.UpdateServer(server); err != nil {
		t.Fatal(err)
	}
	wantUser, wantPassword, firstToken := readRawServerSecrets(t, store.db, server.ID)
	if !secrets.IsEncrypted(firstToken) {
		t.Fatalf("first token is not encrypted: %q", firstToken)
	}

	server, err = store.ServerByID(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	server.Name = "metadata-only"
	if err := store.UpdateServer(server); err != nil {
		t.Fatal(err)
	}
	gotUser, gotPassword, gotToken := readRawServerSecrets(t, store.db, server.ID)
	if !bytes.Equal(gotUser, wantUser) || !bytes.Equal(gotPassword, wantPassword) || gotToken != firstToken {
		t.Fatalf("metadata update touched secrets: username=%x/%x password=%x/%x token=%q/%q", gotUser, wantUser, gotPassword, wantPassword, gotToken, firstToken)
	}

	server.APIToken = "replacement-token"
	server.HostOverride = "public.example"
	if err := store.UpdateServer(server); err != nil {
		t.Fatal(err)
	}
	gotUser, gotPassword, gotToken = readRawServerSecrets(t, store.db, server.ID)
	if !bytes.Equal(gotUser, wantUser) || !bytes.Equal(gotPassword, wantPassword) {
		t.Fatalf("token replacement touched legacy credentials: username=%x/%x password=%x/%x", gotUser, wantUser, gotPassword, wantPassword)
	}
	if gotToken == firstToken || !secrets.IsEncrypted(gotToken) {
		t.Fatalf("token replacement=%q previous=%q", gotToken, firstToken)
	}
}

func TestTask10LegacyMigrationTokenSaveRequiresKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	createLegacyDatabase(t, path, "legacy-user", "legacy-password")
	wantUser, wantPassword := readRawLegacyCredentials(t, path)

	withoutKey, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	server, err := withoutKey.ServerByID(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	server.APIToken = "native-token"
	if err := withoutKey.UpdateServer(server); !errors.Is(err, ErrMasterKeyRequired) {
		t.Fatalf("save without key err=%v", err)
	}
	if err := withoutKey.Close(); err != nil {
		t.Fatal(err)
	}
	gotUser, gotPassword := readRawLegacyCredentials(t, path)
	if !bytes.Equal(gotUser, wantUser) || !bytes.Equal(gotPassword, wantPassword) {
		t.Fatal("rejected token save changed legacy credentials")
	}

	withKey, err := Open(path, secrets.New("task-10-master-key"))
	if err != nil {
		t.Fatal(err)
	}
	defer withKey.Close()
	server, err = withKey.ServerByID(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	server.APIToken = "native-token"
	if err := withKey.UpdateServer(server); err != nil {
		t.Fatal(err)
	}
	reloaded, err := withKey.ServerByID(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.APIToken != "native-token" || reloaded.TokenError != nil {
		t.Fatalf("reloaded token state=%+v", reloaded)
	}
	gotUser, gotPassword = readRawLegacyCredentials(t, path)
	if !bytes.Equal(gotUser, wantUser) || !bytes.Equal(gotPassword, wantPassword) {
		t.Fatal("successful token save changed legacy credentials")
	}
}

func readRawServerSecrets(t *testing.T, db *sql.DB, id int64) ([]byte, []byte, string) {
	t.Helper()
	var username, password []byte
	var token string
	if err := db.QueryRow(
		`SELECT CAST(username AS BLOB), CAST(password AS BLOB), api_token FROM servers WHERE id=?`, id,
	).Scan(&username, &password, &token); err != nil {
		t.Fatal(err)
	}
	return username, password, token
}
