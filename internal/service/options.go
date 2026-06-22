package service

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Options struct {
	AuthEnabled       bool
	EnableCustomSpend bool
	JWTSecret         []byte
	DataKey           []byte
	AccessTTL         time.Duration
	RefreshTTL        time.Duration
}

func defaultOptions() Options {
	return Options{
		AccessTTL:  15 * time.Minute,
		RefreshTTL: 24 * time.Hour,
	}
}

func (o Options) withDefaults(dataDir string) (Options, error) {
	if o.AccessTTL == 0 {
		o.AccessTTL = 15 * time.Minute
	}
	if o.RefreshTTL == 0 {
		o.RefreshTTL = 24 * time.Hour
	}
	var err error
	if len(o.DataKey) == 0 {
		o.DataKey, err = loadOrCreateSecret(filepath.Join(dataDir, "data.key"))
		if err != nil {
			return o, fmt.Errorf("data key: %w", err)
		}
	}
	if len(o.DataKey) != 32 {
		return o, fmt.Errorf("data key must be 32 bytes")
	}
	if o.AuthEnabled && len(o.JWTSecret) == 0 {
		o.JWTSecret, err = loadOrCreateSecret(filepath.Join(dataDir, "jwt.secret"))
		if err != nil {
			return o, fmt.Errorf("jwt secret: %w", err)
		}
	}
	if o.AuthEnabled && len(o.JWTSecret) < 32 {
		return o, fmt.Errorf("jwt secret must be at least 32 bytes")
	}
	return o, nil
}

func loadOrCreateSecret(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		key, err := base64.StdEncoding.DecodeString(string(raw))
		if err != nil {
			return nil, err
		}
		return key, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}
