//go:build windows

package securestore

import (
	"context"
	"strings"
	"testing"
)

func TestDPAPIProtectUnprotectRoundTrip(t *testing.T) {
	protector := NewDPAPIProtector()
	plaintext := []byte("private dpapi round trip marker")
	envelope, err := protector.Protect(context.Background(), plaintext)
	if err != nil {
		t.Fatal("DPAPI Protect failed")
	}
	if !strings.HasPrefix(envelope, "dpapi:v1:") {
		t.Fatal("DPAPI Protect returned an unexpected envelope")
	}
	if strings.Contains(envelope, string(plaintext)) {
		t.Fatal("DPAPI envelope contains plaintext")
	}
	unprotected, err := protector.Unprotect(context.Background(), envelope)
	if err != nil {
		t.Fatal("DPAPI Unprotect failed")
	}
	if string(unprotected) != string(plaintext) {
		t.Fatal("DPAPI round trip returned different plaintext")
	}
}

func TestDPAPIRejectsMalformedAndUnreadableEnvelopes(t *testing.T) {
	protector := NewDPAPIProtector()
	for _, envelope := range []string{
		"not-an-envelope",
		"dpapi:v1:not-base64!",
		"dpapi:v1:AQIDBAUGBwg=",
	} {
		if _, err := protector.Unprotect(context.Background(), envelope); err == nil {
			t.Fatal("DPAPI Unprotect accepted an unreadable envelope")
		}
	}
}
