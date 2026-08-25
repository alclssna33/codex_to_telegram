package main

import (
	"bytes"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/alclssna33/codex_to_telegram/internal/config"
	"github.com/alclssna33/codex_to_telegram/internal/telegram"
)

func TestDaemonLogOutputCanBeDisabled(t *testing.T) {
	if got := daemonLogOutput(config.Config{LogEnabled: false}); got != io.Discard {
		t.Fatalf("daemonLogOutput(false) = %#v, want io.Discard", got)
	}
	if got := daemonLogOutput(config.Config{LogEnabled: true}); got == io.Discard {
		t.Fatal("daemonLogOutput(true) = io.Discard, want stdout logger")
	}
}

func TestDiagnosticLoggerHonorsFlags(t *testing.T) {
	logger := log.New(io.Discard, "", 0)

	if got := diagnosticLogger(config.Config{LogEnabled: true, DiagnosticLogs: true}, logger); got != logger {
		t.Fatal("diagnosticLogger(enabled, enabled) did not return logger")
	}
	if got := diagnosticLogger(config.Config{LogEnabled: false, DiagnosticLogs: true}, logger); got != nil {
		t.Fatal("diagnosticLogger(log disabled) returned logger, want nil")
	}
	if got := diagnosticLogger(config.Config{LogEnabled: true, DiagnosticLogs: false}, logger); got != nil {
		t.Fatal("diagnosticLogger(diagnostics disabled) returned logger, want nil")
	}
}

func TestRunInitWritesPrivateConfigAndRefusesOverwrite(t *testing.T) {
	storedSecret := overrideInitSecretStoreForTest(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	t.Setenv("CTR_GO_CONFIG", configPath)
	token := "123456:abcdefghijklmnopqrstuvwxyz"
	input := strings.Join([]string{
		token,
		"42",
		"",
		filepath.Join(dir, "project"),
		filepath.Join(dir, "chats"),
		"codex",
		"false",
		"",
	}, "\n")
	var out bytes.Buffer

	if err := runWithIO([]string{"init"}, strings.NewReader(input), &out); err != nil {
		t.Fatalf("run init failed: %v", err)
	}
	if strings.Contains(out.String(), token) {
		t.Fatal("init output leaked Telegram bot token")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	text := string(data)
	want := []string{
		`CTR_GO_ALLOWED_USER_IDS="42"`,
		`CTR_GO_CODEX_BIN="codex"`,
		`CTR_GO_NOTIFY_NEW_RUN="false"`,
	}
	if runtime.GOOS == "windows" {
		if strings.Contains(text, token) || strings.Contains(text, "CTR_GO_TELEGRAM_BOT_TOKEN") {
			t.Fatalf("Windows init persisted token in config:\n%s", text)
		}
		if storedSecret == nil || storedSecret("CodexTelegramBridge/TelegramBotToken") != token {
			t.Fatal("Windows init did not store token in the test credential store")
		}
	} else {
		want = append(want, `CTR_GO_TELEGRAM_BOT_TOKEN="`+token+`"`)
	}
	for _, want := range want {
		if !strings.Contains(text, want) {
			t.Fatalf("config file missing %q:\n%s", want, text)
		}
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(configPath)
		if err != nil {
			t.Fatalf("Stat failed: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("config mode = %o, want 0600", got)
		}
	}
	if err := runWithIO([]string{"init"}, strings.NewReader(input), io.Discard); err == nil {
		t.Fatal("second init succeeded, want overwrite refusal")
	}
}

func TestRunInitForceOverwritesConfig(t *testing.T) {
	_ = overrideInitSecretStoreForTest(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	t.Setenv("CTR_GO_CONFIG", configPath)
	if err := os.WriteFile(configPath, []byte("old=true\n"), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	input := strings.Join([]string{
		"token",
		"42",
		"",
		filepath.Join(dir, "project"),
		filepath.Join(dir, "chats"),
		"codex",
		"true",
		"",
	}, "\n")

	if err := runWithIO([]string{"init", "--force"}, strings.NewReader(input), io.Discard); err != nil {
		t.Fatalf("run init --force failed: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if strings.Contains(string(data), "old=true") {
		t.Fatalf("config was not overwritten:\n%s", data)
	}
}

func TestStatusAndDoctorDoNotLeakConfigFileToken(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	home := filepath.Join(dir, "home")
	token := "123456:abcdefghijklmnopqrstuvwxyz"
	if err := os.WriteFile(configPath, []byte(strings.Join([]string{
		`CTR_GO_HOME="` + home + `"`,
		`CTR_GO_TELEGRAM_BOT_TOKEN="` + token + `"`,
		`CTR_GO_ALLOWED_USER_IDS="42"`,
		`CTR_GO_DEFAULT_CWD="` + dir + `"`,
		"",
	}, "\n")), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	t.Setenv("CTR_GO_CONFIG", configPath)

	for _, command := range [][]string{{"status"}, {"doctor"}} {
		var out bytes.Buffer
		if err := runWithIO(command, strings.NewReader(""), &out); err != nil {
			t.Fatalf("%v failed: %v", command, err)
		}
		if strings.Contains(out.String(), token) {
			t.Fatalf("%v leaked token:\n%s", command, out.String())
		}
	}
}

func TestFatalErrorSanitizerRedactsTelegramBotURL(t *testing.T) {
	errText := `Post "https://api.telegram.org/bot123456:abcdefghijklmnopqrstuvwxyz/getMe": context deadline exceeded`
	got := telegram.SanitizeLogError(errors.New(errText))
	if strings.Contains(got, "123456:abcdefghijklmnopqrstuvwxyz") {
		t.Fatalf("fatal error sanitizer leaked token: %q", got)
	}
	if !strings.Contains(got, "bot<redacted>") {
		t.Fatalf("fatal error sanitizer = %q, want redacted Telegram bot URL", got)
	}
}
