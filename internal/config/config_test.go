package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLoadMinimalProfileRequiresOneOwnerAndValidProject(t *testing.T) {
	root := t.TempDir()
	projectsPath := filepath.Join(root, "projects.json")
	projectPath := filepath.Join(root, "project")
	if err := os.Mkdir(projectPath, 0o755); err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}
	data := `{"projects":[{"id":"p1","display_name":"Project 1","path":"` + jsonPath(projectPath) + `"}]}`
	if err := os.WriteFile(projectsPath, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	t.Setenv("CTR_GO_CONFIG", filepath.Join(root, "missing.env"))
	t.Setenv("CTR_GO_PROFILE", "minimal")
	t.Setenv("CTR_GO_PROJECTS_FILE", projectsPath)
	t.Setenv("CTR_GO_ALLOWED_USER_IDS", "7")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got, want := len(cfg.Projects), 1; got != want {
		t.Fatalf("projects = %d, want %d", got, want)
	}
	if got, want := cfg.Projects[0].CanonicalPath, projectPath; got != want {
		t.Fatalf("canonical project path = %q, want %q", got, want)
	}

	for name, users := range map[string]string{"no owner": "", "two owners": "7,8"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CTR_GO_ALLOWED_USER_IDS", users)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "exactly one") {
				t.Fatalf("Load error = %v, want exactly one owner error", err)
			}
		})
	}
	for name, users := range map[string]string{
		"malformed owner":    "7,not-an-id",
		"trailing separator": "7,",
		"leading separator":  ",7",
		"empty extra owners": "7;;",
		"zero owner":         "0",
		"negative owner":     "-7",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CTR_GO_ALLOWED_USER_IDS", users)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "positive integer") {
				t.Fatalf("Load error = %v, want positive integer owner error", err)
			}
		})
	}
}

func TestLoadNotifierRequiresOneOwnerButNoProjects(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CTR_GO_CONFIG", filepath.Join(root, "missing.env"))
	t.Setenv("CTR_GO_PROFILE", "notifier")
	t.Setenv("CTR_GO_ALLOWED_USER_IDS", "42")
	t.Setenv("CTR_GO_PROJECTS_FILE", filepath.Join(root, "missing-projects.json"))
	t.Setenv("CTR_GO_TELEGRAM_CREDENTIAL_TARGET", "CodexTelegramBridge/test-missing-telegram")
	t.Setenv("CTR_GO_OPENAI_CREDENTIAL_TARGET", "CodexTelegramBridge/test-missing-openai")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if cfg.Profile != "notifier" {
		t.Fatalf("Profile = %q, want notifier", cfg.Profile)
	}
	if got := cfg.AllowedUserIDs; len(got) != 1 || got[0] != 42 {
		t.Fatalf("AllowedUserIDs = %#v, want [42]", got)
	}
	if len(cfg.Projects) != 0 {
		t.Fatalf("Projects = %#v, want none for notifier", cfg.Projects)
	}
}

func TestFromSourceReplacesMissingWindowsDesktopCodexPath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows Codex Desktop fallback is Windows-only")
	}

	appData := t.TempDir()
	binRoot := filepath.Join(appData, "OpenAI", "Codex", "bin")
	older := filepath.Join(binRoot, "older", "codex.exe")
	newer := filepath.Join(binRoot, "newer", "codex.exe")
	for _, path := range []string{older, newer} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(older, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOCALAPPDATA", appData)
	missing := filepath.Join(binRoot, "removed", "codex.exe")

	cfg := fromSource(envSource{file: map[string]string{"CTR_GO_CODEX_BIN": missing}})
	if got := cfg.CodexBin; got != newer {
		t.Fatalf("CodexBin = %q, want newest installed Codex Desktop binary %q", got, newer)
	}
}

