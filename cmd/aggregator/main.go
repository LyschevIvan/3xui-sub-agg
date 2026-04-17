package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/LyschevIvan/3xui-sub-agg/internal/aggregator"
	"github.com/LyschevIvan/3xui-sub-agg/internal/auth"
	"github.com/LyschevIvan/3xui-sub-agg/internal/config"
	"github.com/LyschevIvan/3xui-sub-agg/internal/httpapi"
	"github.com/LyschevIvan/3xui-sub-agg/internal/storage"
	"github.com/LyschevIvan/3xui-sub-agg/internal/webui"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	store, err := storage.Open(cfg.DBPath)
	if err != nil {
		log.Fatalf("storage: %v", err)
	}
	defer store.Close()

	if err := ensureAdmin(store, cfg.Admin.Login, cfg.Admin.Password); err != nil {
		log.Fatalf("bootstrap admin: %v", err)
	}

	authSvc := &auth.Service{Store: store, Secure: cfg.CookiesSecure}
	agg := aggregator.New(cfg, store)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go agg.Run(ctx)

	ui, err := webui.New(cfg, store, authSvc, agg)
	if err != nil {
		log.Fatalf("webui: %v", err)
	}

	mux := http.NewServeMux()
	ui.Mount(mux)
	(&httpapi.Server{Agg: agg, Store: store}).Mount(mux)

	handler := authSvc.Middleware(mux)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Периодически чистим истёкшие сессии.
	go func() {
		t := time.NewTicker(1 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = store.PurgeExpiredSessions()
			}
		}
	}()

	go func() {
		log.Printf("listening on %s", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	<-ctx.Done()
	log.Println("shutting down")
	shutdownCtx, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	_ = srv.Shutdown(shutdownCtx)
}

// ensureAdmin создаёт bootstrap-админа, если его ещё нет в БД.
// Если пользователь с таким логином уже существует, но не админ — апгрейдит до админа
// и обновляет пароль (это позволяет сбросить пароль через конфиг).
func ensureAdmin(store *storage.Store, login, password string) error {
	u, err := store.UserByLogin(login)
	if err != nil && !errors.Is(err, storage.ErrNotFound) {
		return err
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	if u == nil {
		if _, err := store.CreateUser(login, hash, true); err != nil {
			return err
		}
		log.Printf("bootstrap: admin user %q создан", login)
		return nil
	}
	return nil
}
