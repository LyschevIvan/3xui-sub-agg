package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LyschevIvan/3xui-sub-agg/internal/aggregator"
	"github.com/LyschevIvan/3xui-sub-agg/internal/secrets"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
)

type fakeSubscriptionSource struct {
	result aggregator.SubscriptionResult
	err    error
	userID int64
	subID  string
}

func (f *fakeSubscriptionSource) ResolveSubscription(_ context.Context, userID int64, subID string) (aggregator.SubscriptionResult, error) {
	f.userID = userID
	f.subID = subID
	return f.result, f.err
}

func newHTTPTestServer(t *testing.T, source *fakeSubscriptionSource) (*storage.User, http.Handler) {
	t.Helper()
	store, err := storage.Open(filepath.Join(t.TempDir(), "data.db"), secrets.New("http-task-7-key"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	user, err := store.CreateUser("owner", "unused", false)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&Server{Agg: source, Store: store}).Mount(mux)
	return user, mux
}

func TestSubscriptionHTTPUsesNativeResolverAndPreservesProfileResponse(t *testing.T) {
	source := &fakeSubscriptionSource{result: aggregator.SubscriptionResult{
		Links: []string{"vmess://z", "ss://a", "trojan://m"}, Partial: true,
	}}
	user, handler := newHTTPTestServer(t, source)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sub/"+user.SubPrefix+"/exact", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", rr.Code, rr.Body.String())
	}
	decoded, err := base64.StdEncoding.DecodeString(rr.Body.String())
	if err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if got, want := string(decoded), "ss://a\ntrojan://m\nvmess://z\n"; got != want {
		t.Fatalf("payload=%q want=%q", got, want)
	}
	if source.userID != user.ID || source.subID != "exact" {
		t.Fatalf("resolver user/sub=%d/%q", source.userID, source.subID)
	}
	if got := rr.Header().Get("Profile-Update-Interval"); got != "12" {
		t.Fatalf("Profile-Update-Interval=%q", got)
	}
	if got := rr.Header().Get("Profile-Title"); got != "exact" {
		t.Fatalf("Profile-Title=%q", got)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type=%q", got)
	}
}

func TestSubscriptionHTTPMapsOnlyKnownResolverErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"not found", aggregator.ErrSubscriptionNotFound, http.StatusNotFound},
		{"unavailable", aggregator.ErrSubscriptionUnavailable, http.StatusServiceUnavailable},
		{"unknown", errors.New("raw panel secret and token"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &fakeSubscriptionSource{err: tc.err}
			user, handler := newHTTPTestServer(t, source)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sub/"+user.SubPrefix+"/exact", nil))
			if rr.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%q", rr.Code, tc.want, rr.Body.String())
			}
			for _, secret := range []string{"raw panel secret", "token", "temporarily unavailable", "subscription not found"} {
				if strings.Contains(rr.Body.String(), secret) {
					t.Fatalf("body leaked %q: %q", secret, rr.Body.String())
				}
			}
		})
	}
}

func TestSubscriptionHTTPRejectsUnknownProfileBeforeResolver(t *testing.T) {
	source := &fakeSubscriptionSource{result: aggregator.SubscriptionResult{Links: []string{"vless://secret"}}}
	_, handler := newHTTPTestServer(t, source)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sub/unknown-prefix/exact", nil))
	if rr.Code != http.StatusNotFound || source.userID != 0 || source.subID != "" {
		t.Fatalf("status=%d resolver user/sub=%d/%q", rr.Code, source.userID, source.subID)
	}
}
