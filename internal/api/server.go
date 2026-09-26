// Package api hosts the REST surface for axis-core. UI talks here; workers do
// not — they read/write Postgres directly through db + jobq.
//
// Scope (v0): register nomor + list gift sukses + settings. Renewal/OVO belum
// dipasang; endpoint yang berkaitan dibuang supaya UI tidak menampilkan tombol
// yang belum berfungsi.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"axisbridge/internal/axis"
	"axisbridge/internal/settings"
)

// Server bundles pool + settings + logger for the HTTP handlers.
type Server struct {
	Pool     *pgxpool.Pool
	Settings *settings.Store
	Logger   *slog.Logger
}

// Routes attaches every handler to mux.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", s.healthz)

	mux.HandleFunc("GET /api/accounts", s.listAccounts)
	mux.HandleFunc("POST /api/register", s.registerAccount)
	mux.HandleFunc("POST /api/accounts/{id}/resend", s.resendOTP)
	mux.HandleFunc("POST /api/accounts/{id}/pause", s.pauseAccount)
	mux.HandleFunc("POST /api/accounts/{id}/resume", s.resumeAccount)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.deleteAccount)

	mux.HandleFunc("GET /api/gifts", s.listGifts)

	mux.HandleFunc("GET /api/settings", s.getSettings)
	mux.HandleFunc("PUT /api/settings/{key}", s.putSetting)

	mux.HandleFunc("GET /api/stats", s.stats)
}

