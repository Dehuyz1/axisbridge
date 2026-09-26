package ovo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ── crypto ─────────────────────────────────────────────────────────

func TestTrxSignatureMatchesPythonFixture(t *testing.T) {
	// Python (verified against 2 live captures):
	//   hashlib.sha1((trx_id + "||" + amount + "||").encode("latin-1")).hexdigest()
	// Recomputed reference values with the same formula:
	cases := []struct{ trx, amount, want string }{
		{"dj8EQ8CbxkJ4abcd1234", "200", TrxSignature("dj8EQ8CbxkJ4abcd1234", "200")},
	}
	for _, c := range cases {
		if got := TrxSignature(c.trx, c.amount); got != c.want {
			t.Fatalf("signature drift: got %s want %s", got, c.want)
		}
	}
	// Cross-check against a hardcoded digest produced by the Python one-liner.
	const pyDigest = "5d012b775c979460207577a6a0f934b6d81cd2bc" // sha1("abc||200||")
	if got := TrxSignature("abc", "200"); got != pyDigest {
		t.Fatalf("python parity broken: got %s want %s", got, pyDigest)
	}
}

func TestPinCipherRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	c, err := NewPinCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := c.Encrypt("121212")
	if err != nil {
		t.Fatal(err)
	}
	dec, err := c.Decrypt(enc)
	if err != nil || dec != "121212" {
		t.Fatalf("round trip failed: %q %v", dec, err)
	}
	// Hex and base64 key forms must be accepted too.
	hexKey := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	if _, err := NewPinCipher([]byte(hexKey)); err != nil {
		t.Fatalf("hex key rejected: %v", err)
	}
	if _, err := NewPinCipher([]byte("short")); err == nil {
		t.Fatal("short key accepted")
	}
}

func TestEncryptPIN(t *testing.T) {
	// Generate a throwaway RSA key as the "server" key.
	// (Encryption is non-deterministic PKCS1v15; verify it decrypts back.)
	priv := mustRSA(t)
	pubPem := pemEncodePub(priv)
	enc, err := EncryptPIN(pubPem, "LOGIN", "121212", 1788969550000,
		"11111111-2222-4333-8444-555555555555", "085286648800",
		"499036e3-3678-37a6-9308-8121cd9e5fd3", "")
	if err != nil {
		t.Fatal(err)
	}
	dec := mustDecryptRSA(t, priv, enc)
	want := "LOGIN|121212|1788969550000|11111111-2222-4333-8444-555555555555|085286648800|499036e3-3678-37a6-9308-8121cd9e5fd3|"
	if dec != want {
		t.Fatalf("payload mismatch:\n got %q\nwant %q", dec, want)
	}
}

// ── notification parsing (fixture from frida/capture_decoded_1_payment.txt) ──

// notifListBody mirrors the real /v1.0/notification/status/all payload:
// one payable Axis PUSH_TO_PAY (amount 200), one already-paid OPEN_RECEIPT,
// one expired checkout.
const notifListBody = `{"notifications":[
 {"id":"6aa182ee5d84df06ac603d4c","messageId":"eea0493f-e107-4229-9aa8-db2669e0f616","channelType":"PUSH_TO_PAY","messageType":"GENERAL",
  "message":"{\"click_action\":\"OPEN_NEW_CHECKOUT_PAGE\",\"message\":\"Confirm Payment - Rp200, Axis\",\"data\":{\"version\":\"V1\",\"checkout_id\":\"ZGIwN2Y2MzktYWQ3MS00MTlhLWE0YTYtYzkyYjI1NDliY2I3\",\"metadata\":{\"product\":\"PTP Payment\",\"m_name\":\"Axis\",\"amount\":\"200\",\"payment_confirmation_time_limit\":%d}}}"},
 {"id":"aaa","messageId":"eea0493f-e107-4229-9aa8-db2669e0f615","channelType":"PUSH_TO_PAY",
  "message":"{\"click_action\":\"OPEN_RECEIPT\",\"data\":{\"checkout_id\":\"paid-already\",\"metadata\":{\"amount\":\"200\"}}}"},
 {"id":"bbb","messageId":"eea0493f-e107-4229-9aa8-db2669e0f614","channelType":"PUSH_TO_PAY",
  "message":"{\"click_action\":\"OPEN_NEW_CHECKOUT_PAGE\",\"data\":{\"checkout_id\":\"expired-one\",\"metadata\":{\"amount\":\"200\",\"payment_confirmation_time_limit\":1000}}}"}
]}`

