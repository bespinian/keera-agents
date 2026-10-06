// Package secret seals what keera-agents keeps that must not be readable at
// rest: cookies, agents' gateway keys and the credentials of connections. The
// key lives only in the environment.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
)

type Box struct {
	aead cipher.AEAD
	// mac derives secrets nobody stores, such as a workspace's ingress
	// secret. It is a key of its own, not the sealing key.
	mac []byte
}

func New(key string) (*Box, error) {
	// config.Load checks this too. This guards other callers.
	if len(key) < 16 {
		return nil, errors.New("the secret key must be at least 16 characters; " +
			"generate one with: openssl rand -hex 32")
	}
	// Domain separation: reusing the gateway's key still gives a different key.
	sum := sha256.Sum256([]byte("keera-agents-v1|" + key))
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	mac := sha256.Sum256([]byte("keera-agents-mac-v1|" + key))
	return &Box{aead: aead, mac: mac[:]}, nil
}

// Derive is a secret only this key can make from name and data, so it can be
// made again instead of stored.
func (b *Box) Derive(name string, data []byte) []byte {
	m := hmac.New(sha256.New, b.mac)
	m.Write([]byte(name))
	m.Write([]byte{0})
	m.Write(data)
	return m.Sum(nil)
}

// Seal encrypts plaintext bound to name, so it cannot be opened under
// another name, such as one agent's key under another agent's id.
func (b *Box) Seal(name string, plaintext []byte) []byte {
	return b.aead.Seal(nil, nil, plaintext, []byte(name))
}

// Open decrypts what Seal produced for the same name.
func (b *Box) Open(name string, sealed []byte) ([]byte, error) {
	out, err := b.aead.Open(nil, nil, sealed, []byte(name))
	if err != nil {
		return nil, errors.New("secret: cannot decrypt with the configured key")
	}
	return out, nil
}