func TestLoadNotifierSkipsOpenAICredentialResolution(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows Credential Manager resolution is Windows-only")
	}
	root := t.TempDir()
	configPath := filepath.Join(root, "config.env")
	if err := os.WriteFile(configPath, []byte(strings.Join([]string{
		"CTR_GO_PROFILE=notifier",
		"CTR_GO_ALLOWED_USER_IDS=42",
		`CTR_GO_OPENAI_CREDENTIAL_TARGET="bad\x00target"`,
		`CTR_GO_PROJECTS_FILE="` + filepath.Join(root, "missing-projects.json") + `"`,
		"",
	}, "\n")), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	t.Setenv("CTR_GO_CONFIG", configPath)
	t.Setenv("CTR_GO_ALLOW_ENV_SECRETS", "1")
	t.Setenv("CTR_GO_TELEGRAM_BOT_TOKEN", "test-only-telegram-env-secret")
	t.Setenv("CTR_GO_OPENAI_API_KEY", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed after notifier-only config: %v", err)
	}
	if cfg.TelegramBotToken != "test-only-telegram-env-secret" {
		t.Fatalf("TelegramBotToken = %q, want env Telegram credential", cfg.TelegramBotToken)
	}
	if cfg.OpenAIAPIKey != "" {
		t.Fatal("notifier profile loaded OpenAI credential")
	}
}

func TestLoadNotifierProfileRequiresOneOwner(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CTR_GO_CONFIG", filepath.Join(root, "missing.env"))
	t.Setenv("CTR_GO_PROFILE", "notifier")
	t.Setenv("CTR_GO_PROJECTS_FILE", filepath.Join(root, "missing-projects.json"))
	t.Setenv("CTR_GO_TELEGRAM_CREDENTIAL_TARGET", "CodexTelegramBridge/test-missing-telegram")
	t.Setenv("CTR_GO_OPENAI_CREDENTIAL_TARGET", "CodexTelegramBridge/test-missing-openai")

	for name, users := range map[string]string{
		"no owner":           "",
		"two owners":         "7,8",
		"malformed owner":    "7,not-an-id",
		"trailing separator": "7,",
		"zero owner":         "0",
		"negative owner":     "-7",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CTR_GO_ALLOWED_USER_IDS", users)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), "notifier profile requires exactly one allowed user id as a positive integer") {
				t.Fatalf("Load error = %v, want notifier owner error", err)
			}
		})
	}
}

func TestLoadMinimalProfileRejectsEmptyProjectRegistry(t *testing.T) {
	root := t.TempDir()
	projectsPath := filepath.Join(root, "projects.json")
	if err := os.WriteFile(projectsPath, []byte(`{"projects":[]}`), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	t.Setenv("CTR_GO_CONFIG", filepath.Join(root, "missing.env"))
	t.Setenv("CTR_GO_PROFILE", "minimal")
	t.Setenv("CTR_GO_PROJECTS_FILE", projectsPath)
	t.Setenv("CTR_GO_ALLOWED_USER_IDS", "7")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("Load error = %v, want at least one project error", err)
	}
}

func TestLoadRejectsUnknownProfileInsteadOfFallingBackToLegacy(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CTR_GO_CONFIG", filepath.Join(root, "missing.env"))
	t.Setenv("CTR_GO_PROFILE", "minmal")

	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "unknown profile") {
		t.Fatalf("Load error = %v, want unknown profile error", err)
	}
}

