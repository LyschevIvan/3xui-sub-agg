package httpapi

import (
	"encoding/base64"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/LyschevIvan/3xui-sub-agg/internal/aggregator"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
)

type Server struct {
	Agg   *aggregator.Aggregator
	Store *storage.Store
}

// Mount навешивает публичные эндпоинты на mux.
// /sub/{prefix}/{email} — подписка с пер-юзер префиксом (prefix — непубличный токен).
// /healthz — liveness.
func (s *Server) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/sub/", s.subscription)
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// GET /sub/{prefix}/{email}   — обычные пользователи (prefix непубличный)
// GET /sub/{email}             — админ (legacy, без prefix)
func (s *Server) subscription(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/sub/")
	rest = strings.Trim(rest, "/")
	if rest == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var (
		user  *storage.User
		email string
		err   error
	)
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		// /sub/{prefix}/{email}
		user, err = s.Store.UserBySubPrefix(parts[0])
		email = parts[1]
	} else {
		// /sub/{email} — ищем админа с пустым префиксом
		user, err = s.Store.UserBySubPrefix("")
		email = parts[0]
	}
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	snap := s.Agg.Snapshot()
	idx := snap.UserIndex(user.ID)
	entries, ok := idx[email]
	if !ok || len(entries) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].ServerName < entries[j].ServerName })

	var sb strings.Builder
	for _, e := range entries {
		sb.WriteString(e.Link)
		sb.WriteString("\n")
	}
	payload := base64.StdEncoding.EncodeToString([]byte(sb.String()))

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Profile-Update-Interval", "12")
	w.Header().Set("Profile-Title", email)
	_, _ = w.Write([]byte(payload))
}
