// OVO wallet endpoints: register a payer, drive the OTP login/registration
// flow, refresh balance.
//
// Secrets never leave this file in the clear. pin_enc and token_enc are
// AES-256-GCM ciphertext and are never selected into a response struct.
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"axisbridge/internal/axis"
	"axisbridge/internal/ovo"
)

// ovoRow is the wallet shape the dashboard sees. Deliberately omits pin_enc
// and token_enc.
type ovoRow struct {
	ID          int64      `json:"id"`
	MSISDN      string     `json:"msisdn"`
	Name        *string    `json:"name,omitempty"`
	State       string     `json:"state"`
	Enabled     bool       `json:"enabled"`
	Balance     int64      `json:"balance"`
	BalanceAt   *time.Time `json:"balance_at,omitempty"`
	LastError   *string    `json:"last_error,omitempty"`
	OTPAuthType *string    `json:"otp_auth_type,omitempty"`
	HasToken    bool       `json:"has_token"`
	CreatedAt   time.Time  `json:"created_at"`
}

func (s *Server) listOVO(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Pool.Query(r.Context(), `
        SELECT id, msisdn, name, state::text, enabled, balance, balance_at,
               last_error, otp_auth_type,
               (token_enc IS NOT NULL AND token_enc <> '') AS has_token,
               created_at
          FROM ovo_accounts
         ORDER BY id`)
	if err != nil {
		s.serverError(w, "list ovo", err)
		return
	}
	defer rows.Close()

	out := make([]ovoRow, 0, 16)
	for rows.Next() {
		var a ovoRow
		if err := rows.Scan(&a.ID, &a.MSISDN, &a.Name, &a.State, &a.Enabled,
			&a.Balance, &a.BalanceAt, &a.LastError, &a.OTPAuthType,
			&a.HasToken, &a.CreatedAt); err != nil {
			s.serverError(w, "scan ovo", err)
			return
		}
		out = append(out, a)
	}
	writeJSON(w, http.StatusOK, out)
}

type createOVOReq struct {
	MSISDN string `json:"msisdn"`
	PIN    string `json:"pin"`
	Name   string `json:"name"`
}

// createOVO stores a wallet with its PIN encrypted. No network call happens
// here; logging in is a separate, explicit step.
func (s *Server) createOVO(w http.ResponseWriter, r *http.Request) {
	if s.Vault == nil {
		http.Error(w, "OVO_MASTER_KEY not configured; refusing to store a PIN", http.StatusPreconditionFailed)
		return
	}
	var req createOVOReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	// OVO wallets may sit on any operator, so only normalise — never apply the
	// AXIS prefix check here.
	msisdn := axis.NormalizePhone(req.MSISDN)
	if msisdn == "" {
		http.Error(w, "msisdn required", http.StatusBadRequest)
		return
	}
	if len(req.PIN) < 4 {
		http.Error(w, "pin required", http.StatusBadRequest)
		return
	}

	pinEnc, err := s.Vault.Encrypt(req.PIN)
	if err != nil {
		s.serverError(w, "encrypt pin", err)
		return
	}
	deviceID, err := newDeviceID()
	if err != nil {
		s.serverError(w, "device id", err)
		return
	}

	var id int64
	err = s.Pool.QueryRow(r.Context(), `
        INSERT INTO ovo_accounts (msisdn, name, device_id, pin_enc, state)
        VALUES ($1, NULLIF($2,''), $3, $4, 'new')
        ON CONFLICT (msisdn) DO UPDATE
           SET name    = COALESCE(NULLIF($2,''), ovo_accounts.name),
               pin_enc = $4,
               state   = 'new',
               last_error = NULL
        RETURNING id`, msisdn, req.Name, deviceID, pinEnc).Scan(&id)
	if err != nil {
		s.serverError(w, "insert ovo", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "msisdn": msisdn, "state": "new",
	})
}

type ovoOTPReq struct {
	Channel string `json:"channel"` // "whatsapp" | "sms"
}

