//go:build windows

package main

import "testing"

func overrideInitSecretStoreForTest(t *testing.T) func(string) string {
	t.Helper()
	store := &fakeSecretStore{values: map[string][]byte{}}
	oldFactory := secretStoreFactory
	secretStoreFactory = func() secretStore { return store }
	t.Cleanup(func() { secretStoreFactory = oldFactory })
	return func(target string) string { return string(store.values[target]) }
}
