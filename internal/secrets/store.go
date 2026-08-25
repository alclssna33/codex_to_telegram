package secrets

import "errors"

var (
	ErrNotFound    = errors.New("credential not found")
	ErrUnsupported = errors.New("credential manager unsupported on this platform")
)

// Store keeps secret bytes outside application configuration and durable state.
type Store interface {
	Read(target string) ([]byte, error)
	Write(target string, value []byte) error
	Delete(target string) error
}
