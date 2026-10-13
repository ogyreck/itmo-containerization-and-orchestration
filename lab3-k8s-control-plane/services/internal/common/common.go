// Общая часть api и worker: подключение к postgres, схема, /health, логи, запуск HTTP.
package common

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Version подставляется при сборке: -ldflags "-X .../common.Version=0.2.0".
var Version = "dev"

func Env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func Hostname() string {
	h, _ := os.Hostname()
	return h
}

func NewLogger(service string) *slog.Logger {
	l := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("service", service, "version", Version)
	slog.SetDefault(l)
	return l
}

// healthFailing читает HEALTH_FAIL на каждый запрос, но меняется переменная
// только пересозданием пода — для лабы этого и нужно.
func healthFailing() bool {
	v := strings.ToLower(os.Getenv("HEALTH_FAIL"))
	return v == "true" || v == "1" || v == "yes"
}

// HandleHealth отдаёт ok, а при HEALTH_FAIL=true — 503.
// Состояние БД сюда сознательно не подмешивается: падение postgres не должно
// валить пробы и перезапускать поды api/worker.
func HandleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-App-Version", Version)
	w.Header().Set("X-Pod", Hostname())
	if healthFailing() {
		http.Error(w, "unhealthy (HEALTH_FAIL)", http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintln(w, "ok")
}

func HandleVersion(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "%s %s\n", Version, Hostname())
}

// NewPool создаёт пул соединений. Строка берётся из DATABASE_URL (у CloudNativePG
// это ключ uri в секрете <cluster>-app), а если её нет — pgx сам читает
// PGHOST/PGPORT/PGUSER/PGPASSWORD/PGDATABASE. Пул ленивый: если БД ещё не поднялась,
// сервис всё равно стартует, а запросы к БД получат ошибку.
func NewPool(ctx context.Context) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(os.Getenv("DATABASE_URL"))
	if err != nil {
		return nil, fmt.Errorf("разбор параметров подключения: %w", err)
	}
	cfg.MaxConns = 5
	cfg.ConnConfig.ConnectTimeout = 3 * time.Second
	return pgxpool.NewWithConfig(ctx, cfg)
}

const schema = `
CREATE TABLE IF NOT EXISTS orders (
    id           BIGSERIAL PRIMARY KEY,
    item         TEXT        NOT NULL,
    quantity     INT         NOT NULL DEFAULT 1,
    status       TEXT        NOT NULL DEFAULT 'new',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    processed_at TIMESTAMPTZ,
    processed_by TEXT
);
CREATE INDEX IF NOT EXISTS orders_status_idx ON orders (status, id);
`

// EnsureSchema создаёт таблицу, повторяя попытки, пока БД недоступна.
// Advisory lock нужен, потому что схему одновременно создают все реплики api и worker,
// а параллельный CREATE TABLE IF NOT EXISTS в postgres может упасть на гонке.
func EnsureSchema(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger) error {
	for {
		err := func() error {
			tx, err := pool.Begin(ctx)
			if err != nil {
				return err
			}
			defer tx.Rollback(ctx)
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(424242)`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, schema); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}()
		if err == nil {
			log.Info("схема БД готова")
			return nil
		}
		log.Warn("БД недоступна, повторю через 3с", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// Serve запускает HTTP-сервер и при отмене ctx (SIGTERM) аккуратно его гасит:
// новые соединения больше не принимаются, начатые запросы дорабатывают.
func Serve(ctx context.Context, handler http.Handler, log *slog.Logger) {
	srv := &http.Server{
		Addr:              ":" + Env("PORT", "8080"),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Info("HTTP-сервер запущен", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("HTTP-сервер упал", "err", err)
			os.Exit(1)
		}
	}()
	<-ctx.Done()
	log.Info("получен сигнал, останавливаюсь")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
}
