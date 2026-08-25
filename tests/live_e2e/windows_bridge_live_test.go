package live_e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestWindowsBridgeLiveAcceptance is inert unless an operator explicitly
// authorizes a private contour. The readback seam returns hashes, never text.
func TestWindowsBridgeLiveAcceptance(t *testing.T) {
	if os.Getenv("CTR_GO_LIVE_E2E") != "1" {
		t.Skip("set CTR_GO_LIVE_E2E=1 only for an authorized private Windows acceptance run")
	}
	required := []string{"CTR_GO_LIVE_E2E_AUTHORIZED", "CTR_GO_LIVE_TELEGRAM_READBACK", "CTR_GO_LIVE_PROJECTS_JSON", "CTR_GO_LIVE_CODEX_READY", "CTR_GO_LIVE_FFMPEG_READY", "CTR_GO_LIVE_OPENAI_READY", "CTR_GO_LIVE_BRIDGE_COMMAND", "CTR_GO_LIVE_READBACK_COMMAND", "CTR_GO_LIVE_EXPECTED_ROUTE_HASH", "CTR_GO_LIVE_EXPECTED_CONTENT_HASH", "CTR_GO_LIVE_EXPECTED_TERMINAL_COUNT"}
	var missing []string
	for _, key := range required {
		value := strings.TrimSpace(os.Getenv(key))
		if (strings.HasSuffix(key, "_COMMAND") || strings.HasPrefix(key, "CTR_GO_LIVE_EXPECTED_")) && value == "" || (!strings.HasSuffix(key, "_COMMAND") && !strings.HasPrefix(key, "CTR_GO_LIVE_EXPECTED_") && value != "1") {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		t.Fatal("live gate closed; explicitly supply: " + strings.Join(missing, ", "))
	}
	scenarioID := fmt.Sprintf("task11-%d", time.Now().UTC().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	bridge := exec.CommandContext(ctx, "cmd", "/c", os.Getenv("CTR_GO_LIVE_BRIDGE_COMMAND"))
	bridge.Env = append(os.Environ(), "CTR_GO_LIVE_SCENARIO_ID="+scenarioID)
	if err := bridge.Start(); err != nil {
		t.Fatal("start authorized bridge seam failed")
	}
	defer func() { _ = bridge.Process.Kill(); _, _ = bridge.Process.Wait() }()
	time.Sleep(2 * time.Second)
	readback := exec.CommandContext(ctx, "cmd", "/c", os.Getenv("CTR_GO_LIVE_READBACK_COMMAND"))
	readback.Env = append(os.Environ(), "CTR_GO_LIVE_SCENARIO_ID="+scenarioID)
	output, err := readback.Output()
	if err != nil {
		t.Fatal("authorized Telegram readback failed")
	}
	var facts struct {
		ScenarioID    string `json:"scenario_id"`
		RouteHash     string `json:"route_hash"`
		ContentHash   string `json:"content_hash"`
		TerminalCount string `json:"terminal_count"`
	}
	if json.Unmarshal(output, &facts) != nil || facts.ScenarioID != scenarioID || facts.RouteHash != os.Getenv("CTR_GO_LIVE_EXPECTED_ROUTE_HASH") || facts.ContentHash != os.Getenv("CTR_GO_LIVE_EXPECTED_CONTENT_HASH") || facts.TerminalCount != os.Getenv("CTR_GO_LIVE_EXPECTED_TERMINAL_COUNT") {
		t.Fatal("readback facts were not exactly bound to this live scenario")
	}
}

func TestWindowsBridgeRegisteredPickerObserverLiveAcceptance(t *testing.T) {
	if os.Getenv("CTR_GO_LIVE_REGISTERED_PICKER_OBSERVER_E2E") != "1" {
		t.Skip("set CTR_GO_LIVE_REGISTERED_PICKER_OBSERVER_E2E=1 only for the authorized registered-picker observer contour")
	}
	required := []string{
		"CTR_GO_LIVE_E2E_AUTHORIZED",
		"CTR_GO_LIVE_TELEGRAM_READBACK",
		"CTR_GO_LIVE_REGISTERED_CONTOUR_READY",
		"CTR_GO_LIVE_BRIDGE_COMMAND",
		"CTR_GO_LIVE_READBACK_COMMAND",
		"CTR_GO_LIVE_EXPECTED_EXISTING_CONTINUATION_HASH",
		"CTR_GO_LIVE_EXPECTED_UNSELECTED_FINAL_HASH",
		"CTR_GO_LIVE_EXPECTED_UNSELECTED_TERMINAL_COUNT",
		"CTR_GO_LIVE_EXPECTED_RESTART_REPLAY_COUNT",
	}
	var missing []string
	for _, key := range required {
		value := strings.TrimSpace(os.Getenv(key))
		if (strings.HasSuffix(key, "_COMMAND") || strings.HasPrefix(key, "CTR_GO_LIVE_EXPECTED_")) && value == "" || (!strings.HasSuffix(key, "_COMMAND") && !strings.HasPrefix(key, "CTR_GO_LIVE_EXPECTED_") && value != "1") {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		t.Fatal("registered picker/observer live gate closed; explicitly supply: " + strings.Join(missing, ", "))
	}
	scenarioID := fmt.Sprintf("registered-picker-observer-%d", time.Now().UTC().UnixNano())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	bridge := exec.CommandContext(ctx, "cmd", "/c", os.Getenv("CTR_GO_LIVE_BRIDGE_COMMAND"))
	bridge.Env = append(os.Environ(), "CTR_GO_LIVE_SCENARIO_ID="+scenarioID)
	if err := bridge.Start(); err != nil {
		t.Fatal("start authorized bridge seam failed")
	}
	defer func() { _ = bridge.Process.Kill(); _, _ = bridge.Process.Wait() }()
	time.Sleep(2 * time.Second)
	readback := exec.CommandContext(ctx, "cmd", "/c", os.Getenv("CTR_GO_LIVE_READBACK_COMMAND"))
	readback.Env = append(os.Environ(), "CTR_GO_LIVE_SCENARIO_ID="+scenarioID)
	output, err := readback.Output()
	if err != nil {
		t.Fatal("authorized registered picker/observer readback failed")
	}
	var facts struct {
		ScenarioID               string `json:"scenario_id"`
		ExistingContinuationHash string `json:"existing_continuation_hash"`
		UnselectedFinalHash      string `json:"unselected_final_hash"`
		UnselectedTerminalCount  string `json:"unselected_terminal_count"`
		RestartReplayCount       string `json:"restart_replay_count"`
	}
	if json.Unmarshal(output, &facts) != nil ||
		facts.ScenarioID != scenarioID ||
		facts.ExistingContinuationHash != os.Getenv("CTR_GO_LIVE_EXPECTED_EXISTING_CONTINUATION_HASH") ||
		facts.UnselectedFinalHash != os.Getenv("CTR_GO_LIVE_EXPECTED_UNSELECTED_FINAL_HASH") ||
		facts.UnselectedTerminalCount != os.Getenv("CTR_GO_LIVE_EXPECTED_UNSELECTED_TERMINAL_COUNT") ||
		facts.RestartReplayCount != os.Getenv("CTR_GO_LIVE_EXPECTED_RESTART_REPLAY_COUNT") {
		t.Fatal("registered picker/observer readback facts were not exactly bound to this live scenario")
	}
}
