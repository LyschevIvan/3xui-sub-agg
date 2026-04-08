package httpapi

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/LyschevIvan/3xui-sub-agg/internal/aggregator"
)

type Server struct {
	Agg *aggregator.Aggregator
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/sub/", s.subscription)
	mux.HandleFunc("/debug/snapshot", s.debugSnapshot)
	return mux
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// GET /sub/{email} — подписка для клиента.
func (s *Server) subscription(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimPrefix(r.URL.Path, "/sub/")
	email = strings.Trim(email, "/")
	if email == "" {
		http.Error(w, "email required", http.StatusBadRequest)
		return
	}

	snap := s.Agg.Snapshot()
	idx := snap.Index()
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

// GET /debug/snapshot — человекочитаемое состояние (без UUID, без ссылок целиком).
func (s *Server) debugSnapshot(w http.ResponseWriter, r *http.Request) {
	snap := s.Agg.Snapshot()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "built_at: %s\n\n", snap.BuiltAt.Format("2006-01-02 15:04:05"))
	for _, s := range snap.Servers {
		fmt.Fprintf(w, "[%s] host=%s fetched=%s entries=%d", s.Name, s.PublicHost, s.FetchedAt.Format("15:04:05"), len(s.Entries))
		if s.Err != nil {
			fmt.Fprintf(w, " ERR=%v", s.Err)
		}
		fmt.Fprintln(w)
		for _, e := range s.Entries {
			mark := " "
			if !e.Enabled {
				mark = "x"
			}
			fmt.Fprintf(w, "  [%s] %s\n", mark, e.Email)
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "unique emails:")
	for _, e := range snap.Emails() {
		fmt.Fprintf(w, "  %s\n", e)
	}
}
