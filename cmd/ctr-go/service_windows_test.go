//go:build windows

package main

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/alclssna33/codex_to_telegram/internal/config"
	"github.com/alclssna33/codex_to_telegram/internal/model"
)

func TestWindowsTaskUsesInteractiveCallerAndNoElevatedPrivileges(t *testing.T) {
	xml, err := renderWindowsTask(windowsTaskOptions{
		Executable: `C:\apps\ctr-go.exe`,
		ConfigPath: `C:\Users\me\AppData\Local\CodexTelegramBridge\config.env`,
	})
	if err != nil {
		t.Fatalf("renderWindowsTask failed: %v", err)
	}
	forbidden := []string{"HighestAvailable", "test-only-credential-value", `C:\Users\me\AppData\Local\CodexTelegramBridge\config.env`, "<UserId>"}
	for _, value := range forbidden {
		if strings.Contains(xml, value) {
			t.Fatalf("task XML leaked forbidden value %q:\n%s", value, xml)
		}
	}
	for _, want := range []string{
		"<LogonType>InteractiveToken</LogonType>",
		"<Count>3</Count>",
		"<Interval>PT1M</Interval>",
		"<Command>C:\\apps\\ctr-go.exe</Command>",
		"<Arguments>daemon</Arguments>",
	} {
		if !strings.Contains(xml, want) {
			t.Fatalf("task XML missing %q:\n%s", want, xml)
		}
	}
}

func TestWindowsTaskXMLUsesUTF16LEBOM(t *testing.T) {
	xml, err := renderWindowsTask(windowsTaskOptions{Executable: `C:\apps\ctr-go.exe`})
	if err != nil {
		t.Fatalf("renderWindowsTask failed: %v", err)
	}
	if !strings.HasPrefix(xml, `<?xml version="1.0" encoding="UTF-16"?>`) {
		t.Fatalf("task XML declaration must match UTF-16 bytes: %q", strings.Split(xml, "\n")[0])
	}
	data, err := encodeWindowsTaskXML(xml)
	if err != nil {
		t.Fatalf("encodeWindowsTaskXML failed: %v", err)
	}
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xfe {
		t.Fatalf("task XML must begin with a UTF-16LE BOM, got %x", data[:min(4, len(data))])
	}
	if len(data)%2 != 0 {
		t.Fatalf("UTF-16LE task XML has odd byte length: %d", len(data))
	}
	units := make([]uint16, (len(data)-2)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(data[2+i*2:])
	}
	if got := string(utf16.Decode(units)); got != xml {
		t.Fatalf("UTF-16LE task XML decoded unexpectedly:\n got: %q\nwant: %q", got, xml)
	}
}

func TestValidateWindowsPhase0ReportRejectsNonFinalPassText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "phase0.md")
	if err := os.WriteFile(path, []byte("# report\nPASS\n## Final Phase 0 decision\nPASS is mentioned but not approved.\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := validateWindowsPhase0Report(path); err == nil {
		t.Fatal("validateWindowsPhase0Report accepted a non-approved PASS substring")
	}
}

func TestValidateWindowsInstallNotifierSkipsInteractivePrerequisites(t *testing.T) {
	restore := replaceWindowsInstallHooks(t)
	defer restore()

	executable := writeTestExecutable(t)
	cfg := config.Config{
		Profile:          "notifier",
		CodexBin:         "codex-test-bin",
		FFmpegBin:        "ffmpeg-must-not-run",
		TelegramBotToken: "test-only-telegram-token",
		AllowedUserIDs:   []int64{42},
	}
	windowsInstallLoadConfig = func() (config.Config, error) {
		return cfg, nil
	}

	var versionChecks []string
	windowsRunVersionCheck = func(binary, argument string) error {
		call := binary + " " + argument
		versionChecks = append(versionChecks, call)
		if binary == cfg.FFmpegBin {
			t.Fatalf("notifier validation invoked obsolete ffmpeg prerequisite: %s", call)
		}
		if binary != cfg.CodexBin || argument != "--version" {
			return fmt.Errorf("unexpected version check %s", call)
		}
		return nil
	}
	dpapiChecked := false
	windowsDPAPICheck = func(context.Context) error {
		dpapiChecked = true
		return nil
	}
	telegramChecked := false
	windowsTelegramGetMe = func(_ context.Context, token string) error {
		telegramChecked = true
		if token != cfg.TelegramBotToken {
			t.Fatalf("Telegram getMe token = %q, want configured credential", token)
		}
		return nil
	}

	missingPhase0 := filepath.Join(t.TempDir(), "phase0-must-not-be-read.md")
	if err := validateWindowsInstall(executable, missingPhase0); err != nil {
		t.Fatalf("validateWindowsInstall(notifier) failed: %v", err)
	}
	if got, want := strings.Join(versionChecks, "\n"), cfg.CodexBin+" --version"; got != want {
		t.Fatalf("version checks = %q, want only %q", got, want)
	}
	if !dpapiChecked {
		t.Fatal("notifier validation did not require Windows DPAPI")
	}
	if !telegramChecked {
		t.Fatal("notifier validation did not call Telegram getMe")
	}
}

