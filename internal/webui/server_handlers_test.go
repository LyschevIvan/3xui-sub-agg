package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LyschevIvan/3xui-sub-agg/internal/secrets"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
	"github.com/LyschevIvan/3xui-sub-agg/internal/xui"
)

func TestParseServerFormReturnsTokenSeparately(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(url.Values{
		"name":      {"node"},
		"api_url":   {"https://panel.example"},
		"path":      {"/admin/"},
		"api_token": {"submitted-secret"},
	}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	form, token, err := parseServerForm(r)
	if err != nil {
		t.Fatal(err)
	}
	if token != "submitted-secret" || form.Name != "node" {
		t.Fatalf("form=%+v token=%q", form, token)
	}
	if strings.Contains(fmt.Sprintf("%+v", form), token) {
		t.Fatal("renderable form contains token")
	}
}

func TestParseServerFormRejectsUnsafeAPIURLs(t *testing.T) {
	for _, apiURL := range []string{
		"ftp://panel.example",
		"file:///tmp/panel",
		"https:///missing-host",
		"panel.example",
	} {
		t.Run(apiURL, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(url.Values{
				"name":    {"node"},
				"api_url": {apiURL},
			}.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if _, _, err := parseServerForm(r); err == nil {
				t.Fatalf("parseServerForm accepted %q", apiURL)
			}
		})
	}
}

func TestServerFormNeverRendersStoredOrSubmittedToken(t *testing.T) {
	const (
		storedToken    = "stored-token-must-not-render"
		submittedToken = "submitted-token-must-not-render"
	)
	app := newServerTestApp(t, "master-key")
	server, err := app.store.CreateServer(&storage.Server{
		UserID: app.user.ID, Name: "node", APIURL: "https://panel.example", Path: "/",
		APIToken: storedToken,
	})
	if err != nil {
		t.Fatal(err)
	}

	assertSafe := func(label string, wantStatus int, rr *httptest.ResponseRecorder) {
		t.Helper()
		body := rr.Body.String()
		if rr.Code != wantStatus {
			t.Fatalf("%s: status=%d body=%s", label, rr.Code, body)
		}
		if contentType := rr.Result().Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
			t.Fatalf("%s: Content-Type=%q", label, contentType)
		}
		for _, forbidden := range []string{storedToken, submittedToken, `name="username"`, `name="password"`} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("%s: response contains %q", label, forbidden)
			}
		}
		if !strings.Contains(body, "токен сохранён") {
			t.Fatalf("%s: missing saved-token indicator", label)
		}
		inputStart := strings.Index(body, `<input type="password" name="api_token"`)
		if inputStart < 0 {
			t.Fatalf("%s: missing token input", label)
		}
		inputEnd := strings.Index(body[inputStart:], ">")
		if inputEnd < 0 || strings.Contains(body[inputStart:inputStart+inputEnd], "value=") {
			t.Fatalf("%s: token input has a value attribute", label)
		}
	}

	assertSafe("GET edit", http.StatusOK, app.request(t, http.MethodGet, "/dashboard/servers/"+strconv.FormatInt(server.ID, 10), nil))
	assertSafe("validation error", http.StatusBadRequest, app.request(t, http.MethodPost, "/dashboard/servers/"+strconv.FormatInt(server.ID, 10), url.Values{
		"name":      {"node"},
		"api_url":   {"ftp://unsafe.example"},
		"path":      {"/"},
		"api_token": {submittedToken},
	}))
}

func TestServerCheckUsesSubmittedTokenWithoutMasterKey(t *testing.T) {
	const token = "one-off-secret"
	app := newServerTestApp(t, "")
	app.setChecker(func(_ context.Context, srv storage.Server, gotToken string) (xui.ServerStatus, error) {
		if gotToken != token || srv.APIURL != "https://panel.example" || srv.Path != "/admin/" {
			t.Fatalf("server=%+v token=%q", srv, gotToken)
		}
		return xui.ServerStatus{PanelVersion: "v3.4.2"}, nil
	})

	rr := app.request(t, http.MethodPost, "/dashboard/servers/check", serverValues("", "node", "https://panel.example", "/admin/", token))
	assertJSONResponse(t, rr, http.StatusOK, map[string]any{
		"ok":            true,
		"panel_version": "v3.4.2",
	})
}

