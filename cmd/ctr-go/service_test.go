package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func forceDarwinService(t *testing.T) {
	t.Helper()
	old := serviceGOOS
	serviceGOOS = "darwin"
	t.Cleanup(func() { serviceGOOS = old })
}

func TestServiceInstallNonInteractiveWritesConfigAndLaunchAgent(t *testing.T) {
	forceDarwinService(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	home := filepath.Join(dir, "home")
	t.Setenv("CTR_GO_HOME", home)
	t.Setenv("HOME", dir)
	binary := os.Args[0]
	token := "123456:abcdefghijklmnopqrstuvwxyz"
	var out bytes.Buffer

	err := runWithIO([]string{
		"service", "install",
		"--non-interactive",
		"--config", configPath,
		"--telegram-bot-token", token,
		"--allowed-user-ids", "42",
		"--default-cwd", dir,
		"--codex-chats-root", filepath.Join(dir, "Codex"),
		"--codex-bin", binary,
		"--ctr-go-bin", binary,
		"--notify-new-run", "false",
	}, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("service install failed: %v", err)
	}
	if strings.Contains(out.String(), token) {
		t.Fatalf("service install leaked token:\n%s", out.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile config failed: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`CTR_GO_TELEGRAM_BOT_TOKEN="` + token + `"`,
		`CTR_GO_ALLOWED_USER_IDS="42"`,
		`CTR_GO_NOTIFY_NEW_RUN="false"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "CTR_GO_CTR_GO_BIN") {
		t.Fatalf("internal service binary key leaked into config:\n%s", text)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(configPath)
		if err != nil {
			t.Fatalf("Stat config failed: %v", err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("config mode = %o, want 0600", got)
		}
	}
	plistPath := filepath.Join(home, "service", serviceLabel+".plist")
	plist, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("ReadFile plist failed: %v", err)
	}
	plistText := string(plist)
	if strings.Contains(plistText, token) {
		t.Fatalf("plist leaked token:\n%s", plistText)
	}
	for _, want := range []string{
		"<key>CTR_GO_CONFIG</key>",
		configPath,
		"<string>" + binary + "</string>",
		"<string>daemon</string>",
		"<string>run</string>",
	} {
		if !strings.Contains(plistText, want) {
			t.Fatalf("plist missing %q:\n%s", want, plistText)
		}
	}
}

func TestServiceInstallForcePreservesExistingUnknownConfigKeys(t *testing.T) {
	forceDarwinService(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	home := filepath.Join(dir, "home")
	t.Setenv("CTR_GO_HOME", home)
	t.Setenv("HOME", dir)
	binary := os.Args[0]
	token := "123456:abcdefghijklmnopqrstuvwxyz"
	if err := os.WriteFile(configPath, []byte("CTR_GO_HOME=/custom/home\nCTR_GO_EXTRA_FLAG=yes\n"), 0o600); err != nil {
		t.Fatalf("WriteFile existing config failed: %v", err)
	}

	err := runWithIO([]string{
		"service", "install",
		"--force",
		"--non-interactive",
		"--config", configPath,
		"--telegram-bot-token", token,
		"--allowed-user-ids", "42",
		"--default-cwd", dir,
		"--codex-chats-root", filepath.Join(dir, "Codex"),
		"--codex-bin", binary,
		"--ctr-go-bin", binary,
		"--notify-new-run", "true",
	}, strings.NewReader(""), io.Discard)
	if err != nil {
		t.Fatalf("service install failed: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile config failed: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`CTR_GO_HOME="/custom/home"`,
		`CTR_GO_EXTRA_FLAG="yes"`,
		`CTR_GO_ALLOWED_USER_IDS="42"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing preserved value %q:\n%s", want, text)
		}
	}
}

func TestServiceInstallCapturesRuntimeProxyEnvInConfig(t *testing.T) {
	forceDarwinService(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	home := filepath.Join(dir, "home")
	t.Setenv("CTR_GO_HOME", home)
	t.Setenv("HOME", dir)
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:18080")
	t.Setenv("NODE_USE_ENV_PROXY", "1")
	binary := os.Args[0]

	err := runWithIO([]string{
		"service", "install",
		"--non-interactive",
		"--config", configPath,
		"--telegram-bot-token", "123456:abcdefghijklmnopqrstuvwxyz",
		"--allowed-user-ids", "42",
		"--default-cwd", dir,
		"--codex-chats-root", filepath.Join(dir, "Codex"),
		"--codex-bin", binary,
		"--ctr-go-bin", binary,
		"--notify-new-run", "true",
	}, strings.NewReader(""), io.Discard)
	if err != nil {
		t.Fatalf("service install failed: %v", err)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile config failed: %v", err)
	}
	text := string(data)
	for _, want := range []string{
		`HTTPS_PROXY="http://127.0.0.1:18080"`,
		`NODE_USE_ENV_PROXY="1"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("config missing runtime env %q:\n%s", want, text)
		}
	}
}

func TestServiceInstallInteractiveWizardRetriesInvalidValues(t *testing.T) {
	forceDarwinService(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	home := filepath.Join(dir, "home")
	t.Setenv("CTR_GO_HOME", home)
	t.Setenv("HOME", dir)
	binary := os.Args[0]
	token := "123456:abcdefghijklmnopqrstuvwxyz"
	input := strings.Join([]string{
		"bad-token",
		token,
		"abc",
		"42",
		"chat",
		"",
		dir,
		filepath.Join(dir, "Codex"),
		binary,
		"maybe",
		"true",
		"",
	}, "\n")
	var out bytes.Buffer

	err := runWithIO([]string{
		"service", "install",
		"--config", configPath,
		"--ctr-go-bin", binary,
	}, strings.NewReader(input), &out)
	if err != nil {
		t.Fatalf("service install wizard failed: %v", err)
	}
	got := out.String()
	if strings.Contains(got, token) {
		t.Fatalf("wizard leaked token:\n%s", got)
	}
	for _, want := range []string{
		"codex-tg service setup",
		"[1/7] Telegram bot token",
		"Telegram bot token should look like",
		"ids must be integers",
		"value must be true or false",
		"Next steps",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("wizard output missing %q:\n%s", want, got)
		}
	}
}

func TestServiceInstallNonInteractiveReportsMissingFlags(t *testing.T) {
	forceDarwinService(t)
	dir := t.TempDir()
	t.Setenv("CTR_GO_HOME", filepath.Join(dir, "home"))
	t.Setenv("HOME", dir)

	err := runWithIO([]string{"service", "install", "--non-interactive", "--config", filepath.Join(dir, "config.env")}, strings.NewReader(""), io.Discard)
	if err == nil {
		t.Fatal("service install succeeded, want missing values error")
	}
	for _, want := range []string{"--telegram-bot-token", "--allowed-user-ids"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
}

func TestRenderLaunchAgentPlistContainsOnlyConfigEnvironment(t *testing.T) {
	plist, err := renderLaunchAgentPlist(launchAgentConfig{
		Label:      serviceLabel,
		BinaryPath: "/usr/local/bin/ctr-go",
		ConfigPath: "/Users/you/.codex-tg/config.env",
		WorkingDir: "/Users/you/project",
		StdoutPath: "/Users/you/.codex-tg/logs/daemon.out.log",
		StderrPath: "/Users/you/.codex-tg/logs/daemon.err.log",
		KeepAlive:  true,
		RunAtLoad:  true,
	})
	if err != nil {
		t.Fatalf("renderLaunchAgentPlist failed: %v", err)
	}
	text := string(plist)
	for _, want := range []string{"CTR_GO_CONFIG", "daemon", "run", "KeepAlive", "RunAtLoad"} {
		if !strings.Contains(text, want) {
			t.Fatalf("plist missing %q:\n%s", want, text)
		}
	}
	for _, bad := range []string{"TELEGRAM_BOT_TOKEN", "CTR_GO_TELEGRAM_BOT_TOKEN", "CTR_GO_ALLOWED_USER_IDS"} {
		if strings.Contains(text, bad) {
			t.Fatalf("plist contains forbidden env %q:\n%s", bad, text)
		}
	}
}

func TestServiceLifecycleUsesLaunchctlRunner(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CTR_GO_HOME", filepath.Join(dir, "home"))
	t.Setenv("HOME", dir)
	paths, err := defaultServicePaths(filepath.Join(dir, "config.env"))
	if err != nil {
		t.Fatalf("defaultServicePaths failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.ServicePlistPath), 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	if err := os.WriteFile(paths.ServicePlistPath, []byte("plist"), 0o644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	fake := &fakeServiceRunner{}
	oldRunner, oldGOOS, oldUID := serviceRunner, serviceGOOS, serviceUID
	serviceRunner = fake
	serviceGOOS = "darwin"
	serviceUID = func() int { return 501 }
	defer func() {
		serviceRunner = oldRunner
		serviceGOOS = oldGOOS
		serviceUID = oldUID
	}()

	var out bytes.Buffer
	if err := runServiceStart(&out); err != nil {
		t.Fatalf("service start failed: %v", err)
	}
	if err := runServiceRestart(&out); err != nil {
		t.Fatalf("service restart failed: %v", err)
	}
	if err := runServiceStatus(&out); err != nil {
		t.Fatalf("service status failed: %v", err)
	}
	joined := strings.Join(fake.calls, "\n")
	for _, want := range []string{
		"launchctl bootstrap gui/501 " + paths.ServicePlistPath,
		"launchctl kickstart -k gui/501/" + serviceLabel,
		"launchctl bootout gui/501/" + serviceLabel,
		"launchctl print gui/501/" + serviceLabel,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing launchctl call %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(out.String(), "Restarting can interrupt") {
		t.Fatalf("restart warning missing:\n%s", out.String())
	}
}

func TestServiceStartAcceptsKickstartFailureWhenServiceLoaded(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CTR_GO_HOME", filepath.Join(dir, "home"))
	t.Setenv("HOME", dir)
	paths, err := defaultServicePaths(filepath.Join(dir, "config.env"))
	if err != nil {
		t.Fatalf("defaultServicePaths failed: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.ServicePlistPath), 0o755); err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	if err := os.WriteFile(paths.ServicePlistPath, []byte("plist"), 0o644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	fake := &fakeServiceRunner{fail: map[string]error{"kickstart": errors.New("transient kickstart failure")}}
	oldRunner, oldGOOS, oldUID := serviceRunner, serviceGOOS, serviceUID
	serviceRunner = fake
	serviceGOOS = "darwin"
	serviceUID = func() int { return 501 }
	defer func() {
		serviceRunner = oldRunner
		serviceGOOS = oldGOOS
		serviceUID = oldUID
	}()

	var out bytes.Buffer
	if err := runServiceStart(&out); err != nil {
		t.Fatalf("service start failed: %v", err)
	}
	joined := strings.Join(fake.calls, "\n")
	if !strings.Contains(joined, "launchctl print gui/501/"+serviceLabel) {
		t.Fatalf("start did not re-check loaded service after kickstart failure:\n%s", joined)
	}
}

type fakeServiceRunner struct {
	calls  []string
	fail   map[string]error
	loaded bool
}

func (f *fakeServiceRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
	if len(args) > 0 && f.fail != nil {
		if err := f.fail[args[0]]; err != nil {
			return nil, err
		}
	}
	if len(args) > 0 {
		switch args[0] {
		case "bootstrap":
			f.loaded = true
		case "bootout":
			f.loaded = false
		case "print":
			if f.loaded {
				return []byte("ok"), nil
			}
			return nil, errors.New("not loaded")
		}
	}
	if name == "fail" {
		return nil, errors.New("failed")
	}
	return []byte("ok"), nil
}
