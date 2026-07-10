package webui

import (
	"bytes"
	"context"
	"database/sql"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LyschevIvan/3xui-sub-agg/internal/aggregator"
	"github.com/LyschevIvan/3xui-sub-agg/internal/auth"
	"github.com/LyschevIvan/3xui-sub-agg/internal/config"
	"github.com/LyschevIvan/3xui-sub-agg/internal/secrets"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
	"github.com/LyschevIvan/3xui-sub-agg/internal/xui"
)

type serverTestApp struct {
	handler *Handler
	store   *storage.Store
	user    *storage.User
	mux     http.Handler
	cookies []*http.Cookie
	csrf    string
	dbPath  string
}

func newServerTestApp(t *testing.T, masterKey string) *serverTestApp {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "data.db")
	store, err := storage.Open(dbPath, secrets.New(masterKey))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	user, err := store.CreateUser("owner", "unused", false)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{RequestTimeout: time.Second, RefreshInterval: time.Hour}
	authService := &auth.Service{Store: store}
	handler, err := New(cfg, store, authService, aggregator.New(cfg, store))
	if err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	handler.Mount(mux)

	session := httptest.NewRecorder()
	if err := authService.StartSession(session, user.ID); err != nil {
		t.Fatal(err)
	}
	app := &serverTestApp{
		handler: handler,
		store:   store,
		user:    user,
		mux:     authService.Middleware(mux),
		cookies: session.Result().Cookies(),
		dbPath:  dbPath,
	}
	for _, cookie := range app.cookies {
		if cookie.Name == auth.CSRFCookieName {
			app.csrf = cookie.Value
		}
	}
	if app.csrf == "" {
		t.Fatal("session did not issue a CSRF cookie")
	}
	return app
}

type fakeServerChecker struct {
	check func(context.Context, storage.Server, string) (xui.ServerStatus, error)
}

func (f fakeServerChecker) Check(ctx context.Context, srv storage.Server, token string) (xui.ServerStatus, error) {
	return f.check(ctx, srv, token)
}

func (a *serverTestApp) setChecker(check func(context.Context, storage.Server, string) (xui.ServerStatus, error)) {
	a.handler.checker = fakeServerChecker{check: check}
}

func (a *serverTestApp) request(t *testing.T, method, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	if form == nil {
		form = url.Values{}
	}
	if method != http.MethodGet && method != http.MethodHead {
		form.Set("csrf_token", a.csrf)
	}
	req := httptest.NewRequest(method, target, strings.NewReader(form.Encode()))
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, cookie := range a.cookies {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	a.mux.ServeHTTP(rr, req)
	return rr
}

func (a *serverTestApp) multipartRequest(t *testing.T, method, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, values := range form {
		for _, value := range values {
			if err := writer.WriteField(key, value); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.WriteField("csrf_token", a.csrf); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, target, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	for _, cookie := range a.cookies {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	a.mux.ServeHTTP(rr, req)
	return rr
}

func (a *serverTestApp) rawAPIToken(t *testing.T, serverID int64) string {
	t.Helper()
	db, err := sql.Open("sqlite", a.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var token sql.NullString
	if err := db.QueryRow(`SELECT api_token FROM servers WHERE id = ?`, serverID).Scan(&token); err != nil {
		t.Fatal(err)
	}
	return token.String
}

func (a *serverTestApp) setRawAPIToken(t *testing.T, serverID int64, token string) {
	t.Helper()
	db, err := sql.Open("sqlite", a.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE servers SET api_token = ? WHERE id = ?`, token, serverID); err != nil {
		t.Fatal(err)
	}
}
