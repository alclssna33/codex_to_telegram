//go:build !windows

package main

import "testing"

func overrideInitSecretStoreForTest(*testing.T) func(string) string { return nil }
