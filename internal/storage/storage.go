package storage

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/LyschevIvan/3xui-sub-agg/internal/secrets"
)

var (
	ErrNotFound          = errors.New("not found")
	ErrMasterKeyRequired = errors.New("master_key is required to store API tokens")
	ErrPlaintextAPIToken = errors.New("stored API token is not encrypted")
)

type Store struct {
	db     *sql.DB
	cipher *secrets.Cipher
}

// Open открывает БД и применяет миграции. cipher опционален, но для
// сохранения API-токенов должен быть включён master_key.
func Open(path string, cipher *secrets.Cipher) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, cipher: cipher}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) encryptAPIToken(token string) (string, error) {
	if token == "" {
		return "", nil
	}
	if s.cipher == nil || !s.cipher.Enabled() {
		return "", ErrMasterKeyRequired
	}
	return s.cipher.Encrypt(token)
}

func (s *Store) CanStoreAPITokens() bool {
	return s.cipher != nil && s.cipher.Enabled()
}

func (s *Store) decodeAPIToken(stored sql.NullString, srv *Server) {
	srv.HasAPIToken = stored.Valid && stored.String != ""
	if !srv.HasAPIToken {
		return
	}
	if !secrets.IsEncrypted(stored.String) {
		srv.TokenError = ErrPlaintextAPIToken
		return
	}
	if s.cipher == nil || !s.cipher.Enabled() {
		srv.TokenError = ErrMasterKeyRequired
		return
	}
	plain, err := s.cipher.Decrypt(stored.String)
	if err != nil {
		srv.TokenError = fmt.Errorf("decrypt API token: %w", err)
		return
	}
	srv.APIToken = plain
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("migrate: begin: %w", err)
	}
	rollback := func(err error) error {
		_ = tx.Rollback()
		return fmt.Errorf("migrate: %w", err)
	}

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			login TEXT NOT NULL UNIQUE,
			password_hash TEXT NOT NULL,
			is_admin INTEGER NOT NULL DEFAULT 0,
			sub_prefix TEXT NOT NULL UNIQUE,
			created_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS invites (
			token TEXT PRIMARY KEY,
			created_by INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			used_at INTEGER,
			used_by INTEGER,
			FOREIGN KEY (created_by) REFERENCES users(id) ON DELETE CASCADE,
			FOREIGN KEY (used_by) REFERENCES users(id) ON DELETE SET NULL
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS servers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			api_url TEXT NOT NULL,
			path TEXT NOT NULL,
			username TEXT NOT NULL,
			password TEXT NOT NULL,
			api_token TEXT,
			insecure_skip_verify INTEGER NOT NULL DEFAULT 0,
			host_override TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS idx_servers_user ON servers(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id)`,
		// Админ использует пустой sub_prefix → подписки вида /sub/{email} без prefix.
		// Это сохраняет обратную совместимость с URL старых клиентов.
		`UPDATE users SET sub_prefix = '' WHERE is_admin = 1 AND sub_prefix <> ''`,
	}
	for _, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return rollback(err)
		}
	}

	rows, err := tx.Query(`PRAGMA table_info(servers)`)
	if err != nil {
		return rollback(err)
	}
	hasAPIToken := false
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return rollback(err)
		}
		if name == "api_token" {
			hasAPIToken = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return rollback(err)
	}
	if err := rows.Close(); err != nil {
		return rollback(err)
	}
	if !hasAPIToken {
		if _, err := tx.Exec(`ALTER TABLE servers ADD COLUMN api_token TEXT`); err != nil {
			return rollback(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate: commit: %w", err)
	}
	return nil
}

// User — пользователь системы.
type User struct {
	ID           int64
	Login        string
	PasswordHash string
	IsAdmin      bool
	SubPrefix    string
	CreatedAt    time.Time
}

// Server — 3x-ui панель, принадлежит пользователю.
type Server struct {
	ID, UserID         int64
	Name, APIURL, Path string
	APIToken           string `json:"-"`
	HasAPIToken        bool
	TokenError         error
	InsecureSkipVerify bool
	HostOverride       string
	CreatedAt          time.Time
}

// Invite — одноразовый токен на регистрацию.
type Invite struct {
	Token     string
	CreatedBy int64
	ExpiresAt time.Time
	UsedAt    *time.Time
	UsedBy    *int64
}

// randomHex возвращает n случайных байт в hex (длина строки = 2n).
func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ---------- Users ----------

func (s *Store) CreateUser(login, passwordHash string, isAdmin bool) (*User, error) {
	var prefix string
	if !isAdmin {
		p, err := randomHex(12)
		if err != nil {
			return nil, err
		}
		prefix = p
	}
	now := time.Now()
	admin := 0
	if isAdmin {
		admin = 1
	}
	res, err := s.db.Exec(
		`INSERT INTO users (login, password_hash, is_admin, sub_prefix, created_at) VALUES (?, ?, ?, ?, ?)`,
		login, passwordHash, admin, prefix, now.Unix(),
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &User{
		ID: id, Login: login, PasswordHash: passwordHash,
		IsAdmin: isAdmin, SubPrefix: prefix, CreatedAt: now,
	}, nil
}

func scanUser(row interface {
	Scan(...any) error
}) (*User, error) {
	var u User
	var isAdmin int
	var createdAt int64
	if err := row.Scan(&u.ID, &u.Login, &u.PasswordHash, &isAdmin, &u.SubPrefix, &createdAt); err != nil {
		return nil, err
	}
	u.IsAdmin = isAdmin != 0
	u.CreatedAt = time.Unix(createdAt, 0)
	return &u, nil
}

func (s *Store) UserByLogin(login string) (*User, error) {
	row := s.db.QueryRow(`SELECT id, login, password_hash, is_admin, sub_prefix, created_at FROM users WHERE login = ?`, login)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (s *Store) UserByID(id int64) (*User, error) {
	row := s.db.QueryRow(`SELECT id, login, password_hash, is_admin, sub_prefix, created_at FROM users WHERE id = ?`, id)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (s *Store) UserBySubPrefix(prefix string) (*User, error) {
	row := s.db.QueryRow(`SELECT id, login, password_hash, is_admin, sub_prefix, created_at FROM users WHERE sub_prefix = ?`, prefix)
	u, err := scanUser(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return u, err
}

func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT id, login, password_hash, is_admin, sub_prefix, created_at FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

func (s *Store) DeleteUser(id int64) error {
	_, err := s.db.Exec(`DELETE FROM users WHERE id = ?`, id)
	return err
}

// ---------- Servers ----------

func (s *Store) CreateServer(srv *Server) (*Server, error) {
	now := time.Now()
	insecure := 0
	if srv.InsecureSkipVerify {
		insecure = 1
	}
	var storedToken sql.NullString
	if srv.APIToken != "" {
		encToken, err := s.encryptAPIToken(srv.APIToken)
		if err != nil {
			return nil, fmt.Errorf("encrypt API token: %w", err)
		}
		storedToken = sql.NullString{String: encToken, Valid: true}
	}
	res, err := s.db.Exec(
		`INSERT INTO servers (user_id, name, api_url, path, username, password, api_token, insecure_skip_verify, host_override, created_at)
		 VALUES (?, ?, ?, ?, '', '', ?, ?, ?, ?)`,
		srv.UserID, srv.Name, srv.APIURL, srv.Path, storedToken, insecure, srv.HostOverride, now.Unix(),
	)
	if err != nil {
		return nil, err
	}
	srv.ID, _ = res.LastInsertId()
	srv.HasAPIToken = storedToken.Valid
	srv.TokenError = nil
	srv.CreatedAt = now
	return srv, nil
}

func (s *Store) UpdateServer(srv *Server) error {
	insecure := 0
	if srv.InsecureSkipVerify {
		insecure = 1
	}
	updateMetadata := func() error {
		_, err := s.db.Exec(
			`UPDATE servers SET name=?, api_url=?, path=?, insecure_skip_verify=?, host_override=?
			 WHERE id = ? AND user_id = ?`,
			srv.Name, srv.APIURL, srv.Path, insecure, srv.HostOverride,
			srv.ID, srv.UserID,
		)
		return err
	}
	if srv.APIToken == "" {
		return updateMetadata()
	}

	var current sql.NullString
	if err := s.db.QueryRow(
		`SELECT api_token FROM servers WHERE id = ? AND user_id = ?`, srv.ID, srv.UserID,
	).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if current.Valid && secrets.IsEncrypted(current.String) && s.cipher != nil && s.cipher.Enabled() {
		plain, err := s.cipher.Decrypt(current.String)
		if err == nil && plain == srv.APIToken {
			return updateMetadata()
		}
	}

	encToken, err := s.encryptAPIToken(srv.APIToken)
	if err != nil {
		return fmt.Errorf("encrypt API token: %w", err)
	}
	_, err = s.db.Exec(
		`UPDATE servers SET name=?, api_url=?, path=?, api_token=?, insecure_skip_verify=?, host_override=?
		 WHERE id = ? AND user_id = ?`,
		srv.Name, srv.APIURL, srv.Path, encToken, insecure, srv.HostOverride,
		srv.ID, srv.UserID,
	)
	return err
}

func (s *Store) DeleteServer(userID, id int64) error {
	_, err := s.db.Exec(`DELETE FROM servers WHERE id = ? AND user_id = ?`, id, userID)
	return err
}

func (s *Store) scanServer(row interface {
	Scan(...any) error
}) (*Server, error) {
	var srv Server
	var storedToken sql.NullString
	var insecure int
	var createdAt int64
	if err := row.Scan(
		&srv.ID, &srv.UserID, &srv.Name, &srv.APIURL, &srv.Path, &storedToken,
		&insecure, &srv.HostOverride, &createdAt,
	); err != nil {
		return nil, err
	}
	s.decodeAPIToken(storedToken, &srv)
	srv.InsecureSkipVerify = insecure != 0
	srv.CreatedAt = time.Unix(createdAt, 0)
	return &srv, nil
}

const serverCols = `id, user_id, name, api_url, path, api_token, insecure_skip_verify, host_override, created_at`

func (s *Store) ServerByID(userID, id int64) (*Server, error) {
	row := s.db.QueryRow(`SELECT `+serverCols+` FROM servers WHERE id = ? AND user_id = ?`, id, userID)
	srv, err := s.scanServer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return srv, nil
}

func (s *Store) ListServersByUser(userID int64) ([]Server, error) {
	rows, err := s.db.Query(`SELECT `+serverCols+` FROM servers WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Server
	for rows.Next() {
		srv, err := s.scanServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *srv)
	}
	return out, rows.Err()
}