func TestFromSourceReadsMinimalProfileFields(t *testing.T) {
	t.Parallel()

	values := map[string]string{
		"CTR_GO_PROFILE":                    "minimal",
		"CTR_GO_PROJECTS_FILE":              `C:\config\projects.json`,
		"CTR_GO_COMMAND_MAX_AGE_SECONDS":    "420",
		"CTR_GO_FFMPEG_BIN":                 `C:\tools\ffmpeg.exe`,
		"CTR_GO_TRANSCRIBE_MODEL":           "gpt-test-transcribe",
		"CTR_GO_OPENAI_API_KEY":             "PRIVATE_OPENAI_KEY_2f789b",
		"CTR_GO_TELEGRAM_BOT_TOKEN":         "PRIVATE_TELEGRAM_TOKEN_2f789b",
		"CTR_GO_TELEGRAM_CREDENTIAL_TARGET": "bridge/telegram",
		"CTR_GO_OPENAI_CREDENTIAL_TARGET":   "bridge/openai",
	}
	cfg := fromSource(envSource{file: values})
	if got, want := cfg.Profile, "minimal"; got != want {
		t.Fatalf("Profile = %q, want %q", got, want)
	}
	if got, want := cfg.ProjectsFile, filepath.Clean(values["CTR_GO_PROJECTS_FILE"]); got != want {
		t.Fatalf("ProjectsFile = %q, want %q", got, want)
	}
	if got, want := cfg.CommandMaxAge, 7*time.Minute; got != want {
		t.Fatalf("CommandMaxAge = %s, want %s", got, want)
	}
	if got, want := cfg.FFmpegBin, values["CTR_GO_FFMPEG_BIN"]; got != want {
		t.Fatalf("FFmpegBin = %q, want %q", got, want)
	}
	if got, want := cfg.TranscribeModel, "gpt-test-transcribe"; got != want {
		t.Fatalf("TranscribeModel = %q, want %q", got, want)
	}
	if cfg.OpenAIAPIKey != "" || cfg.TelegramBotToken != "" {
		t.Fatal("ordinary config must not load environment secret values before explicit runtime resolution")
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{values["CTR_GO_OPENAI_API_KEY"], values["CTR_GO_TELEGRAM_BOT_TOKEN"], "openai_api_key"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("config JSON leaked secret material: %s", encoded)
		}
	}
	if got, want := cfg.TelegramCredentialTarget, "bridge/telegram"; got != want {
		t.Fatalf("TelegramCredentialTarget = %q, want %q", got, want)
	}
	if got, want := cfg.OpenAICredentialTarget, "bridge/openai"; got != want {
		t.Fatalf("OpenAICredentialTarget = %q, want %q", got, want)
	}
}

func TestLoadWindowsEnvironmentSecretsRequireExplicitOptIn(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows Credential Manager resolution is Windows-only")
	}
	root := t.TempDir()
	projectPath := filepath.Join(root, "project")
	if err := os.Mkdir(projectPath, 0o755); err != nil {
		t.Fatalf("Mkdir project: %v", err)
	}
	projectsPath := filepath.Join(root, "projects.json")
	if err := os.WriteFile(projectsPath, []byte(`{"projects":[{"id":"p1","display_name":"Project 1","path":"`+jsonPath(projectPath)+`"}]}`), 0o600); err != nil {
		t.Fatalf("WriteFile projects: %v", err)
	}
	t.Setenv("CTR_GO_CONFIG", filepath.Join(root, "missing.env"))
	t.Setenv("CTR_GO_PROFILE", "minimal")
	t.Setenv("CTR_GO_PROJECTS_FILE", projectsPath)
	t.Setenv("CTR_GO_ALLOWED_USER_IDS", "7")
	t.Setenv("CTR_GO_TELEGRAM_CREDENTIAL_TARGET", "CodexTelegramBridge/test-missing-telegram")
	t.Setenv("CTR_GO_OPENAI_CREDENTIAL_TARGET", "CodexTelegramBridge/test-missing-openai")
	t.Setenv("CTR_GO_TELEGRAM_BOT_TOKEN", "test-only-telegram-env-secret")
	t.Setenv("CTR_GO_OPENAI_API_KEY", "test-only-openai-env-secret")
	t.Setenv("CTR_GO_ALLOW_ENV_SECRETS", "0")

	withoutOptIn, err := Load()
	if err != nil {
		t.Fatalf("Load without opt-in: %v", err)
	}
	if withoutOptIn.TelegramBotToken != "" || withoutOptIn.OpenAIAPIKey != "" {
		t.Fatal("environment secrets loaded without CTR_GO_ALLOW_ENV_SECRETS=1")
	}

	t.Setenv("CTR_GO_ALLOW_ENV_SECRETS", "true")
	withTextOptIn, err := Load()
	if err != nil {
		t.Fatalf("Load with textual opt-in: %v", err)
	}
	if withTextOptIn.TelegramBotToken != "" || withTextOptIn.OpenAIAPIKey != "" {
		t.Fatal("environment secrets loaded without the exact CTR_GO_ALLOW_ENV_SECRETS=1 opt-in")
	}

	t.Setenv("CTR_GO_ALLOW_ENV_SECRETS", "1")
	withOptIn, err := Load()
	if err != nil {
		t.Fatalf("Load with opt-in: %v", err)
	}
	if withOptIn.TelegramBotToken != "test-only-telegram-env-secret" || withOptIn.OpenAIAPIKey != "test-only-openai-env-secret" {
		t.Fatal("environment secrets were not loaded after explicit opt-in")
	}
}

func TestFromEnvReadsCodexChatsRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Codex")
	t.Setenv("CTR_GO_CONFIG", filepath.Join(t.TempDir(), "missing.env"))
	t.Setenv("CTR_GO_CODEX_CHATS_ROOT", root)

	cfg := FromEnv()

	if cfg.CodexChatsRoot != root {
		t.Fatalf("CodexChatsRoot = %q, want %q", cfg.CodexChatsRoot, root)
	}
}

func TestMarshalJSONIncludesPublicRuntimeConfig(t *testing.T) {
	t.Parallel()

	data, err := json.Marshal(Config{Profile: "notifier", NotifyNewRun: true, ControlAPIListen: "127.0.0.1:8765"})
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}
	if got["notify_new_run"] != true {
		t.Fatalf("notify_new_run = %#v, want true", got["notify_new_run"])
	}
	if got["control_api_listen"] != "127.0.0.1:8765" {
		t.Fatalf("control_api_listen = %#v, want listen address", got["control_api_listen"])
	}
	if got["profile"] != "notifier" {
		t.Fatalf("profile = %#v, want notifier", got["profile"])
	}
}

func TestParseEnvFileSupportsCommentsAndQuotes(t *testing.T) {
	t.Parallel()

	values, err := ParseEnvFile([]byte(`
# comment
CTR_GO_TELEGRAM_BOT_TOKEN="token with spaces"
CTR_GO_ALLOWED_USER_IDS='123,456'
CTR_GO_NOTIFY_NEW_RUN=off
`), "test.env")
	if err != nil {
		t.Fatalf("ParseEnvFile failed: %v", err)
	}
	want := map[string]string{
		"CTR_GO_TELEGRAM_BOT_TOKEN": "token with spaces",
		"CTR_GO_ALLOWED_USER_IDS":   "123,456",
		"CTR_GO_NOTIFY_NEW_RUN":     "off",
	}
	if !reflect.DeepEqual(values, want) {
		t.Fatalf("values = %#v, want %#v", values, want)
	}
}

func TestParseEnvFilePreservesBackslashesInDoubleQuotedWindowsPaths(t *testing.T) {
	t.Parallel()

	values, err := ParseEnvFile([]byte(`CTR_GO_HOME="C:\Users\me\AppData\Local\Temp\home"`+"\n"), "test.env")
	if err != nil {
		t.Fatalf("ParseEnvFile failed: %v", err)
	}
	if got, want := values["CTR_GO_HOME"], `C:\Users\me\AppData\Local\Temp\home`; got != want {
		t.Fatalf("CTR_GO_HOME = %q, want %q", got, want)
	}
}

func TestParseEnvFileRejectsMalformedDoubleQuotedValues(t *testing.T) {
	t.Parallel()

	for _, value := range []string{`"\q"`, `"\x"`, `"C:\path"suffix"`, `"C:\path\"`} {
		t.Run(value, func(t *testing.T) {
			_, err := ParseEnvFile([]byte("CTR_GO_HOME="+value+"\n"), "test.env")
			if err == nil {
				t.Fatalf("ParseEnvFile(%q) succeeded, want invalid syntax error", value)
			}
		})
	}
}

func TestParseEnvFileRejectsInvalidLine(t *testing.T) {
	t.Parallel()

	_, err := ParseEnvFile([]byte("not-an-assignment\n"), "bad.env")
	if err == nil {
		t.Fatal("ParseEnvFile succeeded, want invalid line error")
	}
	if !strings.Contains(err.Error(), "expected KEY=VALUE") {
		t.Fatalf("error = %v, want KEY=VALUE message", err)
	}
}

