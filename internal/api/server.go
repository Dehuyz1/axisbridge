// Package api hosts the REST surface for axis-core. UI talks here; workers do
// not — they read/write Postgres directly through db + jobq.
//
// Scope: register nomor + list gift sukses + settings + bulk upload + CSV
// export. Renewal/OVO belum dipasang; setting `renew.minus_days` disiapkan
// tetapi worker renew belum ada.
package api

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
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
	mux.HandleFunc("POST /api/accounts/bulk", s.bulkRegister)
	mux.HandleFunc("GET /api/accounts/{id}/token", s.revealToken)
	mux.HandleFunc("POST /api/accounts/{id}/resend", s.resendOTP)
	mux.HandleFunc("POST /api/accounts/{id}/pause", s.pauseAccount)
	mux.HandleFunc("POST /api/accounts/{id}/resume", s.resumeAccount)
	mux.HandleFunc("DELETE /api/accounts/{id}", s.deleteAccount)
	mux.HandleFunc("GET /api/accounts.csv", s.exportAccountsCSV)

	mux.HandleFunc("GET /api/gifts", s.listGifts)
	mux.HandleFunc("POST /api/gifts/bulk", s.bulkGift)
	mux.HandleFunc("DELETE /api/gifts/{id}", s.deleteGift)
	mux.HandleFunc("POST /api/gifts/delete_all", s.deleteAllGifts)
	mux.HandleFunc("GET /api/gifts.csv", s.exportGiftsCSV)

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

