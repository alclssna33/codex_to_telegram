//go:build windows

package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/alclssna33/codex_to_telegram/internal/config"
	"github.com/alclssna33/codex_to_telegram/internal/securestore"
	"github.com/alclssna33/codex_to_telegram/internal/telegram"
)

const windowsTaskName = "Codex Telegram Bridge"

var (
	windowsInstallLoadConfig = config.Load
	windowsRunVersionCheck   = runVersionCheck
	windowsTelegramGetMe     = func(ctx context.Context, token string) error {
		_, err := telegram.NewClient(token).GetMe(ctx)
		return err
	}
	windowsDPAPICheck = func(ctx context.Context) error {
		protector := securestore.NewDPAPIProtector()
		envelope, err := protector.Protect(ctx, []byte("ctr-go-install-check"))
		if err != nil {
			return err
		}
		_, err = protector.Unprotect(ctx, envelope)
		return err
	}
)

func windowsBridgeApplicationStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return "", errors.New("resolve current user home for bridge application state")
	}
	return filepath.Join(home, ".codex-tg"), nil
}

type windowsTaskOptions struct {
	Executable string
	ConfigPath string
}

func renderWindowsTask(opts windowsTaskOptions) (string, error) {
	if !filepath.IsAbs(opts.Executable) {
		return "", errors.New("scheduled task executable path must be absolute")
	}
	return "<?xml version=\"1.0\" encoding=\"UTF-16\"?>\n" +
		"<Task version=\"1.4\" xmlns=\"http://schemas.microsoft.com/windows/2004/02/mit/task\">\n" +
		"  <Triggers><LogonTrigger><Enabled>true</Enabled></LogonTrigger></Triggers>\n" +
		"  <Principals><Principal id=\"Author\"><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>\n" +
		"  <Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><RestartOnFailure><Interval>PT1M</Interval><Count>3</Count></RestartOnFailure></Settings>\n" +
		"  <Actions Context=\"Author\"><Exec><Command>" + xmlEscape(opts.Executable) + "</Command><Arguments>daemon</Arguments></Exec></Actions>\n" +
		"</Task>\n", nil
}

// encodeWindowsTaskXML produces the UTF-16LE BOM form accepted by schtasks
// when importing a Task Scheduler XML definition.
func encodeWindowsTaskXML(xml string) ([]byte, error) {
	if !strings.HasPrefix(xml, `<?xml version="1.0" encoding="UTF-16"?>`) {
		return nil, errors.New("task XML must declare UTF-16 encoding")
	}
	units := utf16.Encode([]rune(xml))
	data := make([]byte, 2+len(units)*2)
	data[0], data[1] = 0xff, 0xfe
	for i, unit := range units {
		binary.LittleEndian.PutUint16(data[2+i*2:], unit)
	}
	return data, nil
}

func runWindowsService(args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		printWindowsServiceUsage(out)
		return nil
	}
	switch args[0] {
	case "install":
		return runWindowsServiceInstall(args[1:], out)
	case "start":
		return runWindowsTaskCommand(out, "/Run", "/TN", windowsTaskName)
	case "stop":
		return runWindowsTaskCommand(out, "/End", "/TN", windowsTaskName)
	case "restart":
		if err := runWindowsTaskCommand(io.Discard, "/End", "/TN", windowsTaskName); err != nil {
			return err
		}
		return runWindowsTaskCommand(out, "/Run", "/TN", windowsTaskName)
	case "status":
		return runWindowsTaskCommand(out, "/Query", "/TN", windowsTaskName, "/FO", "LIST", "/V")
	case "uninstall":
		return runWindowsServiceUninstall(args[1:], in, out)
	case "help", "--help", "-h":
		printWindowsServiceUsage(out)
		return nil
	default:
		return fmt.Errorf("unknown service command: %s", strings.Join(args, " "))
	}
}

func runWindowsServiceInstall(args []string, out io.Writer) error {
	var executable, phase0Report string
	fs := flag.NewFlagSet("ctr-go service install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if current, err := serviceExecutable(); err == nil {
		executable = current
	}
	fs.StringVar(&executable, "ctr-go-bin", executable, "absolute ctr-go binary path")
	fs.StringVar(&phase0Report, "phase0-report", "", "PASS Windows Phase 0 report path for minimal profile")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("usage: ctr-go service install [--phase0-report PATH] [--ctr-go-bin ABSOLUTE_PATH]")
	}
	executable, err := filepath.Abs(executable)
	if err != nil {
		return err
	}
	if err := validateWindowsInstall(executable, phase0Report); err != nil {
		return err
	}
	xml, err := renderWindowsTask(windowsTaskOptions{Executable: executable, ConfigPath: config.ConfigFilePath()})
	if err != nil {
		return err
	}
	taskXML, err := encodeWindowsTaskXML(xml)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp("", "ctr-go-task-*.xml")
	if err != nil {
		return err
	}
	path := file.Name()
	defer os.Remove(path)
	if _, err := file.Write(taskXML); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return runWindowsTaskCommand(out, "/Create", "/TN", windowsTaskName, "/XML", path, "/F")
}

