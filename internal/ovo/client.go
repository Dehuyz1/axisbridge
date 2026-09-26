package ovo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	agwBase = "https://agw.ovo.id"
	apiBase = "https://api.ovo.id"

	appVersion = "3.168.0"
	clientID   = "ovo_android"
	userAgent  = "okhttp/4.12.0"
	osVersion  = "14"

	// tokenMaxAge is how long a cached token is trusted before a probe
	// (OVO issues 7-day tokens; refresh a day early).
	tokenMaxAge = 6 * 24 * time.Hour
)

// ErrAuthExpired signals a dead session — caller should re-login once.
var ErrAuthExpired = errors.New("ovo auth expired")

// Client is one OVO account: PIN login, notification polling, payment.
// Not safe for concurrent use — by design all calls for one account are
// serialized by the single payment loop that owns the client.
type Client struct {
	cfg    ClientConfig
	hc     *http.Client
	token  string // access_token (raw; Bearer prepended per-host)
	pubKey string // server RSA public key PEM
	// agw/api are the per-host bases; fields (not the consts) so tests can
	// point the client at httptest servers.
	agw string
	api string
	// processedCheckouts mirrors the Python session guard: checkouts already
	// paid/skipped by THIS process are never retried, even before the
	// persistent guard in the bridge DB is consulted.
	processed map[string]bool
	// LoadCache/SaveCache persist the token (bridge wires these to its DB).
	LoadCache func() (*TokenCache, error)
	SaveCache func(*TokenCache) error
}

// NewClient builds a client for one account.
func NewClient(cfg ClientConfig) *Client {
	return &Client{
		cfg:       cfg,
		hc:        &http.Client{Timeout: 30 * time.Second},
		agw:       agwBase,
		api:       apiBase,
		processed: make(map[string]bool),
	}
}
// phonePlus returns +62xxx.
func (c *Client) phonePlus() string {
	p := c.cfg.Phone
	if strings.HasPrefix(p, "+") {
		return p
	}
	return "+" + p
}

// phone08 returns 08xxx.
func (c *Client) phone08() string {
	p := strings.TrimPrefix(c.cfg.Phone, "+")
	if strings.HasPrefix(p, "62") {
		return "0" + p[2:]
	}
	if strings.HasPrefix(p, "0") {
		return p
	}
	return "0" + p
}

// do performs one request with the per-host Authorization quirk:
// agw.ovo.id wants "Bearer <jwt>", api.ovo.id wants the raw JWT.
func (c *Client) do(ctx context.Context, method, base, path string, body any, extra map[string]string) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("OS", "Android")
	req.Header.Set("OS-Version", osVersion)
	req.Header.Set("App-Version", appVersion)
	req.Header.Set("client-id", clientID)
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	req.Header.Set("device-id", c.cfg.DeviceID)
	if c.token != "" {
		if base == c.agw {
			req.Header.Set("Authorization", "Bearer "+c.token)
		} else {
			req.Header.Set("Authorization", c.token)
		}
	}
	for k, v := range extra {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// OV00217 (pin not authorized) arrives as 403 mid-payment and is NOT
		// an expired session; the caller handles it by authorizing the trxId.
		// Only 401 means the token itself is dead.
		resp.Body.Close()
		return nil, ErrAuthExpired
	}
	return resp, nil
}