// ovoRequestOTP asks OVO to send an onboarding code. The response tells the
// operator whether this number will be logged into or created, because OVO
// itself decides that.
func (s *Server) ovoRequestOTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req ovoOTPReq
	_ = json.NewDecoder(r.Body).Decode(&req)

	channel := ovo.ChannelWhatsApp
	if strings.EqualFold(req.Channel, "sms") {
		channel = ovo.ChannelSMS
	}

	client, _, err := s.ovoClient(r.Context(), id)
	if err != nil {
		s.ovoFail(r.Context(), w, id, "build client", err)
		return
	}

	ch, err := client.RequestOTPLogin(r.Context(), channel)
	if err != nil {
		s.ovoFail(r.Context(), w, id, "request otp", err)
		return
	}

	if _, err := s.Pool.Exec(r.Context(), `
        UPDATE ovo_accounts
           SET otp_ref_id = $2, otp_auth_type = $3, state = 'otp_sent',
               last_error = NULL
         WHERE id = $1`, id, ch.RefID, ch.AuthType); err != nil {
		s.serverError(w, "save otp ref", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"auth_type": ch.AuthType,
		"options":   ch.Options,
		"channel":   string(channel),
	})
}

type ovoVerifyReq struct {
	Code string `json:"code"`
}

// ovoVerifyOTP exchanges the operator-supplied code for a session.
//
// Two details from the OVO client drive this flow:
//   - ValidateLoginOTP may rotate the reference id, and the rotated value is
//     the one the PIN encryption must use, so the returned ref is passed on
//     rather than the stored one;
//   - a LOGIN-typed number can still turn out not to exist (OV00612), in which
//     case registration is the correct follow-up.
func (s *Server) ovoVerifyOTP(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req ovoVerifyReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Code == "" {
		http.Error(w, "code required", http.StatusBadRequest)
		return
	}

	client, meta, err := s.ovoClient(r.Context(), id)
	if err != nil {
		s.ovoFail(r.Context(), w, id, "build client", err)
		return
	}
	if meta.otpRefID == "" {
		http.Error(w, "no OTP in flight; request one first", http.StatusConflict)
		return
	}

	otpToken, newRef, err := client.ValidateLoginOTP(r.Context(), meta.otpRefID, req.Code)
	if err != nil {
		s.ovoFail(r.Context(), w, id, "validate otp", err)
		return
	}

	register := strings.EqualFold(meta.otpAuthType, "REGISTER")
	if register {
		err = client.Register(r.Context(), otpToken, newRef, meta.name)
	} else {
		err = client.LoginWithToken(r.Context(), otpToken, newRef)
		if err != nil && ovo.IsAccountNotFound(err) {
			// OVO reported LOGIN but has no such account: create it.
			err = client.Register(r.Context(), otpToken, newRef, meta.name)
			register = true
		}
	}
	if err != nil {
		s.ovoFail(r.Context(), w, id, "login", err)
		return
	}

	// The client persisted the token itself through SaveCache; only the
	// wallet state and the spent OTP reference remain to be cleared.
	if _, err := s.Pool.Exec(r.Context(), `
        UPDATE ovo_accounts
           SET state = 'active', otp_ref_id = NULL, last_error = NULL
         WHERE id = $1`, id); err != nil {
		s.serverError(w, "activate wallet", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state": "active", "registered": register,
	})
}

// ovoBalance refreshes the wallet balance, reusing the cached token when it is
// still good.
func (s *Server) ovoBalance(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	client, _, err := s.ovoClient(r.Context(), id)
	if err != nil {
		s.ovoFail(r.Context(), w, id, "build client", err)
		return
	}
	if err := client.EnsureLogin(r.Context()); err != nil {
		s.ovoFail(r.Context(), w, id, "ensure login", err)
		return
	}
	bal, err := client.Balance(r.Context())
	if err != nil {
		s.ovoFail(r.Context(), w, id, "balance", err)
		return
	}

	if _, err := s.Pool.Exec(r.Context(), `
        UPDATE ovo_accounts
           SET balance = $2, balance_at = NOW(), state = 'active',
               last_error = NULL
         WHERE id = $1`, id, bal); err != nil {
		s.serverError(w, "save balance", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"balance": bal})
}

