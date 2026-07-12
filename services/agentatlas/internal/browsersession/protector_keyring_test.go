package browsersession

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"testing"
)

func TestProtectorKeyringWritesCurrentKeyIDAndReadsPrevious(t *testing.T) {
	oldKey := EncryptionKey{ID: "2026-06", Key: bytes.Repeat([]byte{1}, 32)}
	newKey := EncryptionKey{ID: "2026-07", Key: bytes.Repeat([]byte{2}, 32)}
	oldProtector, err := NewProtectorKeyring(oldKey)
	if err != nil {
		t.Fatal(err)
	}
	oldCiphertext, err := oldProtector.Seal("credential")
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := NewProtectorKeyring(newKey, oldKey)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := rotated.Open(oldCiphertext); err != nil || got != "credential" {
		t.Fatalf("open previous=%q err=%v", got, err)
	}
	currentCiphertext, err := rotated.Seal("new-credential")
	if err != nil {
		t.Fatal(err)
	}
	if got := CiphertextKeyID(currentCiphertext); got != newKey.ID {
		t.Fatalf("ciphertext key id=%q", got)
	}
	if _, err := oldProtector.Open(currentCiphertext); err == nil {
		t.Fatal("ciphertext encrypted with unknown current key was accepted")
	}
}

func TestProtectorKeyringReadsUnversionedCiphertextDuringRollingDeployment(t *testing.T) {
	oldRaw := bytes.Repeat([]byte{5}, 32)
	block, _ := aes.NewCipher(oldRaw)
	aead, _ := cipher.NewGCM(block)
	nonce := bytes.Repeat([]byte{9}, aead.NonceSize())
	legacy := aead.Seal(nonce, nonce, []byte("legacy-credential"), []byte("agentatlas-browser-session-v1"))
	protector, err := NewProtectorKeyring(
		EncryptionKey{ID: "current", Key: bytes.Repeat([]byte{6}, 32)},
		EncryptionKey{ID: "previous", Key: oldRaw},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := protector.Open(legacy); err != nil || got != "legacy-credential" {
		t.Fatalf("open legacy=%q err=%v", got, err)
	}
}
