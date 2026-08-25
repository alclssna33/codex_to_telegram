package securestore

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
)

const envelopePrefix = "dpapi:v1:"

var (
	ErrInvalidEnvelope     = errors.New("invalid protected payload envelope")
	ErrUnsupportedPlatform = errors.New("Windows DPAPI is unavailable on this platform")
)

type Protector interface {
	Protect(context.Context, []byte) (string, error)
	Unprotect(context.Context, string) ([]byte, error)
}

// NewDeterministicTestProtector provides a non-secret deterministic envelope
// for storage boundary tests. It must not be used to protect production data.
func NewDeterministicTestProtector() Protector {
	return deterministicTestProtector{}
}

type deterministicTestProtector struct{}

func (deterministicTestProtector) Protect(ctx context.Context, plaintext []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return envelopePrefix + base64.StdEncoding.EncodeToString(plaintext), nil
}

func (deterministicTestProtector) Unprotect(ctx context.Context, envelope string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(envelope, envelopePrefix) {
		return nil, ErrInvalidEnvelope
	}
	plaintext, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(envelope, envelopePrefix))
	if err != nil {
		return nil, ErrInvalidEnvelope
	}
	return plaintext, nil
}