func TestValidateWindowsInstallNotifierRequiresOneOwnerAndTelegramCredential(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     config.Config
		wantErr string
	}{
		{
			name: "missing owner",
			cfg: config.Config{
				Profile:          "notifier",
				CodexBin:         "codex-test-bin",
				TelegramBotToken: "test-only-telegram-token",
			},
			wantErr: "exactly one Telegram owner",
		},
		{
			name: "multiple owners",
			cfg: config.Config{
				Profile:          "notifier",
				CodexBin:         "codex-test-bin",
				TelegramBotToken: "test-only-telegram-token",
				AllowedUserIDs:   []int64{1, 2},
			},
			wantErr: "exactly one Telegram owner",
		},
		{
			name: "missing telegram credential",
			cfg: config.Config{
				Profile:        "notifier",
				CodexBin:       "codex-test-bin",
				AllowedUserIDs: []int64{42},
			},
			wantErr: "Telegram credential",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := replaceWindowsInstallHooks(t)
			defer restore()

			windowsInstallLoadConfig = func() (config.Config, error) {
				return tc.cfg, nil
			}
			windowsRunVersionCheck = func(binary, argument string) error {
				t.Fatalf("external prerequisite %s %s ran before notifier contract rejection", binary, argument)
				return nil
			}
			windowsDPAPICheck = func(context.Context) error {
				t.Fatal("DPAPI check ran before notifier contract rejection")
				return nil
			}
			windowsTelegramGetMe = func(context.Context, string) error {
				t.Fatal("Telegram getMe ran before notifier contract rejection")
				return nil
			}

			err := validateWindowsInstall(writeTestExecutable(t), filepath.Join(t.TempDir(), "missing-phase0.md"))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateWindowsInstall error = %v, want containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateWindowsInstallNotifierSkipsProjectAndOpenAIConfigReads(t *testing.T) {
	oldLoadConfig := windowsInstallLoadConfig
	oldVersionCheck := windowsRunVersionCheck
	oldGetMe := windowsTelegramGetMe
	oldDPAPI := windowsDPAPICheck
	defer func() {
		windowsInstallLoadConfig = oldLoadConfig
		windowsRunVersionCheck = oldVersionCheck
		windowsTelegramGetMe = oldGetMe
		windowsDPAPICheck = oldDPAPI
	}()

	root := t.TempDir()
	configPath := filepath.Join(root, "config.env")
	missingProjects := filepath.Join(root, "missing-projects.json")
	if err := os.WriteFile(configPath, []byte(strings.Join([]string{
		"CTR_GO_PROFILE=notifier",
		"CTR_GO_ALLOWED_USER_IDS=42",
		"CTR_GO_CODEX_BIN=codex-test-bin",
		`CTR_GO_PROJECTS_FILE="` + missingProjects + `"`,
		`CTR_GO_OPENAI_CREDENTIAL_TARGET="bad\x00target"`,
	}, "\n")), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("CTR_GO_CONFIG", configPath)
	t.Setenv("CTR_GO_ALLOW_ENV_SECRETS", "1")
	t.Setenv("CTR_GO_TELEGRAM_BOT_TOKEN", "test-only-telegram-token")
	t.Setenv("CTR_GO_OPENAI_API_KEY", "")

	windowsRunVersionCheck = func(binary, argument string) error {
		if binary != "codex-test-bin" || argument != "--version" {
			return fmt.Errorf("unexpected version check %s %s", binary, argument)
		}
		return nil
	}
	windowsDPAPICheck = func(context.Context) error {
		return nil
	}
	windowsTelegramGetMe = func(_ context.Context, token string) error {
		if token != "test-only-telegram-token" {
			t.Fatalf("Telegram getMe token = %q, want configured credential", token)
		}
		return nil
	}

	if err := validateWindowsInstall(writeTestExecutable(t), filepath.Join(root, "missing-phase0.md")); err != nil {
		t.Fatalf("validateWindowsInstall(notifier) read project/OpenAI-only config: %v", err)
	}
}

func TestValidateWindowsInstallMinimalPreservesInteractivePrerequisites(t *testing.T) {
	restore := replaceWindowsInstallHooks(t)
	defer restore()

	phase0Path := filepath.Join(t.TempDir(), "phase0.md")
	if err := os.WriteFile(phase0Path, []byte(phase0DecisionReport("# evidence\n", "PASS")), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg := config.Config{
		Profile:          "minimal",
		Projects:         []model.Project{{ID: "project-1", DisplayName: "Project", CanonicalPath: t.TempDir()}},
		CodexBin:         "codex-test-bin",
		FFmpegBin:        "ffmpeg-test-bin",
		TelegramBotToken: "test-only-telegram-token",
		OpenAIAPIKey:     "test-only-openai-key",
		AllowedUserIDs:   []int64{42},
	}
	windowsInstallLoadConfig = func() (config.Config, error) {
		return cfg, nil
	}
	var versionChecks []string
	windowsRunVersionCheck = func(binary, argument string) error {
		versionChecks = append(versionChecks, binary+" "+argument)
		return nil
	}
	telegramChecked := false
	windowsTelegramGetMe = func(_ context.Context, token string) error {
		telegramChecked = true
		if token != cfg.TelegramBotToken {
			t.Fatalf("Telegram getMe token = %q, want configured credential", token)
		}
		return nil
	}

	if err := validateWindowsInstall(writeTestExecutable(t), phase0Path); err != nil {
		t.Fatalf("validateWindowsInstall(minimal) failed: %v", err)
	}
	got := strings.Join(versionChecks, "\n")
	for _, want := range []string{cfg.CodexBin + " --version", cfg.FFmpegBin + " -version"} {
		if !strings.Contains(got, want) {
			t.Fatalf("version checks missing %q:\n%s", want, got)
		}
	}
	if !telegramChecked {
		t.Fatal("minimal validation did not call Telegram getMe")
	}
}

func TestWindowsPhase0ReportDocumentUsesFinalDecisionContract(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "validation", "windows-phase0.md")
	if err := validateWindowsPhase0Report(path); err != nil {
		t.Fatalf("documented Windows Phase 0 report does not satisfy the final-decision contract: %v", err)
	}
}

func TestValidateWindowsPhase0ReportRequiresTerminalDecisionMarker(t *testing.T) {
	t.Run("accepts checksum-bound terminal pass", func(t *testing.T) {
		for name, body := range map[string]string{
			"ascii": "# evidence\n",
			"utf8":  "# evidence 증거\n",
			"bom":   "\ufeff# evidence\n",
		} {
			t.Run(name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "phase0.md")
				if err := os.WriteFile(path, []byte(phase0DecisionReport(body, "PASS")), 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				if err := validateWindowsPhase0Report(path); err != nil {
					t.Fatalf("validateWindowsPhase0Report rejected terminal decision marker: %v", err)
				}
			})
		}
	})
	t.Run("rejects malformed and noncanonical decisions", func(t *testing.T) {
		wrongField := strings.Replace(phase0DecisionReport("# evidence\n", "PASS"), "content-sha256=", "", 1)
		uppercaseChecksum := uppercasePhase0Checksum(phase0DecisionReport("# evidence\n", "PASS"))
		for name, content := range map[string]string{
			"missing":            "# evidence\n",
			"malformed":          "# evidence\n<!-- ctr-go-phase0-final-decision:v1\ndecision=PASS\ncontent-sha256=broken\n-->",
			"wrong-field":        wrongField,
			"uppercase-checksum": uppercaseChecksum,
			"invalid-utf8":       phase0DecisionReport("# evidence \xff\n", "PASS"),
			"negative":           phase0DecisionReport("# evidence\n", "FAIL"),
			"trailing":           phase0DecisionReport("# evidence\n", "PASS") + "\npost-decision note",
		} {
			t.Run(name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "phase0.md")
				if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
					t.Fatalf("WriteFile: %v", err)
				}
				if err := validateWindowsPhase0Report(path); err == nil {
					t.Fatal("validateWindowsPhase0Report accepted invalid final decision")
				}
			})
		}
	})
	t.Run("rejects a copied legacy pass marker after failure prose", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "phase0.md")
		legacyMarker := "## Final Phase 0 decision\n\nDecision: PASS"
		content := "# evidence\n\n## Later failure\nQuoted historic authorization follows:\n" + legacyMarker
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := validateWindowsPhase0Report(path); err == nil {
			t.Fatal("validateWindowsPhase0Report accepted a copied authorization footer after later failure prose")
		}
	})
	t.Run("rejects a copied checksum footer after failure prose", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "phase0.md")
		footer := phase0DecisionReport("# evidence\n", "PASS")
		content := footer + "\n## Later failure\nQuoted authorization follows:\n" + footer
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := validateWindowsPhase0Report(path); err == nil {
			t.Fatal("validateWindowsPhase0Report accepted a copied checksum footer after later failure prose")
		}
	})
}

