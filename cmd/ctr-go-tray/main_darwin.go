//go:build darwin

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"fyne.io/systray"

	"github.com/alclssna33/codex_to_telegram/internal/config"
	"github.com/alclssna33/codex_to_telegram/internal/trayapp"
)

var templateIcon = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00,
	0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x60, 0x00, 0x00, 0x00,
	0x02, 0x00, 0x01, 0xe2, 0x21, 0xbc, 0x33, 0x00, 0x00, 0x00, 0x00, 0x49,
	0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}

func main() {
	systray.Run(onReady, func() {})
}

func onReady() {
	ctrGo := resolveCTRGo()
	systray.SetTitle("ctg")
	systray.SetTooltip("codex-tg")
	if len(templateIcon) > 0 {
		systray.SetTemplateIcon(templateIcon, templateIcon)
	}

	statusItem := systray.AddMenuItem("Status: checking...", "Show service status")
	startItem := systray.AddMenuItem("Start", "Start codex-tg service")
	stopItem := systray.AddMenuItem("Stop", "Stop codex-tg service")
	restartItem := systray.AddMenuItem("Restart", "Restart codex-tg service")
	loginItem := systray.AddMenuItemCheckbox("Start with system", "Toggle login LaunchAgent", false)
	systray.AddSeparator()
	doctorItem := systray.AddMenuItem("Run doctor", "Run ctr-go doctor in Terminal")
	configItem := systray.AddMenuItem("Open config", "Open config.env")
	logsItem := systray.AddMenuItem("Open logs", "Open codex-tg logs")
	setupItem := systray.AddMenuItem("Open Terminal Setup", "Run friendly setup wizard")
	systray.AddSeparator()
	quitItem := systray.AddMenuItem("Quit", "Quit tray app")

	refresh := func() {
		status := readServiceStatus(ctrGo)
		statusItem.SetTitle("Status: " + status.summary)
		if status.startAtLogin {
			loginItem.Check()
		} else {
			loginItem.Uncheck()
		}
		if !status.configExists {
			statusItem.SetTitle("Status: Needs setup")
		}
	}
	refresh()
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			refresh()
		}
	}()

	go func() {
		for {
			select {
			case <-statusItem.ClickedCh:
				runTerminalCommand(ctrGo, "service", "status")
			case <-startItem.ClickedCh:
				runAndRefresh(refresh, ctrGo, trayapp.ActionStart)
			case <-stopItem.ClickedCh:
				runAndRefresh(refresh, ctrGo, trayapp.ActionStop)
			case <-restartItem.ClickedCh:
				runAndRefresh(refresh, ctrGo, trayapp.ActionRestart)
			case <-loginItem.ClickedCh:
				if readServiceStatus(ctrGo).startAtLogin {
					runAndRefresh(refresh, ctrGo, trayapp.ActionDisableLogin)
				} else {
					runAndRefresh(refresh, ctrGo, trayapp.ActionEnableLogin)
				}
			case <-doctorItem.ClickedCh:
				runTerminalCommand(ctrGo, "doctor")
			case <-configItem.ClickedCh:
				_ = exec.Command("open", config.ConfigFilePath()).Start()
			case <-logsItem.ClickedCh:
				_ = exec.Command("open", config.DefaultPaths().LogDir).Start()
			case <-setupItem.ClickedCh:
				runTerminalCommand(ctrGo, trayapp.ServiceSetupArgs()...)
			case <-quitItem.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

type serviceStatus struct {
	summary      string
	startAtLogin bool
	configExists bool
}

func readServiceStatus(ctrGo string) serviceStatus {
	out, err := runCTRGo(ctrGo, trayapp.ActionStatus)
	status := serviceStatus{summary: "unknown", configExists: fileExists(config.ConfigFilePath())}
	if err != nil {
		if !status.configExists {
			status.summary = "needs setup"
		}
		return status
	}
	text := string(out)
	status.startAtLogin = strings.Contains(text, "Start with system: true")
	if strings.Contains(text, "Loaded: true") {
		status.summary = "running"
	} else if status.configExists {
		status.summary = "stopped"
	}
	return status
}

func runAndRefresh(refresh func(), ctrGo string, action trayapp.Action) {
	_, _ = runCTRGo(ctrGo, action)
	refresh()
}

func runCTRGo(ctrGo string, action trayapp.Action) ([]byte, error) {
	args, err := trayapp.CTRGoArgs(action)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ctrGo, args...)
	return cmd.CombinedOutput()
}

func runTerminalCommand(ctrGo string, args ...string) {
	var script bytes.Buffer
	script.WriteString(shellQuote(ctrGo))
	for _, arg := range args {
		script.WriteByte(' ')
		script.WriteString(shellQuote(arg))
	}
	escaped := strings.ReplaceAll(script.String(), `"`, `\"`)
	_ = exec.Command("osascript", "-e", fmt.Sprintf(`tell application "Terminal" to do script "%s"`, escaped)).Start()
}

func resolveCTRGo() string {
	if value := strings.TrimSpace(os.Getenv("CTR_GO_BIN")); value != "" {
		return value
	}
	if found, err := exec.LookPath("ctr-go"); err == nil {
		return found
	}
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "ctr-go")
		if fileExists(candidate) {
			return candidate
		}
	}
	return "/usr/local/bin/ctr-go"
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
