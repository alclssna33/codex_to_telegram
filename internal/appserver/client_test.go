package appserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/control"
)

func TestClientCloseContextReapsOnlyItsProcess(t *testing.T) {
	first, stdin := startCloseContextStdinHelperProcess(t, `package main

import "io"
import "os"

func main() {
	_, _ = io.Copy(io.Discard, os.Stdin)
}
`)
	second := startCloseContextHelperProcess(t)
	secondExited := make(chan error, 1)
	go func() {
		secondExited <- second.Wait()
	}()
	t.Cleanup(func() {
		if second.ProcessState != nil {
			return
		}
		_ = second.Process.Kill()
		select {
		case <-secondExited:
		case <-time.After(2 * time.Second):
			t.Fatal("second helper process did not exit during cleanup")
		}
	})

	pending := make(chan rpcResponse, 1)
	client := NewClient("codex", "stdio://", t.TempDir(), time.Second)
	client.mu.Lock()
	client.started = true
	client.cmd = first
	client.stdin = stdin
	client.pending[7] = pending
	client.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.CloseContext(ctx); err != nil {
		t.Fatalf("CloseContext returned error: %v", err)
	}
	if first.ProcessState == nil || !first.ProcessState.Exited() {
		t.Fatalf("first helper process was not reaped: state=%#v", first.ProcessState)
	}
	select {
	case response, ok := <-pending:
		if !ok || response.Error == nil || !strings.Contains(response.Error.Error(), "closed before response") {
			t.Fatalf("pending response = %#v ok=%t, want closed-before-response error", response, ok)
		}
	default:
		t.Fatal("CloseContext did not fail pending request")
	}
	select {
	case err := <-secondExited:
		t.Fatalf("CloseContext exited unrelated helper process: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestClientCloseContextWaitsForGracefulExitAfterStdinClose(t *testing.T) {
	cmd, stdin := startCloseContextStdinHelperProcess(t, `package main

import "io"
import "os"

func main() {
	_, _ = io.Copy(io.Discard, os.Stdin)
}
`)
	pending := make(chan rpcResponse, 1)
	client := NewClient("codex", "stdio://", t.TempDir(), time.Second)
	client.mu.Lock()
	client.started = true
	client.cmd = cmd
	client.stdin = stdin
	client.pending[7] = pending
	client.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.CloseContext(ctx); err != nil {
		t.Fatalf("CloseContext returned error for graceful stdin exit: %v", err)
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatalf("helper process was not reaped after graceful close: state=%#v", cmd.ProcessState)
	}
	if code := cmd.ProcessState.ExitCode(); code != 0 {
		t.Fatalf("helper exit code = %d, want graceful 0", code)
	}
	select {
	case response, ok := <-pending:
		if !ok || response.Error == nil || !strings.Contains(response.Error.Error(), "closed before response") {
			t.Fatalf("pending response = %#v ok=%t, want closed-before-response error", response, ok)
		}
	default:
		t.Fatal("CloseContext did not fail pending request")
	}
}

func TestClientCloseContextKillsAndReapsExactChildAfterGraceTimeout(t *testing.T) {
	first, stdin := startCloseContextStdinHelperProcess(t, `package main

import "time"

func main() {
	for {
		time.Sleep(time.Hour)
	}
}
`)
	second := startCloseContextHelperProcess(t)
	secondExited := make(chan error, 1)
	go func() {
		secondExited <- second.Wait()
	}()
	t.Cleanup(func() {
		if second.ProcessState != nil {
			return
		}
		_ = second.Process.Kill()
		select {
		case <-secondExited:
		case <-time.After(2 * time.Second):
			t.Fatal("second helper process did not exit during cleanup")
		}
	})

	client := NewClient("codex", "stdio://", t.TempDir(), time.Second)
	client.mu.Lock()
	client.started = true
	client.cmd = first
	client.stdin = stdin
	client.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := client.CloseContext(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CloseContext error = %v, want context deadline after forced kill", err)
	}
	if !CommandExitConfirmed(err) {
		t.Fatalf("CloseContext error = %v, want confirmed exact child exit", err)
	}
	if first.ProcessState == nil || !first.ProcessState.Exited() {
		t.Fatalf("timed out helper process was not reaped: state=%#v", first.ProcessState)
	}
	select {
	case err := <-secondExited:
		t.Fatalf("CloseContext exited unrelated helper process: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
}

func startCloseContextHelperProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd, _ := startCloseContextStdinHelperProcess(t, `package main

import "time"

func main() {
	for {
		time.Sleep(time.Hour)
	}
}
`)
	return cmd
}

func startCloseContextStdinHelperProcess(t *testing.T, source string) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	root := t.TempDir()
	sourcePath := filepath.Join(root, "main.go")
	if err := os.WriteFile(sourcePath, []byte(source), 0o644); err != nil {
		t.Fatalf("write helper source: %v", err)
	}
	exePath := filepath.Join(root, "close-context-helper")
	if runtime.GOOS == "windows" {
		exePath += ".exe"
	}
	build := exec.Command("go", "build", "-o", exePath, sourcePath)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build helper process: %v\n%s", err, output)
	}
	cmd := exec.Command(exePath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("helper stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper process: %v", err)
	}
	return cmd, stdin
}

func TestRPCStringSkipsNilLikeValues(t *testing.T) {
	t.Parallel()

	for _, value := range []any{nil, "", " ", "<nil>"} {
		if got := rpcString(value); got != "" {
			t.Fatalf("rpcString(%#v) = %q, want empty", value, got)
		}
	}
	if got := rpcString(float64(42)); got != "42" {
		t.Fatalf("rpcString(42) = %q, want 42", got)
	}
}

func TestRequestTimeoutPreservesDeadlineExceeded(t *testing.T) {
	client := NewClient("codex", "stdio://", t.TempDir(), 10*time.Millisecond)
	writer := &testWriteCloser{}
	client.mu.Lock()
	client.started = true
	client.stdin = writer
	client.mu.Unlock()

	_, err := client.Request(context.Background(), "thread/read", map[string]any{"threadId": "thread-1"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Request timeout error = %v, want context.DeadlineExceeded", err)
	}
	if err == nil || !strings.Contains(err.Error(), "thread/read") {
		t.Fatalf("Request timeout error = %v, want method context", err)
	}
}

func TestReadStdoutAcceptsLargeThreadReadResponse(t *testing.T) {
	reader, writer := io.Pipe()
	client := NewClient("codex", "stdio://", t.TempDir(), time.Second)
	reply := make(chan rpcResponse, 1)
	client.mu.Lock()
	client.started = true
	client.generation = 1
	client.stdout = reader
	client.pending[1] = reply
	client.mu.Unlock()
	go client.readStdout(1)

	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"result": map[string]any{
			"thread": map[string]any{"large": strings.Repeat("x", 17*1024*1024)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = writer.Write(append(payload, '\n'))
		_ = writer.Close()
	}()
	select {
	case response := <-reply:
		if response.Error != nil {
			t.Fatalf("large response error = %v", response.Error)
		}
	case <-time.After(time.Second):
		t.Fatal("large thread/read response was not delivered")
	}
}

func TestHandlePayloadIgnoresStaleGeneration(t *testing.T) {
	t.Parallel()

	client := NewClient("codex", "stdio", t.TempDir(), time.Second)
	events := client.Subscribe()
	reply := make(chan rpcResponse, 1)
	client.mu.Lock()
	client.started = true
	client.generation = 2
	client.pending[1] = reply
	client.mu.Unlock()

	client.handlePayload(map[string]any{
		"jsonrpc": "2.0",
		"id":      float64(1),
		"result":  map[string]any{"ok": true},
	}, 1)
	select {
	case response := <-reply:
		t.Fatalf("stale generation resolved pending response: %#v", response)
	default:
	}
	client.mu.Lock()
	if _, ok := client.pending[1]; !ok {
		t.Fatal("stale generation deleted pending response")
	}
	client.mu.Unlock()

	client.handlePayload(map[string]any{
		"jsonrpc": "2.0",
		"id":      "req-stale",
		"method":  "serverRequest/approval",
		"params":  map[string]any{"requestId": "req-stale"},
	}, 1)
	client.mu.Lock()
	_, stored := client.serverRequests["req-stale"]
	client.mu.Unlock()
	if stored {
		t.Fatal("stale generation stored server request")
	}
	client.handlePayload(map[string]any{
		"jsonrpc": "2.0",
		"method":  "thread/status/changed",
		"params":  map[string]any{"threadId": "thread-stale"},
	}, 1)
	select {
	case event := <-events:
		t.Fatalf("stale generation broadcast event: %#v", event)
	default:
	}

	client.handlePayload(map[string]any{
		"jsonrpc": "2.0",
		"id":      float64(1),
		"result":  map[string]any{"ok": true},
	}, 2)
	select {
	case response := <-reply:
		if response.Error != nil {
			t.Fatalf("current generation response error: %v", response.Error)
		}
	default:
		t.Fatal("current generation did not resolve pending response")
	}
}

func TestRPCErrorPreservesCodeAndMessage(t *testing.T) {
	reply := make(chan rpcResponse, 1)
	client := &Client{started: true, generation: 4, pending: map[uint64]chan rpcResponse{7: reply}}
	client.handlePayload(map[string]any{
		"id":    float64(7),
		"error": map[string]any{"code": float64(-32600), "message": "thread parent-1 already has an active writer"},
	}, 4)
	response := <-reply
	var rpcErr *RPCError
	if !errors.As(response.Error, &rpcErr) || rpcErr.Code != -32600 || rpcErr.Message != "thread parent-1 already has an active writer" {
		t.Fatalf("error = %#v", response.Error)
	}
}

func TestHandlePayloadPreservesServerRequestID(t *testing.T) {
	t.Parallel()

	client := NewClient("codex", "stdio", t.TempDir(), time.Second)
	events := client.Subscribe()
	client.mu.Lock()
	client.started = true
	client.generation = 1
	client.mu.Unlock()

	const requestID = "req-actionable-1"
	client.handlePayload(map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"method":  "item/commandExecution/requestApproval",
		"params": map[string]any{
			"threadId": "thread-1",
			"turnId":   "turn-1",
		},
	}, 1)

	select {
	case event := <-events:
		if event.Channel != "server_request" || event.Method != "item/commandExecution/requestApproval" {
			t.Fatalf("event = %#v, want approval server request", event)
		}
		if got, ok := event.ID.(string); !ok || got != requestID {
			t.Fatalf("event.ID = %#v, want unchanged string %q", event.ID, requestID)
		}
	default:
		t.Fatal("server request event was not broadcast")
	}
}

func TestRespondServerRequestRejectsUnknownRequest(t *testing.T) {
	client := NewClient("codex", "stdio://", t.TempDir(), time.Second)
	writer := &testWriteCloser{}
	client.mu.Lock()
	client.started = true
	client.generation = 1
	client.stdin = writer
	client.mu.Unlock()

	err := client.RespondServerRequest(context.Background(), "req-unknown", map[string]any{"decision": "decline"})
	if err == nil {
		t.Fatal("RespondServerRequest succeeded for unknown request")
	}
	if writer.Len() != 0 {
		t.Fatalf("unknown request wrote %d bytes", writer.Len())
	}
}

func TestRespondServerRequestRejectsResolvedRequest(t *testing.T) {
	client := NewClient("codex", "stdio://", t.TempDir(), time.Second)
	writer := &testWriteCloser{}
	client.mu.Lock()
	client.started = true
	client.generation = 1
	client.stdin = writer
	client.mu.Unlock()
	client.handlePayload(map[string]any{
		"id":     "req-resolved",
		"method": "item/commandExecution/requestApproval",
		"params": map[string]any{"threadId": "thread-1", "turnId": "turn-1"},
	}, 1)
	client.handlePayload(map[string]any{
		"method": "serverRequest/resolved",
		"params": map[string]any{"requestId": "req-resolved"},
	}, 1)

	err := client.RespondServerRequest(context.Background(), "req-resolved", map[string]any{"decision": "decline"})
	if err == nil {
		t.Fatal("RespondServerRequest succeeded for resolved request")
	}
	if writer.Len() != 0 {
		t.Fatalf("resolved request wrote %d bytes", writer.Len())
	}
}

func TestRespondServerRequestWriteFailureKeepsRequestPending(t *testing.T) {
	client := NewClient("codex", "stdio://", t.TempDir(), time.Second)
	writer := &testWriteCloser{writeErr: errors.New("write failed")}
	client.mu.Lock()
	client.started = true
	client.generation = 1
	client.stdin = writer
	client.mu.Unlock()
	client.handlePayload(map[string]any{
		"id":     "req-retry",
		"method": "item/commandExecution/requestApproval",
		"params": map[string]any{"threadId": "thread-1", "turnId": "turn-1"},
	}, 1)

	if err := client.RespondServerRequest(context.Background(), "req-retry", map[string]any{"decision": "decline"}); err == nil {
		t.Fatal("RespondServerRequest succeeded despite write failure")
	}
	client.mu.Lock()
	_, pending := client.serverRequests["req-retry"]
	client.mu.Unlock()
	if !pending {
		t.Fatal("write failure removed pending server request")
	}
}

func TestRespondServerRequestPreservesNumericWireID(t *testing.T) {
	client := NewClient("codex", "stdio://", t.TempDir(), time.Second)
	writer := &testWriteCloser{}
	client.mu.Lock()
	client.started = true
	client.generation = 1
	client.stdin = writer
	client.mu.Unlock()
	client.handlePayload(map[string]any{
		"id":     float64(42),
		"method": "item/commandExecution/requestApproval",
		"params": map[string]any{"threadId": "thread-1", "turnId": "turn-1"},
	}, 1)

	if err := client.RespondServerRequest(context.Background(), "42", map[string]any{"decision": "decline"}); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(writer.Bytes(), &payload); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got, ok := payload["id"].(float64); !ok || got != 42 {
		t.Fatalf("response id = %#v, want numeric 42", payload["id"])
	}
	client.mu.Lock()
	_, pending := client.serverRequests["42"]
	client.mu.Unlock()
	if pending {
		t.Fatal("successful response left request pending")
	}
}

func TestRespondServerRequestRejectsStaleGeneration(t *testing.T) {
	client := NewClient("codex", "stdio://", t.TempDir(), time.Second)
	writer := &testWriteCloser{}
	client.mu.Lock()
	client.started = true
	client.generation = 1
	client.stdin = writer
	client.mu.Unlock()
	client.handlePayload(map[string]any{
		"id":     "req-stale",
		"method": "item/commandExecution/requestApproval",
		"params": map[string]any{"threadId": "thread-1", "turnId": "turn-1"},
	}, 1)
	client.mu.Lock()
	client.generation = 2
	client.mu.Unlock()

	if err := client.RespondServerRequest(context.Background(), "req-stale", map[string]any{"decision": "decline"}); err == nil {
		t.Fatal("RespondServerRequest succeeded for stale-generation request")
	}
	if err := client.RespondServerRequestExact(context.Background(), "req-stale", "thread-1", "turn-1", "item/commandExecution/requestApproval", map[string]any{"decision": "decline"}); err == nil {
		t.Fatal("RespondServerRequestExact succeeded for stale-generation request")
	}
	if writer.Len() != 0 {
		t.Fatalf("stale request wrote %d bytes", writer.Len())
	}
}

func TestRespondServerRequestExactRejectsReusedIDWithMismatchedLogicalIdentity(t *testing.T) {
	tests := []struct {
		name        string
		threadID    string
		turnID      string
		requestKind string
	}{
		{name: "thread", threadID: "thread-2", turnID: "turn-1", requestKind: "item/commandExecution/requestApproval"},
		{name: "turn", threadID: "thread-1", turnID: "turn-2", requestKind: "item/commandExecution/requestApproval"},
		{name: "kind", threadID: "thread-1", turnID: "turn-1", requestKind: "item/fileChange/requestApproval"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := NewClient("codex", "stdio://", t.TempDir(), time.Second)
			writer := &testWriteCloser{}
			client.mu.Lock()
			client.started = true
			client.generation = 1
			client.stdin = writer
			client.mu.Unlock()
			client.handlePayload(map[string]any{
				"id":     "req-reused-exact",
				"method": "item/commandExecution/requestApproval",
				"params": map[string]any{"threadId": "thread-1", "turnId": "turn-1"},
			}, 1)
			client.handlePayload(map[string]any{
				"id":     "req-reused-exact",
				"method": tc.requestKind,
				"params": map[string]any{"threadId": tc.threadID, "turnId": tc.turnID},
			}, 1)

			err := client.RespondServerRequestExact(context.Background(), "req-reused-exact", "thread-1", "turn-1", "item/commandExecution/requestApproval", map[string]any{"decision": "accept"})
			if err == nil {
				t.Fatal("stale logical identity responded to reused request ID")
			}
			if writer.Len() != 0 {
				t.Fatalf("stale logical identity wrote %d bytes", writer.Len())
			}
			if err := client.RespondServerRequestExact(context.Background(), "req-reused-exact", tc.threadID, tc.turnID, tc.requestKind, map[string]any{"decision": "decline"}); err != nil {
				t.Fatalf("exact current logical identity failed: %v", err)
			}
			if writer.Len() == 0 {
				t.Fatal("exact current logical identity wrote no response")
			}
		})
	}
}

type testWriteCloser struct {
	buffer   bytes.Buffer
	writeErr error
}

func (w *testWriteCloser) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return w.buffer.Write(p)
}