func TestServerCheckBlankEditUsesStoredTokenWithMultipartCSRF(t *testing.T) {
	const token = "stored-secret"
	app := newServerTestApp(t, "master-key")
	server := createTokenServer(t, app, "https://panel.example", "/admin/", token)
	app.setChecker(func(_ context.Context, srv storage.Server, gotToken string) (xui.ServerStatus, error) {
		if gotToken != token || srv.ID != server.ID {
			t.Fatalf("server=%+v token=%q", srv, gotToken)
		}
		return xui.ServerStatus{PanelVersion: "3.4.3"}, nil
	})

	rr := app.multipartRequest(t, http.MethodPost, "/dashboard/servers/check", serverValues(server.ID, "node", server.APIURL, server.Path, ""))
	assertJSONResponse(t, rr, http.StatusOK, map[string]any{
		"ok":            true,
		"panel_version": "3.4.3",
	})
}

func TestServerCheckForeignIDIsNotFound(t *testing.T) {
	app := newServerTestApp(t, "master-key")
	foreign, err := app.store.CreateUser("foreign", "unused", false)
	if err != nil {
		t.Fatal(err)
	}
	server, err := app.store.CreateServer(&storage.Server{
		UserID: foreign.ID, Name: "foreign", APIURL: "https://foreign.example", Path: "/", APIToken: "foreign-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	app.setChecker(func(context.Context, storage.Server, string) (xui.ServerStatus, error) {
		t.Fatal("checker called for a foreign server")
		return xui.ServerStatus{}, nil
	})

	rr := app.request(t, http.MethodPost, "/dashboard/servers/check", serverValues(server.ID, "foreign", server.APIURL, server.Path, ""))
	assertJSONResponse(t, rr, http.StatusNotFound, map[string]any{
		"ok": false, "code": "not_found", "message": "Сервер не найден",
	})
	rr = app.request(t, http.MethodPost, "/dashboard/servers/check", serverValues(server.ID, "foreign", "ftp://invalid.example", server.Path, ""))
	assertJSONResponse(t, rr, http.StatusNotFound, map[string]any{
		"ok": false, "code": "not_found", "message": "Сервер не найден",
	})
}

func TestServerCheckRequiresAuthenticationAndCSRF(t *testing.T) {
	app := newServerTestApp(t, "")
	form := serverValues("", "node", "https://panel.example", "/", "one-off")

	unauthenticated := httptest.NewRequest(http.MethodPost, "/dashboard/servers/check", strings.NewReader(form.Encode()))
	unauthenticated.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()
	app.mux.ServeHTTP(rr, unauthenticated)
	if rr.Code != http.StatusSeeOther || rr.Header().Get("Location") != "/login" {
		t.Fatalf("unauthenticated status=%d location=%q", rr.Code, rr.Header().Get("Location"))
	}

	missingCSRF := httptest.NewRequest(http.MethodPost, "/dashboard/servers/check", strings.NewReader(form.Encode()))
	missingCSRF.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, cookie := range app.cookies {
		missingCSRF.AddCookie(cookie)
	}
	rr = httptest.NewRecorder()
	app.mux.ServeHTTP(rr, missingCSRF)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d body=%s", rr.Code, rr.Body.String())
	}
}

