package browsersession

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"io"
)

type Protector struct{ aead cipher.AEAD }

func NewProtector(key []byte) (*Protector, error) {
	if len(key) != 32 {
		return nil, errors.New("browser session encryption key must be exactly 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Protector{aead: aead}, nil
}
func (p *Protector) Seal(value string) ([]byte, error) {
	if p == nil || p.aead == nil || value == "" {
		return nil, errors.New("browser session protector unavailable")
	}
	nonce := make([]byte, p.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return p.aead.Seal(nonce, nonce, []byte(value), []byte("agentatlas-browser-session-v1")), nil
}
func (p *Protector) Open(ciphertext []byte) (string, error) {
	if p == nil || p.aead == nil || len(ciphertext) < p.aead.NonceSize() {
		return "", errors.New("invalid browser session ciphertext")
	}
	nonce := ciphertext[:p.aead.NonceSize()]
	plain, err := p.aead.Open(nil, nonce, ciphertext[p.aead.NonceSize():], []byte("agentatlas-browser-session-v1"))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