func (w *testWriteCloser) Close() error  { return nil }
func (w *testWriteCloser) Len() int      { return w.buffer.Len() }
func (w *testWriteCloser) Bytes() []byte { return w.buffer.Bytes() }

func TestTurnStartParamsIncludesCollaborationMode(t *testing.T) {
	params, err := turnStartParams("thread-1", "Draft a plan", "/tmp/project", TurnStartOptions{
		CollaborationMode: "plan",
		Model:             "gpt-test",
		ReasoningEffort:   "x-high",
	})
	if err != nil {
		t.Fatalf("turnStartParams failed: %v", err)
	}
	if got, want := params["threadId"], "thread-1"; got != want {
		t.Fatalf("threadId = %v, want %q", got, want)
	}
	collaborationMode, ok := params["collaborationMode"].(map[string]any)
	if !ok {
		t.Fatalf("collaborationMode = %#v, want object", params["collaborationMode"])
	}
	if got, want := collaborationMode["mode"], "plan"; got != want {
		t.Fatalf("mode = %v, want %q", got, want)
	}
	settings, ok := collaborationMode["settings"].(map[string]any)
	if !ok {
		t.Fatalf("settings = %#v, want object", collaborationMode["settings"])
	}
	if got, want := settings["model"], "gpt-test"; got != want {
		t.Fatalf("model = %v, want %q", got, want)
	}
	if got, want := settings["reasoning_effort"], "xhigh"; got != want {
		t.Fatalf("reasoning_effort = %v, want %q", got, want)
	}
	if _, ok := settings["developer_instructions"]; !ok {
		t.Fatal("developer_instructions key is missing")
	}
}

