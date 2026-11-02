// Package jwttest makes signing keys, key sets and service account files
// for tests of the adapters that verify or sign JWTs.
package jwttest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"testing"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Key is an RSA key that signs tokens as a service would.
type Key struct {
	Private *rsa.PrivateKey
	ID      string
}

func NewKey(t testing.TB, id string) *Key {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return &Key{Private: k, ID: id}
}

// JWKS is the public key as a key set document.
func (k *Key) JWKS() []byte {
	b, _ := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &k.Private.PublicKey, KeyID: k.ID, Algorithm: "RS256", Use: "sig",
	}}})
	return b
}

// Sign signs claims with RS256 and the key's id.
func (k *Key) Sign(t testing.TB, claims map[string]any) string {
	t.Helper()
	s, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: k.Private, KeyID: k.ID}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := jwt.Signed(s).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// ServiceAccountJSON is a Google service account key file for the key.
func (k *Key) ServiceAccountJSON(email string) string {
	der, _ := x509.MarshalPKCS8PrivateKey(k.Private)
	b, _ := json.Marshal(map[string]string{
		"type":           "service_account",
		"project_id":     "test-project",
		"private_key_id": k.ID,
		"private_key":    string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
		"client_email":   email,
		"token_uri":      "https://oauth2.googleapis.com/token",
	})
	return string(b)
}

// Claims reads a token's claims without checking it, for fakes that only
// look at what an adapter sent.
func Claims(t testing.TB, token string, pub *rsa.PublicKey) map[string]any {
	t.Helper()
	tok, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := tok.Claims(pub, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
