//go:build !windows

package main

import (
	"errors"
	"io"
)

func runWindowsService([]string, io.Reader, io.Writer) error {
	return errors.New("Windows scheduled tasks are unavailable on this platform")
}
