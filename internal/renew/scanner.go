// Package renew finds numbers whose active period has lapsed and enqueues one
// axis_renew job for each.
//
// The scanner is deliberately the only component that decides "who needs a
// renew". Workers never scan for their own work: if both the scanner and a
// payment worker queried accounts independently, two slightly different
// predicates would eventually disagree and double-charge a number. Here the
// decision lives in exactly one SQL statement.
//
// Safety comes from the database, not from application locking:
//
//   - the unique partial index idx_jobs_active_per_account (migration 0004)
//     allows at most one active job per (account_id, kind), so ON CONFLICT
//     DO NOTHING makes repeated ticks and restarts harmless;
//   - renew.cooldown keeps a number from being charged twice in quick
//     succession even across job completions.
package renew

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"axisbridge/internal/settings"
)

// Scanner periodically enqueues axis_renew jobs.
type Scanner struct {
	Pool     *pgxpool.Pool
	Settings *settings.Store
	Logger   *slog.Logger
}

// Defaults applied when a setting is missing or unparsable.
const (
	defaultMinusDays = -30
	defaultCooldown  = 20 * time.Hour
	defaultScanEvery = 5 * time.Minute
	minScanEvery     = 30 * time.Second
)

// enqueueSQL selects renew candidates and queues them in one statement.
//
// $1 = lapsed days (positive integer; see Tick for the sign handling)
// $2 = cooldown seconds
//
// Criteria:
//   - masa_aktif_until has lapsed by at least $1 days
//   - the SIM is not dead and not parked by the operator
//   - gift_only numbers are excluded: they were uploaded straight to the gift
//     queue and never belonged to the renew population
//   - the number was not charged within the cooldown window
const enqueueSQL = `
INSERT INTO jobs (kind, account_id, payload, max_attempts)
SELECT 'axis_renew'::job_kind,
       a.id,
       jsonb_build_object('reason', 'lapsed', 'lapsed_days', $1::int),
       3
  FROM accounts a
 WHERE a.dead = FALSE
   AND a.gift_only = FALSE
   AND a.status <> 'paused'
   AND a.masa_aktif_until IS NOT NULL
   AND a.masa_aktif_until <= CURRENT_DATE - $1::int
   AND (a.last_renew_at IS NULL
        OR a.last_renew_at < NOW() - ($2::int || ' seconds')::interval)
ON CONFLICT DO NOTHING`

// Run ticks until ctx is cancelled. The interval is re-read every cycle so a
// change to renew.scan_every from the dashboard takes effect without a
// restart.
func (s *Scanner) Run(ctx context.Context) {
	s.Logger.Info("renew scanner started",
		"scan_every", s.scanEvery(), "minus_days", s.Settings.Int("renew.minus_days", defaultMinusDays))

	timer := time.NewTimer(s.scanEvery())
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			s.Logger.Info("renew scanner stopped")
			return
		case <-timer.C:
			if n, err := s.Tick(ctx); err != nil {
				// A failed tick is not fatal: the next one retries the same
				// population, and the dedup index keeps that safe.
				s.Logger.Error("renew scan", "err", err)
			} else if n > 0 {
				s.Logger.Info("renew jobs enqueued", "count", n)
			}
			timer.Reset(s.scanEvery())
		}
	}
}

// Tick runs one scan. Exported so a smoke test can drive it directly instead
// of waiting out a full interval. Returns how many jobs were enqueued.
func (s *Scanner) Tick(ctx context.Context) (int64, error) {
	if !s.Settings.Bool("renew.enabled", false) {
		// Debug, not info: this fires every interval while renew is off and
		// would otherwise drown the log.
		s.Logger.Debug("renew disabled, skipping scan")
		return 0, nil
	}

	lapsed := lapsedDays(s.Settings.Int("renew.minus_days", defaultMinusDays))
	cooldown := s.Settings.Duration("renew.cooldown", defaultCooldown)

	tag, err := s.Pool.Exec(ctx, enqueueSQL, lapsed, int(cooldown.Seconds()))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// lapsedDays converts the renew.minus_days setting into a positive number of
// days to subtract from CURRENT_DATE.
//
// The setting is negative by convention (-30 reads as "30 days past expiry"),
// matching RENEW_STALE_DAYS in the monolith, whose documented behaviour was to
// renew only once the active period had lapsed by N days. A positive value is
// treated as the same magnitude rather than silently inverting the comparison
// into "renew everything", which is the dangerous reading.
func lapsedDays(setting int) int {
	if setting < 0 {
		return -setting
	}
	if setting == 0 {
		return 0
	}
	return setting
}

// scanEvery clamps the configured interval so a typo cannot turn the scanner
// into a busy loop against Postgres.
func (s *Scanner) scanEvery() time.Duration {
	d := s.Settings.Duration("renew.scan_every", defaultScanEvery)
	if d < minScanEvery {
		return minScanEvery
	}
	return d
}
