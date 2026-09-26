// Package settings loads runtime configuration from the settings table and
// keeps a snapshot in memory. Callers read via Get(); a background refresher
// pulls updates every RefreshEvery so UI edits become effective without any
// worker restart.
package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is a thread-safe snapshot of the settings table.
type Store struct {
	pool         *pgxpool.Pool
	refreshEvery time.Duration

	mu   sync.RWMutex
	data map[string]json.RawMessage
}

// New builds a Store and performs the first load synchronously so callers
// never observe a zero snapshot.
func New(ctx context.Context, pool *pgxpool.Pool, refreshEvery time.Duration) (*Store, error) {
	if refreshEvery <= 0 {
		refreshEvery = 10 * time.Second
	}
	s := &Store{pool: pool, refreshEvery: refreshEvery, data: map[string]json.RawMessage{}}
	if err := s.reload(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Run keeps the snapshot fresh; blocks until ctx is cancelled.
func (s *Store) Run(ctx context.Context) {
	t := time.NewTicker(s.refreshEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.reload(ctx); err != nil {
				// Best-effort; keep old snapshot on failure.
				continue
			}
		}
	}
}

func (s *Store) reload(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return fmt.Errorf("settings query: %w", err)
	}
	defer rows.Close()
	next := map[string]json.RawMessage{}
	for rows.Next() {
		var k string
		var v json.RawMessage
		if err := rows.Scan(&k, &v); err != nil {
			return err
		}
		next[k] = v
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.data = next
	s.mu.Unlock()
	return nil
}

// Raw returns the JSON blob for key or (nil, false).
func (s *Store) Raw(key string) (json.RawMessage, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	return v, ok
}

// String decodes a JSON string value; returns def when missing/invalid.
func (s *Store) String(key, def string) string {
	raw, ok := s.Raw(key)
	if !ok {
		return def
	}
	var out string
	if err := json.Unmarshal(raw, &out); err != nil {
		return def
	}
	return out
}

// Int decodes a JSON number value into int; returns def when missing/invalid.
func (s *Store) Int(key string, def int) int {
	raw, ok := s.Raw(key)
	if !ok {
		return def
	}
	var out int
	if err := json.Unmarshal(raw, &out); err != nil {
		return def
	}
	return out
}

// Bool decodes a JSON boolean value; returns def when missing/invalid.
func (s *Store) Bool(key string, def bool) bool {
	raw, ok := s.Raw(key)
	if !ok {
		return def
	}
	var out bool
	if err := json.Unmarshal(raw, &out); err != nil {
		return def
	}
	return out
}

// Duration decodes a JSON string like "30s" via time.ParseDuration.
func (s *Store) Duration(key string, def time.Duration) time.Duration {
	str := s.String(key, "")
	if str == "" {
		return def
	}
	d, err := time.ParseDuration(str)
	if err != nil {
		return def
	}
	return d
}

// All returns a shallow copy of the current snapshot; useful for /api/settings.
func (s *Store) All() map[string]json.RawMessage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]json.RawMessage, len(s.data))
	for k, v := range s.data {
		out[k] = v
	}
	return out
}

// Put persists a single key. The next reload picks it up; callers may also
// call ForceReload to see the change immediately.
func (s *Store) Put(ctx context.Context, key string, value any, updatedBy string) error {
	if key == "" {
		return errors.New("empty key")
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
        INSERT INTO settings(key, value, updated_by) VALUES($1, $2, $3)
        ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by
    `, key, raw, updatedBy)
	return err
}

// ForceReload synchronously re-reads the settings table.
func (s *Store) ForceReload(ctx context.Context) error { return s.reload(ctx) }
