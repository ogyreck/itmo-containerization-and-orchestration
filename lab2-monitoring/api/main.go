// Сервис api для лабораторных: метрики RED, структурные логи с trace_id,
// трассировка OpenTelemetry и ручки для провокации нагрузки и отказов.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "api_requests_total",
		Help: "Общее число HTTP-запросов",
	}, []string{"method", "path", "status"})

	errorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "api_errors_total",
		Help: "Число запросов, завершившихся ошибкой 5xx",
	}, []string{"path"})

	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "api_request_duration_seconds",
		Help:    "Длительность обработки запроса",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 10},
	}, []string{"method", "path"})

	inFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "api_requests_in_flight",
		Help: "Число запросов в обработке прямо сейчас",
	})

	// Память, съеденная через /eat, держится здесь, чтобы её не собрал GC.
	eatenMu sync.Mutex
	eaten   [][]byte

	logger *slog.Logger
)

// statusWriter запоминает код ответа: сам http.ResponseWriter его не отдаёт.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// observe снимает метрики RED и пишет лог в JSON с trace_id текущего спана.
func observe(path string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		inFlight.Inc()
		defer inFlight.Dec()

		next(sw, r)

		if sw.status == 0 {
			sw.status = http.StatusOK
		}
		dur := time.Since(start)
		status := strconv.Itoa(sw.status)

		requestsTotal.WithLabelValues(r.Method, path, status).Inc()
		requestDuration.WithLabelValues(r.Method, path).Observe(dur.Seconds())
		if sw.status >= 500 {
			errorsTotal.WithLabelValues(path).Inc()
		}

		sc := trace.SpanContextFromContext(r.Context())
		attrs := []any{
			"method", r.Method,
			"path", path,
			"status", sw.status,
			"duration_ms", dur.Milliseconds(),
		}
		if sc.IsValid() {
			attrs = append(attrs, "trace_id", sc.TraceID().String(), "span_id", sc.SpanID().String())
		}
		if sw.status >= 500 {
			logger.Error("запрос завершился ошибкой", attrs...)
		} else {
			logger.Info("запрос обработан", attrs...)
		}
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintln(w, "ok")
}

// handleFail отдаёт 500 и помечает спан как неуспешный: в Jaeger он станет красным.
func handleFail(w http.ResponseWriter, r *http.Request) {
	span := trace.SpanFromContext(r.Context())
	span.SetStatus(codes.Error, "искусственная ошибка /fail")
	span.SetAttributes(attribute.Bool("fail.injected", true))
	http.Error(w, "внутренняя ошибка сервиса", http.StatusInternalServerError)
}

// handleSlow спит 1-3 секунды во вложенном спане slow-op:
// в водопаде Jaeger будет видно, что время ушло именно туда.
func handleSlow(w http.ResponseWriter, r *http.Request) {
	tracer := otel.Tracer("api")
	ctx, span := tracer.Start(r.Context(), "slow-op")
	delay := time.Duration(1000+rand.Intn(2000)) * time.Millisecond
	span.SetAttributes(attribute.Int64("slow.delay_ms", delay.Milliseconds()))
	select {
	case <-time.After(delay):
	case <-ctx.Done():
	}
	span.End()
	fmt.Fprintf(w, "спал %s\n", delay)
}

// handleLoad бьёт запросами сам в себя, чтобы поднять RPS на графиках.
func handleLoad(w http.ResponseWriter, r *http.Request) {
	n := intParam(r, "n", 200)
	concurrency := intParam(r, "c", 20)
	target := "http://127.0.0.1:" + port() + "/health"

	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	client := &http.Client{Timeout: 5 * time.Second}
	for i := 0; i < n; i++ {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			resp, err := client.Get(target)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	wg.Wait()
	fmt.Fprintf(w, "отправлено %d запросов при параллельности %d\n", n, concurrency)
}

// handleEat выделяет N мегабайт и удерживает их: так ловится OOMKilled.
func handleEat(w http.ResponseWriter, r *http.Request) {
	mb := intParam(r, "mb", 64)
	block := make([]byte, mb*1024*1024)
	for i := range block {
		block[i] = 1 // страницы нужно потрогать, иначе они не будут выделены физически
	}
	eatenMu.Lock()
	eaten = append(eaten, block)
	total := 0
	for _, b := range eaten {
		total += len(b) / (1024 * 1024)
	}
	eatenMu.Unlock()
	fmt.Fprintf(w, "съедено %d МБ, всего удерживается %d МБ\n", mb, total)
}

// handleBurn грузит одно ядро в пустом цикле: так ловится throttling по CPU.
func handleBurn(w http.ResponseWriter, r *http.Request) {
	seconds := intParam(r, "seconds", 30)
	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		for time.Now().Before(deadline) {
		}
	}()
	fmt.Fprintf(w, "жгу одно ядро %d секунд\n", seconds)
}

func intParam(r *http.Request, name string, def int) int {
	if v := r.URL.Query().Get(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func port() string {
	if p := os.Getenv("PORT"); p != "" {
		return p
	}
	return "8080"
}

func serviceName() string {
	if n := os.Getenv("OTEL_SERVICE_NAME"); n != "" {
		return n
	}
	return "api"
}

// initTracing поднимает экспорт трейсов по OTLP. Адрес коллектора берётся из
// переменной OTEL_EXPORTER_OTLP_ENDPOINT, её читает сам SDK.
func initTracing(ctx context.Context) func(context.Context) error {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		logger.Warn("OTEL_EXPORTER_OTLP_ENDPOINT не задан, трейсы никуда не уходят")
		return func(context.Context) error { return nil }
	}
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		logger.Error("не удалось создать экспортёр OTLP", "err", err)
		return func(context.Context) error { return nil }
	}
	res, _ := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(serviceName()),
	))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown
}

func main() {
	logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	shutdownTracing := initTracing(ctx)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	for path, h := range map[string]http.HandlerFunc{
		"/health": handleHealth,
		"/fail":   handleFail,
		"/slow":   handleSlow,
		"/load":   handleLoad,
		"/eat":    handleEat,
		"/burn":   handleBurn,
	} {
		// otelhttp создаёт корневой спан на каждый входящий запрос,
		// observe снимает метрики и пишет лог с trace_id из этого спана.
		mux.Handle(path, otelhttp.NewHandler(observe(path, h), path))
	}

	srv := &http.Server{
		Addr:              ":" + port(),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		logger.Info("сервис запущен", "addr", srv.Addr, "service", serviceName())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Error("сервер остановился с ошибкой", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("получен сигнал, останавливаюсь")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	shutdownTracing(shutdownCtx)
}