func validateWindowsInstall(executable, phase0Report string) error {
	cfg, err := windowsInstallLoadConfig()
	if err != nil {
		return fmt.Errorf("validate Windows install configuration: %w", err)
	}
	profile := strings.ToLower(strings.TrimSpace(cfg.Profile))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	switch profile {
	case "notifier":
		if len(cfg.AllowedUserIDs) != 1 {
			return errors.New("notifier installation requires exactly one Telegram owner")
		}
		if strings.TrimSpace(cfg.TelegramBotToken) == "" {
			return errors.New("Telegram credential must be set before notifier installation")
		}
		if err := windowsDPAPICheck(ctx); err != nil {
			return fmt.Errorf("validate Windows DPAPI: %w", err)
		}
		if err := windowsRunVersionCheck(cfg.CodexBin, "--version"); err != nil {
			return fmt.Errorf("validate codex: %w", err)
		}
		if err := windowsTelegramGetMe(ctx, cfg.TelegramBotToken); err != nil {
			return errors.New("validate Telegram getMe: request failed")
		}
		if _, err := os.Stat(executable); err != nil {
			return fmt.Errorf("validate ctr-go binary: %w", err)
		}
	case "minimal":
		if len(cfg.Projects) == 0 {
			return errors.New("validate project registry: Windows installation requires the minimal profile with at least one registered project")
		}
		if strings.TrimSpace(cfg.TelegramBotToken) == "" || strings.TrimSpace(cfg.OpenAIAPIKey) == "" {
			return errors.New("both current-user credentials must be set before installation")
		}
		if err := windowsRunVersionCheck(cfg.CodexBin, "--version"); err != nil {
			return fmt.Errorf("validate codex: %w", err)
		}
		if err := windowsRunVersionCheck(cfg.FFmpegBin, "-version"); err != nil {
			return fmt.Errorf("validate ffmpeg: %w", err)
		}
		if err := windowsTelegramGetMe(ctx, cfg.TelegramBotToken); err != nil {
			return errors.New("validate Telegram getMe: request failed")
		}
		if _, err := os.Stat(executable); err != nil {
			return fmt.Errorf("validate ctr-go binary: %w", err)
		}
		if err := validateWindowsPhase0Report(phase0Report); err != nil {
			return err
		}
	default:
		return errors.New("Windows installation requires the minimal or notifier profile")
	}
	return nil
}

func validateWindowsPhase0Report(path string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("a PASS Windows Phase 0 report is required; pass --phase0-report PATH")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read Phase 0 report: %w", err)
	}
	if !hasTerminalPhase0PassDecision(string(data)) {
		return errors.New("Phase 0 report final decision is not PASS")
	}
	return nil
}

const (
	phase0DecisionFooterStart = "<!-- ctr-go-phase0-final-decision:v1"
	phase0DecisionFooterEnd   = "-->"
)

// hasTerminalPhase0PassDecision accepts only the documented four-line
// checksum-bound footer at EOF. The SHA-256 covers all normalized report bytes
// before the footer, so copying a valid footer into later failure prose cannot
// authorize installation.
func hasTerminalPhase0PassDecision(text string) bool {
	if !utf8.ValidString(text) {
		return false
	}
	normalized := strings.ReplaceAll(text, "\r\n", "\n")
	if strings.HasSuffix(normalized, "\n") {
		normalized = strings.TrimSuffix(normalized, "\n")
	}
	lines := strings.Split(normalized, "\n")
	if len(lines) < 5 {
		return false
	}
	footerStart := len(lines) - 4
	footer := lines[footerStart:]
	if footer[0] != phase0DecisionFooterStart || footer[1] != "decision=PASS" || footer[3] != phase0DecisionFooterEnd {
		return false
	}
	if !strings.HasPrefix(footer[2], "content-sha256=") {
		return false
	}
	encoded := strings.TrimPrefix(footer[2], "content-sha256=")
	actual, err := hex.DecodeString(encoded)
	if err != nil || len(actual) != sha256.Size || hex.EncodeToString(actual) != encoded {
		return false
	}
	body := strings.Join(lines[:footerStart], "\n") + "\n"
	expected := sha256.Sum256([]byte(body))
	return subtle.ConstantTimeCompare(actual, expected[:]) == 1
}

func runVersionCheck(binary, argument string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, binary, argument).Run()
}

func runWindowsTaskCommand(out io.Writer, args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	data, err := serviceRunner.Run(ctx, "schtasks.exe", args...)
	if len(data) > 0 {
		_, _ = out.Write(data)
	}
	if err != nil {
		return fmt.Errorf("scheduled task command failed: %w", err)
	}
	return nil
}

func runWindowsServiceUninstall(args []string, in io.Reader, out io.Writer) error {
	confirmed := false
	fs := flag.NewFlagSet("ctr-go service uninstall", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&confirmed, "yes", false, "confirm removal of bridge application state")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return errors.New("usage: ctr-go service uninstall [--yes]")
	}
	if !confirmed {
		_, _ = fmt.Fprint(out, "Remove the scheduled task and Codex Telegram Bridge application state? Type YES: ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		confirmed = strings.TrimSpace(line) == "YES"
	}
	if !confirmed {
		return errors.New("uninstall cancelled; credentials and Codex files were not changed")
	}
	if err := runWindowsTaskCommand(io.Discard, "/Delete", "/TN", windowsTaskName, "/F"); err != nil {
		return err
	}
	statePath, err := windowsBridgeApplicationStatePath()
	if err != nil {
		return err
	}
	if err := os.RemoveAll(statePath); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, "Scheduled task and bridge application state removed. Credentials and Codex files were kept.")
	return nil
}

func printWindowsServiceUsage(out io.Writer) {
	_, _ = fmt.Fprintln(out, "Usage:")
	_, _ = fmt.Fprintln(out, "  ctr-go service install [--phase0-report PATH] [--ctr-go-bin ABSOLUTE_PATH]")
	_, _ = fmt.Fprintln(out, "  ctr-go service start|stop|restart|status")
	_, _ = fmt.Fprintln(out, "  ctr-go service uninstall [--yes]")
}