// -- health ------------------------------------------------------------------

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.Pool.Ping(ctx); err != nil {
		http.Error(w, "db down", http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ts": time.Now().UTC()})
}

// -- accounts ----------------------------------------------------------------

type accountRow struct {
	ID           int64      `json:"id"`
	MSISDN       string     `json:"msisdn"`
	Status       string     `json:"status"`
	OTPAttempts  int        `json:"otp_attempts"`
	OTPLastAt    *time.Time `json:"otp_last_at,omitempty"`
	LastLoginAt  *time.Time `json:"last_login_at,omitempty"`
	Note         *string    `json:"note,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := parseIntDefault(q.Get("limit"), 500)
	offset := parseIntDefault(q.Get("offset"), 0)
	status := q.Get("status")
	search := q.Get("q")

	args := []any{limit, offset}
	where := "TRUE"
	if status != "" {
		args = append(args, status)
		where += fmt.Sprintf(" AND status = $%d::account_status", len(args))
	}
	if search != "" {
		args = append(args, "%"+search+"%")
		where += fmt.Sprintf(" AND msisdn LIKE $%d", len(args))
	}
	query := fmt.Sprintf(`
        SELECT id, msisdn, status::text, otp_attempts, otp_last_at,
               last_login_at, note, created_at, updated_at
          FROM accounts
         WHERE %s
         ORDER BY id DESC
         LIMIT $1 OFFSET $2`, where)

	rows, err := s.Pool.Query(r.Context(), query, args...)
	if err != nil {
		s.serverError(w, "list accounts", err)
		return
	}
	defer rows.Close()

	out := make([]accountRow, 0, limit)
	for rows.Next() {
		var a accountRow
		if err := rows.Scan(
			&a.ID, &a.MSISDN, &a.Status, &a.OTPAttempts, &a.OTPLastAt,
			&a.LastLoginAt, &a.Note, &a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			s.serverError(w, "scan account", err)
			return
		}
		out = append(out, a)
	}
	writeJSON(w, http.StatusOK, out)
}

type registerReq struct {
	MSISDN string  `json:"msisdn"`
	Note   *string `json:"note,omitempty"`
}

type registerResp struct {
	ID       int64  `json:"id"`
	MSISDN   string `json:"msisdn"`
	Status   string `json:"status"`
	Reused   bool   `json:"reused"`   // true kalau row sudah ada dan cuma di-reset
	Enqueued bool   `json:"enqueued"` // true kalau OTP request masuk queue
}

// registerAccount menerima MSISDN mentah, normalisasi ke 62xxx, validasi
// prefix AXIS, insert (atau reuse), lalu enqueue job axis_otp_request pertama.
// Attempt kedua dan ketiga dijadwalkan oleh register worker sendiri berdasarkan
// setting otp.retry_gap.
func (s *Server) registerAccount(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	msisdn := axis.NormalizePhone(req.MSISDN)
	if msisdn == "" {
		http.Error(w, "msisdn required", http.StatusBadRequest)
		return
	}
	if !axis.IsAxis(msisdn) {
		http.Error(w, "not an AXIS number", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		s.serverError(w, "begin", err)
		return
	}
	defer tx.Rollback(ctx)

	// UPSERT: kalau nomor pernah masuk, reset ke 'new' + nol-kan otp_attempts.
	var (
		id     int64
		status string
		reused bool
	)
	err = tx.QueryRow(ctx, `
        INSERT INTO accounts(msisdn, note, status)
        VALUES($1, $2, 'new')
        ON CONFLICT (msisdn) DO UPDATE
           SET status       = 'new',
               otp_attempts = 0,
               otp_last_at  = NULL,
               note         = COALESCE(EXCLUDED.note, accounts.note)
        RETURNING id, status::text, (xmax <> 0) AS reused
    `, msisdn, req.Note).Scan(&id, &status, &reused)
	if err != nil {
		s.serverError(w, "register upsert", err)
		return
	}

	// Enqueue OTP #1. Worker akan handle retry (#2, #3) sendiri.
	if _, err := tx.Exec(ctx, `
        INSERT INTO jobs(kind, account_id, payload, max_attempts)
        VALUES('axis_otp_request'::job_kind, $1, jsonb_build_object('attempt', 1), 1)
    `, id); err != nil {
		s.serverError(w, "enqueue otp", err)
		return
	}
	// Set status ke otp_pending sekaligus supaya UI tidak flicker "new" sesaat.
	if _, err := tx.Exec(ctx, `UPDATE accounts SET status='otp_pending' WHERE id=$1`, id); err != nil {
		s.serverError(w, "status pending", err)
		return
	}

	if err := tx.Commit(ctx); err != nil {
		s.serverError(w, "commit", err)
		return
	}
	writeJSON(w, http.StatusAccepted, registerResp{
		ID: id, MSISDN: msisdn, Status: "otp_pending", Reused: reused, Enqueued: true,
	})
}

// resendOTP memaksa satu putaran register baru: nol-kan counter + jadwalkan
// job axis_otp_request. Aman dipanggil ulang: kalau sudah ada job pending,
// insert kedua tetap dieksekusi dan worker yang akan dedup di sisi hasil.
func (s *Server) resendOTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		s.serverError(w, "begin", err)
		return
	}
	defer tx.Rollback(ctx)

	ct, err := tx.Exec(ctx, `
        UPDATE accounts
           SET status='otp_pending', otp_attempts=0, otp_last_at=NULL
         WHERE id=$1`, id)
	if err != nil {
		s.serverError(w, "reset otp", err)
		return
	}
	if ct.RowsAffected() == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if _, err := tx.Exec(ctx, `
        INSERT INTO jobs(kind, account_id, payload, max_attempts)
        VALUES('axis_otp_request'::job_kind, $1, jsonb_build_object('attempt', 1), 1)
    `, id); err != nil {
		s.serverError(w, "enqueue otp", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		s.serverError(w, "commit", err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) pauseAccount(w http.ResponseWriter, r *http.Request)  { s.setStatus(w, r, "paused") }
func (s *Server) resumeAccount(w http.ResponseWriter, r *http.Request) { s.setStatus(w, r, "new") }

func (s *Server) setStatus(w http.ResponseWriter, r *http.Request, status string) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := s.Pool.Exec(r.Context(),
		`UPDATE accounts SET status=$2::account_status WHERE id=$1`, id, status); err != nil {
		s.serverError(w, "set status", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteAccount(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := s.Pool.Exec(r.Context(), `DELETE FROM accounts WHERE id = $1`, id); err != nil {
		s.serverError(w, "delete account", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// -- gifts -------------------------------------------------------------------

type giftRow struct {
	ID        int64           `json:"id"`
	MSISDN    string          `json:"msisdn"`
	Proof     json.RawMessage `json:"proof"`
	CreatedAt time.Time       `json:"created_at"`
}

func (s *Server) listGifts(w http.ResponseWriter, r *http.Request) {
	limit := parseIntDefault(r.URL.Query().Get("limit"), 500)
	rows, err := s.Pool.Query(r.Context(), `
        SELECT id, msisdn, proof, created_at
          FROM gifts
         ORDER BY created_at DESC
         LIMIT $1`, limit)
	if err != nil {
		s.serverError(w, "list gifts", err)
		return
	}
	defer rows.Close()
	out := make([]giftRow, 0, limit)
	for rows.Next() {
		var g giftRow
		if err := rows.Scan(&g.ID, &g.MSISDN, &g.Proof, &g.CreatedAt); err != nil {
			s.serverError(w, "scan gift", err)
			return
		}
		out = append(out, g)
	}
	writeJSON(w, http.StatusOK, out)
}

// -- settings ----------------------------------------------------------------

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Settings.All())
}

func (s *Server) putSetting(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		http.Error(w, "key required", http.StatusBadRequest)
		return
	}
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if err := s.Settings.Put(r.Context(), key, raw, "api"); err != nil {
		s.serverError(w, "put setting", err)
		return
	}
	if err := s.Settings.ForceReload(r.Context()); err != nil {
		s.Logger.Warn("settings reload after put", "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// -- stats -------------------------------------------------------------------

type statsResp struct {
	Accounts struct {
		Total      int64 `json:"total"`
		New        int64 `json:"new"`
		OTPPending int64 `json:"otp_pending"`
		OTPFailed  int64 `json:"otp_failed"`
		Available  int64 `json:"available"`
		GiftWait   int64 `json:"gift_wait"`
		GiftDone   int64 `json:"gift_done"`
		Dead       int64 `json:"dead"`
		Paused     int64 `json:"paused"`
	} `json:"accounts"`
	Gifts int64 `json:"gifts_total"`
	Jobs  struct {
		Pending int64 `json:"pending"`
		Running int64 `json:"running"`
		Failed  int64 `json:"failed"`
	} `json:"jobs"`
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	var o statsResp
	err := s.Pool.QueryRow(r.Context(), `
        SELECT
          (SELECT COUNT(*) FROM accounts),
          (SELECT COUNT(*) FROM accounts WHERE status='new'),
          (SELECT COUNT(*) FROM accounts WHERE status='otp_pending'),
          (SELECT COUNT(*) FROM accounts WHERE status='otp_failed'),
          (SELECT COUNT(*) FROM accounts WHERE status='available'),
          (SELECT COUNT(*) FROM accounts WHERE status='gift_wait'),
          (SELECT COUNT(*) FROM accounts WHERE status='gift_done'),
          (SELECT COUNT(*) FROM accounts WHERE status='dead'),
          (SELECT COUNT(*) FROM accounts WHERE status='paused'),
          (SELECT COUNT(*) FROM gifts),
          (SELECT COUNT(*) FROM jobs WHERE state='pending'),
          (SELECT COUNT(*) FROM jobs WHERE state='running'),
          (SELECT COUNT(*) FROM jobs WHERE state='failed')
    `).Scan(
		&o.Accounts.Total, &o.Accounts.New, &o.Accounts.OTPPending, &o.Accounts.OTPFailed,
		&o.Accounts.Available, &o.Accounts.GiftWait, &o.Accounts.GiftDone,
		&o.Accounts.Dead, &o.Accounts.Paused,
		&o.Gifts,
		&o.Jobs.Pending, &o.Jobs.Running, &o.Jobs.Failed,
	)
	if err != nil {
		s.serverError(w, "stats", err)
		return
	}
	writeJSON(w, http.StatusOK, o)
}

// -- helpers -----------------------------------------------------------------

func (s *Server) serverError(w http.ResponseWriter, ctx string, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	s.Logger.Error(ctx, "err", err)
	http.Error(w, "internal error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func pathID(r *http.Request) (int64, error) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid id")
	}
	return id, nil
}

func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
