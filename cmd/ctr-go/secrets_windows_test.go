//go:build windows

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeSecretStore struct {
	values  map[string][]byte
	deleted []string
}

func (s *fakeSecretStore) Read(target string) ([]byte, error) {
	value, ok := s.values[target]
	if !ok {
		return nil, errors.New("missing")
	}
	return append([]byte(nil), value...), nil
}
func (s *fakeSecretStore) Write(target string, value []byte) error {
	s.values[target] = append([]byte(nil), value...)
	return nil
}
func (s *fakeSecretStore) Delete(target string) error {
	s.deleted = append(s.deleted, target)
	delete(s.values, target)
	return nil
}

func TestSecretsSetDoesNotEchoSecretInput(t *testing.T) {
	store := &fakeSecretStore{values: map[string][]byte{}}
	oldFactory := secretStoreFactory
	secretStoreFactory = func() secretStore { return store }
	t.Cleanup(func() { secretStoreFactory = oldFactory })

	const input = "test-only-credential-value\n"
	var out bytes.Buffer
	if err := runSecrets([]string{"set", "telegram"}, strings.NewReader(input), &out); err != nil {
		t.Fatalf("runSecrets set failed: %v", err)
	}
	if strings.Contains(out.String(), "test-only-credential-value") {
		t.Fatalf("secret command echoed input: %s", out.String())
	}
	if got := string(store.values["CodexTelegramBridge/TelegramBotToken"]); got != "test-only-credential-value" {
		t.Fatalf("stored credential = %q, want input", got)
	}
}

func TestSecretsSetUsesConfiguredCredentialTarget(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.env")
	if err := os.WriteFile(configPath, []byte("CTR_GO_TELEGRAM_CREDENTIAL_TARGET=CodexTelegramBridge/test-custom-telegram\n"), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	t.Setenv("CTR_GO_CONFIG", configPath)
	store := &fakeSecretStore{values: map[string][]byte{}}
	oldFactory := secretStoreFactory
	secretStoreFactory = func() secretStore { return store }
	t.Cleanup(func() { secretStoreFactory = oldFactory })

	if err := runSecrets([]string{"set", "telegram"}, strings.NewReader("test-only-custom-target-value\n"), io.Discard); err != nil {
		t.Fatalf("runSecrets set failed: %v", err)
	}
	if _, ok := store.values["CodexTelegramBridge/test-custom-telegram"]; !ok {
		t.Fatalf("credential stored at default target, want configured target; values=%v", store.values)
	}
	if err := runSecrets([]string{"delete", "telegram"}, strings.NewReader(""), io.Discard); err != nil {
		t.Fatalf("runSecrets delete failed: %v", err)
	}
	if len(store.deleted) != 1 || store.deleted[0] != "CodexTelegramBridge/test-custom-telegram" {
		t.Fatalf("credential delete targets = %v, want configured target", store.deleted)
	}
}

func TestWindowsInitStoresTelegramTokenOutsideConfig(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	t.Setenv("CTR_GO_CONFIG", configPath)
	store := &fakeSecretStore{values: map[string][]byte{}}
	oldFactory := secretStoreFactory
	secretStoreFactory = func() secretStore { return store }
	t.Cleanup(func() { secretStoreFactory = oldFactory })
	input := strings.Join([]string{
		"test-only-init-token",
		"42",
		"",
		dir,
		filepath.Join(dir, "Chats"),
		os.Args[0],
		"true",
		"",
	}, "\n")
	var out bytes.Buffer
	if err := runInit(nil, strings.NewReader(input), &out); err != nil {
		t.Fatalf("runInit failed: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile config: %v", err)
	}
	if bytes.Contains(data, []byte("test-only-init-token")) || bytes.Contains(data, []byte("CTR_GO_TELEGRAM_BOT_TOKEN")) {
		t.Fatalf("Windows init persisted Telegram token in config: %s", data)
	}
	if got := string(store.values["CodexTelegramBridge/TelegramBotToken"]); got != "test-only-init-token" {
		t.Fatalf("Credential Manager value = %q, want init token", got)
	}
	if strings.Contains(out.String(), "test-only-init-token") {
		t.Fatalf("init echoed Telegram token: %s", out.String())
	}
}
