//go:build windows

package securestore

import (
	"context"
	"encoding/base64"
	"fmt"
	"runtime"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var dpapiEntropy = []byte("codex-tg/minimal/v1")

type dpapiProtector struct{}

func NewDPAPIProtector() Protector {
	return dpapiProtector{}
}

func (dpapiProtector) Protect(ctx context.Context, plaintext []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	input := dataBlob(plaintext)
	entropy := dataBlob(dpapiEntropy)
	var output windows.DataBlob
	if err := windows.CryptProtectData(&input, nil, &entropy, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return "", fmt.Errorf("protect payload with DPAPI: %w", err)
	}
	defer freeDataBlob(&output)
	runtime.KeepAlive(plaintext)
	runtime.KeepAlive(dpapiEntropy)
	protected := append([]byte(nil), unsafe.Slice(output.Data, output.Size)...)
	return envelopePrefix + base64.StdEncoding.EncodeToString(protected), nil
}

func (dpapiProtector) Unprotect(ctx context.Context, envelope string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(envelope, envelopePrefix) {
		return nil, ErrInvalidEnvelope
	}
	protected, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(envelope, envelopePrefix))
	if err != nil || len(protected) == 0 {
		return nil, ErrInvalidEnvelope
	}
	input := dataBlob(protected)
	entropy := dataBlob(dpapiEntropy)
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(&input, nil, &entropy, 0, nil, windows.CRYPTPROTECT_UI_FORBIDDEN, &output); err != nil {
		return nil, fmt.Errorf("unprotect payload with DPAPI: %w", err)
	}
	defer freeDataBlob(&output)
	runtime.KeepAlive(protected)
	runtime.KeepAlive(dpapiEntropy)
	return append([]byte(nil), unsafe.Slice(output.Data, output.Size)...), nil
}

func dataBlob(data []byte) windows.DataBlob {
	if len(data) == 0 {
		return windows.DataBlob{}
	}
	return windows.DataBlob{Size: uint32(len(data)), Data: &data[0]}
}

func freeDataBlob(blob *windows.DataBlob) {
	if blob.Data == nil {
		return
	}
	_, _ = windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(blob.Data))))
}