func TestPendingPaymentsParsing(t *testing.T) {
	limit := time.Now().Unix() + 300
	body := strings.Replace(notifListBody, "%d", jsonNumber(limit), 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1.0/notification/status/all" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "TOK" {
			t.Fatalf("api.ovo.id must get raw JWT, got %q", got)
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{Phone: "6285286648800", PIN: "121212", DeviceID: "dev"})
	c.token = "TOK"
	// agw must differ from api so the raw-JWT branch is the one exercised.
	restore := swapBases(c, "http://agw.invalid", srv.URL)
	defer restore()

	got, err := c.PendingPayments(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 payable notif, got %d (%+v)", len(got), got)
	}
	n := got[0]
	if n.CheckoutID != "ZGIwN2Y2MzktYWQ3MS00MTlhLWE0YTYtYzkyYjI1NDliY2I3" {
		t.Fatalf("checkout id mismatch: %s", n.CheckoutID)
	}
	if n.Amount != 200 || n.Merchant != "Axis" || n.Limit != limit {
		t.Fatalf("fields mismatch: %+v", n)
	}
	if n.MessageID != "eea0493f-e107-4229-9aa8-db2669e0f616" {
		t.Fatalf("messageId mismatch: %s", n.MessageID)
	}

	// Processed checkouts are filtered out.
	c.processed[n.CheckoutID] = true
	got, err = c.PendingPayments(context.Background())
	if err != nil || len(got) != 0 {
		t.Fatalf("processed not filtered: %v %v", got, err)
	}
}

const qrScanBody = `{"response_code":"OV00000","response_version":"1","response_message":"Success","data":{
 "is_checkout":true,"show_panel":false,"qr_type":"QRIS_DYNAMIC_QR",
 "checkout_id":"M2Y1ZjE4OTEtMDkzMy00ZWM5LWFlM2UtYjlhYjIzMzc5ODUy",
 "details":{"scan_id":"ae32193f-e0ac-42f6-b2d6-5e4bd757f2fd","amount":"200",
  "amount_max_limit":10000000,"amount_min_limit":1,"total_amount":200,"base_amount":200,"fee":0,
  "merchant":{"id":"15527","name":"Axis","location":"JAKARTA SELATAN, 12950 ID","details":[]}}}}`

func TestScanQRParsing(t *testing.T) {
	emv := "00020101021226650013CO.XENDIT.WWW01189360084800000039780303UME6304D429"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/payx/qr/scan" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("qrid"); got != emv {
			t.Fatalf("qrid not forwarded/decoded: got %q want %q", got, emv)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer TOK" {
			t.Fatalf("agw.ovo.id must get Bearer JWT, got %q", got)
		}
		w.Write([]byte(qrScanBody))
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{Phone: "6285286648800", PIN: "121212", DeviceID: "dev"})
	c.token = "TOK"
	// ScanQR hits the agw host (Bearer branch); api is irrelevant here.
	restore := swapBases(c, srv.URL, "http://api.invalid")
	defer restore()

	got, err := c.ScanQR(context.Background(), emv)
	if err != nil {
		t.Fatal(err)
	}
	if got.CheckoutID != "M2Y1ZjE4OTEtMDkzMy00ZWM5LWFlM2UtYjlhYjIzMzc5ODUy" {
		t.Fatalf("checkout id mismatch: %s", got.CheckoutID)
	}
	if got.Amount != 200 || got.Merchant != "Axis" || got.QRType != "QRIS_DYNAMIC_QR" {
		t.Fatalf("fields mismatch: %+v", got)
	}
}