func (s *Store) ListAllServers() ([]Server, error) {
	rows, err := s.db.Query(`SELECT ` + serverCols + ` FROM servers ORDER BY user_id, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Server
	for rows.Next() {
		srv, err := s.scanServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *srv)
	}
	return out, rows.Err()
}

// ---------- Sessions ----------

func (s *Store) CreateSession(userID int64, ttl time.Duration) (string, error) {
	token, err := randomHex(32)
	if err != nil {
		return "", err
	}
	exp := time.Now().Add(ttl).Unix()
	if _, err := s.db.Exec(`INSERT INTO sessions (token, user_id, expires_at) VALUES (?, ?, ?)`, token, userID, exp); err != nil {
		return "", err
	}
	return token, nil
}

// SessionUser возвращает пользователя по токену. Истёкшие сессии удаляются.
func (s *Store) SessionUser(token string) (*User, error) {
	if token == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRow(
		`SELECT u.id, u.login, u.password_hash, u.is_admin, u.sub_prefix, u.created_at, s.expires_at
		 FROM sessions s JOIN users u ON u.id = s.user_id
		 WHERE s.token = ?`, token,
	)
	var u User
	var isAdmin int
	var createdAt, expiresAt int64
	if err := row.Scan(&u.ID, &u.Login, &u.PasswordHash, &isAdmin, &u.SubPrefix, &createdAt, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if time.Now().Unix() > expiresAt {
		_, _ = s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
		return nil, ErrNotFound
	}
	u.IsAdmin = isAdmin != 0
	u.CreatedAt = time.Unix(createdAt, 0)
	return &u, nil
}

func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token)
	return err
}

func (s *Store) PurgeExpiredSessions() error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
	return err
}

// ---------- Invites ----------

func (s *Store) CreateInvite(createdBy int64, ttl time.Duration) (*Invite, error) {
	token, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	exp := time.Now().Add(ttl)
	if _, err := s.db.Exec(
		`INSERT INTO invites (token, created_by, expires_at) VALUES (?, ?, ?)`,
		token, createdBy, exp.Unix(),
	); err != nil {
		return nil, err
	}
	return &Invite{Token: token, CreatedBy: createdBy, ExpiresAt: exp}, nil
}

func (s *Store) InviteByToken(token string) (*Invite, error) {
	row := s.db.QueryRow(`SELECT token, created_by, expires_at, used_at, used_by FROM invites WHERE token = ?`, token)
	var inv Invite
	var exp int64
	var usedAt sql.NullInt64
	var usedBy sql.NullInt64
	if err := row.Scan(&inv.Token, &inv.CreatedBy, &exp, &usedAt, &usedBy); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	inv.ExpiresAt = time.Unix(exp, 0)
	if usedAt.Valid {
		t := time.Unix(usedAt.Int64, 0)
		inv.UsedAt = &t
	}
	if usedBy.Valid {
		v := usedBy.Int64
		inv.UsedBy = &v
	}
	return &inv, nil
}

func (s *Store) MarkInviteUsed(token string, userID int64) error {
	now := time.Now().Unix()
	_, err := s.db.Exec(`UPDATE invites SET used_at = ?, used_by = ? WHERE token = ?`, now, userID, token)
	return err
}

func (s *Store) ListInvites() ([]Invite, error) {
	rows, err := s.db.Query(`SELECT token, created_by, expires_at, used_at, used_by FROM invites ORDER BY expires_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Invite
	for rows.Next() {
		var inv Invite
		var exp int64
		var usedAt sql.NullInt64
		var usedBy sql.NullInt64
		if err := rows.Scan(&inv.Token, &inv.CreatedBy, &exp, &usedAt, &usedBy); err != nil {
			return nil, err
		}
		inv.ExpiresAt = time.Unix(exp, 0)
		if usedAt.Valid {
			t := time.Unix(usedAt.Int64, 0)
			inv.UsedAt = &t
		}
		if usedBy.Valid {
			v := usedBy.Int64
			inv.UsedBy = &v
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

func (s *Store) DeleteInvite(token string) error {
	_, err := s.db.Exec(`DELETE FROM invites WHERE token = ?`, token)
	return err
}
