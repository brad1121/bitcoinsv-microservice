package service

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"strings"
)

const sealedPrefix = "v1:"

func sealString(key []byte, plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	aead, err := aeadForKey(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ciphertext := aead.Seal(nil, nonce, []byte(plaintext), nil)
	buf := append(nonce, ciphertext...)
	return sealedPrefix + base64.RawURLEncoding.EncodeToString(buf), nil
}

func openString(key []byte, sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	if !strings.HasPrefix(sealed, sealedPrefix) {
		return "", fmt.Errorf("unsupported sealed value")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, sealedPrefix))
	if err != nil {
		return "", err
	}
	aead, err := aeadForKey(key)
	if err != nil {
		return "", err
	}
	if len(raw) < aead.NonceSize() {
		return "", fmt.Errorf("sealed value too short")
	}
	nonce := raw[:aead.NonceSize()]
	ciphertext := raw[aead.NonceSize():]
	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func aeadForKey(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("data key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
