//go:build !windows

package main

import (
	"errors"
	"io"
)

func runSecrets([]string, io.Reader, io.Writer) error {
	return errors.New("Windows Credential Manager is unavailable on this platform")
}
