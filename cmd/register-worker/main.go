// register-worker: menangani job axis_otp_request.
//
// Behavior:
//
//  1. Klaim job axis_otp_request untuk sebuah account.
//  2. Panggil AXIS RequestOTP (BELUM DIPASANG: stub men-simulasikan sukses).
//  3. Update accounts: otp_attempts++, otp_last_at=NOW, simpan otp_enc_msisdn.
//  4. Kalau attempt >= otp.max_attempt, tandai account sebagai gift_wait.
//     Selain itu, jadwalkan job axis_otp_request berikutnya setelah otp.retry_gap.
//
// SMS balasan (OTP asli) yang mengubah status jadi 'available' datang lewat
// endpoint POST /api/provider/sms yang akan ditambahkan ketika client AXIS
// benar-benar di-port. Untuk sekarang worker cuma memutar loop request OTP.
//
// Env:
//
//	DATABASE_URL         postgres DSN
//	WORKER_CONCURRENCY   int, default 2
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
	concurrency := envInt("WORKER_CONCURRENCY", 2)

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
		Kinds:          []string{"axis_otp_request"},
		Concurrency:    concurrency,
		PollInterval:   5 * time.Second,
		DefaultBackoff: 30 * time.Second,
		Logger:         logger,
	}

	h := func(ctx context.Context, job jobq.Job) jobq.Result {
		if !store.Bool("worker.register_enabled", true) {
			return jobq.Result{Retry: 30 * time.Second, Err: errDisabled}
		}
		if job.AccountID == nil {
			return jobq.Result{Done: true, Err: errors.New("no account_id")}
		}
		return handleOTPRequest(ctx, logger, pool, store, *job.AccountID)
	}

	if err := jobq.Run(ctx, pool, cfg, h); err != nil && err != context.Canceled {
		logger.Error("worker exit", "err", err)
		os.Exit(1)
	}
}

// handleOTPRequest bumps otp_attempts, memanggil AXIS RequestOTP (stub), dan
// menjadwalkan attempt berikutnya atau mendorong ke gift_wait.
func handleOTPRequest(ctx context.Context, logger *slog.Logger, pool *pgxpool.Pool, store *settings.Store, accountID int64) jobq.Result {
	// Load state.
	var msisdn string
	var attempts int
	var status string
	err := pool.QueryRow(ctx, `
        SELECT msisdn, otp_attempts, status::text FROM accounts WHERE id=$1
    `, accountID).Scan(&msisdn, &attempts, &status)
	if err != nil {
		return jobq.Result{Done: true, Err: fmt.Errorf("load account: %w", err)}
	}
	if status == "paused" || status == "gift_wait" || status == "gift_done" || status == "available" {
		// Tidak relevan lagi.
		return jobq.Result{Done: true, Data: map[string]string{"skipped": status}}
	}

	maxAttempt := store.Int("otp.max_attempt", 3)
	retryGap := store.Duration("otp.retry_gap", 90*time.Second)

	// TODO: panggil AXIS RequestOTP di sini. Stub: anggap sukses, tidak ada enc.
	// enc, err := axis.RequestOTP(ctx, msisdn)
	// if err != nil { return jobq.Result{Err: err} }
	nextAttempt := attempts + 1

	// Update accounts.
	if _, err := pool.Exec(ctx, `
        UPDATE accounts
           SET otp_attempts = $2,
               otp_last_at  = NOW(),
               status       = 'otp_pending'
         WHERE id = $1
    `, accountID, nextAttempt); err != nil {
		return jobq.Result{Err: fmt.Errorf("update account: %w", err)}
	}
	logger.Info("otp requested", "account", accountID, "msisdn", msisdn, "attempt", nextAttempt, "max", maxAttempt)

	if nextAttempt >= maxAttempt {
		// Sudah habis. Dorong ke gift_wait + antre satu axis_gift job.
		if _, err := pool.Exec(ctx, `UPDATE accounts SET status='gift_wait' WHERE id=$1`, accountID); err != nil {
			return jobq.Result{Err: fmt.Errorf("mark gift_wait: %w", err)}
		}
		if _, err := pool.Exec(ctx, `
            INSERT INTO jobs(kind, account_id, payload, max_attempts)
            VALUES('axis_gift'::job_kind, $1, '{}'::jsonb, 3)
        `, accountID); err != nil {
			return jobq.Result{Err: fmt.Errorf("enqueue gift: %w", err)}
		}
		return jobq.Result{Done: true, Data: map[string]any{
			"attempt": nextAttempt, "outcome": "gift_wait",
		}}
	}

	// Jadwalkan attempt berikutnya.
	if _, err := pool.Exec(ctx, `
        INSERT INTO jobs(kind, account_id, payload, scheduled_at, max_attempts)
        VALUES('axis_otp_request'::job_kind, $1,
               jsonb_build_object('attempt', $2::int),
               NOW() + ($3::int || ' seconds')::interval, 1)
    `, accountID, nextAttempt+1, int(retryGap.Seconds())); err != nil {
		return jobq.Result{Err: fmt.Errorf("schedule next: %w", err)}
	}
	return jobq.Result{Done: true, Data: map[string]any{
		"attempt": nextAttempt, "next_in": retryGap.String(),
	}}
}


var errDisabled = &disabledErr{}

type disabledErr struct{}

func (*disabledErr) Error() string { return "worker.register_enabled = false" }

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

// static assertion helper so JSON encoder is retained by the linker when
// stub code above evolves to serialize payloads.
var _ = json.Marshal