// accountRow adalah bentuk yang di-render UI Management. `Dead` bool
// menyederhanakan filter "mati / tidak" tanpa memaksa UI paham enum penuh.
type accountRow struct {
	ID             int64      `json:"id"`
	MSISDN         string     `json:"msisdn"`
	Status         string     `json:"status"`
	Dead           bool       `json:"dead"`
	MasaAktifUntil *time.Time `json:"masa_aktif_until,omitempty"`
	Paket          *string    `json:"paket,omitempty"`
	Pulsa          int64      `json:"pulsa"`
	OTPAttempts    int        `json:"otp_attempts"`
	OTPLastAt      *time.Time `json:"otp_last_at,omitempty"`
	LastLoginAt    *time.Time `json:"last_login_at,omitempty"`
	HasToken       bool       `json:"has_token"`
	Note           *string    `json:"note,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

// sortable columns → whitelist SQL supaya UI bisa kirim ORDER BY apa saja
// tanpa injection.
var accountSortCols = map[string]string{
	"id":               "id",
	"msisdn":           "msisdn",
	"status":           "status::text",
	"dead":             "dead",
	"masa_aktif_until": "masa_aktif_until",
	"paket":            "paket",
	"pulsa":            "pulsa",
	"otp_attempts":     "otp_attempts",
	"otp_last_at":      "otp_last_at",
	"last_login_at":    "last_login_at",
	"created_at":       "created_at",
	"updated_at":       "updated_at",
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := parseIntDefault(q.Get("limit"), 500)
	offset := parseIntDefault(q.Get("offset"), 0)
	// UI hanya mengenal 2 status: `dead` / `alive`. Filter enum penuh masih
	// didukung via ?status= untuk debugging.
	statusEnum := q.Get("status")
	statusFlag := strings.ToLower(q.Get("dead")) // "true" | "false" | ""
	search := q.Get("q")

	sortKey := q.Get("sort")
	sortCol, ok := accountSortCols[sortKey]
	if !ok {
		sortCol = "id"
	}
	sortDir := "DESC"
	if strings.ToLower(q.Get("order")) == "asc" {
		sortDir = "ASC"
	}

	args := []any{limit, offset}
	// Management page hanya melihat nomor register-flow. Nomor gift_only
	// (upload via Gift page) sengaja disembunyikan supaya list tetap bersih.
	where := "gift_only = FALSE"
	if statusEnum != "" {
		args = append(args, statusEnum)
		where += fmt.Sprintf(" AND status = $%d::account_status", len(args))
	}
	switch statusFlag {
	case "true", "1", "dead":
		where += " AND dead = TRUE"
	case "false", "0", "alive":
		where += " AND dead = FALSE"
	}
	if search != "" {
		args = append(args, "%"+search+"%")
		where += fmt.Sprintf(" AND msisdn LIKE $%d", len(args))
	}
	query := fmt.Sprintf(`
        SELECT id, msisdn, status::text, dead, masa_aktif_until, paket, pulsa,
               otp_attempts, otp_last_at, last_login_at,
               (axis_token IS NOT NULL AND axis_token <> '') AS has_token,
               note, created_at, updated_at
          FROM accounts
         WHERE %s
         ORDER BY %s %s NULLS LAST, id DESC
         LIMIT $1 OFFSET $2`, where, sortCol, sortDir)

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
			&a.ID, &a.MSISDN, &a.Status, &a.Dead, &a.MasaAktifUntil, &a.Paket, &a.Pulsa,
			&a.OTPAttempts, &a.OTPLastAt, &a.LastLoginAt, &a.HasToken,
			&a.Note, &a.CreatedAt, &a.UpdatedAt,
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
	Reused   bool   `json:"reused"`
	Enqueued bool   `json:"enqueued"`
}

// registerAccount: satu nomor.
func (s *Server) registerAccount(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	resp, err := s.registerOne(r.Context(), req.MSISDN, req.Note)
	if err != nil {
		s.writeRegisterErr(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// bulkRegister menerima list nomor (JSON array atau text/plain satu-per-baris)
// dan mendaftarkan semuanya. Response = ringkasan per baris.
type bulkResult struct {
	Input    string `json:"input"`
	MSISDN   string `json:"msisdn,omitempty"`
	Status   string `json:"status,omitempty"`
	ID       int64  `json:"id,omitempty"`
	Reused   bool   `json:"reused,omitempty"`
	Enqueued bool   `json:"enqueued,omitempty"`
	Error    string `json:"error,omitempty"`
}

func (s *Server) bulkRegister(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var inputs []string

	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		// dua bentuk: array of string, atau {"numbers":["..","..."]}.
		var arr []string
		if json.Unmarshal(body, &arr) == nil && len(arr) > 0 {
			inputs = arr
		} else {
			var obj struct {
				Numbers []string `json:"numbers"`
			}
			if err := json.Unmarshal(body, &obj); err != nil {
				http.Error(w, "invalid json", http.StatusBadRequest)
				return
			}
			inputs = obj.Numbers
		}
	} else {
		// text/plain: pisah dengan whitespace, koma, atau newline.
		for _, tok := range strings.FieldsFunc(string(body), func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\r' || r == '\t'
		}) {
			if tok != "" {
				inputs = append(inputs, tok)
			}
		}
	}

	results := make([]bulkResult, 0, len(inputs))
	for _, raw := range inputs {
		item := bulkResult{Input: raw}
		resp, err := s.registerOne(r.Context(), raw, nil)
		if err != nil {
			item.Error = err.Error()
		} else {
			item.MSISDN = resp.MSISDN
			item.Status = resp.Status
			item.ID = resp.ID
			item.Reused = resp.Reused
			item.Enqueued = resp.Enqueued
		}
		results = append(results, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":   len(results),
		"results": results,
	})
}

// registerOne dipakai baik oleh /api/register maupun /api/accounts/bulk.
func (s *Server) registerOne(ctx context.Context, raw string, note *string) (registerResp, error) {
	msisdn := axis.NormalizePhone(raw)
	if msisdn == "" {
		return registerResp{}, errInvalid("msisdn required")
	}
	if !axis.IsAxis(msisdn) {
		return registerResp{}, errInvalid("not an AXIS number")
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return registerResp{}, err
	}
	defer tx.Rollback(ctx)

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
    `, msisdn, note).Scan(&id, &status, &reused)
	if err != nil {
		return registerResp{}, err
	}
	if _, err := tx.Exec(ctx, `
        INSERT INTO jobs(kind, account_id, payload, max_attempts)
        VALUES('axis_otp_request'::job_kind, $1, jsonb_build_object('attempt', 1), 1)
    `, id); err != nil {
		return registerResp{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE accounts SET status='otp_pending' WHERE id=$1`, id); err != nil {
		return registerResp{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return registerResp{}, err
	}
	return registerResp{ID: id, MSISDN: msisdn, Status: "otp_pending", Reused: reused, Enqueued: true}, nil
}

// revealToken dipanggil UI ketika user double-click status. Cuma nomor +
// token; kosong kalau belum ada.
func (s *Server) revealToken(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var (
		msisdn  string
		token   *string
		refresh *string
	)
	err = s.Pool.QueryRow(r.Context(), `
        SELECT msisdn, axis_token, axis_refresh FROM accounts WHERE id=$1
    `, id).Scan(&msisdn, &token, &refresh)
	if err != nil {
		s.serverError(w, "reveal token", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      id,
		"msisdn":  msisdn,
		"token":   strPtr(token),
		"refresh": strPtr(refresh),
	})
}

// resendOTP memaksa satu putaran register baru.
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

// exportAccountsCSV: seluruh kolom master.
func (s *Server) exportAccountsCSV(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Pool.Query(r.Context(), `
        SELECT id, msisdn, status::text, dead, masa_aktif_until, paket, pulsa,
               otp_attempts, otp_last_at, last_login_at,
               COALESCE(note, ''), created_at, updated_at
          FROM accounts
         ORDER BY id`)
	if err != nil {
		s.serverError(w, "export accounts", err)
		return
	}
	defer rows.Close()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="accounts.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"id", "msisdn", "status", "dead", "masa_aktif_until", "paket", "pulsa",
		"otp_attempts", "otp_last_at", "last_login_at", "note",
		"created_at", "updated_at",
	})
	for rows.Next() {
		var (
			id                       int64
			msisdn, status, note     string
			dead                     bool
			masaAktif                *time.Time
			paket                    *string
			pulsa                    int64
			otpAttempts              int
			otpLastAt, lastLoginAt   *time.Time
			createdAt, updatedAt     time.Time
		)
		if err := rows.Scan(&id, &msisdn, &status, &dead, &masaAktif, &paket, &pulsa,
			&otpAttempts, &otpLastAt, &lastLoginAt, &note, &createdAt, &updatedAt); err != nil {
			s.Logger.Error("export scan", "err", err)
			return
		}
		_ = cw.Write([]string{
			strconv.FormatInt(id, 10), msisdn, status, strconv.FormatBool(dead),
			fmtDate(masaAktif), strPtr(paket), strconv.FormatInt(pulsa, 10),
			strconv.Itoa(otpAttempts), fmtTime(otpLastAt), fmtTime(lastLoginAt), note,
			createdAt.Format(time.RFC3339), updatedAt.Format(time.RFC3339),
		})
	}
	cw.Flush()
}

// -- gifts -------------------------------------------------------------------

// giftRow: format flat (bukan JSON blob) supaya UI bisa tabel biasa.
type giftRow struct {
	ID         int64      `json:"id"`
	MSISDN     string     `json:"msisdn"`
	Paket      *string    `json:"paket,omitempty"`
	TrxID      *string    `json:"trx_id,omitempty"`
	Amount     *int64     `json:"amount,omitempty"`
	OVOMSISDN  *string    `json:"ovo_msisdn,omitempty"`
	OVOTrxID   *string    `json:"ovo_trx_id,omitempty"`
	OVOAmount  *int64     `json:"ovo_amount,omitempty"`
	Proof      json.RawMessage `json:"proof,omitempty"` // legacy blob (stub)
	CreatedAt  time.Time  `json:"created_at"`
}

func (s *Server) listGifts(w http.ResponseWriter, r *http.Request) {
	limit := parseIntDefault(r.URL.Query().Get("limit"), 1000)
	search := r.URL.Query().Get("q")
	args := []any{limit}
	where := "TRUE"
	if search != "" {
		args = append(args, "%"+search+"%")
		where += fmt.Sprintf(" AND msisdn LIKE $%d", len(args))
	}
	query := fmt.Sprintf(`
        SELECT id, msisdn, paket, trx_id, amount,
               ovo_msisdn, ovo_trx_id, ovo_amount,
               proof, created_at
          FROM gifts
         WHERE %s
         ORDER BY created_at DESC
         LIMIT $1`, where)
	rows, err := s.Pool.Query(r.Context(), query, args...)
	if err != nil {
		s.serverError(w, "list gifts", err)
		return
	}
	defer rows.Close()
	out := make([]giftRow, 0, limit)
	for rows.Next() {
		var g giftRow
		if err := rows.Scan(&g.ID, &g.MSISDN, &g.Paket, &g.TrxID, &g.Amount,
			&g.OVOMSISDN, &g.OVOTrxID, &g.OVOAmount, &g.Proof, &g.CreatedAt); err != nil {
			s.serverError(w, "scan gift", err)
			return
		}
		out = append(out, g)
	}
	writeJSON(w, http.StatusOK, out)
}

// bulkGift menerima list nomor (JSON array atau text/plain) dan langsung
// mengirimnya ke antrian gift-worker TANPA melalui OTP loop. Nomor ditandai
// `gift_only=true` supaya tidak muncul di dashboard Management.
//
// Cocok untuk skenario: user sudah punya nomor yang siap dikasih gift dan
// tidak butuh proses register/OTP (misal nomor dari batch external).
func (s *Server) bulkGift(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var inputs []string
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		var arr []string
		if json.Unmarshal(body, &arr) == nil && len(arr) > 0 {
			inputs = arr
		} else {
			var obj struct {
				Numbers []string `json:"numbers"`
			}
			if err := json.Unmarshal(body, &obj); err != nil {
				http.Error(w, "invalid json", http.StatusBadRequest)
				return
			}
			inputs = obj.Numbers
		}
	} else {
		for _, tok := range strings.FieldsFunc(string(body), func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\r' || r == '\t'
		}) {
			if tok != "" {
				inputs = append(inputs, tok)
			}
		}
	}

	results := make([]bulkResult, 0, len(inputs))
	for _, raw := range inputs {
		item := bulkResult{Input: raw}
		resp, err := s.enqueueGiftOnly(r.Context(), raw)
		if err != nil {
			item.Error = err.Error()
		} else {
			item.MSISDN = resp.MSISDN
			item.Status = resp.Status
			item.ID = resp.ID
			item.Reused = resp.Reused
			item.Enqueued = resp.Enqueued
		}
		results = append(results, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total":   len(results),
		"results": results,
	})
}

// enqueueGiftOnly: insert/reuse account dengan flag gift_only=true dan langsung
// enqueue job axis_gift. Tidak menyentuh OTP fields.
func (s *Server) enqueueGiftOnly(ctx context.Context, raw string) (registerResp, error) {
	msisdn := axis.NormalizePhone(raw)
	if msisdn == "" {
		return registerResp{}, errInvalid("msisdn required")
	}
	if !axis.IsAxis(msisdn) {
		return registerResp{}, errInvalid("not an AXIS number")
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return registerResp{}, err
	}
	defer tx.Rollback(ctx)

	var (
		id     int64
		reused bool
	)
	err = tx.QueryRow(ctx, `
        INSERT INTO accounts(msisdn, status, gift_only)
        VALUES($1, 'gift_wait', TRUE)
        ON CONFLICT (msisdn) DO UPDATE
           SET status    = 'gift_wait',
               gift_only = TRUE
        RETURNING id, (xmax <> 0) AS reused
    `, msisdn).Scan(&id, &reused)
	if err != nil {
		return registerResp{}, err
	}
	if _, err := tx.Exec(ctx, `
        INSERT INTO jobs(kind, account_id, payload, max_attempts)
        VALUES('axis_gift'::job_kind, $1, jsonb_build_object('gift_only', true), 3)
    `, id); err != nil {
		return registerResp{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return registerResp{}, err
	}
	return registerResp{ID: id, MSISDN: msisdn, Status: "gift_wait", Reused: reused, Enqueued: true}, nil
}

// deleteGift menghapus satu row gift. Account tidak ikut dihapus supaya
// audit trail tetap ada; kalau user mau hapus account, pakai
// DELETE /api/accounts/{id}.
func (s *Server) deleteGift(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := s.Pool.Exec(r.Context(), `DELETE FROM gifts WHERE id=$1`, id); err != nil {
		s.serverError(w, "delete gift", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// deleteAllGifts: nuke seluruh tabel gifts (butuh body {"confirm":true}).
func (s *Server) deleteAllGifts(w http.ResponseWriter, r *http.Request) {
	var body struct{ Confirm bool `json:"confirm"` }
	_ = json.NewDecoder(r.Body).Decode(&body)
	if !body.Confirm {
		http.Error(w, "need {\"confirm\":true}", http.StatusBadRequest)
		return
	}
	ct, err := s.Pool.Exec(r.Context(), `DELETE FROM gifts`)
	if err != nil {
		s.serverError(w, "delete all gifts", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": ct.RowsAffected()})
}

func (s *Server) exportGiftsCSV(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Pool.Query(r.Context(), `
        SELECT id, msisdn, COALESCE(paket, ''), COALESCE(trx_id, ''),
               COALESCE(amount, 0), COALESCE(ovo_msisdn, ''),
               COALESCE(ovo_trx_id, ''), COALESCE(ovo_amount, 0),
               created_at
          FROM gifts ORDER BY created_at DESC`)
	if err != nil {
		s.serverError(w, "export gifts", err)
		return
	}
	defer rows.Close()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="gifts.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"id", "msisdn", "paket", "trx_id", "amount",
		"ovo_msisdn", "ovo_trx_id", "ovo_amount", "created_at",
	})
	for rows.Next() {
		var (
			id, amount, ovoAmt          int64
			msisdn, paket, trx          string
			ovoMsi, ovoTrx              string
			createdAt                   time.Time
		)
		if err := rows.Scan(&id, &msisdn, &paket, &trx, &amount,
			&ovoMsi, &ovoTrx, &ovoAmt, &createdAt); err != nil {
			s.Logger.Error("export gifts scan", "err", err)
			return
		}
		_ = cw.Write([]string{
			strconv.FormatInt(id, 10), msisdn, paket, trx,
			strconv.FormatInt(amount, 10), ovoMsi, ovoTrx,
			strconv.FormatInt(ovoAmt, 10), createdAt.Format(time.RFC3339),
		})
	}
	cw.Flush()
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
		Total int64 `json:"total"`
		Alive int64 `json:"alive"`
		Dead  int64 `json:"dead"`
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
          (SELECT COUNT(*) FROM accounts WHERE dead = FALSE),
          (SELECT COUNT(*) FROM accounts WHERE dead = TRUE),
          (SELECT COUNT(*) FROM gifts),
          (SELECT COUNT(*) FROM jobs WHERE state='pending'),
          (SELECT COUNT(*) FROM jobs WHERE state='running'),
          (SELECT COUNT(*) FROM jobs WHERE state='failed')
    `).Scan(
		&o.Accounts.Total, &o.Accounts.Alive, &o.Accounts.Dead,
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

type validationErr struct{ msg string }

func (e *validationErr) Error() string { return e.msg }

func errInvalid(msg string) error { return &validationErr{msg: msg} }

func (s *Server) writeRegisterErr(w http.ResponseWriter, err error) {
	var ve *validationErr
	if errors.As(err, &ve) {
		http.Error(w, ve.msg, http.StatusBadRequest)
		return
	}
	s.serverError(w, "register", err)
}

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

func strPtr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func fmtDate(p *time.Time) string {
	if p == nil {
		return ""
	}
	return p.Format("2006-01-02")
}

func fmtTime(p *time.Time) string {
	if p == nil {
		return ""
	}
	return p.Format(time.RFC3339)
}
