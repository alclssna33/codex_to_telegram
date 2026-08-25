//go:build windows

package secrets

import (
	"crypto/rand"
	"errors"
	"fmt"
	"testing"
)

func TestCredentialRoundTripDeletesTemporaryTarget(t *testing.T) {
	store := NewStore()
	target := fmt.Sprintf("CodexTelegramBridge/test-%x", randomTargetSuffix(t))
	t.Cleanup(func() { _ = store.Delete(target) })

	secret := []byte("test-only-credential-value")
	if err := store.Write(target, secret); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	got, err := store.Read(target)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if string(got) != string(secret) {
		t.Fatalf("Read = %q, want exact credential bytes", got)
	}
	if err := store.Delete(target); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
	if _, err := store.Read(target); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Read after Delete error = %v, want ErrNotFound", err)
	}
}

func randomTargetSuffix(t *testing.T) []byte {
	t.Helper()
	value := make([]byte, 12)
	if _, err := rand.Read(value); err != nil {
		t.Fatalf("random target suffix: %v", err)
	}
	return value
}
