//go:build windows

package secrets

import (
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	credTypeGeneric         = 1
	credPersistLocalMachine = 2
)

var (
	advapi32       = windows.NewLazySystemDLL("advapi32.dll")
	credReadProc   = advapi32.NewProc("CredReadW")
	credWriteProc  = advapi32.NewProc("CredWriteW")
	credDeleteProc = advapi32.NewProc("CredDeleteW")
	credFreeProc   = advapi32.NewProc("CredFree")
)

type credential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         unsafe.Pointer
	TargetAlias        *uint16
	UserName           *uint16
}

type credentialStore struct{}

func NewStore() Store { return credentialStore{} }

func (credentialStore) Read(target string) ([]byte, error) {
	targetPtr, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return nil, fmt.Errorf("credential target: %w", err)
	}
	var raw *credential
	r1, _, callErr := credReadProc.Call(uintptr(unsafe.Pointer(targetPtr)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&raw)))
	if r1 == 0 {
		if errors.Is(callErr, windows.ERROR_NOT_FOUND) {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, target)
		}
		return nil, fmt.Errorf("read credential %q: %w", target, callErr)
	}
	defer credFreeProc.Call(uintptr(unsafe.Pointer(raw)))
	if raw.CredentialBlobSize == 0 {
		return []byte{}, nil
	}
	if raw.CredentialBlob == nil {
		return nil, fmt.Errorf("read credential %q: missing credential blob", target)
	}
	value := append([]byte(nil), unsafe.Slice(raw.CredentialBlob, int(raw.CredentialBlobSize))...)
	runtime.KeepAlive(raw)
	return value, nil
}

func (credentialStore) Write(target string, value []byte) error {
	if len(value) == 0 {
		return errors.New("credential value is required")
	}
	targetPtr, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return fmt.Errorf("credential target: %w", err)
	}
	copyValue := append([]byte(nil), value...)
	credential := credential{
		Type:               credTypeGeneric,
		TargetName:         targetPtr,
		CredentialBlobSize: uint32(len(copyValue)),
		CredentialBlob:     &copyValue[0],
		Persist:            credPersistLocalMachine,
	}
	r1, _, callErr := credWriteProc.Call(uintptr(unsafe.Pointer(&credential)), 0)
	runtime.KeepAlive(copyValue)
	if r1 == 0 {
		return fmt.Errorf("write credential %q: %w", target, callErr)
	}
	return nil
}

func (credentialStore) Delete(target string) error {
	targetPtr, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return fmt.Errorf("credential target: %w", err)
	}
	r1, _, callErr := credDeleteProc.Call(uintptr(unsafe.Pointer(targetPtr)), credTypeGeneric, 0)
	if r1 == 0 {
		if errors.Is(callErr, windows.ERROR_NOT_FOUND) {
			return fmt.Errorf("%w: %s", ErrNotFound, target)
		}
		return fmt.Errorf("delete credential %q: %w", target, callErr)
	}
	return nil
}