func (s *Server) deleteOVO(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := s.Pool.Exec(r.Context(), `DELETE FROM ovo_accounts WHERE id = $1`, id); err != nil {
		s.serverError(w, "delete ovo", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// -- helpers -----------------------------------------------------------------

// ovoMeta carries the per-account fields the handlers need beyond the client.
type ovoMeta struct {
	msisdn      string
	name        string
	otpRefID    string
	otpAuthType string
}

// ovoClient loads one wallet, decrypts its PIN, and builds a client seeded
// with any cached token. Plaintext PIN exists only inside the returned client.
func (s *Server) ovoClient(ctx context.Context, id int64) (*ovo.Client, ovoMeta, error) {
	if s.Vault == nil {
		return nil, ovoMeta{}, errInvalid("OVO_MASTER_KEY not configured")
	}
	var (
		meta     ovoMeta
		name     *string
		deviceID string
		pinEnc   string
		refID    *string
		authType *string
	)
	err := s.Pool.QueryRow(ctx, `
        SELECT msisdn, name, device_id, pin_enc, otp_ref_id, otp_auth_type
          FROM ovo_accounts WHERE id = $1`, id).
		Scan(&meta.msisdn, &name, &deviceID, &pinEnc, &refID, &authType)
	if err != nil {
		return nil, ovoMeta{}, err
	}
	meta.name = strPtr(name)
	meta.otpRefID = strPtr(refID)
	meta.otpAuthType = strPtr(authType)

	pin, err := s.Vault.Decrypt(pinEnc)
	if err != nil {
		return nil, meta, fmt.Errorf("decrypt pin: %w", err)
	}

	client := ovo.NewClient(ovo.ClientConfig{
		Phone:    meta.msisdn,
		PIN:      pin,
		DeviceID: deviceID,
		Name:     meta.name,
	})
	// Token persistence goes through the client's own cache hooks rather than
	// a getter: the client also re-logs in inside call() on ErrAuthExpired,
	// and those refreshes must be saved too, not just the ones a handler sees.
	client.LoadCache = s.ovoTokenLoader(ctx, id)
	client.SaveCache = s.ovoTokenSaver(ctx, id)
	return client, meta, nil
}

// ovoTokenLoader reads the encrypted session for one wallet.
func (s *Server) ovoTokenLoader(ctx context.Context, id int64) func() (*ovo.TokenCache, error) {
	return func() (*ovo.TokenCache, error) {
		var (
			enc     *string
			savedAt *time.Time
		)
		err := s.Pool.QueryRow(ctx, `
            SELECT token_enc, token_saved_at FROM ovo_accounts WHERE id = $1`, id).
			Scan(&enc, &savedAt)
		if err != nil {
			return nil, err
		}
		if enc == nil || *enc == "" || savedAt == nil {
			return nil, nil
		}
		tok, err := s.Vault.Decrypt(*enc)
		if err != nil {
			// A key rotation makes old ciphertext unreadable. Treat it as "no
			// cache" so the client falls back to a fresh PIN login.
			s.Logger.Warn("ovo token undecryptable, forcing relogin", "ovo_id", id)
			return nil, nil
		}
		return &ovo.TokenCache{AccessToken: tok, SavedAt: *savedAt}, nil
	}
}

// ovoTokenSaver writes the encrypted session back for one wallet.
func (s *Server) ovoTokenSaver(ctx context.Context, id int64) func(*ovo.TokenCache) error {
	return func(tc *ovo.TokenCache) error {
		if tc == nil || tc.AccessToken == "" {
			return nil
		}
		enc, err := s.Vault.Encrypt(tc.AccessToken)
		if err != nil {
			return err
		}
		_, err = s.Pool.Exec(ctx, `
            UPDATE ovo_accounts SET token_enc = $2, token_saved_at = $3
             WHERE id = $1`, id, enc, tc.SavedAt)
		return err
	}
}

// ovoFail records a wallet-level failure and answers the request. The stored
// message is the short form; nothing secret is ever written here.
func (s *Server) ovoFail(ctx context.Context, w http.ResponseWriter, id int64, stage string, err error) {
	var ve *validationErr
	if errors.As(err, &ve) {
		http.Error(w, ve.msg, http.StatusPreconditionFailed)
		return
	}
	msg := stage + ": " + err.Error()
	if _, uerr := s.Pool.Exec(ctx, `
        UPDATE ovo_accounts
           SET last_error = $2,
               state = CASE WHEN state = 'active' THEN 'login_needed' ELSE state END
         WHERE id = $1`, id, msg); uerr != nil {
		s.Logger.Error("record ovo failure", "err", uerr)
	}
	s.Logger.Warn("ovo call failed", "ovo_id", id, "stage", stage, "err", err)
	http.Error(w, msg, http.StatusBadGateway)
}

// newDeviceID returns a random 32-hex-character device identifier. OVO expects
// a stable value per account, so it is generated once at creation and stored.
func newDeviceID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