func TestServerCheckMapsErrorsWithoutSecrets(t *testing.T) {
	const token = "never-render-this-token"
	tests := []struct {
		name    string
		err     error
		code    string
		message string
	}{
		{"unauthorized", &xui.Error{Kind: xui.ErrorUnauthorized, Message: token}, "unauthorized", "API-токен отклонён панелью"},
		{"unsupported version", &xui.Error{Kind: xui.ErrorUnsupportedVersion, Message: token}, "unsupported_version", "Версия 3x-ui не поддерживается"},
		{"transport", &xui.Error{Kind: xui.ErrorTransport, Message: token}, "transport", "Не удалось подключиться к панели"},
		{"decode", &xui.Error{Kind: xui.ErrorDecode, Message: token}, "decode", "Панель вернула некорректный ответ"},
		{"API", &xui.Error{Kind: xui.ErrorAPI, Message: token}, "api", "Панель отклонила запрос"},
		{"unknown", errors.New("internal " + token), "connection_failed", "Не удалось проверить подключение"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newServerTestApp(t, "")
			app.setChecker(func(context.Context, storage.Server, string) (xui.ServerStatus, error) {
				return xui.ServerStatus{}, tt.err
			})
			rr := app.request(t, http.MethodPost, "/dashboard/servers/check", serverValues("", "node", "https://panel.example", "/", token))
			assertJSONResponse(t, rr, http.StatusBadGateway, map[string]any{
				"ok":      false,
				"code":    tt.code,
				"message": tt.message,
			})
			if strings.Contains(rr.Body.String(), token) {
				t.Fatalf("response leaked token: %s", rr.Body.String())
			}
		})
	}
}

func TestServerCheckNeverEchoesTokenAsPanelVersion(t *testing.T) {
	const token = "echoed-secret"
	app := newServerTestApp(t, "")
	app.setChecker(func(context.Context, storage.Server, string) (xui.ServerStatus, error) {
		return xui.ServerStatus{PanelVersion: "v3.4.2-" + token}, nil
	})
	rr := app.request(t, http.MethodPost, "/dashboard/servers/check", serverValues("", "node", "https://panel.example", "/", token))
	assertJSONResponse(t, rr, http.StatusBadGateway, map[string]any{
		"ok": false, "code": "decode", "message": "Панель вернула некорректный ответ",
	})
	if strings.Contains(rr.Body.String(), token) {
		t.Fatalf("response echoed token: %s", rr.Body.String())
	}
}

func TestServerCheckMapsUnreadableSavedTokenToConfigurationError(t *testing.T) {
	app := newServerTestApp(t, "master-key")
	server := createTokenServer(t, app, "https://panel.example", "/", "stored-secret")
	app.setRawAPIToken(t, server.ID, "unreadable-plaintext-secret")
	app.setChecker(func(context.Context, storage.Server, string) (xui.ServerStatus, error) {
		t.Fatal("checker called with unreadable saved token")
		return xui.ServerStatus{}, nil
	})

	rr := app.request(t, http.MethodPost, "/dashboard/servers/check", serverValues(server.ID, "node", server.APIURL, server.Path, ""))
	assertJSONResponse(t, rr, http.StatusConflict, map[string]any{
		"ok":      false,
		"code":    "configuration",
		"message": "Сохранённый API-токен недоступен; замените его",
	})
	if strings.Contains(rr.Body.String(), "unreadable-plaintext-secret") {
		t.Fatalf("response leaked stored token: %s", rr.Body.String())
	}
}

