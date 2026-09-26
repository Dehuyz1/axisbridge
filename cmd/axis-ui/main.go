// axis-ui: serves 3 dashboards (Management, Gift List, Setting) and
// reverse-proxies /api/* to axis-core. Basic auth on everything.
//
// Env:
//
//	UI_ADDR      listen addr, default :8080
//	CORE_URL     axis-core base URL, default http://localhost:1213
//	UI_USER      basic auth user, default admin
//	UI_PASS      basic auth password (required)
//	LOG_LEVEL    debug|info|warn|error, default info
package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

//go:embed ui/*
var uiFS embed.FS

func main() {
	logger := newLogger(os.Getenv("LOG_LEVEL"))

	addr := envDefault("UI_ADDR", ":8080")
	coreURL := envDefault("CORE_URL", "http://localhost:1213")
	user := envDefault("UI_USER", "admin")
	pass := os.Getenv("UI_PASS")
	if pass == "" {
		logger.Error("UI_PASS is required")
		os.Exit(2)
	}

	coreParsed, err := url.Parse(coreURL)
	if err != nil {
		logger.Error("invalid CORE_URL", "err", err)
		os.Exit(2)
	}
	proxy := httputil.NewSingleHostReverseProxy(coreParsed)
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		logger.Warn("core proxy error", "path", r.URL.Path, "err", err)
		http.Error(w, "core upstream unreachable", http.StatusBadGateway)
	}

	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		logger.Error("embed sub", "err", err)
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.Handle("/api/", proxy)
	mux.Handle("/static/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/", "/management":
			serveFile(w, r, sub, "management.html")
		case "/gifts":
			serveFile(w, r, sub, "gifts.html")
		case "/setting":
			serveFile(w, r, sub, "setting.html")
		default:
			http.NotFound(w, r)
		}
	})

	handler := basicAuth(user, pass, mux)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              addr,
		Handler:           requestLog(logger, handler),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Info("axis-ui listening", "addr", addr, "core", coreURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("listen", "err", err)
			stop()
		}
	}()
	<-ctx.Done()
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

func serveFile(w http.ResponseWriter, r *http.Request, sub fs.FS, name string) {
	b, err := fs.ReadFile(sub, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(b)
}

func basicAuth(user, pass string, next http.Handler) http.Handler {
	realm := `Basic realm="axisbridge"`
	uB := []byte(user)
	pB := []byte(pass)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u), uB) != 1 || subtle.ConstantTimeCompare([]byte(p), pB) != 1 {
			w.Header().Set("WWW-Authenticate", realm)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
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
		if strings.HasPrefix(r.URL.Path, "/static/") {
			return
		}
		log.Info("http", "method", r.Method, "path", r.URL.Path, "status", rw.code, "dur", time.Since(start))
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(c int) { r.code = c; r.ResponseWriter.WriteHeader(c) }
