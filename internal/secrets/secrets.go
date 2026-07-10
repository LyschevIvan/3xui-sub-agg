// Package secrets реализует прозрачное симметричное шифрование секретов в БД.
//
// Формат шифрованного значения: "enc:v1:" + base64url(nonce || ciphertext),
// где AES-256-GCM, ключ = SHA-256 от master_key.
//
// Без master_key Cipher остаётся pass-through для legacy-совместимости. Код хранилища
// должен отдельно запрещать pass-through для секретов, требующих шифрования.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

const Prefix = "enc:v1:"

type Cipher struct {
	key [32]byte
	on  bool
}

// New создаёт Cipher. Пустой masterKey → pass-through (без шифрования).
func New(masterKey string) *Cipher {
	if strings.TrimSpace(masterKey) == "" {
		return &Cipher{on: false}
	}
	return &Cipher{key: sha256.Sum256([]byte(masterKey)), on: true}
}

// Enabled — true, если шифрование активировано (master_key задан).
func (c *Cipher) Enabled() bool { return c != nil && c.on }

// Encrypt шифрует строку. Если шифрование выключено — возвращает значение как есть.
func (c *Cipher) Encrypt(plain string) (string, error) {
	if !c.Enabled() {
		return plain, nil
	}
	block, err := aes.NewCipher(c.key[:])
	if err != nil {
		return "", err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ct := g.Seal(nil, nonce, []byte(plain), nil)
	return Prefix + base64.RawURLEncoding.EncodeToString(append(nonce, ct...)), nil
}

// Decrypt расшифровывает значение. Если строка не имеет префикса — она
// считается legacy plaintext и возвращается как есть. Если есть префикс,
// но шифрование не включено — ошибка (значит ключ потеряли).
func (c *Cipher) Decrypt(stored string) (string, error) {
	if !strings.HasPrefix(stored, Prefix) {
		return stored, nil
	}
	if !c.Enabled() {
		return "", errors.New("encrypted secret but no master key configured")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(stored, Prefix))
	if err != nil {
		return "", fmt.Errorf("decode: %w", err)
	}
	block, err := aes.NewCipher(c.key[:])
	if err != nil {
		return "", err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < g.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	nonce, ct := raw[:g.NonceSize()], raw[g.NonceSize():]
	pt, err := g.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt: %w", err)
	}
	return string(pt), nil
}

// IsEncrypted сообщает, выглядит ли значение как зашифрованное (по префиксу).
func IsEncrypted(stored string) bool {
	return strings.HasPrefix(stored, Prefix)
}