func TestServerCreateChecksBeforeInsert(t *testing.T) {
	const token = "new-server-secret"
	t.Run("success", func(t *testing.T) {
		app := newServerTestApp(t, "master-key")
		app.setChecker(func(_ context.Context, srv storage.Server, gotToken string) (xui.ServerStatus, error) {
			servers, err := app.store.ListServersByUser(app.user.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(servers) != 0 {
				t.Fatal("server inserted before connection check")
			}
			if gotToken != token || srv.Name != "node" {
				t.Fatalf("server=%+v token=%q", srv, gotToken)
			}
			return xui.ServerStatus{PanelVersion: "3.4.2"}, nil
		})

		rr := app.request(t, http.MethodPost, "/dashboard/servers/new", serverValues("", "node", "https://panel.example", "/", token))
		if rr.Code != http.StatusSeeOther {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		servers, err := app.store.ListServersByUser(app.user.ID)
		if err != nil || len(servers) != 1 || servers[0].APIToken != token {
			t.Fatalf("servers=%+v err=%v", servers, err)
		}
	})

	t.Run("connection failure", func(t *testing.T) {
		app := newServerTestApp(t, "master-key")
		app.setChecker(func(context.Context, storage.Server, string) (xui.ServerStatus, error) {
			return xui.ServerStatus{}, errors.New("outbound failed with " + token)
		})
		rr := app.request(t, http.MethodPost, "/dashboard/servers/new", serverValues("", "node", "https://panel.example", "/", token))
		if rr.Code != http.StatusBadGateway {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
		}
		servers, err := app.store.ListServersByUser(app.user.ID)
		if err != nil || len(servers) != 0 {
			t.Fatalf("servers=%+v err=%v", servers, err)
		}
		if body := rr.Body.String(); strings.Contains(body, token) || !strings.Contains(body, "Не удалось проверить подключение") {
			t.Fatalf("unsafe response: %s", body)
		}
	})
}

func TestServerCreateRequiresToken(t *testing.T) {
	app := newServerTestApp(t, "master-key")
	app.setChecker(func(context.Context, storage.Server, string) (xui.ServerStatus, error) {
		t.Fatal("checker called without a token")
		return xui.ServerStatus{}, nil
	})
	rr := app.request(t, http.MethodPost, "/dashboard/servers/new", serverValues("", "node", "https://panel.example", "/", ""))
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "API-токен обязателен") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	servers, err := app.store.ListServersByUser(app.user.ID)
	if err != nil || len(servers) != 0 {
		t.Fatalf("servers=%+v err=%v", servers, err)
	}
}

func TestServerCreateWithoutMasterKeyStoresNothing(t *testing.T) {
	const token = "cannot-persist-secret"
	app := newServerTestApp(t, "")
	checked := false
	app.setChecker(func(context.Context, storage.Server, string) (xui.ServerStatus, error) {
		checked = true
		return xui.ServerStatus{PanelVersion: "3.4.2"}, nil
	})
	rr := app.request(t, http.MethodPost, "/dashboard/servers/new", serverValues("", "node", "https://panel.example", "/", token))
	if !checked {
		t.Fatal("connection was not checked before persistence")
	}
	if rr.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	servers, err := app.store.ListServersByUser(app.user.ID)
	if err != nil || len(servers) != 0 {
		t.Fatalf("servers=%+v err=%v", servers, err)
	}
	if body := rr.Body.String(); strings.Contains(body, token) || !strings.Contains(body, "master_key") {
		t.Fatalf("unsafe response: %s", body)
	}
}

func TestServerEditBlankTokenRetainsCiphertextWithoutConnectionCheck(t *testing.T) {
	app := newServerTestApp(t, "master-key")
	server := createTokenServer(t, app, "https://panel.example", "/admin/", "stored-secret")
	beforeCiphertext := app.rawAPIToken(t, server.ID)
	app.setChecker(func(context.Context, storage.Server, string) (xui.ServerStatus, error) {
		t.Fatal("name/host-only edit checked the panel")
		return xui.ServerStatus{}, nil
	})

	form := serverValues(server.ID, "renamed", server.APIURL, server.Path, "")
	form.Set("host_override", "vpn.example")
	rr := app.request(t, http.MethodPost, "/dashboard/servers/"+strconv.FormatInt(server.ID, 10), form)
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	after, err := app.store.ServerByID(app.user.ID, server.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "renamed" || after.HostOverride != "vpn.example" || after.APIToken != "stored-secret" {
		t.Fatalf("server=%+v", after)
	}
	if afterCiphertext := app.rawAPIToken(t, server.ID); afterCiphertext != beforeCiphertext {
		t.Fatalf("ciphertext changed: before=%q after=%q", beforeCiphertext, afterCiphertext)
	}
}

func TestServerEditConnectionChangesCheckSavedTokenBeforeUpdate(t *testing.T) {
	tests := []struct {
		name   string
		change func(url.Values)
	}{
		{"API URL", func(v url.Values) { v.Set("api_url", "https://new-panel.example") }},
		{"panel path", func(v url.Values) { v.Set("path", "/new-path/") }},
		{"TLS verification", func(v url.Values) { v.Set("insecure_skip_verify", "1") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := newServerTestApp(t, "master-key")
			server := createTokenServer(t, app, "https://panel.example", "/admin/", "stored-secret")
			form := serverValues(server.ID, "renamed", server.APIURL, server.Path, "")
			tt.change(form)
			checked := false
			app.setChecker(func(_ context.Context, candidate storage.Server, token string) (xui.ServerStatus, error) {
				checked = true
				stored, err := app.store.ServerByID(app.user.ID, server.ID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Name != "node" || token != "stored-secret" {
					t.Fatalf("stored=%+v token=%q", stored, token)
				}
				if candidate.APIURL != form.Get("api_url") || candidate.Path != normalizedPath(form.Get("path")) || candidate.InsecureSkipVerify != (form.Get("insecure_skip_verify") == "1") {
					t.Fatalf("candidate=%+v form=%v", candidate, form)
				}
				return xui.ServerStatus{PanelVersion: "3.4.2"}, nil
			})

			rr := app.request(t, http.MethodPost, "/dashboard/servers/"+strconv.FormatInt(server.ID, 10), form)
			if rr.Code != http.StatusSeeOther || !checked {
				t.Fatalf("status=%d checked=%v body=%s", rr.Code, checked, rr.Body.String())
			}
		})
	}
}

func TestServerEditFailedReplacementPreservesMetadataAndCiphertext(t *testing.T) {
	const replacement = "replacement-must-not-render"
	app := newServerTestApp(t, "master-key")
	server := createTokenServer(t, app, "https://panel.example", "/admin/", "stored-secret")
	beforeCiphertext := app.rawAPIToken(t, server.ID)
	app.setChecker(func(_ context.Context, candidate storage.Server, token string) (xui.ServerStatus, error) {
		if candidate.Name != "renamed" || candidate.APIURL != "https://new.example" || token != replacement {
			t.Fatalf("candidate=%+v token=%q", candidate, token)
		}
		return xui.ServerStatus{}, errors.New("checker leaked " + replacement)
	})

	form := serverValues(server.ID, "renamed", "https://new.example", "/new/", replacement)
	rr := app.request(t, http.MethodPost, "/dashboard/servers/"+strconv.FormatInt(server.ID, 10), form)
	if rr.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	after, err := app.store.ServerByID(app.user.ID, server.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != server.Name || after.APIURL != server.APIURL || after.Path != server.Path || after.APIToken != "stored-secret" {
		t.Fatalf("server changed after failed replacement: %+v", after)
	}
	if got := app.rawAPIToken(t, server.ID); got != beforeCiphertext {
		t.Fatalf("ciphertext changed: before=%q after=%q", beforeCiphertext, got)
	}
	if body := rr.Body.String(); strings.Contains(body, replacement) || strings.Contains(body, "checker leaked") {
		t.Fatalf("unsafe response: %s", body)
	}
}

func TestServerEditFailedSavedTokenCheckPreservesMetadataAndCiphertext(t *testing.T) {
	app := newServerTestApp(t, "master-key")
	server := createTokenServer(t, app, "https://panel.example", "/admin/", "stored-secret")
	beforeCiphertext := app.rawAPIToken(t, server.ID)
	app.setChecker(func(_ context.Context, candidate storage.Server, token string) (xui.ServerStatus, error) {
		if candidate.Name != "renamed" || candidate.APIURL != "https://new.example" || token != "stored-secret" {
			t.Fatalf("candidate=%+v token=%q", candidate, token)
		}
		return xui.ServerStatus{}, &xui.Error{Kind: xui.ErrorTransport, Message: "unsafe internal detail"}
	})

	form := serverValues(server.ID, "renamed", "https://new.example", "/new/", "")
	rr := app.request(t, http.MethodPost, "/dashboard/servers/"+strconv.FormatInt(server.ID, 10), form)
	if rr.Code != http.StatusBadGateway || !strings.Contains(rr.Body.String(), "Не удалось подключиться к панели") {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	after, err := app.store.ServerByID(app.user.ID, server.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != server.Name || after.APIURL != server.APIURL || after.Path != server.Path || after.APIToken != "stored-secret" {
		t.Fatalf("server changed after failed saved-token check: %+v", after)
	}
	if got := app.rawAPIToken(t, server.ID); got != beforeCiphertext {
		t.Fatalf("ciphertext changed: before=%q after=%q", beforeCiphertext, got)
	}
}

func TestServerEditReplacementWithoutMasterKeyChangesNothing(t *testing.T) {
	const replacement = "replacement-without-master"
	app := newServerTestApp(t, "")
	server, err := app.store.CreateServer(&storage.Server{
		UserID: app.user.ID, Name: "node", APIURL: "https://panel.example", Path: "/admin/",
	})
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := secrets.New("original-key").Encrypt("stored-secret")
	if err != nil {
		t.Fatal(err)
	}
	app.setRawAPIToken(t, server.ID, ciphertext)
	checked := false
	app.setChecker(func(context.Context, storage.Server, string) (xui.ServerStatus, error) {
		checked = true
		return xui.ServerStatus{PanelVersion: "3.4.2"}, nil
	})

	rr := app.request(t, http.MethodPost, "/dashboard/servers/"+strconv.FormatInt(server.ID, 10), serverValues(server.ID, "renamed", "https://new.example", "/new/", replacement))
	if !checked || rr.Code != http.StatusConflict {
		t.Fatalf("checked=%v status=%d body=%s", checked, rr.Code, rr.Body.String())
	}
	after, err := app.store.ServerByID(app.user.ID, server.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "node" || after.APIURL != "https://panel.example" || after.Path != "/admin/" {
		t.Fatalf("metadata changed: %+v", after)
	}
	if got := app.rawAPIToken(t, server.ID); got != ciphertext {
		t.Fatalf("ciphertext changed: before=%q after=%q", ciphertext, got)
	}
	if body := rr.Body.String(); strings.Contains(body, replacement) || !strings.Contains(body, "master_key") {
		t.Fatalf("unsafe response: %s", body)
	}
}

func TestServerEditSuccessfulReplacementTriggersAggregator(t *testing.T) {
	app := newServerTestApp(t, "master-key")
	server := createTokenServer(t, app, "http://127.0.0.1:1", "/", "stored-secret")
	initialBuild := app.handler.Agg.Snapshot().BuiltAt
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go app.handler.Agg.Run(ctx)
	waitFor(t, func() bool { return app.handler.Agg.Snapshot().BuiltAt.After(initialBuild) })
	baselineBuild := app.handler.Agg.Snapshot().BuiltAt
	app.setChecker(func(context.Context, storage.Server, string) (xui.ServerStatus, error) {
		return xui.ServerStatus{PanelVersion: "3.4.2"}, nil
	})

	rr := app.request(t, http.MethodPost, "/dashboard/servers/"+strconv.FormatInt(server.ID, 10), serverValues(server.ID, "node", server.APIURL, "/", "replacement-secret"))
	if rr.Code != http.StatusSeeOther {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
	}
	waitFor(t, func() bool { return app.handler.Agg.Snapshot().BuiltAt.After(baselineBuild) })
	after, err := app.store.ServerByID(app.user.ID, server.ID)
	if err != nil || after.APIToken != "replacement-secret" {
		t.Fatalf("server=%+v err=%v", after, err)
	}
}

func serverValues(id any, name, apiURL, path, token string) url.Values {
	values := url.Values{
		"name":      {name},
		"api_url":   {apiURL},
		"path":      {path},
		"api_token": {token},
	}
	if fmt.Sprint(id) != "" {
		values.Set("server_id", fmt.Sprint(id))
	}
	return values
}

func createTokenServer(t *testing.T, app *serverTestApp, apiURL, path, token string) *storage.Server {
	t.Helper()
	server, err := app.store.CreateServer(&storage.Server{
		UserID: app.user.ID, Name: "node", APIURL: apiURL, Path: path, APIToken: token,
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func assertJSONResponse(t *testing.T, rr *httptest.ResponseRecorder, status int, expected map[string]any) {
	t.Helper()
	if rr.Code != status {
		t.Fatalf("status=%d want=%d body=%s", rr.Code, status, rr.Body.String())
	}
	if contentType := rr.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("Content-Type=%q", contentType)
	}
	var actual map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &actual); err != nil {
		t.Fatalf("decode response: %v body=%s", err, rr.Body.String())
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("response=%v want=%v", actual, expected)
	}
}

func normalizedPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "/"
	}
	if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	if !strings.HasSuffix(value, "/") {
		value += "/"
	}
	return value
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met")
}