func TestTurnStartParamsIncludesDefaultCollaborationMode(t *testing.T) {
	params, err := turnStartParams("thread-1", "Run it", "/tmp/project", TurnStartOptions{
		CollaborationMode: "default",
		Model:             "gpt-test",
	})
	if err != nil {
		t.Fatalf("turnStartParams failed: %v", err)
	}
	collaborationMode, ok := params["collaborationMode"].(map[string]any)
	if !ok {
		t.Fatalf("collaborationMode = %#v, want object", params["collaborationMode"])
	}
	if got, want := collaborationMode["mode"], "default"; got != want {
		t.Fatalf("mode = %v, want %q", got, want)
	}
}

func TestTurnStartParamsRejectsModeWithoutModel(t *testing.T) {
	_, err := turnStartParams("thread-1", "Draft a plan", "", TurnStartOptions{CollaborationMode: "plan"})
	if err == nil {
		t.Fatal("turnStartParams succeeded, want missing model error")
	}
}

func TestTurnStartParamsPinsWorkspaceWriteAndApproval(t *testing.T) {
	writableRoots := []string{`C:\work\p1`}
	params, err := turnStartParams("thread-1", "do work", `C:\work\p1`, TurnStartOptions{
		ApprovalPolicy: "unlessTrusted",
		SandboxPolicy: SandboxPolicy{
			Type:          "workspaceWrite",
			WritableRoots: writableRoots,
			NetworkAccess: false,
		},
	})
	if err != nil {
		t.Fatalf("turnStartParams failed: %v", err)
	}
	if got, want := params["approvalPolicy"], "unlessTrusted"; got != want {
		t.Fatalf("approvalPolicy = %#v, want %q", got, want)
	}
	sandbox, ok := params["sandboxPolicy"].(map[string]any)
	if !ok {
		t.Fatalf("sandboxPolicy = %#v, want object", params["sandboxPolicy"])
	}
	if got, want := sandbox["type"], "workspaceWrite"; got != want {
		t.Fatalf("sandbox type = %#v, want %q", got, want)
	}
	if got, want := sandbox["writableRoots"], writableRoots; !reflect.DeepEqual(got, want) {
		t.Fatalf("writableRoots = %#v, want %#v", got, want)
	}
	if got, want := sandbox["networkAccess"], false; got != want {
		t.Fatalf("networkAccess = %#v, want false", got)
	}

	writableRoots[0] = `C:\changed`
	if got := sandbox["writableRoots"].([]string)[0]; got != `C:\work\p1` {
		t.Fatalf("request policy aliased caller slice: %q", got)
	}
}

