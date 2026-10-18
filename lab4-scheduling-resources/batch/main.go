// batch — «ночная аналитика»: бесконечно жжёт CPU и держит память, ничего полезного не делая.
//
// Нагрузка задаётся через env, чтобы крутить её из values.yaml:
//
//	CPU_WORKERS        — сколько горутин крутят CPU (по умолчанию 1);
//	MEMORY_MB          — сколько памяти занять и держать резидентной (по умолчанию 64);
//	MEMORY_STEP_MB     — на сколько МБ расти за шаг, 0 — занять всё сразу (по умолчанию 16);
//	MEMORY_STEP_PERIOD — пауза между шагами роста (по умолчанию 1s);
//	PORT               — порт для /health и /metrics (по умолчанию 8080).
//
// Память трогается постранично и периодически перезаписывается, поэтому она реально
// попадает в RSS / working set пода, а не остаётся виртуальной.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	mb       = 1 << 20
	pageSize = 4096
)

var heldBytes atomic.Int64

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

func envDuration(name string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(name)); err == nil {
		return v
	}
	return def
}

// burnCPU крутит бессмысленную арифметику, пока не отменят контекст.
func burnCPU(ctx context.Context) {
	x := uint64(1)
	for {
		for i := 0; i < 1_000_000; i++ {
			x = x*6364136223846793005 + 1442695040888963407
		}
		if ctx.Err() != nil {
			_ = x
			return
		}
	}
}

// holdMemory наращивает до targetMB по stepMB за шаг и держит память «горячей».
func holdMemory(ctx context.Context, targetMB, stepMB int, period time.Duration, log *slog.Logger) {
	if stepMB <= 0 {
		stepMB = targetMB
	}
	var chunks [][]byte
	touch := func(b []byte, v byte) {
		for i := 0; i < len(b); i += pageSize {
			b[i] = v
		}
	}
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	var gen byte
	for {
		if len(chunks) < targetMB {
			n := min(stepMB, targetMB-len(chunks))
			for i := 0; i < n; i++ {
				c := make([]byte, mb)
				touch(c, 1)
				chunks = append(chunks, c)
			}
			heldBytes.Store(int64(len(chunks)) * mb)
			log.Info("memory grown", "held_mb", len(chunks), "target_mb", targetMB)
		} else {
			// Перезаписываем страницы, чтобы ядро видело их активными.
			gen++
			for _, c := range chunks {
				touch(c, gen)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cpuWorkers := envInt("CPU_WORKERS", 1)
	memoryMB := envInt("MEMORY_MB", 64)
	stepMB := envInt("MEMORY_STEP_MB", 16)
	stepPeriod := envDuration("MEMORY_STEP_PERIOD", time.Second)
	port := envInt("PORT", 8080)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Info("batch started", "cpu_workers", cpuWorkers, "memory_mb", memoryMB,
		"memory_step_mb", stepMB, "memory_step_period", stepPeriod.String())

	for i := 0; i < cpuWorkers; i++ {
		go burnCPU(ctx)
	}
	go holdMemory(ctx, memoryMB, stepMB, stepPeriod, log)

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "# HELP batch_memory_held_bytes Память, которую batch держит намеренно\n")
		fmt.Fprintf(w, "# TYPE batch_memory_held_bytes gauge\n")
		fmt.Fprintf(w, "batch_memory_held_bytes %d\n", heldBytes.Load())
		fmt.Fprintf(w, "# HELP batch_cpu_workers Число горутин, жгущих CPU\n")
		fmt.Fprintf(w, "# TYPE batch_cpu_workers gauge\n")
		fmt.Fprintf(w, "batch_cpu_workers %d\n", cpuWorkers)
	})
	srv := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server failed", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	log.Info("batch stopping")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
