// gift-worker: memproses account yang berada di status gift_wait.
//
// Behavior:
//
//  1. Klaim job axis_gift.
//  2. Panggil AXIS gift flow (BELUM DIPASANG: stub yang men-simulasi sukses).
//  3. Kalau sukses: insert baris ke tabel gifts + set account.status='gift_done'.
//  4. Kalau gagal: naikkan attempts; ketika habis, biarkan status tetap
//     gift_wait supaya bisa dicoba manual dari dashboard.
//
// Env:
//
//	DATABASE_URL         postgres DSN
//	WORKER_CONCURRENCY   int, default 1
//	LOG_LEVEL            debug|info|warn|error
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"axisbridge/internal/db"
	"axisbridge/internal/jobq"
	"axisbridge/internal/settings"
)

func main() {
	logger := newLogger(os.Getenv("LOG_LEVEL"))

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		logger.Error("DATABASE_URL is required")
		os.Exit(2)
	}
	concurrency := envInt("WORKER_CONCURRENCY", 1)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

	cfg := jobq.Config{
		Kinds:          []string{"axis_gift"},
		Concurrency:    concurrency,
		PollInterval:   5 * time.Second,
		DefaultBackoff: 60 * time.Second,
		Logger:         logger,
	}

	h := func(ctx context.Context, job jobq.Job) jobq.Result {
		if !store.Bool("worker.gift_enabled", true) {
			return jobq.Result{Retry: 60 * time.Second, Err: errDisabled}
		}
		if job.AccountID == nil {
			return jobq.Result{Done: true, Err: errors.New("no account_id")}
		}
		return handleGift(ctx, logger, pool, *job.AccountID)
	}

	if err := jobq.Run(ctx, pool, cfg, h); err != nil && err != context.Canceled {
		logger.Error("worker exit", "err", err)
		os.Exit(1)
	}
}

func handleGift(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, accountID int64) jobq.Result {
	var msisdn, status string
	err := pool.QueryRow(ctx, `
        SELECT msisdn, status::text FROM accounts WHERE id=$1
    `, accountID).Scan(&msisdn, &status)
	if err != nil {
		return jobq.Result{Done: true, Err: fmt.Errorf("load account: %w", err)}
	}
	if status != "gift_wait" {
		return jobq.Result{Done: true, Data: map[string]string{"skipped": status}}
	}

	// TODO: panggil AXIS gift flow di sini. Stub: bukti dummy supaya UI bisa
	// diuji end-to-end sebelum client asli dipasang.
	proof := map[string]any{
		"stub":    true,
		"msisdn":  msisdn,
		"trx_id":  fmt.Sprintf("STUB-%d-%d", accountID, time.Now().Unix()),
		"message": "gift sukses (stub)",
	}
	proofJSON, _ := json.Marshal(proof)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return jobq.Result{Err: fmt.Errorf("begin: %w", err)}
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `
        INSERT INTO gifts(account_id, msisdn, proof) VALUES($1, $2, $3)
    `, accountID, msisdn, proofJSON); err != nil {
		return jobq.Result{Err: fmt.Errorf("insert gift: %w", err)}
	}
	if _, err := tx.Exec(ctx, `
        UPDATE accounts SET status='gift_done' WHERE id=$1
    `, accountID); err != nil {
		return jobq.Result{Err: fmt.Errorf("mark done: %w", err)}
	}
	if err := tx.Commit(ctx); err != nil {
		return jobq.Result{Err: fmt.Errorf("commit: %w", err)}
	}
	logger.Info("gift done", "account", accountID, "msisdn", msisdn)
	return jobq.Result{Done: true, Data: proof}
}

var errDisabled = &disabledErr{}

type disabledErr struct{}

func (*disabledErr) Error() string { return "worker.gift_enabled = false" }

var _ = db.Migrate // keep import used across small refactors

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
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