func TestThreadListParamsSupportsNotifierInteractiveFilter(t *testing.T) {
	archived := false
	params := threadListParams(control.ThreadListOptions{
		Limit:         100,
		Cursor:        "next-page",
		SortKey:       "updated_at",
		SortDirection: "desc",
		SourceKinds:   []string{"cli", "vscode"},
		Archived:      &archived,
	})

	if _, ok := params["cwd"]; ok {
		t.Fatalf("notifier params unexpectedly contain cwd: %#v", params)
	}
	if got := params["sourceKinds"]; !reflect.DeepEqual(got, []string{"cli", "vscode"}) {
		t.Fatalf("sourceKinds = %#v", got)
	}
	if params["limit"] != 100 || params["cursor"] != "next-page" || params["archived"] != false {
		t.Fatalf("params = %#v", params)
	}
}

func TestControlPlaneThreadForkParams(t *testing.T) {
	params := threadForkParams("thread-1", control.ThreadForkOptions{CWD: "/tmp/project"})
	if got, want := params["threadId"], "thread-1"; got != want {
		t.Fatalf("threadId = %v, want %q", got, want)
	}
	if got, want := params["cwd"], "/tmp/project"; got != want {
		t.Fatalf("cwd = %v, want %q", got, want)
	}

	params = threadForkParams("thread-1", control.ThreadForkOptions{})
	if _, ok := params["cwd"]; ok {
		t.Fatalf("cwd should be omitted for empty cwd: %#v", params)
	}
}

