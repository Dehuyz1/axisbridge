package ovo

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"testing"
)

// mustRSA generates a throwaway 2048-bit key for tests.
func mustRSA(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// pemEncodePub returns the PKIX PEM of the public key, like /v3/user/public_keys.
func pemEncodePub(priv *rsa.PrivateKey) string {
	der, err := x509.MarshalPKIXPublicKey(&priv.PublicKey)
	if err != nil {
		panic(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

// mustDecryptRSA reverses EncryptPIN with the private half.
func mustDecryptRSA(t *testing.T, priv *rsa.PrivateKey, encB64 string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(encB64)
	if err != nil {
		t.Fatal(err)
	}
	pt, err := rsa.DecryptPKCS1v15(rand.Reader, priv, raw)
	if err != nil {
		t.Fatal(err)
	}
	return string(pt)
}
// swapBases points an already-built client at httptest servers by swapping
// its per-host base fields. Returns a restore function.
func swapBases(c *Client, agw, api string) func() {
	oldAGW, oldAPI := c.agw, c.api
	c.agw, c.api = agw, api
	return func() { c.agw, c.api = oldAGW, oldAPI }
}
