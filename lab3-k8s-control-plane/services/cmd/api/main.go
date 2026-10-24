// api — HTTP-сервис магазина: принимает заказы и отдаёт их список.
//
//	GET  /health  — ok, или 503 при HEALTH_FAIL=true
//	POST /order   — создать заказ, тело {"item":"book","quantity":2} (пустое тело тоже можно)
//	GET  /orders  — список заказов, ?limit=N (по умолчанию 50)
//	GET  /version — версия сборки и имя пода
//	GET  /metrics — метрики Prometheus
//
// Служебный порт ADMIN_PORT (по умолчанию 8081, в Service не публикуется):
//
//	GET  /prestop?seconds=N — для preStop-хука kubelet: включает drain и держит ответ N секунд
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ogyreck/shop/internal/common"
)

var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "api_requests_total",
		Help: "Общее число HTTP-запросов",
	}, []string{"method", "path", "status"})

	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "api_request_duration_seconds",
		Help:    "Длительность обработки запроса",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	}, []string{"method", "path"})

	dbErrorsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "api_db_errors_total",
		Help: "Число запросов, на которых не удалось сходить в postgres",
	})

	ordersCreated = promauto.NewCounter(prometheus.CounterOpts{
		Name: "shop_orders_created_total",
		Help: "Число созданных заказов",
	})
)

type order struct {
	ID          int64      `json:"id"`
	Item        string     `json:"item"`
	Quantity    int        `json:"quantity"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	ProcessedAt *time.Time `json:"processed_at,omitempty"`
	ProcessedBy *string    `json:"processed_by,omitempty"`
}

type server struct {
	db  *pgxpool.Pool
	log *slog.Logger
	// draining включается preStop-хуком: под уже выведен из EndpointSlice, но клиенты
	// с keep-alive соединениями всё ещё ходят в него напрямую (conntrack помнит DNAT).
	draining atomic.Bool
}

// closeWhenDraining в режиме drain отвечает с Connection: close — клиент закрывает соединение
// сам, после полного ответа, и следующий запрос открывает заново через Service, уже на живой под.
// Без этого keep-alive соединения закрывает srv.Shutdown по SIGTERM, и запрос, отправленный
// в этот момент в простаивающее соединение, получает EOF.
func (s *server) closeWhenDraining(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.draining.Load() {
			w.Header().Set("Connection", "close")
		}
		next.ServeHTTP(w, r)
	})
}

// handlePreStop вызывается kubelet-ом как preStop httpGet: включает drain и держит ответ
// seconds секунд — kubelet ждёт ответа, и только потом шлёт SIGTERM.
func (s *server) handlePreStop(w http.ResponseWriter, r *http.Request) {
	seconds, err := strconv.Atoi(r.URL.Query().Get("seconds"))
	if err != nil || seconds < 0 || seconds > 60 {
		seconds = 5
	}
	s.draining.Store(true)
	s.log.Info("preStop: drain включён", "seconds", seconds)
	select {
	case <-time.After(time.Duration(seconds) * time.Second):
	case <-r.Context().Done():
	}
	w.WriteHeader(http.StatusOK)
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// observe снимает метрики RED; path — шаблон маршрута, а не сырой URL, чтобы не плодить серии.
func (s *server) observe(path string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next(sw, r)
		requestsTotal.WithLabelValues(r.Method, path, strconv.Itoa(sw.status)).Inc()
		requestDuration.WithLabelValues(r.Method, path).Observe(time.Since(start).Seconds())
		if path != "/health" {
			s.log.Info("запрос", "method", r.Method, "path", path, "status", sw.status,
				"duration_ms", time.Since(start).Milliseconds())
		}
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func (s *server) dbError(w http.ResponseWriter, err error) {
	dbErrorsTotal.Inc()
	s.log.Error("ошибка БД", "err", err)
	http.Error(w, "database unavailable", http.StatusServiceUnavailable)
}

func (s *server) handleCreateOrder(w http.ResponseWriter, r *http.Request) {
	req := struct {
		Item     string `json:"item"`
		Quantity int    `json:"quantity"`
	}{}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Item == "" {
		req.Item = "widget"
	}
	if req.Quantity <= 0 {
		req.Quantity = 1
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var o order
	err := s.db.QueryRow(ctx,
		`INSERT INTO orders (item, quantity) VALUES ($1, $2)
		 RETURNING id, item, quantity, status, created_at, processed_at, processed_by`,
		req.Item, req.Quantity,
	).Scan(&o.ID, &o.Item, &o.Quantity, &o.Status, &o.CreatedAt, &o.ProcessedAt, &o.ProcessedBy)
	if err != nil {
		s.dbError(w, err)
		return
	}
	ordersCreated.Inc()
	writeJSON(w, http.StatusCreated, o)
}

func (s *server) handleListOrders(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := s.db.Query(ctx,
		`SELECT id, item, quantity, status, created_at, processed_at, processed_by
		 FROM orders ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		s.dbError(w, err)
		return
	}
	orders, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (order, error) {
		var o order
		err := row.Scan(&o.ID, &o.Item, &o.Quantity, &o.Status, &o.CreatedAt, &o.ProcessedAt, &o.ProcessedBy)
		return o, err
	})
	if err != nil {
		s.dbError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, orders)
}

func main() {
	log := common.NewLogger("api")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := common.NewPool(ctx)
	if err != nil {
		log.Error("не удалось настроить подключение к БД", "err", err)
		return
	}
	defer pool.Close()
	// Схему создаём в фоне: api должен отвечать на /health, даже пока postgres поднимается.
	go common.EnsureSchema(ctx, pool, log)

	s := &server{db: pool, log: log}
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("GET /health", s.observe("/health", common.HandleHealth))
	mux.HandleFunc("GET /version", s.observe("/version", common.HandleVersion))
	mux.HandleFunc("POST /order", s.observe("/order", s.handleCreateOrder))
	mux.HandleFunc("GET /orders", s.observe("/orders", s.handleListOrders))

	admin := http.NewServeMux()
	admin.HandleFunc("GET /prestop", s.handlePreStop)
	adminSrv := &http.Server{Addr: ":" + common.Env("ADMIN_PORT", "8081"), Handler: admin, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := adminSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("admin-сервер упал", "err", err)
		}
	}()
	defer adminSrv.Close()

	common.Serve(ctx, s.closeWhenDraining(mux), log)
}