func TestThreadForkParamsPinsCompletedTurn(t *testing.T) {
	params := threadForkParams("parent-1", control.ThreadForkOptions{
		CWD:        `C:\work\project`,
		LastTurnID: "turn-completed",
	})
	if params["threadId"] != "parent-1" || params["cwd"] != `C:\work\project` || params["lastTurnId"] != "turn-completed" {
		t.Fatalf("fork params = %#v", params)
	}
	if _, present := params["ephemeral"]; present {
		t.Fatalf("persistent fork must not serialize ephemeral: %#v", params)
	}
}

func TestControlPlaneSkillsListParams(t *testing.T) {
	params := skillsListParams([]string{"/tmp/a", "/tmp/b"}, true)
	cwds, ok := params["cwds"].([]string)
	if !ok {
		t.Fatalf("cwds = %#v, want []string", params["cwds"])
	}
	if got, want := len(cwds), 2; got != want {
		t.Fatalf("cwds len = %d, want %d", got, want)
	}
	if got, want := params["forceReload"], true; got != want {
		t.Fatalf("forceReload = %v, want %v", got, want)
	}

	params = skillsListParams(nil, false)
	if len(params) != 0 {
		t.Fatalf("empty params = %#v, want empty", params)
	}
}

func TestStartConcurrentCallsShareInitializedProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake app-server shell script is Unix-only")
	}
	root := t.TempDir()
	logPath := filepath.Join(root, "rpc.log")
	t.Setenv("CODEX_TG_FAKE_APPSERVER_LOG", logPath)
	script := writeFakeAppServer(t, root, `#!/bin/sh
set -eu
log="${CODEX_TG_FAKE_APPSERVER_LOG:-}"
if IFS= read -r line; then
  if [ -n "$log" ]; then printf '%s\n' "$line" >> "$log"; fi
  sleep 0.2
  printf '{"jsonrpc":"2.0","id":1,"result":{}}\n'
fi
if IFS= read -r line; then
  if [ -n "$log" ]; then printf '%s\n' "$line" >> "$log"; fi
fi
sleep 5
`)
	client := NewClient(script, "stdio", root, 5*time.Second)
	defer client.Close()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			errs[index] = client.Start(ctx)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Start[%d] failed: %v", i, err)
		}
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(%s) failed: %v", logPath, err)
	}
	if got := strings.Count(string(data), `"method":"initialize"`); got != 1 {
		t.Fatalf("initialize requests = %d, want 1; log:\n%s", got, data)
	}
}

func TestStartCleansUpAfterInitializeFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake app-server shell script is Unix-only")
	}
	root := t.TempDir()
	script := writeFakeAppServer(t, root, `#!/bin/sh
set -eu
if IFS= read -r line; then
  printf '{"jsonrpc":"2.0","id":1,"error":{"message":"init failed"}}\n'
fi
sleep 5
`)
	client := NewClient(script, "stdio", root, 5*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := client.Start(ctx)
	if err == nil {
		t.Fatal("Start succeeded, want initialize failure")
	}

	client.mu.Lock()
	started := client.started
	cmd := client.cmd
	stdin := client.stdin
	pending := len(client.pending)
	client.mu.Unlock()
	if started || cmd != nil || stdin != nil || pending != 0 {
		t.Fatalf("client state after failed Start: started=%t cmd_nil=%t stdin_nil=%t pending=%d", started, cmd == nil, stdin == nil, pending)
	}
	if _, requestErr := client.Request(context.Background(), "thread/list", nil); requestErr == nil || !strings.Contains(requestErr.Error(), "not running") {
		t.Fatalf("Request after failed Start error = %v, want not running", requestErr)
	}
}

func writeFakeAppServer(t *testing.T, root, body string) string {
	t.Helper()
	path := filepath.Join(root, "fake-codex")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("WriteFile(fake app-server) failed: %v", err)
	}
	return path
}
