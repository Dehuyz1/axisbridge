package ovo

import "time"

// Notification is one payable PUSH_TO_PAY notification parsed from
// GET /v1.0/notification/status/all.
type Notification struct {
	NotifID    string // notification id (for mark-read)
	CheckoutID string // checkout to pay
	Amount     int64  // total payment in IDR
	Merchant   string // merchant display name (m_name)
	Limit      int64  // payment_confirmation_time_limit, unix seconds; 0 = none
	MessageID  string // notification messageId — ordering key for baselines
}

// PayResult is the outcome of a successful /v1/checkout.
type PayResult struct {
	OrderID   string
	PaymentID string // merchant invoice
}

// ClientConfig carries the per-account identity. Phone is 62xxx; PIN is the
// plaintext 6-digit security code (only ever held in memory).
type ClientConfig struct {
	Phone    string // 62xxx
	PIN      string // plaintext, in-memory only
	DeviceID string // stable UUID per account
	Name     string // display name
}

// TokenCache is the persisted session. Stored per account so a restart
// within the 7-day token lifetime skips the login round-trip.
type TokenCache struct {
	AccessToken string    `json:"access_token"`
	SavedAt     time.Time `json:"saved_at"`
}
