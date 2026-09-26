// Package jobq implements the job queue Workers use to pull work from
// Postgres. Every worker binary calls Run(ctx, cfg, handler) and blocks; the
// package hides LISTEN/NOTIFY, SKIP LOCKED claim, and backoff.
package jobq

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"axisbridge/internal/db"
)

// Job is one unit of work handed to Handler. Payload stays raw so each
// handler can decode into its own struct.
type Job struct {
	ID          int64
	Kind        string
	AccountID   *int64
	Payload     json.RawMessage
	Attempts    int
	MaxAttempts int
}

// Result reports the outcome of a Handler call.
type Result struct {
	// Done marks the job successful. If false the job is retried up to
	// MaxAttempts, otherwise marked failed.
	Done bool
	// Retry, when non-zero and Done is false, overrides the default backoff.
	Retry time.Duration
	// Data is stored back into jobs.result (JSONB) if non-nil.
	Data any
	// Err populates jobs.error on failure/retry. Optional.
	Err error
}

// Handler processes one job. Return Result to control retry/finish.
type Handler func(ctx context.Context, job Job) Result

// Config drives the worker loop.
type Config struct {
	// Kinds this worker claims (e.g. []string{"axis_refresh","axis_renew"}).
	Kinds []string
	// Concurrency is how many jobs may run in parallel per worker process.
	Concurrency int
	// PollInterval is the fallback poll cadence when NOTIFY is quiet.
	PollInterval time.Duration
	// DefaultBackoff is used when a Handler asks for a retry without setting
	// Retry explicitly.
	DefaultBackoff time.Duration
	// Logger is required.
	Logger *slog.Logger
}

// Run blocks until ctx is cancelled. Each Kinds slice element triggers a
// LISTEN on channel jobs_<kind>.
func Run(ctx context.Context, pool *pgxpool.Pool, cfg Config, h Handler) error {
	if len(cfg.Kinds) == 0 {
		return errors.New("jobq: Kinds required")
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.DefaultBackoff <= 0 {
		cfg.DefaultBackoff = 30 * time.Second
	}
	if cfg.Logger == nil {
		return errors.New("jobq: Logger required")
	}

	// Signal channel woken by NOTIFY or poll.
	wake := make(chan struct{}, 1)
	go listen(ctx, pool, cfg, wake)

	slots := make(chan struct{}, cfg.Concurrency)
	for i := 0; i < cfg.Concurrency; i++ {
		slots <- struct{}{}
	}

	timer := time.NewTimer(cfg.PollInterval)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wake:
		case <-timer.C:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(cfg.PollInterval)

		// Drain as many jobs as we have free slots.
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-slots:
			default:
				goto waitAgain
			}
			job, ok, err := claim(ctx, pool, cfg.Kinds)
			if err != nil {
				cfg.Logger.Error("claim failed", "err", err)
				slots <- struct{}{}
				break
			}
			if !ok {
				slots <- struct{}{}
				break
			}
			go func(j Job) {
				defer func() { slots <- struct{}{} }()
				process(ctx, pool, cfg, h, j)
			}(job)
		}
	waitAgain:
	}
}

func listen(ctx context.Context, pool *pgxpool.Pool, cfg Config, wake chan<- struct{}) {
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		conn, err := pool.Acquire(ctx)
		if err != nil {
			cfg.Logger.Warn("listen acquire", "err", err)
			time.Sleep(backoff)
			backoff = minDur(backoff*2, 30*time.Second)
			continue
		}
		if err := listenOnce(ctx, conn.Conn(), cfg, wake); err != nil && !errors.Is(err, context.Canceled) {
			cfg.Logger.Warn("listen loop", "err", err)
		}
		conn.Release()
		time.Sleep(backoff)
		backoff = minDur(backoff*2, 30*time.Second)
	}
}

func listenOnce(ctx context.Context, conn *pgx.Conn, cfg Config, wake chan<- struct{}) error {
	for _, k := range cfg.Kinds {
		if _, err := conn.Exec(ctx, "LISTEN "+quoteIdent("jobs_"+k)); err != nil {
			return fmt.Errorf("listen %s: %w", k, err)
		}
	}
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err
		}
		select {
		case wake <- struct{}{}:
		default:
		}
	}
}

func claim(ctx context.Context, pool *pgxpool.Pool, kinds []string) (Job, bool, error) {
	var job Job
	err := pool.QueryRow(ctx, db.ClaimJobSQL, kinds).Scan(
		&job.ID, &job.Kind, &job.AccountID, &job.Payload, &job.Attempts, &job.MaxAttempts,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	return job, true, nil
}

func process(ctx context.Context, pool *pgxpool.Pool, cfg Config, h Handler, job Job) {
	log := cfg.Logger.With("job_id", job.ID, "kind", job.Kind, "attempt", job.Attempts)
	log.Info("job start")

	res := safeInvoke(ctx, h, job)

	var (
		newState string
		errText  *string
		dataJSON []byte
	)
	if res.Data != nil {
		if b, err := json.Marshal(res.Data); err == nil {
			dataJSON = b
		}
	}
	if res.Err != nil {
		s := res.Err.Error()
		errText = &s
	}

	switch {
	case res.Done:
		newState = "done"
	case job.Attempts >= job.MaxAttempts:
		newState = "failed"
	default:
		delay := res.Retry
		if delay <= 0 {
			delay = cfg.DefaultBackoff
		}
		if _, err := pool.Exec(ctx, db.RescheduleJobSQL, job.ID, int(delay.Seconds())); err != nil {
			log.Error("reschedule failed", "err", err)
		} else {
			log.Info("job retry", "delay", delay, "err", errText)
		}
		return
	}

	if _, err := pool.Exec(ctx, db.FinishJobSQL, job.ID, newState, dataJSON, errText); err != nil {
		log.Error("finish failed", "err", err, "state", newState)
		return
	}
	log.Info("job done", "state", newState)
}

func safeInvoke(ctx context.Context, h Handler, job Job) (res Result) {
	defer func() {
		if r := recover(); r != nil {
			res = Result{Done: false, Err: fmt.Errorf("panic: %v", r)}
		}
	}()
	return h(ctx, job)
}

func quoteIdent(s string) string {
	// pg identifier quoting: double any embedded double quote.
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			out = append(out, '"', '"')
		} else {
			out = append(out, s[i])
		}
	}
	out = append(out, '"')
	return string(out)
}

func minDur(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}
