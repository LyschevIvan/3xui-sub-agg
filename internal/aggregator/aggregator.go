package aggregator

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ivanlyschev/3xui-sub-agg/internal/config"
	"github.com/ivanlyschev/3xui-sub-agg/internal/link"
	"github.com/ivanlyschev/3xui-sub-agg/internal/xui"
)

// ClientEntry — один «кусочек» для итоговой подписки.
type ClientEntry struct {
	ServerName string
	Email      string
	Link       string
	Enabled    bool
}

// ServerSnapshot — состояние одного 3x-ui сервера.
type ServerSnapshot struct {
	Name      string
	PublicHost string
	Entries   []ClientEntry
	FetchedAt time.Time
	Err       error
}

// Snapshot — агрегированное состояние всех серверов.
type Snapshot struct {
	Servers []ServerSnapshot
	BuiltAt time.Time
}

// Index возвращает map email -> []ClientEntry (только enabled клиенты).
func (s *Snapshot) Index() map[string][]ClientEntry {
	m := make(map[string][]ClientEntry)
	for _, srv := range s.Servers {
		for _, e := range srv.Entries {
			if !e.Enabled {
				continue
			}
			m[e.Email] = append(m[e.Email], e)
		}
	}
	return m
}

// Emails возвращает отсортированный уникальный список email'ов.
func (s *Snapshot) Emails() []string {
	set := map[string]struct{}{}
	for _, srv := range s.Servers {
		for _, e := range srv.Entries {
			if e.Enabled {
				set[e.Email] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	return out
}

type serverClient struct {
	cfg    config.Server
	client *xui.Client
	host   string
}

type Aggregator struct {
	cfg     *config.Config
	servers []*serverClient
	snap    atomic.Pointer[Snapshot]
}

func New(cfg *config.Config) (*Aggregator, error) {
	a := &Aggregator{cfg: cfg}
	for _, s := range cfg.Servers {
		c, err := xui.New(s.APIURL, s.Path, s.Username, s.Password, s.InsecureSkipVerify, cfg.RequestTimeout)
		if err != nil {
			return nil, fmt.Errorf("xui client %q: %w", s.Name, err)
		}
		host := normalizeHost(s.HostOverride)
		if host == "" {
			u, err := url.Parse(s.APIURL)
			if err != nil {
				return nil, fmt.Errorf("parse api_url %q: %w", s.APIURL, err)
			}
			host = u.Hostname()
		}
		a.servers = append(a.servers, &serverClient{cfg: s, client: c, host: host})
	}
	// пустой начальный снапшот
	a.snap.Store(&Snapshot{BuiltAt: time.Now()})
	return a, nil
}

func (a *Aggregator) Snapshot() *Snapshot { return a.snap.Load() }

// normalizeHost принимает либо чистый хост ("1.2.3.4", "vpn.example.com"),
// либо полный URL ("http://1.2.3.4:8080") и возвращает голый hostname.
func normalizeHost(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	return strings.TrimSuffix(s, "/")
}

// Run блокируется до отмены контекста, периодически обновляя snapshot.
func (a *Aggregator) Run(ctx context.Context) {
	a.refresh(ctx)
	t := time.NewTicker(a.cfg.RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.refresh(ctx)
		}
	}
}

func (a *Aggregator) refresh(ctx context.Context) {
	prev := a.snap.Load()
	prevByName := map[string]ServerSnapshot{}
	if prev != nil {
		for _, s := range prev.Servers {
			prevByName[s.Name] = s
		}
	}

	results := make([]ServerSnapshot, len(a.servers))
	var wg sync.WaitGroup
	for i, sc := range a.servers {
		wg.Add(1)
		go func(i int, sc *serverClient) {
			defer wg.Done()
			entries, err := a.fetchServer(ctx, sc)
			if err != nil {
				log.Printf("aggregator: server %q failed: %v (keeping previous data)", sc.cfg.Name, err)
				if old, ok := prevByName[sc.cfg.Name]; ok {
					old.Err = err
					results[i] = old
					return
				}
				results[i] = ServerSnapshot{Name: sc.cfg.Name, PublicHost: sc.host, FetchedAt: time.Now(), Err: err}
				return
			}
			results[i] = ServerSnapshot{
				Name:       sc.cfg.Name,
				PublicHost: sc.host,
				Entries:    entries,
				FetchedAt:  time.Now(),
			}
		}(i, sc)
	}
	wg.Wait()

	a.snap.Store(&Snapshot{Servers: results, BuiltAt: time.Now()})
	log.Printf("aggregator: refreshed, servers=%d", len(results))
}

func (a *Aggregator) fetchServer(ctx context.Context, sc *serverClient) ([]ClientEntry, error) {
	inbounds, err := sc.client.ListInbounds(ctx)
	if err != nil {
		return nil, err
	}
	var out []ClientEntry
	for _, ib := range inbounds {
		if ib.Protocol != "vless" {
			// Фаза 1: только vless
			continue
		}
		ss, err := xui.ParseStream(ib.StreamSettings)
		if err != nil {
			log.Printf("aggregator: %s inbound %d streamSettings parse: %v", sc.cfg.Name, ib.ID, err)
			continue
		}
		clients, err := xui.ParseClients(ib.Settings)
		if err != nil {
			log.Printf("aggregator: %s inbound %d settings parse: %v", sc.cfg.Name, ib.ID, err)
			continue
		}
		for _, c := range clients {
			remark := fmt.Sprintf("%s — %s", sc.cfg.Name, ib.Remark)
			if ib.Remark == "" {
				remark = sc.cfg.Name
			}
			ln, err := link.BuildVless(sc.host, ib.Port, remark, c, ss)
			if err != nil {
				log.Printf("aggregator: %s inbound %d build link: %v", sc.cfg.Name, ib.ID, err)
				continue
			}
			out = append(out, ClientEntry{
				ServerName: sc.cfg.Name,
				Email:      c.Email,
				Link:       ln,
				Enabled:    ib.Enable && c.Enable,
			})
		}
	}
	return out, nil
}
