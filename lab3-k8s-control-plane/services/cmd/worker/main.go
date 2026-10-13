// worker — забирает новые заказы из postgres и помечает их обработанными.
//
// Каждые POLL_INTERVAL берёт до BATCH_SIZE заказов со статусом new через
// FOR UPDATE SKIP LOCKED: несколько реплик worker не возьмут один и тот же заказ.
// Наружу отдаёт /health (ok или 503 при HEALTH_FAIL=true), /version и /metrics.
package main

import (
	"context"
	"log/slog"
	"net/http"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ogyreck/shop/internal/common"
)

var (
	processedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "worker_orders_processed_total",
		Help: "Число заказов, обработанных этой репликой",
	})

	pollErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "worker_poll_errors_total",
		Help: "Число неудачных циклов опроса БД",
	})

	lastSuccess = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "worker_last_success_timestamp_seconds",
		Help: "Unix-время последнего успешного цикла опроса БД",
	})

	pendingOrders = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "shop_orders_pending",
		Help: "Число заказов в статусе new на момент последнего опроса",
	})
)

const claimSQL = `
UPDATE orders SET status = 'processed', processed_at = now(), processed_by = $1
WHERE id IN (
    SELECT id FROM orders WHERE status = 'new'
    ORDER BY id LIMIT $2
    FOR UPDATE SKIP LOCKED
)`

// pollOnce обрабатывает одну пачку заказов и обновляет метрики.
func pollOnce(ctx context.Context, db *pgxpool.Pool, batch int, delay time.Duration, log *slog.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, claimSQL, common.Hostname(), batch)
	if err != nil {
		return err
	}
	// Имитация полезной работы: заказы заблокированы, пока транзакция открыта.
	if n := tag.RowsAffected(); n > 0 && delay > 0 {
		time.Sleep(time.Duration(n) * delay)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	var pending int64
	if err := db.QueryRow(ctx, `SELECT count(*) FROM orders WHERE status = 'new'`).Scan(&pending); err != nil {
		return err
	}
	processedTotal.Add(float64(tag.RowsAffected()))
	pendingOrders.Set(float64(pending))
	lastSuccess.SetToCurrentTime()
	if tag.RowsAffected() > 0 {
		log.Info("заказы обработаны", "count", tag.RowsAffected(), "pending", pending)
	}
	return nil
}

func run(ctx context.Context, db *pgxpool.Pool, log *slog.Logger) {
	if err := common.EnsureSchema(ctx, db, log); err != nil {
		return
	}
	interval, _ := time.ParseDuration(common.Env("POLL_INTERVAL", "2s"))
	delay, _ := time.ParseDuration(common.Env("PROCESS_DELAY", "100ms"))
	batch, _ := strconv.Atoi(common.Env("BATCH_SIZE", "10"))
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if batch <= 0 {
		batch = 10
	}
	log.Info("worker запущен", "poll_interval", interval, "batch", batch, "process_delay", delay)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := pollOnce(ctx, db, batch, delay, log); err != nil && ctx.Err() == nil {
			pollErrorsTotal.Inc()
			log.Error("ошибка опроса БД", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func main() {
	log := common.NewLogger("worker")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := common.NewPool(ctx)
	if err != nil {
		log.Error("не удалось настроить подключение к БД", "err", err)
		return
	}
	defer pool.Close()
	go run(ctx, pool, log)

	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("GET /health", common.HandleHealth)
	mux.HandleFunc("GET /version", common.HandleVersion)
	common.Serve(ctx, mux, log)
}