// readJSON decodes a response body, capping size for safety.
func readJSON(resp *http.Response, out any) error {
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// extractError condenses an OVO error body ({data:{title,message,response_code}})
// into a short human string, mirroring the Python helper.
func extractError(raw []byte, fallback string) string {
	var j struct {
		Data struct {
			Title        string `json:"title"`
			Message      string `json:"message"`
			ResponseCode string `json:"response_code"`
		} `json:"data"`
		Title        string `json:"title"`
		Message      string `json:"message"`
		ResponseCode string `json:"response_code"`
	}
	if json.Unmarshal(raw, &j) != nil {
		return fallback
	}
	title := firstStr(j.Data.Title, j.Title)
	msg := firstStr(j.Data.Message, j.Message)
	code := firstStr(j.Data.ResponseCode, j.ResponseCode)
	var parts []string
	if title != "" {
		parts = append(parts, title)
	}
	if msg != "" && msg != title {
		parts = append(parts, msg)
	}
	if code != "" {
		parts = append(parts, "["+code+"]")
	}
	if len(parts) == 0 {
		return fallback
	}
	return strings.Join(parts, " — ")
}

func firstStr(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// EnsureLogin guarantees a live session: valid cache → reuse; otherwise a
// full PIN login. Called once at bridge start per account, then only again
// after ErrAuthExpired. This is the "always logged in, idle in between"
// contract — no keepalive traffic exists outside this function.
func (c *Client) EnsureLogin(ctx context.Context) error {
	if c.token != "" {
		return nil
	}
	if c.LoadCache != nil {
		if tc, err := c.LoadCache(); err == nil && tc != nil &&
			tc.AccessToken != "" && time.Since(tc.SavedAt) < tokenMaxAge {
			c.token = tc.AccessToken
			if _, err := c.Balance(ctx); err == nil {
				return nil
			}
			c.token = ""
		}
	}
	return c.login(ctx)
}

// OTPChannel selects where OVO sends the login verification code.
type OTPChannel string

const (
	ChannelWhatsApp OTPChannel = "WhatsApp"
	ChannelSMS      OTPChannel = "SMS"
)

// LoginChallenge is the result of requesting an onboarding OTP: the reference
// the user-visible code validates against, which flow OVO demands for this
// number, and the channels it reports as available.
type LoginChallenge struct {
	RefID string
	// AuthType is OVO's decision for this number+device: "REGISTER" means the
	// number has no account yet (Confirm must create it), "PIN"/"OTP_PIN"
	// means an existing account (Confirm logs in). Anything else is treated
	// as "login, fall back to register when the server rejects with OV00612".
	AuthType string
	Options  []string
}

// RequestOTPLogin asks OVO to send an onboarding OTP to the given channel
// (WhatsApp or SMS) and returns the challenge. Works for both existing
// accounts (login) and numbers OVO has never seen (registration) — the
// AuthType field says which. Serialized by the caller (worker ovoCall).
func (c *Client) RequestOTPLogin(ctx context.Context, channel OTPChannel) (*LoginChallenge, error) {
	if channel != ChannelSMS {
		channel = ChannelWhatsApp
	}
	body := map[string]any{
		"channel_code": clientID,
		"device_id":    c.cfg.DeviceID,
		"msisdn":       c.phonePlus(),
		"otp":          map[string]string{"locale": "ID", "sms_hash": "m9mj4ctIVR8", "channel": string(channel)},
	}
	resp, err := c.do(ctx, http.MethodPost, c.agw, "/v4/api/oauth/otp/onboardingType", body, nil)
	if err != nil {
		return nil, err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var ot struct {
		Data struct {
			AuthType string `json:"auth_type"`
			OTP      struct {
				RefID string `json:"otp_ref_id"`
			} `json:"otp"`
			Health struct {
				Options []string `json:"available_options"`
			} `json:"health"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &ot)
	if ot.Data.OTP.RefID == "" {
		return nil, fmt.Errorf("request OTP: %s", extractError(raw, "no otp_ref_id"))
	}
	return &LoginChallenge{
		RefID:    ot.Data.OTP.RefID,
		AuthType: ot.Data.AuthType,
		Options:  ot.Data.Health.Options,
	}, nil
}

// Register creates a brand-new OVO account for the number: POST
// /v3/user/accounts with the PIN encrypted under the CREATE action. The
// response carries the same 7-day access token login returns, so after this
// the client is fully usable (poll, pay) without a separate login.
func (c *Client) Register(ctx context.Context, otpToken, otpRef, name string) error {
	if err := c.fetchPubKey(ctx); err != nil {
		return err
	}
	encPIN, err := EncryptPIN(c.pubKey, "CREATE", c.cfg.PIN,
		time.Now().UnixMilli(), newUUID(), c.phone08(), c.cfg.DeviceID, otpRef)
	if err != nil {
		return fmt.Errorf("encrypt pin: %w", err)
	}
	if name == "" {
		name = "User" + c.phone08()[len(c.phone08())-4:]
	}
	body := map[string]any{
		"account": map[string]any{
			"device_id":            c.cfg.DeviceID,
			"msisdn":               c.phonePlus(),
			"name":                 name,
			"push_notification_id": genFCMToken(),
		},
		"channel_code": clientID,
		"credentials": map[string]any{
			"otp_token": otpToken,
			"password":  map[string]string{"value": encPIN, "format": "rsa"},
		},
	}
	resp, err := c.do(ctx, http.MethodPost, c.agw, "/v3/user/accounts", body, nil)
	if err != nil {
		return err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var rr struct {
		Data struct {
			Auth struct {
				AccessToken string `json:"access_token"`
			} `json:"auth"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &rr)
	if rr.Data.Auth.AccessToken == "" {
		return fmt.Errorf("register: %s", extractError(raw, fmt.Sprintf("http %d", resp.StatusCode)))
	}
	c.token = rr.Data.Auth.AccessToken
	if c.SaveCache != nil {
		_ = c.SaveCache(&TokenCache{AccessToken: c.token, SavedAt: time.Now()})
	}
	return nil
}

// IsAccountNotFound reports whether err is OVO's "account not found"
// (OV00612) — the signal that a login attempt on a REGISTER-type number
// should fall back to creating the account.
func IsAccountNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "OV00612")
}

// ValidateLoginOTP exchanges the user-typed code for an otp_token usable by
// LoginWithToken. The RefID may rotate in the response — the new one is
// returned and MUST be used for the PIN encryption payload.
func (c *Client) ValidateLoginOTP(ctx context.Context, refID, code string) (otpToken, newRefID string, err error) {
	body := map[string]any{
		"channel_code": clientID,
		"device_id":    c.cfg.DeviceID,
		"msisdn":       c.phonePlus(),
		"otp":          map[string]string{"otp": code, "otp_ref_id": refID, "type": "LOGIN"},
	}
	resp, err := c.do(ctx, http.MethodPost, c.agw, "/v3/user/accounts/otp/validation", body, nil)
	if err != nil {
		return "", "", err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var ov struct {
		Data struct {
			OTP struct {
				Token string `json:"otp_token"`
				RefID string `json:"otp_ref_id"`
			} `json:"otp"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &ov)
	if ov.Data.OTP.Token == "" {
		return "", "", fmt.Errorf("validate OTP: %s", extractError(raw, "no otp_token"))
	}
	newRefID = ov.Data.OTP.RefID
	if newRefID == "" {
		newRefID = refID
	}
	return ov.Data.OTP.Token, newRefID, nil
}

// LoginWithToken completes the login using an otp_token obtained out-of-band
// (ValidateLoginOTP): public_keys → RSA PIN → accounts/login.
func (c *Client) LoginWithToken(ctx context.Context, otpToken, otpRef string) error {
	return c.loginWithToken(ctx, otpToken, otpRef)
}

// login performs the PIN-only flow: onboardingType → public_keys → login.
func (c *Client) login(ctx context.Context) error {
	body := map[string]any{
		"channel_code": clientID,
		"device_id":    c.cfg.DeviceID,
		"msisdn":       c.phonePlus(),
		"otp":          map[string]string{"locale": "ID", "type": "LOGIN"},
	}
	resp, err := c.do(ctx, http.MethodPost, c.agw, "/v4/api/oauth/otp/onboardingType", body, nil)
	if err != nil {
		return err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var ot struct {
		Data struct {
			AuthType string `json:"auth_type"`
			OTP      struct {
				Token string `json:"otp_token"`
				RefID string `json:"otp_ref_id"`
			} `json:"otp"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &ot)
	otpToken := ot.Data.OTP.Token
	otpRef := ot.Data.OTP.RefID
	if otpToken == "" {
		return fmt.Errorf("onboardingType: %s", extractError(raw, "no otp_token"))
	}
	return c.loginWithToken(ctx, otpToken, otpRef)
}

// loginWithToken is the shared tail of both login paths: fetch the RSA key,
// encrypt the PIN, exchange for the 7-day access token.
func (c *Client) loginWithToken(ctx context.Context, otpToken, otpRef string) error {
	if err := c.fetchPubKey(ctx); err != nil {
		return err
	}
	encPIN, err := EncryptPIN(c.pubKey, "LOGIN", c.cfg.PIN,
		time.Now().UnixMilli(), newUUID(), c.phone08(), c.cfg.DeviceID, otpRef)
	if err != nil {
		return fmt.Errorf("encrypt pin: %w", err)
	}
	loginBody := map[string]any{
		"msisdn":               c.phonePlus(),
		"device_id":            c.cfg.DeviceID,
		"push_notification_id": genFCMToken(),
		"channel_code":         clientID,
		"credentials": map[string]any{
			"otp_token": otpToken,
			"password":  map[string]string{"value": encPIN, "format": "rsa"},
		},
	}
	resp, err := c.do(ctx, http.MethodPost, c.agw, "/v3/user/accounts/login", loginBody, nil)
	if err != nil {
		return err
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var lr struct {
		Data struct {
			Auth struct {
				AccessToken string `json:"access_token"`
			} `json:"auth"`
		} `json:"data"`
	}
	_ = json.Unmarshal(raw, &lr)
	if lr.Data.Auth.AccessToken == "" {
		return fmt.Errorf("login: %s", extractError(raw, fmt.Sprintf("http %d", resp.StatusCode)))
	}
	c.token = lr.Data.Auth.AccessToken
	if c.SaveCache != nil {
		_ = c.SaveCache(&TokenCache{AccessToken: c.token, SavedAt: time.Now()})
	}
	return nil
}

// fetchPubKey loads the server RSA public key used to encrypt the PIN.
func (c *Client) fetchPubKey(ctx context.Context) error {
	resp, err := c.do(ctx, http.MethodGet, c.agw, "/v3/user/public_keys", nil, nil)
	if err != nil {
		return err
	}
	var keys struct {
		Data struct {
			Keys []struct {
				Key string `json:"key"`
			} `json:"keys"`
		} `json:"data"`
	}
	if err := readJSON(resp, &keys); err != nil || len(keys.Data.Keys) == 0 {
		return errors.New("public_keys: no key returned")
	}
	c.pubKey = keys.Data.Keys[0].Key
	return nil
}

// call runs fn once; on ErrAuthExpired it re-logs in once and retries once.
func (c *Client) call(ctx context.Context, fn func() error) error {
	if err := c.EnsureLogin(ctx); err != nil {
		return err
	}
	err := fn()
	if errors.Is(err, ErrAuthExpired) {
		c.token = ""
		if err2 := c.login(ctx); err2 != nil {
			return err2
		}
		return fn()
	}
	return err
}

// Balance returns the OVO Cash balance (wallet "001").
func (c *Client) Balance(ctx context.Context) (int64, error) {
	var bal int64
	err := c.call(ctx, func() error {
		resp, err := c.do(ctx, http.MethodGet, c.api, "/wallet/inquiry", nil, nil)
		if err != nil {
			return err
		}
		// data is a map of wallet type → {card_balance}; the live payload also
		// carries extra keys (and may encode the balance as a string), so parse
		// each wallet entry independently and ignore anything unrecognized.
		var j struct {
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := readJSON(resp, &j); err != nil {
			return err
		}
		raw, ok := j.Data["001"]
		if !ok {
			return errors.New("wallet 001 missing")
		}
		var w struct {
			CardBalance any `json:"card_balance"`
		}
		if err := json.Unmarshal(raw, &w); err != nil {
			return err
		}
		bal = toInt64(w.CardBalance)
		return nil
	})
	return bal, err
}

// PendingPayments returns payable PUSH_TO_PAY notifications, oldest-limit
// first, skipping expired ones and checkouts this client already processed.
func (c *Client) PendingPayments(ctx context.Context) ([]Notification, error) {
	var out []Notification
	err := c.call(ctx, func() error {
		resp, err := c.do(ctx, http.MethodGet, c.api, "/v1.0/notification/status/all", nil, nil)
		if err != nil {
			return err
		}
		var j struct {
			Notifications []struct {
				ID          string `json:"id"`
				MessageID   string `json:"messageId"`
				ChannelType string `json:"channelType"`
				Message     string `json:"message"`
			} `json:"notifications"`
		}
		if err := readJSON(resp, &j); err != nil {
			return err
		}
		out = out[:0]
		now := time.Now().Unix()
		for _, n := range j.Notifications {
			if n.ChannelType != "PUSH_TO_PAY" {
				continue
			}
			var msg struct {
				ClickAction string `json:"click_action"`
				Data        struct {
					CheckoutID string `json:"checkout_id"`
					Amount     any    `json:"amount"`
					Limit      any    `json:"payment_confirmation_time_limit"`
					Metadata   struct {
						Amount   any    `json:"amount"`
						Limit    any    `json:"payment_confirmation_time_limit"`
						Merchant string `json:"m_name"`
					} `json:"metadata"`
				} `json:"data"`
			}
			if json.Unmarshal([]byte(n.Message), &msg) != nil {
				continue
			}
			if msg.ClickAction != "OPEN_NEW_CHECKOUT_PAGE" {
				continue // already paid -> OPEN_RECEIPT
			}
			amount := toInt64(firstAny(msg.Data.Metadata.Amount, msg.Data.Amount))
			limit := toInt64(firstAny(msg.Data.Metadata.Limit, msg.Data.Limit))
			if limit > 0 && limit < now {
				continue // expired
			}
			if msg.Data.CheckoutID == "" || c.processed[msg.Data.CheckoutID] {
				continue
			}
			out = append(out, Notification{
				NotifID:    n.ID,
				CheckoutID: msg.Data.CheckoutID,
				Amount:     amount,
				Merchant:   msg.Data.Metadata.Merchant,
				Limit:      limit,
				MessageID:  n.MessageID,
			})
		}
		return nil
	})
	return out, err
}

// ScanResult is the resolved merchant + checkout for one QRIS EMV string,
// from GET /v1/payx/qr/scan. CheckoutID feeds the shared payment pipeline
// (PayNotif) directly — no notification polling, no amount matching.
type ScanResult struct {
	CheckoutID string
	Amount     int64
	Merchant   string
	QRType     string
}

// ScanQR resolves a QRIS EMVCo payload (the string AXIS returns in the "qr"
// field of a payment="qris" purchase) into a payable checkout. The returned
// CheckoutID is specific to this one charge, so paying it cannot be confused
// with another same-amount charge the way PUSH_TO_PAY notifications can.
func (c *Client) ScanQR(ctx context.Context, emv string) (*ScanResult, error) {
	var out *ScanResult
	err := c.call(ctx, func() error {
		resp, err := c.do(ctx, http.MethodGet, c.agw,
			"/v1/payx/qr/scan?qrid="+url.QueryEscape(emv), nil, nil)
		if err != nil {
			return err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("qr/scan http %d: %.200s", resp.StatusCode, raw)
		}
		var j struct {
			ResponseCode string `json:"response_code"`
			Data         struct {
				IsCheckout bool   `json:"is_checkout"`
				QRType     string `json:"qr_type"`
				CheckoutID string `json:"checkout_id"`
				Details    struct {
					Amount      any `json:"amount"`
					TotalAmount any `json:"total_amount"`
					Merchant    struct {
						Name string `json:"name"`
					} `json:"merchant"`
				} `json:"details"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &j); err != nil {
			return fmt.Errorf("qr/scan decode: %v", err)
		}
		if j.ResponseCode != "OV00000" {
			return fmt.Errorf("qr/scan response_code %s", j.ResponseCode)
		}
		if j.Data.CheckoutID == "" {
			return errors.New("qr/scan returned no checkout_id")
		}
		out = &ScanResult{
			CheckoutID: j.Data.CheckoutID,
			Amount:     toInt64(firstAny(j.Data.Details.TotalAmount, j.Data.Details.Amount)),
			Merchant:   j.Data.Details.Merchant.Name,
			QRType:     j.Data.QRType,
		}
		return nil
	})
	return out, err
}

func firstAny(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

// toInt64 tolerates JSON numbers arriving as float64 or string.
func toInt64(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case string:
		n, _ := strconv.ParseInt(t, 10, 64)
		return n
	case json.Number:
		n, _ := t.Int64()
		return n
	}
	return 0
}

// PayNotif pays one PUSH_TO_PAY notification end to end. Returns
// (result, nil) on success, (nil, nil) when the checkout is already gone
// (paid/expired elsewhere — skip, do not retry), or (nil, err) on failure.
func (c *Client) PayNotif(ctx context.Context, n Notification) (*PayResult, error) {
	if c.processed[n.CheckoutID] {
		return nil, nil
	}
	var res *PayResult
	err := c.call(ctx, func() error {
		// 1. checkout/page — resolves the bill; OV00209/OV00216 = gone.
		resp, err := c.do(ctx, http.MethodPost, c.agw, "/v1/checkout/page",
			map[string]any{"checkout_id": n.CheckoutID, "metadata": map[string]any{}}, nil)
		if err != nil {
			return err
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			s := string(raw)
			if strings.Contains(s, "OV00209") || strings.Contains(s, "OV00216") ||
				strings.Contains(strings.ToLower(s), "not found") ||
				strings.Contains(strings.ToLower(s), "already initiated") {
				c.processed[n.CheckoutID] = true
				_ = c.MarkRead(ctx, n.NotifID)
				return errCheckoutGone
			}
			return fmt.Errorf("checkout/page http %d: %.200s", resp.StatusCode, s)
		}

		// 2. genTrxId
		amountStr := strconv.FormatInt(n.Amount, 10)
		resp, err = c.do(ctx, http.MethodPost, c.api, "/v1.0/api/auth/customer/genTrxId",
			map[string]string{"actionMark": "PAY_TRX_ID", "amount": amountStr}, nil)
		if err != nil {
			return err
		}
		var trx struct {
			TrxID string `json:"trxId"`
		}
		if err := readJSON(resp, &trx); err != nil || trx.TrxID == "" {
			return fmt.Errorf("genTrxId failed: %v", err)
		}

		// 3. unlockAndValidateTrxId — SHA1 signature + plaintext PIN.
		resp, err = c.do(ctx, http.MethodPost, c.api,
			"/v1.0/api/auth/customer/unlockAndValidateTrxId",
			map[string]string{
				"signature":    TrxSignature(trx.TrxID, amountStr),
				"trxId":        trx.TrxID,
				"appVersion":   appVersion,
				"securityCode": c.cfg.PIN,
			}, nil)
		if err != nil {
			return err
		}
		raw, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(strings.ToLower(string(raw)), "true") {
			return fmt.Errorf("unlockAndValidateTrxId http %d: %.200s", resp.StatusCode, raw)
		}

		// 4. /v1/checkout with the trx-id header.
		resp, err = c.do(ctx, http.MethodPost, c.agw, "/v1/checkout",
			map[string]any{
				"bill":        []map[string]any{{"amount": n.Amount, "type": "ovo_cash"}},
				"campaign_id": "",
				"checkout_id": n.CheckoutID,
				"metadata":    map[string]string{"product_name": ""},
			}, map[string]string{"trx-id": trx.TrxID})
		if err != nil {
			return err
		}
		raw, _ = io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		var cr struct {
			ResponseCode string `json:"response_code"`
			Data         struct {
				Orders []struct {
					OrderID   string `json:"order_id"`
					PaymentID string `json:"payment_id"`
				} `json:"orders"`
			} `json:"data"`
		}
		_ = json.Unmarshal(raw, &cr)
		if resp.StatusCode != http.StatusOK ||
			(cr.ResponseCode != "" && cr.ResponseCode != "OV00000") {
			return fmt.Errorf("checkout http %d: %.300s", resp.StatusCode, raw)
		}
		res = &PayResult{}
		if len(cr.Data.Orders) > 0 {
			res.OrderID = cr.Data.Orders[0].OrderID
			res.PaymentID = cr.Data.Orders[0].PaymentID
		}
		c.processed[n.CheckoutID] = true
		return nil
	})
	if errors.Is(err, errCheckoutGone) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	_ = c.MarkRead(ctx, n.NotifID)
	return res, nil
}

var errCheckoutGone = errors.New("checkout already processed elsewhere")

// MarkRead marks a notification read (best effort).
func (c *Client) MarkRead(ctx context.Context, notifID string) error {
	return c.call(ctx, func() error {
		resp, err := c.do(ctx, http.MethodPut, c.api, "/v1.0/notification/status/update",
			map[string]string{"notificationId": notifID}, nil)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	})
}

// WasProcessed reports whether this client already paid/skipped a checkout.
func (c *Client) WasProcessed(checkoutID string) bool { return c.processed[checkoutID] }

// Phone returns the configured account phone (62xxx).
func (c *Client) Phone() string { return c.cfg.Phone }
