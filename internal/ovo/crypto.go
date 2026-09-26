// Package ovo implements the OVO payment client used by the embedded payer.
// Protocol ported from OVO/CORE/payment_bot/ovo_pay_bot.py (verified via
// Frida capture, OVO 3.168.0, 2026-09-09). Do not "clean up" request shapes:
// field names, header quirks and signature formats are load-bearing.
package ovo

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
)

// EncryptPIN mirrors the Python _encrypt_pin: RSA PKCS#1 v1.5 over
// "ACTION|{pin}|{ts_ms}|{uuid}|{phone08}|{device_id}|{otp_ref_id}",
// base64 standard output. pemKey is the server public key from
// /v3/user/public_keys.
func EncryptPIN(pemKey, action, pin string, tsMillis int64, uuid, phone08, deviceID, otpRefID string) (string, error) {
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil {
		return "", errors.New("invalid PEM public key")
	}
	pubAny, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		// Some servers return PKCS#1; try that before giving up.
		if pk, err2 := x509.ParsePKCS1PublicKey(block.Bytes); err2 == nil {
			pubAny = pk
		} else {
			return "", fmt.Errorf("parse public key: %w", err)
		}
	}
	pub, ok := pubAny.(*rsa.PublicKey)
	if !ok {
		return "", errors.New("not an RSA public key")
	}
	payload := fmt.Sprintf("%s|%s|%d|%s|%s|%s|%s",
		action, pin, tsMillis, uuid, phone08, deviceID, otpRefID)
	ct, err := rsa.EncryptPKCS1v15(rand.Reader, pub, []byte(payload))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ct), nil
}

// TrxSignature mirrors the verified unlockAndValidateTrxId signature:
// SHA1(trxID + "||" + amount + "||") lowercase hex. Amount is the integer
// string of the total payment (e.g. "200"); deviceId is empty.
func TrxSignature(trxID, amount string) string {
	h := sha1.Sum([]byte(trxID + "||" + amount + "||"))
	return hex.EncodeToString(h[:])
}

// PinCipher encrypts/decrypts OVO PINs and cached tokens at rest
// (AES-256-GCM). The key comes from the OVO_MASTER_KEY env (32 bytes raw, or
// hex/base64 encoded).
type PinCipher struct {
	aead cipher.AEAD
}

// NewPinCipher builds the cipher from raw key material. Accepts a 32-byte
// slice directly, or a hex/base64 string decoding to 32 bytes.
func NewPinCipher(key []byte) (*PinCipher, error) {
	if len(key) != 32 {
		if d, err := hex.DecodeString(string(key)); err == nil && len(d) == 32 {
			key = d
		} else if d, err := base64.StdEncoding.DecodeString(string(key)); err == nil && len(d) == 32 {
			key = d
		} else {
			return nil, errors.New("pin key must be 32 bytes (raw, hex or base64)")
		}
	}
	aead, err := cipher.NewGCM(mustAES(key))
	if err != nil {
		return nil, err
	}
	return &PinCipher{aead: aead}, nil
}

func mustAES(key []byte) cipher.Block {
	b, err := aes.NewCipher(key)
	if err != nil {
		panic(err) // unreachable: key length already validated
	}
	return b
}

// Encrypt returns base64(nonce || ciphertext).
func (c *PinCipher) Encrypt(plain string) (string, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := c.aead.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

// Decrypt reverses Encrypt.
func (c *PinCipher) Decrypt(enc string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", err
	}
	ns := c.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("ciphertext too short")
	}
	pt, err := c.aead.Open(nil, raw[:ns], raw[ns:], nil)
	if err != nil {
		return "", err
	}
	return string(pt), nil
}