func TestLoadReadsConfigFileAndEnvOverridesIt(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	fileDefaultCWD := filepath.Join(dir, "from-file")
	envDefaultCWD := filepath.Join(dir, "from-env")
	home := filepath.Join(dir, "home")
	if err := os.WriteFile(configPath, []byte(strings.Join([]string{
		`CTR_GO_HOME="` + home + `"`,
		`CTR_GO_TELEGRAM_BOT_TOKEN="file-token"`,
		`CTR_GO_ALLOWED_USER_IDS="101 202"`,
		`CTR_GO_DEFAULT_CWD="` + fileDefaultCWD + `"`,
		`CTR_GO_CONTROL_API_LISTEN="127.0.0.1:9876"`,
		`CTR_GO_NOTIFY_NEW_RUN="off"`,
		"",
	}, "\n")), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	t.Setenv("CTR_GO_CONFIG", configPath)
	t.Setenv("CTR_GO_DEFAULT_CWD", envDefaultCWD)
	// A developer's real Windows Credential Manager entries must not affect a
	// config-file precedence test.
	t.Setenv("CTR_GO_TELEGRAM_CREDENTIAL_TARGET", "CodexTelegramBridge/test-missing-telegram")
	t.Setenv("CTR_GO_OPENAI_CREDENTIAL_TARGET", "CodexTelegramBridge/test-missing-openai")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if runtime.GOOS == "windows" && cfg.TelegramBotToken != "" {
		t.Fatalf("TelegramBotToken = %q, want config-file secrets ignored on Windows", cfg.TelegramBotToken)
	}
	if runtime.GOOS != "windows" && cfg.TelegramBotToken != "file-token" {
		t.Fatalf("TelegramBotToken = %q, want file-token", cfg.TelegramBotToken)
	}
	if !reflect.DeepEqual(cfg.AllowedUserIDs, []int64{101, 202}) {
		t.Fatalf("AllowedUserIDs = %#v, want 101,202", cfg.AllowedUserIDs)
	}
	if cfg.DefaultCWD != envDefaultCWD {
		t.Fatalf("DefaultCWD = %q, want env override %q", cfg.DefaultCWD, envDefaultCWD)
	}
	if cfg.Paths.Home != home {
		t.Fatalf("Home = %q, want %q", cfg.Paths.Home, home)
	}
	if cfg.NotifyNewRun {
		t.Fatal("NotifyNewRun = true, want false from config file")
	}
	if cfg.ControlAPIListen != "127.0.0.1:9876" {
		t.Fatalf("ControlAPIListen = %q, want configured listen", cfg.ControlAPIListen)
	}
}

func TestLoadAppliesRuntimeProxyEnvFromConfigFile(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	proxy := "http://127.0.0.1:18080"
	if err := os.WriteFile(configPath, []byte(strings.Join([]string{
		`CTR_GO_HOME="` + filepath.Join(dir, "home") + `"`,
		`HTTPS_PROXY="` + proxy + `"`,
		`NODE_USE_ENV_PROXY="1"`,
		"",
	}, "\n")), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	t.Setenv("CTR_GO_CONFIG", configPath)
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("NODE_USE_ENV_PROXY", "")

	if _, err := Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := os.Getenv("HTTPS_PROXY"); got != proxy {
		t.Fatalf("HTTPS_PROXY = %q, want %q", got, proxy)
	}
	if got := os.Getenv("NODE_USE_ENV_PROXY"); got != "1" {
		t.Fatalf("NODE_USE_ENV_PROXY = %q, want 1", got)
	}
}

func TestLoadDoesNotOverrideExplicitRuntimeProxyEnv(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.env")
	if err := os.WriteFile(configPath, []byte(`HTTPS_PROXY="http://file-proxy"`+"\n"), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	t.Setenv("CTR_GO_CONFIG", configPath)
	t.Setenv("HTTPS_PROXY", "http://env-proxy")

	if _, err := Load(); err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if got := os.Getenv("HTTPS_PROXY"); got != "http://env-proxy" {
		t.Fatalf("HTTPS_PROXY = %q, want env value", got)
	}
}

func TestConfigFilePathOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "custom.env")
	t.Setenv("CTR_GO_CONFIG", path)

	if got := ConfigFilePath(); got != path {
		t.Fatalf("ConfigFilePath = %q, want %q", got, path)
	}
}