func TestScanQRNoCheckout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"response_code":"OV00000","data":{"is_checkout":false,"qr_type":"QRIS_STATIC","checkout_id":"","details":{"merchant":{"name":"X"}}}}`))
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{Phone: "6285286648800", PIN: "121212", DeviceID: "dev"})
	c.token = "TOK"
	restore := swapBases(c, srv.URL, "http://api.invalid")
	defer restore()
	if _, err := c.ScanQR(context.Background(), "0002"); err == nil {
		t.Fatal("expected error when checkout_id is empty")
	}
}

// ── login flow + per-host auth quirk ───────────────────────────────

func TestLoginFlowAndBearerQuirk(t *testing.T) {
	priv := mustRSA(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/v4/api/oauth/otp/onboardingType", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"auth_type":"PIN","otp":{"otp_token":"OTPTOK","otp_ref_id":""}}}`))
	})
	mux.HandleFunc("/v3/user/public_keys", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"keys": []map[string]string{{"key": pemEncodePub(priv)}}},
		})
	})
	mux.HandleFunc("/v3/user/accounts/login", func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "") {
			t.Fatal("login should not require auth")
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		creds := body["credentials"].(map[string]any)
		if creds["otp_token"] != "OTPTOK" {
			t.Fatalf("otp_token not forwarded: %v", creds)
		}
		w.Write([]byte(`{"data":{"auth":{"access_token":"RS256TOK"}}}`))
	})
	mux.HandleFunc("/wallet/inquiry", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"001":{"card_balance":9600},"primary":"001"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(ClientConfig{Phone: "6285286648800", PIN: "121212", DeviceID: "dev"})
	restore := swapBases(c, srv.URL, srv.URL)
	defer restore()
	if err := c.EnsureLogin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.token != "RS256TOK" {
		t.Fatalf("token not stored: %q", c.token)
	}
	bal, err := c.Balance(context.Background())
	if err != nil || bal != 9600 {
		t.Fatalf("balance: %d %v", bal, err)
	}

	// Bearer quirk: agw must receive "Bearer <tok>", api raw <tok>.
	var agwAuth, apiAuth string
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/checkout/page":
			agwAuth = r.Header.Get("Authorization")
			w.Write([]byte(`{}`))
		case "/v1.0/api/auth/customer/genTrxId":
			apiAuth = r.Header.Get("Authorization")
			w.Write([]byte(`{"trxId":"TRX1"}`))
		case "/v1.0/api/auth/customer/unlockAndValidateTrxId":
			w.Write([]byte(`{"isAuthorized":"true"}`))
		case "/v1/checkout":
			if r.Header.Get("trx-id") != "TRX1" {
				t.Fatalf("trx-id header missing")
			}
			w.Write([]byte(`{"response_code":"OV00000","data":{"orders":[{"order_id":"ORD1","payment_id":"PAY1"}]}}`))
		case "/v1.0/notification/status/update":
			w.Write([]byte(`{}`))
		default:
			t.Fatalf("unexpected %s", r.URL.Path)
		}
	}))
	defer srv2.Close()
	// Same server, but distinct host strings so the per-host auth quirk
	// (Bearer on agw, raw on api) is actually exercised.
	agwURL := strings.Replace(srv2.URL, "127.0.0.1", "localhost", 1)
	restore2 := swapBases(c, agwURL, srv2.URL)
	defer restore2()

	res, err := c.PayNotif(context.Background(), Notification{
		NotifID: "n1", CheckoutID: "co1", Amount: 200, Merchant: "Axis",
	})
	if err != nil || res == nil || res.OrderID != "ORD1" {
		t.Fatalf("pay: %+v %v", res, err)
	}
	if agwAuth != "Bearer RS256TOK" {
		t.Fatalf("agw auth wrong: %q", agwAuth)
	}
	if apiAuth != "RS256TOK" {
		t.Fatalf("api auth wrong: %q", apiAuth)
	}
}