func phase0DecisionReport(body, decision string) string {
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	digest := sha256.Sum256([]byte(body))
	return body + "<!-- ctr-go-phase0-final-decision:v1\n" +
		"decision=" + decision + "\n" +
		"content-sha256=" + hex.EncodeToString(digest[:]) + "\n-->"
}

func uppercasePhase0Checksum(report string) string {
	const prefix = "content-sha256="
	start := strings.LastIndex(report, prefix)
	if start == -1 {
		return report
	}
	start += len(prefix)
	end := strings.IndexByte(report[start:], '\n')
	if end == -1 {
		return report
	}
	end += start
	return report[:start] + strings.ToUpper(report[start:end]) + report[end:]
}

func TestWindowsUninstallStatePathIgnoresConfiguredHome(t *testing.T) {
	configuredHome := filepath.Join(t.TempDir(), "Codex", "project")
	t.Setenv("CTR_GO_HOME", configuredHome)
	path, err := windowsBridgeApplicationStatePath()
	if err != nil {
		t.Fatalf("windowsBridgeApplicationStatePath failed: %v", err)
	}
	if filepath.Clean(path) == filepath.Clean(configuredHome) {
		t.Fatalf("uninstall state path trusted configurable CTR_GO_HOME: %q", path)
	}
}

func replaceWindowsInstallHooks(t *testing.T) func() {
	t.Helper()
	oldLoadConfig := windowsInstallLoadConfig
	oldVersionCheck := windowsRunVersionCheck
	oldGetMe := windowsTelegramGetMe
	oldDPAPI := windowsDPAPICheck
	windowsInstallLoadConfig = func() (config.Config, error) {
		return config.Config{}, errors.New("test did not provide config")
	}
	windowsRunVersionCheck = func(binary, argument string) error {
		return fmt.Errorf("unexpected version check %s %s", binary, argument)
	}
	windowsTelegramGetMe = func(context.Context, string) error {
		return errors.New("unexpected Telegram getMe")
	}
	windowsDPAPICheck = func(context.Context) error {
		return errors.New("unexpected DPAPI check")
	}
	return func() {
		windowsInstallLoadConfig = oldLoadConfig
		windowsRunVersionCheck = oldVersionCheck
		windowsTelegramGetMe = oldGetMe
		windowsDPAPICheck = oldDPAPI
	}
}

func writeTestExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ctr-go.exe")
	if err := os.WriteFile(path, []byte("test executable"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return path
}
