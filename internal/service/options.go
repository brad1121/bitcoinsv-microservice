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

	// BroadcastFanout caps how many peers each broadcast pushes to.
	// 0 pushes to every connected peer. Set it to 1 to make delivery
	// observable through WaitForTxRelay: a peer never announces a tx back
	// to whoever sent it, so every peer broadcast to is disqualified as a
	// witness.
	BroadcastFanout int
	// DisablePendingTxTracking stops the node holding every locally
	// broadcast tx in memory until a block confirms it. Turn it on for
	// high-volume deployments that persist and rebroadcast themselves —
	// the ledger reaches millions of entries before the first block clears
	// any of it. PendingTransactions and RebroadcastPendingTransactions
	// report nothing once it is off.
	DisablePendingTxTracking bool
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