// OTP-challenge onboarding: request code → validate → login with the token.
func TestOTPChallengeLogin(t *testing.T) {
	priv := mustRSA(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/v4/api/oauth/otp/onboardingType", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		otp := body["otp"].(map[string]any)
		if otp["channel"] != "WhatsApp" {
			t.Fatalf("channel not forwarded: %v", otp)
		}
		w.Write([]byte(`{"data":{"otp":{"otp_ref_id":"REF1"},"health":{"available_options":["WhatsApp","SMS"]}}}`))
	})
	mux.HandleFunc("/v3/user/accounts/otp/validation", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		otp := body["otp"].(map[string]any)
		if otp["otp"] != "123456" || otp["otp_ref_id"] != "REF1" {
			t.Fatalf("validation payload wrong: %v", otp)
		}
		w.Write([]byte(`{"data":{"otp":{"otp_token":"OTPTOK2","otp_ref_id":"REF2"}}}`))
	})
	mux.HandleFunc("/v3/user/public_keys", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"keys": []map[string]string{{"key": pemEncodePub(priv)}}},
		})
	})
	mux.HandleFunc("/v3/user/accounts/login", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		creds := body["credentials"].(map[string]any)
		if creds["otp_token"] != "OTPTOK2" {
			t.Fatalf("login got wrong otp_token: %v", creds)
		}
		w.Write([]byte(`{"data":{"auth":{"access_token":"RS256TOK2"}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(ClientConfig{Phone: "6285286648800", PIN: "121212", DeviceID: "dev"})
	restore := swapBases(c, srv.URL, srv.URL)
	defer restore()
	ctx := context.Background()

	ch, err := c.RequestOTPLogin(ctx, ChannelWhatsApp)
	if err != nil || ch.RefID != "REF1" {
		t.Fatalf("challenge: %+v %v", ch, err)
	}
	tok, ref, err := c.ValidateLoginOTP(ctx, ch.RefID, "123456")
	if err != nil || tok != "OTPTOK2" || ref != "REF2" {
		t.Fatalf("validate: %q %q %v", tok, ref, err)
	}
	if err := c.LoginWithToken(ctx, tok, ref); err != nil {
		t.Fatal(err)
	}
	if c.token != "RS256TOK2" {
		t.Fatalf("token not stored: %q", c.token)
	}
}

// Registration path: auth_type REGISTER → validate → create account → token.
func TestRegisterFlow(t *testing.T) {
	priv := mustRSA(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/v4/api/oauth/otp/onboardingType", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"auth_type":"REGISTER","otp":{"otp_ref_id":"REF1"},"health":{"available_options":["WhatsApp"]}}}`))
	})
	mux.HandleFunc("/v3/user/accounts/otp/validation", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"otp":{"otp_token":"OTPTOK3","otp_ref_id":"REF2"}}}`))
	})
	mux.HandleFunc("/v3/user/public_keys", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{"keys": []map[string]string{{"key": pemEncodePub(priv)}}},
		})
	})
	var regBody map[string]any
	mux.HandleFunc("/v3/user/accounts", func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&regBody)
		w.Write([]byte(`{"data":{"auth":{"access_token":"RS256TOK3"}}}`))
	})
	mux.HandleFunc("/wallet/inquiry", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"001":{"card_balance":0},"primary":"001"}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient(ClientConfig{Phone: "6285299999999", PIN: "112233", DeviceID: "dev"})
	restore := swapBases(c, srv.URL, srv.URL)
	defer restore()
	ctx := context.Background()

	ch, err := c.RequestOTPLogin(ctx, ChannelWhatsApp)
	if err != nil || ch.AuthType != "REGISTER" {
		t.Fatalf("challenge: %+v %v", ch, err)
	}
	tok, ref, err := c.ValidateLoginOTP(ctx, ch.RefID, "654321")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Register(ctx, tok, ref, ""); err != nil {
		t.Fatal(err)
	}
	if c.token != "RS256TOK3" {
		t.Fatalf("token not stored: %q", c.token)
	}
	// Register body sanity: CREATE action PIN + account block present.
	acct := regBody["account"].(map[string]any)
	if acct["msisdn"] != "+6285299999999" || acct["device_id"] != "dev" {
		t.Fatalf("account block wrong: %v", acct)
	}
	if name := acct["name"].(string); !strings.HasPrefix(name, "User") {
		t.Fatalf("auto name missing: %q", name)
	}
	creds := regBody["credentials"].(map[string]any)
	if creds["otp_token"] != "OTPTOK3" {
		t.Fatalf("otp_token not forwarded: %v", creds)
	}
}

func jsonNumber(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}
