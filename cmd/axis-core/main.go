// axis-core: DB owner, REST API, job producer.
//
// Env:
//
//	DATABASE_URL   postgres DSN
//	CORE_ADDR      listen addr, default :1213
//	LOG_LEVEL      debug|info|warn|error, default info
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"axisbridge/internal/api"
	"axisbridge/internal/db"
	"axisbridge/internal/ovo"
	"axisbridge/internal/renew"
	"axisbridge/internal/settings"
)

func main() {
	logger := newLogger(os.Getenv("LOG_LEVEL"))

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		logger.Error("DATABASE_URL is required")
		os.Exit(2)
	}
	addr := envDefault("CORE_ADDR", ":1213")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := db.Migrate(ctx, dsn); err != nil {
		logger.Error("migrate", "err", err)
		os.Exit(1)
	}
	pool, err := db.Open(ctx, dsn)
	if err != nil {
		logger.Error("db open", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	store, err := settings.New(ctx, pool, 10*time.Second)
	if err != nil {
		logger.Error("settings load", "err", err)
		os.Exit(1)
	}
	go store.Run(ctx)

	// Renew scanner lives here rather than in its own binary: it only issues
	// INSERT ... SELECT against the database axis-core already owns, so it
	// needs no separate crash domain.
	scanner := &renew.Scanner{Pool: pool, Settings: store, Logger: logger}
	go scanner.Run(ctx)

	// OVO_MASTER_KEY is optional: without it the process still serves AXIS
	// fully, and the OVO endpoints refuse rather than persisting PINs in the
	// clear.
	var vault *ovo.PinCipher
	if key := os.Getenv("OVO_MASTER_KEY"); key != "" {
		vault, err = ovo.NewPinCipher([]byte(key))
		if err != nil {
			logger.Error("ovo master key", "err", err)
			os.Exit(1)
		}
	} else {
		logger.Warn("OVO_MASTER_KEY unset; OVO endpoints disabled")
	}

	srv := &api.Server{Pool: pool, Settings: store, Logger: logger, Vault: vault}
	mux := http.NewServeMux()
	srv.Routes(mux)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           requestLog(logger, mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("axis-core listening", "addr", addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("listen", "err", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutCtx)
}

func envDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func newLogger(level string) *slog.Logger {
	lv := slog.LevelInfo
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}

func requestLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &statusRecorder{ResponseWriter: w, code: 200}
		next.ServeHTTP(rw, r)
		log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rw.code, "dur", time.Since(start))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(c int) { r.code = c; r.ResponseWriter.WriteHeader(c) }
