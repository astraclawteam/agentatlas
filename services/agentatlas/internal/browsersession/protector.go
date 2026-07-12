package browsersession

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

var ciphertextV2Magic = []byte("AAS2")

type EncryptionKey struct {
	ID  string
	Key []byte
}

type Protector struct {
	current string
	keys    map[string]cipher.AEAD
	order   []string
}

func NewProtector(key []byte) (*Protector, error) {
	return NewProtectorKeyring(EncryptionKey{ID: "legacy", Key: key})
}

func NewProtectorKeyring(current EncryptionKey, previous ...EncryptionKey) (*Protector, error) {
	all := append([]EncryptionKey{current}, previous...)
	p := &Protector{current: current.ID, keys: make(map[string]cipher.AEAD, len(all))}
	for _, item := range all {
		if !validKeyID(item.ID) || len(item.Key) != 32 {
			return nil, errors.New("browser session encryption key requires a safe ID and exactly 32 bytes")
		}
		if _, exists := p.keys[item.ID]; exists {
			return nil, errors.New("browser session encryption key IDs must be unique")
		}
		block, err := aes.NewCipher(item.Key)
		if err != nil {
			return nil, err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		p.keys[item.ID] = aead
		p.order = append(p.order, item.ID)
	}
	return p, nil
}

func (p *Protector) Seal(value string) ([]byte, error) {
	if p == nil || value == "" {
		return nil, errors.New("browser session protector unavailable")
	}
	aead := p.keys[p.current]
	if aead == nil {
		return nil, errors.New("browser session current encryption key unavailable")
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	id := []byte(p.current)
	prefix := make([]byte, 0, len(ciphertextV2Magic)+1+len(id)+len(nonce))
	prefix = append(prefix, ciphertextV2Magic...)
	prefix = append(prefix, byte(len(id)))
	prefix = append(prefix, id...)
	prefix = append(prefix, nonce...)
	return aead.Seal(prefix, nonce, []byte(value), v2AAD(p.current)), nil
}

func (p *Protector) Open(ciphertext []byte) (string, error) {
	if p == nil {
		return "", errors.New("browser session protector unavailable")
	}
	if id := CiphertextKeyID(ciphertext); id != "" {
		aead := p.keys[id]
		if aead == nil {
			return "", fmt.Errorf("browser session ciphertext uses unavailable key %q", id)
		}
		offset := len(ciphertextV2Magic) + 1 + len(id)
		if len(ciphertext) < offset+aead.NonceSize()+aead.Overhead() {
			return "", errors.New("invalid browser session ciphertext")
		}
		nonce := ciphertext[offset : offset+aead.NonceSize()]
		plain, err := aead.Open(nil, nonce, ciphertext[offset+aead.NonceSize():], v2AAD(id))
		if err != nil {
			return "", err
		}
		return string(plain), nil
	}
	// Read pre-keyring ciphertext during a rolling deployment. New writes always
	// carry a key ID, so this compatibility path disappears once old rows expire.
	for _, id := range p.order {
		aead := p.keys[id]
		if len(ciphertext) < aead.NonceSize()+aead.Overhead() {
			continue
		}
		nonce := ciphertext[:aead.NonceSize()]
		plain, err := aead.Open(nil, nonce, ciphertext[aead.NonceSize():], []byte("agentatlas-browser-session-v1"))
		if err == nil {
			return string(plain), nil
		}
	}
	return "", errors.New("invalid browser session ciphertext")
}

func CiphertextKeyID(ciphertext []byte) string {
	if len(ciphertext) < len(ciphertextV2Magic)+2 || !bytes.Equal(ciphertext[:len(ciphertextV2Magic)], ciphertextV2Magic) {
		return ""
	}
	n := int(ciphertext[len(ciphertextV2Magic)])
	start := len(ciphertextV2Magic) + 1
	if n < 1 || n > 64 || len(ciphertext) < start+n {
		return ""
	}
	id := string(ciphertext[start : start+n])
	if !validKeyID(id) {
		return ""
	}
	return id
}

func validKeyID(id string) bool {
	if len(id) < 1 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

func v2AAD(id string) []byte { return []byte("agentatlas-browser-session-v2:" + id) }
