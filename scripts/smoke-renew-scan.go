//go:build ignore

// Smoke test for the renew scanner against a real Postgres.
//
// Seeds one account per exclusion rule, runs Tick twice, and asserts that only
// the eligible numbers were queued and that the second tick adds nothing.
//
//	go run scripts/smoke-renew-scan.go
//
// Requires DATABASE_URL pointing at a scratch database.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"axisbridge/internal/db"
	"axisbridge/internal/renew"
	"axisbridge/internal/settings"
)

type seed struct {
	msisdn     string
	lapsedDays int // how many days ago masa_aktif_until fell; negative = future
	dead       bool
	giftOnly   bool
	status     string
	lastRenew  string // SQL expression or empty for NULL
	wantQueued bool
	why        string
}

func main() {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		fmt.Println("DATABASE_URL required")
		os.Exit(2)
	}
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	pool, err := db.Open(ctx, dsn)
	must(err)
	defer pool.Close()

	// Clean slate.
	_, err = pool.Exec(ctx, `DELETE FROM jobs; DELETE FROM accounts;`)
	must(err)

	// renew.minus_days = -30 means "lapsed 30 days or more".
	_, err = pool.Exec(ctx, `
		INSERT INTO settings(key, value) VALUES
			('renew.enabled','true'),('renew.minus_days','-30'),
			('renew.cooldown','"20h"'),('renew.scan_every','"5m"')
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`)
	must(err)

	seeds := []seed{
		{msisdn: "6283190000001", lapsedDays: 40, wantQueued: true,
			why: "lapsed 40d, well past the -30 threshold"},
		{msisdn: "6283190000002", lapsedDays: 30, wantQueued: true,
			why: "lapsed exactly 30d, boundary is inclusive"},
		{msisdn: "6283190000003", lapsedDays: 29, wantQueued: false,
			why: "lapsed 29d, one day short of the threshold"},
		{msisdn: "6283190000004", lapsedDays: -5, wantQueued: false,
			why: "still active, expires in 5 days"},
		{msisdn: "6283190000005", lapsedDays: 40, dead: true, wantQueued: false,
			why: "dead SIM"},
		{msisdn: "6283190000006", lapsedDays: 40, giftOnly: true, wantQueued: false,
			why: "gift_only, never part of the renew population"},
		{msisdn: "6283190000007", lapsedDays: 40, status: "paused", wantQueued: false,
			why: "paused by operator"},
		{msisdn: "6283190000008", lapsedDays: 40, lastRenew: "NOW() - INTERVAL '2 hours'",
			wantQueued: false, why: "charged 2h ago, inside the 20h cooldown"},
		{msisdn: "6283190000009", lapsedDays: 40, lastRenew: "NOW() - INTERVAL '30 hours'",
			wantQueued: true, why: "charged 30h ago, cooldown elapsed"},
	}

	for _, s := range seeds {
		status := s.status
		if status == "" {
			status = "available"
		}
		lastRenew := "NULL"
		if s.lastRenew != "" {
			lastRenew = s.lastRenew
		}
		q := fmt.Sprintf(`
			INSERT INTO accounts (msisdn, status, dead, gift_only, masa_aktif_until, last_renew_at)
			VALUES ($1, $2::account_status, $3, $4, CURRENT_DATE - $5::int, %s)`, lastRenew)
		_, err := pool.Exec(ctx, q, s.msisdn, status, s.dead, s.giftOnly, s.lapsedDays)
		must(err)
	}

	store, err := settings.New(ctx, pool, time.Hour)
	must(err)
	sc := &renew.Scanner{Pool: pool, Settings: store, Logger: logger}

	first, err := sc.Tick(ctx)
	must(err)
	fmt.Printf("tick 1 enqueued: %d\n", first)

	second, err := sc.Tick(ctx)
	must(err)
	fmt.Printf("tick 2 enqueued: %d (idempotency: must be 0)\n\n", second)

	fail := 0
	for _, s := range seeds {
		var n int
		err := pool.QueryRow(ctx, `
			SELECT count(*) FROM jobs j
			  JOIN accounts a ON a.id = j.account_id
			 WHERE a.msisdn = $1 AND j.kind = 'axis_renew'`, s.msisdn).Scan(&n)
		must(err)

		queued := n > 0
		mark := "ok  "
		if queued != s.wantQueued {
			mark = "FAIL"
			fail++
		}
		fmt.Printf("%s %s queued=%v want=%v  (%s)\n", mark, s.msisdn, queued, s.wantQueued, s.why)
		if n > 1 {
			fmt.Printf("FAIL %s has %d duplicate jobs\n", s.msisdn, n)
			fail++
		}
	}

	// Disabling renew must stop enqueueing entirely.
	_, err = pool.Exec(ctx, `UPDATE settings SET value='false' WHERE key='renew.enabled'`)
	must(err)
	must(store.ForceReload(ctx))
	_, err = pool.Exec(ctx, `DELETE FROM jobs`)
	must(err)
	off, err := sc.Tick(ctx)
	must(err)
	fmt.Printf("\nrenew.enabled=false enqueued: %d (must be 0)\n", off)
	if off != 0 {
		fail++
	}

	if second != 0 {
		fail++
	}
	fmt.Println()
	if fail > 0 {
		fmt.Printf("RESULT: %d failure(s)\n", fail)
		os.Exit(1)
	}
	fmt.Println("RESULT: all assertions passed")
}

func must(err error) {
	if err != nil {
		fmt.Println("fatal:", err)
		os.Exit(1)
	}
}
