package browsersession

import (
	"errors"
	"os"
	"path/filepath"
)

func readSecretFile(path string) ([]byte, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("browser secret path must be canonical and absolute")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("browser secret must be a regular non-symlink file")
	}
	return os.ReadFile(path)
}
func LoadEncryptionKey(path string) ([]byte, error) {
	raw, err := readSecretFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) != 32 {
		return nil, errors.New("browser session encryption key must contain exactly 32 bytes")
	}
	return raw, nil
}
func LoadConsoleClientSecret(path string) (string, error) {
	raw, err := readSecretFile(path)
	if err != nil {
		return "", err
	}
	if len(raw) < 32 || len(raw) > 256 {
		return "", errors.New("browser client secret must contain 32..256 bytes")
	}
	for _, b := range raw {
		if b < 0x21 || b > 0x7e {
			return "", errors.New("browser client secret must be printable ASCII without whitespace")
		}
	}
	return string(raw), nil
}
