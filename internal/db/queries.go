package db

// Common SQL fragments shared across binaries. Keep queries close to their
// usage sites; this file only holds ones that need to stay in lock-step
// between core and workers.

const (
	// ClaimJobSQL atomically picks one pending job of the requested kinds and
	// flips it to running. Returns id, kind, account_id, payload.
	//
	// $1 = kinds (text[] cast to job_kind[] on the SQL side)
	// $2 = worker note (short string logged into events)
	ClaimJobSQL = `
WITH picked AS (
    SELECT id
      FROM jobs
     WHERE state = 'pending'
       AND kind::text = ANY($1)
       AND scheduled_at <= NOW()
     ORDER BY scheduled_at
     FOR UPDATE SKIP LOCKED
     LIMIT 1
)
UPDATE jobs j
   SET state      = 'running',
       started_at = NOW(),
       attempts   = j.attempts + 1
  FROM picked
 WHERE j.id = picked.id
 RETURNING j.id, j.kind::text, j.account_id, j.payload, j.attempts, j.max_attempts;
`

	// FinishJobSQL marks a job done or failed.
	//
	// $1 = job id, $2 = new state ('done'|'failed'|'cancelled'),
	// $3 = result jsonb (may be NULL), $4 = error text (may be NULL)
	FinishJobSQL = `
UPDATE jobs
   SET state       = $2::job_state,
       finished_at = NOW(),
       result      = COALESCE($3::jsonb, result),
       error       = $4
 WHERE id = $1;
`

	// RescheduleJobSQL flips a running job back to pending with backoff.
	//
	// $1 = job id, $2 = delay seconds
	RescheduleJobSQL = `
UPDATE jobs
   SET state        = 'pending',
       scheduled_at = NOW() + ($2::int || ' seconds')::interval,
       started_at   = NULL
 WHERE id = $1;
`
)
