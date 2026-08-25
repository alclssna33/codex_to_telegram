//go:build !windows

package securestore

import (
	"context"
)

type unsupportedProtector struct{}

func NewDPAPIProtector() Protector {
	return unsupportedProtector{}
}

func (unsupportedProtector) Protect(context.Context, []byte) (string, error) {
	return "", ErrUnsupportedPlatform
}

func (unsupportedProtector) Unprotect(context.Context, string) ([]byte, error) {
	return nil, ErrUnsupportedPlatform
}
