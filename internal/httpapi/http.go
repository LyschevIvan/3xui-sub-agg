package httpapi

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/LyschevIvan/3xui-sub-agg/internal/aggregator"
	"github.com/LyschevIvan/3xui-sub-agg/internal/ratelimit"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
)

type Server struct {
	Agg   subscriptionSource
	Store *storage.Store

	// Опциональный rate-limit на /sub/ — против перебора email/subId по IP.
	// Если nil — лимит не применяется.
	SubLimiter *ratelimit.Limiter
}

type subscriptionSource interface {
	ResolveSubscription(context.Context, int64, string) (aggregator.SubscriptionResult, error)
}

// Mount навешивает публичные эндпоинты на mux.
// /sub/{prefix}/{subId} — подписка с пер-юзер префиксом (prefix — непубличный токен).
// /healthz — liveness.
func (s *Server) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", s.healthz)
	sub := s.subscription
	if s.SubLimiter != nil {
		sub = s.SubLimiter.Wrap(sub)
	}
	mux.HandleFunc("/sub/", sub)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// GET /sub/{prefix}/{subId} — обычные пользователи (prefix непубличный)
// GET /sub/{subId}           — админ (legacy URL без prefix)
//
// Ключ подписки — subId клиента в 3x-ui. Несколько inbound'ов с одним subId
// (на одном или разных серверах пользователя) склеиваются в одну ссылку.
// Клиенты без subId через этот эндпоинт не доступны.
func (s *Server) subscription(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/sub/")
	rest = strings.Trim(rest, "/")
	if rest == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var (
		user  *storage.User
		subID string
		err   error
	)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		user, err = s.Store.UserBySubPrefix(parts[0])
		subID = parts[1]
	} else {
		// legacy: /sub/{subId} — админ с пустым префиксом
		user, err = s.Store.UserBySubPrefix("")
		subID = parts[0]
	}
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	result, err := s.Agg.ResolveSubscription(r.Context(), user.ID, subID)
	if err != nil {
		switch {
		case errors.Is(err, aggregator.ErrSubscriptionNotFound):
			http.Error(w, "not found", http.StatusNotFound)
		case errors.Is(err, aggregator.ErrSubscriptionUnavailable):
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		default:
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
		return
	}
	if len(result.Links) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	links := append([]string(nil), result.Links...)
	sort.Strings(links)

	var sb strings.Builder
	for _, link := range links {
		sb.WriteString(link)
		sb.WriteString("\n")
	}
	payload := base64.StdEncoding.EncodeToString([]byte(sb.String()))

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Profile-Update-Interval", "12")
	w.Header().Set("Profile-Title", subID)
	_, _ = w.Write([]byte(payload))
}
