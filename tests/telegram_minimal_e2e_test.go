package tests

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/config"
	"github.com/alclssna33/codex_to_telegram/internal/daemon"
	"github.com/alclssna33/codex_to_telegram/internal/model"
	"github.com/alclssna33/codex_to_telegram/internal/securestore"
	"github.com/alclssna33/codex_to_telegram/internal/storage"
	"github.com/alclssna33/codex_to_telegram/internal/telegram"
	"github.com/alclssna33/codex_to_telegram/internal/transcription"
)

const task11SecretMarker = "task11-private-marker-7f3d"
const task11FinalMarker = "task11-final-marker-b49c"
const task11ApprovalFinalMarker = "approval-finished-marker"
const minimalRestartE2EChildEnv = "CTR_GO_MINIMAL_RESTART_E2E_CHILD"
const minimalRestartE2EBaseEnv = "CTR_GO_MINIMAL_RESTART_E2E_BASE"

func init() {
	if os.Getenv("CTR_GO_TASK11_FAKE_APPSERVER") == "1" {
		fakeAppServerMain()
		os.Exit(0)
	}
}

func TestMinimalBridgeExistingThreadContinuation(t *testing.T) {
	h := newRegisteredBridgeHarness(t)
	h.selectProjectByLabel("Project One")
	actions := h.waitOne("작업폴더: Project One")
	h.callback(actions.MessageID, "기존 대화 열기")
	list := h.waitOne("기존 대화를 선택하세요.")
	h.callback(list.MessageID, "Existing Alpha")
	h.waitOne("최근 요약")

	threadStartsBefore := h.app.threadStarts()
	turnStartsBefore := h.app.turnStarts()
	response := h.text("continue selected existing conversation", 0)
	if response.ThreadID != "thread-p1-existing" {
		t.Fatalf("plain text thread = %s, want selected existing thread", response.ThreadID)
	}
	if got := h.app.threadStarts(); got != threadStartsBefore {
		t.Fatalf("plain text created a new thread: starts=%d before=%d", got, threadStartsBefore)
	}
	if got := h.app.turnStarts(); got != turnStartsBefore+1 || h.app.lastTurnStartThread() != "thread-p1-existing" {
		t.Fatalf("turn/start did not continue selected thread: starts=%d before=%d last=%q", got, turnStartsBefore, h.app.lastTurnStartThread())
	}
	h.noSecret("existing-alpha-summary-marker")
}

func TestMinimalBridgeRegisteredObserverAcrossRestart(t *testing.T) {
	runMinimalBridgeRestartE2ESubprocess(t, "registered-observer", func(t *testing.T, root string) {
		runMinimalBridgeRegisteredObserverAcrossRestart(t, root)
	})
}

func runMinimalBridgeRegisteredObserverAcrossRestart(t *testing.T, root string) {
	h := newRegisteredBridgeHarnessAtRoot(t, root)
	h.selectProjectByLabel("Project One")
	h.app.waitThreadRead("thread-p2-active")
	h.restart()

	final := strings.Repeat("result ", 1200) + "registered-observer-final-marker"
	h.app.complete("thread-p2-active", "turn-p2-active", final)
	finals := h.waitText("[완료] Project Two")
	assertFullReassembledFinalWithHeader(t, finals, "[완료] Project Two\n대화: Observer Beta\nThread: thread-p\n\n", final)
	if got := len(h.tg.findAll("[완료] Project Two")); got != 1 {
		t.Fatalf("Project Two terminal groups = %d, want 1", got)
	}
	h.noSecret("registered-observer-final-marker")
	h.closeAndAssertStoreReusable()
}

func TestMinimalBridgeEndToEndAcrossRestart(t *testing.T) {
	runMinimalBridgeRestartE2ESubprocess(t, "end-to-end", func(t *testing.T, root string) {
		runMinimalBridgeEndToEndAcrossRestart(t, root)
	})
}

func runMinimalBridgeEndToEndAcrossRestart(t *testing.T, root string) {
	h := newBridgeHarnessAtRoot(t, root)
	h.selectProject()
	start := h.text("run tests", 0)
	h.app.waitThreadRead(start.ThreadID)
	time.Sleep(100 * time.Millisecond)
	final := strings.Repeat("result ", 1200) + task11FinalMarker
	h.app.complete(start.ThreadID, start.TurnID, final)
	h.tg.failNext(http.StatusBadGateway)
	h.waitFailedSends(1)
	h.restart()
	finals := h.waitText("[완료]")
	assertFullReassembledFinal(t, finals, final)
	assertNoDuplicateTerminal(t, finals, 3)
	if h.tg.failedSends() != 1 {
		t.Fatalf("injected terminal 502 count = %d, want 1", h.tg.failedSends())
	}
	startsBefore := h.app.turnStarts()
	followup := h.text("fix the failure", finals[0].MessageID)
	if followup.ThreadID != start.ThreadID {
		t.Fatalf("thread changed: %s", followup.ThreadID)
	}
	if got := h.app.turnStarts(); got != startsBefore+1 || h.app.lastTurnStartThread() != start.ThreadID {
		t.Fatalf("reply turn/start did not target original thread: starts=%d before=%d last=%q want=%q", got, startsBefore, h.app.lastTurnStartThread(), start.ThreadID)
	}
	h.app.waitThreadTurns(start.ThreadID, []fakeTurnExpectation{
		{ID: start.TurnID, Status: "completed"},
		{ID: followup.TurnID, Status: "inProgress"},
	})

	queuedBefore := h.app.turnStarts()
	queued := h.textQueued("need approval", 0)
	if queued.ThreadID != start.ThreadID {
		t.Fatalf("queued approval thread = %s, want %s", queued.ThreadID, start.ThreadID)
	}
	if queued.TurnID != followup.TurnID {
		t.Fatalf("queued approval turn = %s, want active %s", queued.TurnID, followup.TurnID)
	}
	h.app.complete(followup.ThreadID, followup.TurnID, "fixed")
	afterDrain := h.app.waitTurnStarts(queuedBefore + 1)
	if got := len(afterDrain.TurnStarts); got != queuedBefore+1 || h.app.lastTurnStartThread() != start.ThreadID {
		t.Fatalf("queued approval did not drain as exactly one turn/start: starts=%d before=%d last=%q want=%q", got, queuedBefore, h.app.lastTurnStartThread(), start.ThreadID)
	}
	h.app.waitThreadTurns(start.ThreadID, []fakeTurnExpectation{
		{ID: start.TurnID, Status: "completed"},
		{ID: followup.TurnID, Status: "completed"},
		{ID: afterDrain.Turn, Status: "waitingOnApproval"},
	})
	approvalCard := h.waitOne("요청: 명령 실행")
	h.callback(approvalCard.MessageID, "승인")
	h.waitAppApproved()
	if afterDrain.Thread != start.ThreadID {
		t.Fatalf("approval thread = %s, want %s", afterDrain.Thread, start.ThreadID)
	}
	h.waitOne(task11ApprovalFinalMarker)
	h.app.waitThreadTurns(start.ThreadID, []fakeTurnExpectation{
		{ID: start.TurnID, Status: "completed"},
		{ID: followup.TurnID, Status: "completed"},
		{ID: afterDrain.Turn, Status: "completed"},
	})

	cancel := h.voice(task11SecretMarker)
	turnsBeforeCancel := h.app.turnStarts()
	h.callback(cancel, "취소")
	if got := h.app.turnStarts(); got != turnsBeforeCancel {
		t.Fatalf("voice cancel started a turn: before %d after %d", turnsBeforeCancel, got)
	}
	execute := h.voice(task11SecretMarker)
	h.callback(execute, "실행")
	h.waitAppPrompt(task11SecretMarker)
	h.noSecret(task11SecretMarker, task11FinalMarker)
	h.closeAndAssertStoreReusable()
}

func TestFakeAppStateWriteDoesNotExposePartialJSON(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "appserver.json")
	initial := fakeState{CWD: "project", Thread: "thread-0", Turn: "turn-0", Final: "old"}
	if err := writeFakeStateFile(path, initial); err != nil {
		t.Fatal(err)
	}

	paused := make(chan struct{})
	release := make(chan struct{})
	fakeStateWriteTestHook = func(phase fakeStateWritePhase, hookPath string) {
		if phase != fakeStateWriteBeforeReplace || hookPath != path {
			return
		}
		close(paused)
		<-release
	}
	t.Cleanup(func() { fakeStateWriteTestHook = nil })

	done := make(chan error, 1)
	go func() {
		done <- writeStateMain(path, fakeState{
			CWD:        "project",
			Thread:     "thread-1",
			Turn:       "turn-1",
			Status:     "inProgress",
			Final:      strings.Repeat("child-final", 4096),
			Prompt:     "child prompt",
			Next:       1,
			TurnStarts: []string{"thread-1"},
		})
	}()

	select {
	case <-paused:
	case err := <-done:
		t.Fatalf("write finished before replace hook: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("write did not reach replace hook")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Fatalf("paused fake App Server state is invalid JSON: %q", string(data))
	}
	var observed fakeState
	if err := json.Unmarshal(data, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Final != initial.Final {
		t.Fatalf("paused write exposed new state before replace: final=%q want %q", observed.Final, initial.Final)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	final, err := readFakeStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if final.Final != strings.Repeat("child-final", 4096) {
		t.Fatal("final fake App Server state was not persisted")
	}
}

func TestWaitTextCollectsOnlyMatchingNumberedTerminalGroup(t *testing.T) {
	h := &bridgeHarness{t: t, tg: &fakeTelegram{t: t}}
	h.tg.messages = []fakeMessage{
		{MessageID: 101, Text: "[1/1]\n[완료] Project Two\n대화: Other\nThread: thread-2\n\nother-final"},
		{MessageID: 102, Text: "[1/3]\n[완료] Project One\n대화: Project One\nThread: thread-1\n\npart-one"},
		{MessageID: 103, Text: "[2/3]\npart-two"},
		{MessageID: 104, Text: "[3/3]\npart-three"},
		{MessageID: 105, Text: "[1/1]\n[완료] Project Three\n대화: Later\nThread: thread-3\n\nlater-final"},
	}

	got := h.waitText("[완료] Project One")
	gotIDs := make([]int64, 0, len(got))
	for _, message := range got {
		gotIDs = append(gotIDs, message.MessageID)
	}
	wantIDs := []int64{102, 103, 104}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("terminal group message ids = %v, want %v", gotIDs, wantIDs)
	}
}

func TestWaitTextRejectsInterleavedDuplicateTerminalGroups(t *testing.T) {
	h := &bridgeHarness{t: t, tg: &fakeTelegram{t: t}}
	h.tg.messages = []fakeMessage{
		{MessageID: 201, Text: "[1/3]\n[완료] Project One\n대화: Project One\nThread: thread-1\n\nstale-part-one"},
		{MessageID: 202, Text: "[1/3]\n[완료] Project Two\n대화: Project Two\nThread: thread-2\n\nforeign-part-one"},
		{MessageID: 203, Text: "[2/3]\nforeign-part-two"},
		{MessageID: 204, Text: "[3/3]\nforeign-part-three"},
		{MessageID: 205, Text: "[1/3]\n[완료] Project One\n대화: Project One\nThread: thread-1\n\nfresh-part-one"},
		{MessageID: 206, Text: "[2/3]\nfresh-part-two"},
		{MessageID: 207, Text: "[3/3]\nfresh-part-three"},
	}

	got := h.waitText("[완료] Project One")
	gotIDs := make([]int64, 0, len(got))
	for _, message := range got {
		gotIDs = append(gotIDs, message.MessageID)
	}
	wantIDs := []int64{205, 206, 207}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("terminal group message ids = %v, want contiguous target group %v", gotIDs, wantIDs)
	}
	assertNoDuplicateTerminal(t, got, 3)
}

func TestWaitTextFailsOnDuplicateContiguousTerminalGroups(t *testing.T) {
	if os.Getenv("CTR_GO_WAIT_TEXT_DUPLICATE_HELPER") == "1" {
		h := &bridgeHarness{t: t, tg: &fakeTelegram{t: t}}
		h.tg.messages = []fakeMessage{
			{MessageID: 301, Text: "[1/3]\n[완료] Project One\n대화: Project One\nThread: thread-1\n\npart-one"},
			{MessageID: 302, Text: "[2/3]\npart-two"},
			{MessageID: 303, Text: "[3/3]\npart-three"},
			{MessageID: 304, Text: "[1/3]\n[완료] Project One\n대화: Project One\nThread: thread-1\n\npart-one"},
			{MessageID: 305, Text: "[2/3]\npart-two"},
			{MessageID: 306, Text: "[3/3]\npart-three"},
		}
		_ = h.waitText("[완료] Project One")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestWaitTextFailsOnDuplicateContiguousTerminalGroups$", "-test.v")
	cmd.Env = append(os.Environ(), "CTR_GO_WAIT_TEXT_DUPLICATE_HELPER=1")
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("duplicate helper exceeded deadline: %v\n%s", ctx.Err(), output)
	}
	if err == nil {
		t.Fatalf("duplicate helper succeeded; want waitText duplicate failure\n%s", output)
	}
	if !strings.Contains(string(output), "duplicate terminal") {
		t.Fatalf("duplicate helper failed for wrong reason; want duplicate terminal failure\n%s", output)
	}
}

func runMinimalBridgeRestartE2ESubprocess(t *testing.T, scenario string, run func(*testing.T, string)) {
	t.Helper()
	if os.Getenv(minimalRestartE2EChildEnv) == scenario {
		base := os.Getenv(minimalRestartE2EBaseEnv)
		if strings.TrimSpace(base) == "" {
			t.Fatalf("%s is required in child process", minimalRestartE2EBaseEnv)
		}
		root, err := os.MkdirTemp(base, scenario+"-")
		if err != nil {
			t.Fatalf("create child scenario root: %v", err)
		}
		run(t, root)
		return
	}

	base := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^"+t.Name()+"$", "-test.v")
	cmd.Env = append(os.Environ(),
		minimalRestartE2EChildEnv+"="+scenario,
		minimalRestartE2EBaseEnv+"="+base,
	)
	output, err := cmd.CombinedOutput()
	cleanupErr := os.RemoveAll(base)
	if ctx.Err() != nil {
		t.Fatalf("restart E2E child exceeded deadline: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("restart E2E child failed: %v\n%s", err, output)
	}
	if cleanupErr != nil {
		t.Fatalf("parent cleanup after restart E2E child failed: %v\n%s", cleanupErr, output)
	}
}

type bridgeHarness struct {
	t          *testing.T
	ctx        context.Context
	cancel     context.CancelFunc
	cfg        config.Config
	service    *daemon.Service
	bot        *telegram.Bot
	tg         *fakeTelegram
	app        fakeAppControl
	logs       bytes.Buffer
	daemonLogs bytes.Buffer
}

func newBridgeHarness(t *testing.T) *bridgeHarness {
	root := t.TempDir()
	return newBridgeHarnessAtRoot(t, root)
}

func newBridgeHarnessAtRoot(t *testing.T, root string) *bridgeHarness {
	project := filepath.Join(root, "project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	return newBridgeHarnessWithState(t, root, fakeState{CWD: project, ReuseThreadStartID: true}, []model.Project{{ID: "p1", DisplayName: "Project One", CanonicalPath: project}}, project)
}

func newRegisteredBridgeHarness(t *testing.T) *bridgeHarness {
	root := t.TempDir()
	return newRegisteredBridgeHarnessAtRoot(t, root)
}

func newRegisteredBridgeHarnessAtRoot(t *testing.T, root string) *bridgeHarness {
	projectOne := filepath.Join(root, "project-one")
	projectTwo := filepath.Join(root, "project-two")
	if err := os.Mkdir(projectOne, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(projectTwo, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Unix()
	state := fakeState{
		CWD: projectOne,
		Threads: []fakeThreadState{
			{
				ID:        "thread-p1-existing",
				CWD:       projectOne,
				Title:     "Existing Alpha",
				Turn:      "turn-p1-existing",
				Status:    "completed",
				Final:     "existing-alpha-summary-marker",
				UpdatedAt: now - 10,
			},
			{
				ID:        "thread-p2-active",
				CWD:       projectTwo,
				Title:     "Observer Beta",
				Turn:      "turn-p2-active",
				Status:    "inProgress",
				UpdatedAt: now - 5,
			},
		},
	}
	projects := []model.Project{
		{ID: "p1", DisplayName: "Project One", CanonicalPath: projectOne},
		{ID: "p2", DisplayName: "Project Two", CanonicalPath: projectTwo},
	}
	return newBridgeHarnessWithState(t, root, state, projects, projectOne)
}

func newBridgeHarnessWithState(t *testing.T, root string, initial fakeState, projects []model.Project, defaultCWD string) *bridgeHarness {
	state := filepath.Join(root, "appserver.json")
	events := filepath.Join(root, "appserver-events.log")
	saveState(t, state, initial)
	t.Setenv("CTR_GO_TASK11_FAKE_APPSERVER", "1")
	t.Setenv("CTR_GO_TASK11_FAKE_APPSERVER_STATE", state)
	t.Setenv("CTR_GO_TASK11_FAKE_APPSERVER_EVENTS", events)
	ctx, cancel := context.WithCancel(context.Background())
	h := &bridgeHarness{t: t, ctx: ctx, cancel: cancel, app: fakeAppControl{t: t, path: state, eventsPath: events}, cfg: config.Config{Paths: config.Paths{Home: filepath.Join(root, "bridge"), DataDir: filepath.Join(root, "bridge", "data"), LogDir: filepath.Join(root, "bridge", "logs"), DBPath: filepath.Join(root, "bridge", "data", "state.sqlite")}, Profile: "minimal", Projects: projects, TelegramBotToken: "test-token", AllowedUserIDs: []int64{1}, AllowedChatIDs: []int64{1}, CodexBin: os.Args[0], AppServerListen: "stdio://", DefaultCWD: defaultCWD, CommandMaxAge: time.Minute, ObserverPollInterval: 25 * time.Millisecond, RequestTimeout: 2 * time.Second, IndexRefreshInterval: time.Hour, AttachRefreshInterval: time.Hour, DeliveryRetryBase: 25 * time.Millisecond, DeliveryMaxAttempts: 3}}
	h.tg = newFakeTelegram(t)
	h.start()
	deadline := time.Now().Add(2 * time.Second)
	for !h.app.get().Ready && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !h.app.get().Ready {
		t.Fatal("fake App Server did not initialize")
	}
	t.Cleanup(func() {
		cancel()
		if h.service != nil {
			_ = h.service.Close()
		}
		h.tg.close()
	})
	return h
}
func (h *bridgeHarness) start() {
	s, err := daemon.New(h.cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	s.SetVoiceTranscriber(fakeTranscriber{})
	s.SetLogger(log.New(&h.daemonLogs, "", 0))
	b, err := telegram.NewBotWithClient(h.cfg, s, log.New(&h.logs, "", 0), telegram.NewClientWithBaseURLs(h.cfg.TelegramBotToken, h.tg.url(), h.tg.url()))
	if err != nil {
		h.t.Fatal(err)
	}
	s.SetSender(b)
	if err = s.Start(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	h.service, h.bot = s, b
}
func (h *bridgeHarness) restart() {
	h.closeAndAssertStoreReusable()
	h.start()
}

func (h *bridgeHarness) closeAndAssertStoreReusable() {
	if h.service == nil {
		h.waitStoreReusable()
		return
	}
	if err := h.service.Close(); err != nil {
		h.t.Fatal(err)
	}
	h.service = nil
	h.bot = nil
	h.waitStoreReusable()
}

func (h *bridgeHarness) waitStoreReusable() {
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		err := assertSQLiteFilesRenameable(h.cfg.Paths.DBPath)
		if err != nil {
			lastErr = err
			time.Sleep(20 * time.Millisecond)
			continue
		}
		probe, err := storage.OpenWithProtector(h.cfg.Paths.DBPath, securestore.NewDPAPIProtector())
		if err == nil {
			err = probe.Close()
			if err == nil {
				return
			}
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("store did not become reusable after daemon close: %v", lastErr)
}

func assertSQLiteFilesRenameable(dbPath string) error {
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		probePath := path + ".close-probe"
		if err := os.Rename(path, probePath); err != nil {
			return fmt.Errorf("rename %s for close probe: %w", filepath.Base(path), err)
		}
		if err := os.Rename(probePath, path); err != nil {
			return fmt.Errorf("restore %s after close probe: %w", filepath.Base(path), err)
		}
	}
	return nil
}

func (h *bridgeHarness) update(u telegram.Update) {
	if err := h.bot.HandleUpdate(h.ctx, u); err != nil {
		h.t.Fatal(err)
	}
}
func (h *bridgeHarness) inbound(text string, reply int64) *telegram.Message {
	m := &telegram.Message{MessageID: h.tg.inbound(), Date: time.Now().Unix(), From: &telegram.User{ID: 1}, Chat: telegram.Chat{ID: 1}, Text: text}
	if reply != 0 {
		m.ReplyToMessage = &telegram.Message{MessageID: reply}
	}
	return m
}
func (h *bridgeHarness) selectProject() {
	h.selectProjectByLabel("Project One")
}
func (h *bridgeHarness) selectProjectByLabel(label string) {
	h.update(telegram.Update{UpdateID: h.tg.update(), Message: h.inbound("/start", 0)})
	p := h.waitOne("작업폴더를 선택하세요.")
	h.callback(p.MessageID, label)
}
func (h *bridgeHarness) text(text string, reply int64) *daemon.DirectResponse {
	before := h.tg.count()
	turnStartsBefore := h.app.turnStarts()
	h.update(telegram.Update{UpdateID: h.tg.update(), Message: h.inbound(text, reply)})
	h.tg.waitAfter(before)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		response := &daemon.DirectResponse{ThreadID: h.app.thread(), TurnID: h.app.turn()}
		if response.ThreadID != "" && response.TurnID != "" && h.app.turnStarts() > turnStartsBefore {
			return response
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("text %q did not start a fake App Server turn; state=%+v messages=%q daemon_logs=%q", text, h.app.get(), h.tg.texts(), h.daemonLogs.String())
	return nil
}
func (h *bridgeHarness) textQueued(text string, reply int64) *daemon.DirectResponse {
	before := h.tg.count()
	turnStartsBefore := h.app.turnStarts()
	h.update(telegram.Update{UpdateID: h.tg.update(), Message: h.inbound(text, reply)})
	message := h.tg.waitAfter(before)
	if strings.Contains(message.Text, "Request failed inside the local Go bridge") {
		h.t.Fatalf("text %q returned generic bridge failure instead of queue response: %q; state=%+v daemon_logs=%q", text, message.Text, h.app.get(), h.daemonLogs.String())
	}
	if !strings.Contains(message.Text, "대기열") {
		h.t.Fatalf("text %q response = %q, want successful queue response; state=%+v daemon_logs=%q", text, message.Text, h.app.get(), h.daemonLogs.String())
	}
	if got := h.app.turnStarts(); got != turnStartsBefore {
		h.t.Fatalf("text %q started a parallel turn: starts=%d before=%d last=%q", text, got, turnStartsBefore, h.app.lastTurnStartThread())
	}
	return &daemon.DirectResponse{ThreadID: h.app.thread(), TurnID: h.app.turn()}
}
func (h *bridgeHarness) voice(value string) int64 {
	h.tg.voice(value)
	before := h.tg.count()
	h.update(telegram.Update{UpdateID: h.tg.update(), Message: &telegram.Message{MessageID: h.tg.inbound(), Date: time.Now().Unix(), From: &telegram.User{ID: 1}, Chat: telegram.Chat{ID: 1}, Voice: &telegram.Voice{FileID: "voice-1", FileSize: 5, Duration: 1, MimeType: "audio/ogg"}}})
	return h.tg.waitAfter(before).MessageID
}
func (h *bridgeHarness) callback(id int64, label string) {
	token := h.tg.button(id, label)
	if token == "" {
		h.t.Fatalf("button %q missing", label)
	}
	h.update(telegram.Update{UpdateID: h.tg.update(), CallbackQuery: &telegram.CallbackQuery{ID: "callback", From: &telegram.User{ID: 1}, Message: &telegram.Message{MessageID: id, Chat: telegram.Chat{ID: 1}}, Data: token}})
}
func (h *bridgeHarness) waitAppApproved() {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.app.approved() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("approval did not reach fake App Server; state=%+v messages=%q daemon_logs=%q", h.app.get(), h.tg.texts(), h.daemonLogs.String())
}
func (h *bridgeHarness) waitAppPrompt(value string) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.app.hasPrompt(value) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("voice execution did not create a Codex turn; state=%+v messages=%q daemon_logs=%q", h.app.get(), h.tg.texts(), h.daemonLogs.String())
}
func (h *bridgeHarness) waitOne(text string) fakeMessage {
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if m, ok := h.tg.find(text); ok {
			return m
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("missing %q; messages=%q daemon_logs=%q bot_logs=%q", text, h.tg.texts(), h.daemonLogs.String(), h.logs.String())
	return fakeMessage{}
}
func (h *bridgeHarness) waitText(text string) []fakeMessage {
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		var groups [][]fakeMessage
		for _, first := range h.tg.findAll(text) {
			if want := terminalChunkCount(first.Text); want > 0 {
				if group := h.tg.terminalGroup(first, want); len(group) == want {
					groups = append(groups, group)
				}
			}
		}
		if len(groups) > 1 {
			h.t.Fatalf("duplicate terminal %q; groups=%v messages=%q daemon_logs=%q bot_logs=%q", text, terminalGroupIDs(groups), h.tg.texts(), h.daemonLogs.String(), h.logs.String())
		}
		if len(groups) == 1 {
			return groups[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("missing terminal %q; messages=%q daemon_logs=%q bot_logs=%q", text, h.tg.texts(), h.daemonLogs.String(), h.logs.String())
	return nil
}

func terminalGroupIDs(groups [][]fakeMessage) [][]int64 {
	out := make([][]int64, 0, len(groups))
	for _, group := range groups {
		ids := make([]int64, 0, len(group))
		for _, message := range group {
			ids = append(ids, message.MessageID)
		}
		out = append(out, ids)
	}
	return out
}

func terminalChunkCount(text string) int {
	index, total, ok := terminalChunkLabel(text)
	if !ok || index != 1 || total <= 0 {
		return 0
	}
	return total
}

func terminalChunkLabel(text string) (int, int, bool) {
	var index, total int
	if _, err := fmt.Sscanf(text, "[%d/%d]\n", &index, &total); err != nil {
		return 0, 0, false
	}
	return index, total, true
}

func (h *bridgeHarness) noSecret(markers ...string) {
	data, err := os.ReadFile(h.cfg.Paths.DBPath)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, marker := range markers {
		if bytes.Contains(data, []byte(marker)) {
			h.t.Fatal("SQLite contains plaintext marker")
		}
		if strings.Contains(h.logs.String(), marker) || strings.Contains(h.daemonLogs.String(), marker) {
			h.t.Fatal("captured logs contain plaintext marker")
		}
	}
}
func assertFullReassembledFinal(t *testing.T, ms []fakeMessage, want string) {
	assertFullReassembledFinalWithHeader(t, ms, "[완료] Project One\n대화: Project One\nThread: thread-1\n\n", want)
}
func assertFullReassembledFinalWithHeader(t *testing.T, ms []fakeMessage, header, want string) {
	t.Helper()
	var b strings.Builder
	for i, m := range ms {
		label := fmt.Sprintf("[%d/%d]\n", i+1, len(ms))
		if !strings.HasPrefix(m.Text, label) {
			t.Fatalf("chunk %d has wrong label", i+1)
		}
		body := strings.TrimPrefix(m.Text, label)
		if i == 0 {
			if !strings.HasPrefix(body, header) {
				t.Fatalf("first terminal header mismatch: got %q want prefix %q", body, header)
			}
			body = strings.TrimPrefix(body, header)
		}
		b.WriteString(body)
	}
	if got := b.String(); got != want {
		t.Fatalf("final reassembly mismatch: got %d want %d", len(got), len(want))
	}
}
func assertNoDuplicateTerminal(t *testing.T, ms []fakeMessage, count int) {
	if len(ms) != count {
		t.Fatalf("terminal group has %d messages, want %d", len(ms), count)
	}
	for i, m := range ms {
		count := strings.Count(m.Text, "[완료]")
		if i == 0 && count != 1 || i > 0 && count != 0 {
			t.Fatalf("duplicate terminal group marker in chunk %d", i+1)
		}
	}
}

type fakeTranscriber struct{}

func (fakeTranscriber) Transcribe(_ context.Context, r io.Reader, _ transcription.Meta) (string, error) {
	b, _ := io.ReadAll(r)
	return string(b), nil
}

type fakeState struct {
	Ready              bool              `json:"ready"`
	CWD                string            `json:"cwd"`
	Thread             string            `json:"thread"`
	Turn               string            `json:"turn"`
	Status             string            `json:"status"`
	Final              string            `json:"final"`
	Prompt             string            `json:"prompt"`
	Approval           bool              `json:"approval"`
	Approved           bool              `json:"approved"`
	Next               int               `json:"next"`
	TurnStarts         []string          `json:"turn_starts"`
	ThreadStarts       []string          `json:"thread_starts"`
	ThreadReads        []string          `json:"thread_reads"`
	ThreadListCalls    int               `json:"thread_list_calls"`
	ReuseThreadStartID bool              `json:"reuse_thread_start_id"`
	Threads            []fakeThreadState `json:"threads"`
}

type fakeThreadState struct {
	ID        string          `json:"id"`
	CWD       string          `json:"cwd"`
	Title     string          `json:"title"`
	Turn      string          `json:"turn"`
	Status    string          `json:"status"`
	Final     string          `json:"final"`
	Prompt    string          `json:"prompt"`
	Approval  bool            `json:"approval"`
	Approved  bool            `json:"approved"`
	UpdatedAt int64           `json:"updated_at"`
	Turns     []fakeTurnState `json:"turns,omitempty"`
}

type fakeTurnState struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Final     string `json:"final"`
	Prompt    string `json:"prompt"`
	Approval  bool   `json:"approval"`
	Approved  bool   `json:"approved"`
	UpdatedAt int64  `json:"updated_at"`
}

type fakeTurnExpectation struct {
	ID     string
	Status string
}

type fakeAppControl struct {
	t          *testing.T
	path       string
	eventsPath string
}

func (c fakeAppControl) get() fakeState {
	return c.getEventually()
}
func (c fakeAppControl) tryGet() (fakeState, error) {
	return readFakeStateFile(c.path)
}
func (c fakeAppControl) getEventually() fakeState {
	c.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		s, err := c.tryGet()
		if err == nil {
			return s
		}
		if !fakeStateReadRetryable(err) {
			c.t.Fatal(err)
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatal(lastErr)
	return fakeState{}
}
func (c fakeAppControl) put(s fakeState) { saveState(c.t, c.path, s) }
func (c fakeAppControl) complete(thread, turn, final string) {
	s := c.getEventually()
	s.completeThread(thread, turn, final)
	c.put(s)
}
func (c fakeAppControl) thread() string          { return c.get().Thread }
func (c fakeAppControl) turn() string            { return c.get().Turn }
func (c fakeAppControl) approved() bool          { return c.get().Approved }
func (c fakeAppControl) hasPrompt(v string) bool { return strings.Contains(c.get().Prompt, v) }
func (c fakeAppControl) turnStarts() int         { return len(c.get().TurnStarts) }
func (c fakeAppControl) threadStarts() int       { return len(c.get().ThreadStarts) }
func (c fakeAppControl) lastTurnStartThread() string {
	s := c.get()
	if len(s.TurnStarts) == 0 {
		return ""
	}
	return s.TurnStarts[len(s.TurnStarts)-1]
}
func (c fakeAppControl) waitThreadListCalls(want int) {
	c.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		count, err := c.eventCount("thread/list", "")
		if err == nil && count >= want {
			return
		}
		if err != nil && !fakeStateReadRetryable(err) {
			c.t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatalf("thread/list calls did not reach %d", want)
}
func (c fakeAppControl) waitThreadRead(threadID string) {
	c.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		count, err := c.eventCount("thread/read", threadID)
		if err == nil && count > 0 {
			return
		}
		if err != nil && !fakeStateReadRetryable(err) {
			c.t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatalf("thread/read did not reach %s", threadID)
}

func (c fakeAppControl) waitTurnStarts(want int) fakeState {
	c.t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		s := c.get()
		if len(s.TurnStarts) >= want {
			return s
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatalf("turn/start count = %d, want at least %d; state=%+v", c.turnStarts(), want, c.get())
	return fakeState{}
}

func (c fakeAppControl) waitThreadTurns(threadID string, want []fakeTurnExpectation) {
	c.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var got []fakeTurnExpectation
	for time.Now().Before(deadline) {
		s := c.get()
		if record, ok := s.threadByID(threadID); ok {
			got = fakeTurnExpectations(record.Turns)
			if reflect.DeepEqual(got, want) {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatalf("thread %s turns = %#v, want %#v; state=%+v", threadID, got, want, c.get())
}

func (c fakeAppControl) eventCount(method, threadID string) (int, error) {
	lines, err := readFakeEventLines(c.eventsPath)
	if err != nil {
		return 0, err
	}
	want := method + "\t" + threadID
	count := 0
	for _, line := range lines {
		if line == want {
			count++
		}
	}
	return count, nil
}

func fakeStateReadRetryable(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "being used by another process") ||
		strings.Contains(message, "access is denied")
}
func saveState(t *testing.T, path string, s fakeState) {
	t.Helper()
	if err := writeFakeStateFile(path, s); err != nil {
		t.Fatal(err)
	}
}

func (s *fakeState) completeThread(thread, turn, final string) {
	if record, ok := s.threadByID(thread); ok {
		now := time.Now().UTC().Unix()
		record.Turn = turn
		record.Status = "completed"
		record.Final = final
		record.Approval = false
		record.UpdatedAt = now
		record.upsertTurn(fakeTurnState{
			ID:        turn,
			Status:    "completed",
			Final:     final,
			Prompt:    record.Prompt,
			Approved:  record.Approved,
			UpdatedAt: now,
		})
		s.syncCurrentThread(*record)
		return
	}
	s.Thread, s.Turn, s.Status, s.Final = thread, turn, "completed", final
}

func (s *fakeState) resolveCurrentApprovalTurn() (string, string) {
	s.Approved = true
	record, ok := s.threadByID(s.Thread)
	if !ok {
		return "", ""
	}
	now := time.Now().UTC().Unix()
	record.Approved = true
	record.Approval = false
	final := record.Final
	completedThreadID := ""
	completedTurnID := ""
	if strings.Contains(record.Prompt, "need approval") {
		record.Status = "completed"
		record.Final = task11ApprovalFinalMarker
		final = task11ApprovalFinalMarker
		completedThreadID = record.ID
		completedTurnID = record.Turn
	}
	record.upsertTurn(fakeTurnState{
		ID:        record.Turn,
		Status:    record.Status,
		Prompt:    record.Prompt,
		Final:     final,
		Approved:  true,
		UpdatedAt: now,
	})
	s.syncCurrentThread(*record)
	return completedThreadID, completedTurnID
}

func (s *fakeState) threadByID(threadID string) (*fakeThreadState, bool) {
	for i := range s.Threads {
		if s.Threads[i].ID == threadID {
			return &s.Threads[i], true
		}
	}
	return nil, false
}

func (s *fakeState) upsertThread(thread fakeThreadState) {
	for i := range s.Threads {
		if s.Threads[i].ID == thread.ID {
			s.Threads[i] = thread
			return
		}
	}
	s.Threads = append(s.Threads, thread)
}

func (s *fakeState) syncCurrentThread(thread fakeThreadState) {
	s.Thread = thread.ID
	s.Turn = thread.Turn
	s.CWD = thread.CWD
	s.Status = thread.Status
	s.Final = thread.Final
	s.Prompt = thread.Prompt
	s.Approval = thread.Approval
	s.Approved = thread.Approved
}

func (thread *fakeThreadState) upsertTurn(turn fakeTurnState) {
	if turn.UpdatedAt == 0 {
		turn.UpdatedAt = time.Now().UTC().Unix()
	}
	for i := range thread.Turns {
		if thread.Turns[i].ID == turn.ID {
			thread.Turns[i] = mergeFakeTurn(thread.Turns[i], turn)
			return
		}
	}
	thread.Turns = append(thread.Turns, turn)
}

func mergeFakeTurn(existing, update fakeTurnState) fakeTurnState {
	if update.Status == "" {
		update.Status = existing.Status
	}
	if update.Final == "" {
		update.Final = existing.Final
	}
	if update.Prompt == "" {
		update.Prompt = existing.Prompt
	}
	if !update.Approval {
		update.Approval = existing.Approval && update.Status != "completed"
	}
	if !update.Approved {
		update.Approved = existing.Approved
	}
	if update.UpdatedAt == 0 {
		update.UpdatedAt = existing.UpdatedAt
	}
	return update
}

func fakeTurnExpectations(turns []fakeTurnState) []fakeTurnExpectation {
	out := make([]fakeTurnExpectation, 0, len(turns))
	for _, turn := range turns {
		out = append(out, fakeTurnExpectation{ID: turn.ID, Status: fakeTurnStatus(turn)})
	}
	return out
}

type fakeStateWritePhase int

const fakeStateWriteBeforeReplace fakeStateWritePhase = iota

var fakeStateWriteTestHook func(fakeStateWritePhase, string)

func writeFakeStateFile(path string, s fakeState) error {
	return writeFakeStateFileWithHook(path, s, nil)
}
func writeFakeStateFileWithHook(path string, s fakeState, hook func(fakeStateWritePhase, string)) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.new")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if hook != nil {
		hook(fakeStateWriteBeforeReplace, path)
	}
	if err = renameFakeStateFile(tmpName, path); err != nil {
		return err
	}
	removeTemp = false
	return nil
}
func renameFakeStateFile(tmpName, path string) error {
	deadline := time.Now().Add(2 * time.Second)
	var err error
	for attempt := 0; ; attempt++ {
		err = os.Rename(tmpName, path)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		backoff := time.Duration(attempt+1) * time.Millisecond
		if backoff > 25*time.Millisecond {
			backoff = 25 * time.Millisecond
		}
		time.Sleep(backoff)
	}
}
func readFakeStateFile(path string) (fakeState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return fakeState{}, err
	}
	var s fakeState
	if err = json.Unmarshal(data, &s); err != nil {
		return fakeState{}, err
	}
	return s, nil
}

func readFakeEventLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

func appendFakeEvent(path, method, threadID string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	line := method + "\t" + threadID + "\n"
	deadline := time.Now().Add(2 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err == nil {
			_, err = f.WriteString(line)
			if closeErr := f.Close(); err == nil {
				err = closeErr
			}
		}
		if err == nil {
			return
		}
		if !fakeStateReadRetryable(err) || time.Now().After(deadline) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func fakeAppServerMain() {
	path := os.Getenv("CTR_GO_TASK11_FAKE_APPSERVER_STATE")
	eventsPath := os.Getenv("CTR_GO_TASK11_FAKE_APPSERVER_EVENTS")
	scan := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for scan.Scan() {
		var q struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(scan.Bytes(), &q) != nil {
			continue
		}
		s := readState(path)
		r := map[string]any{}
		approvalRequestID := ""
		completedThreadID := ""
		completedTurnID := ""
		dirty := strings.HasPrefix(fmt.Sprint(q.ID), "approval-") && q.Method == ""
		if dirty {
			completedThreadID, completedTurnID = s.resolveCurrentApprovalTurn()
		}
		switch q.Method {
		case "initialize":
			s.Ready = true
			dirty = true
		case "thread/start":
			s.Next++
			cwd := fakeStringParam(q.Params, "cwd")
			if cwd == "" {
				cwd = s.CWD
			}
			if len(s.Threads) > 0 || s.ReuseThreadStartID {
				threadID := fmt.Sprintf("thread-new-%d", s.Next)
				if s.ReuseThreadStartID && len(s.ThreadStarts) == 0 {
					threadID = "thread-1"
				}
				title := "New thread"
				if threadID == "thread-1" {
					title = "Project One"
				}
				thread := fakeThreadState{
					ID:        threadID,
					CWD:       cwd,
					Title:     title,
					Status:    "inProgress",
					UpdatedAt: time.Now().UTC().Unix(),
				}
				s.upsertThread(thread)
				s.ThreadStarts = append(s.ThreadStarts, thread.ID)
				s.syncCurrentThread(thread)
				r = map[string]any{"thread": fakeThreadListItem(thread)}
			} else {
				s.Thread = "thread-1"
				s.CWD = cwd
				s.Status = "inProgress"
				s.ThreadStarts = append(s.ThreadStarts, s.Thread)
				s.upsertThread(fakeThreadState{ID: s.Thread, CWD: s.CWD, Title: "Project One", Status: s.Status, UpdatedAt: time.Now().UTC().Unix()})
				r = map[string]any{"thread": map[string]any{"id": s.Thread, "cwd": s.CWD, "name": "Project One"}}
			}
			dirty = true
		case "turn/start":
			s.Next++
			threadID := fakeStringParam(q.Params, "threadId")
			prompt := fakePromptParam(q.Params)
			turnID := fmt.Sprintf("turn-%d", s.Next)
			approvalRequested := strings.Contains(prompt, "need approval")
			now := time.Now().UTC().Unix()
			if record, ok := s.threadByID(threadID); ok {
				record.Turn = turnID
				record.Status = "inProgress"
				record.Final = ""
				record.Prompt = prompt
				record.Approval = approvalRequested
				record.Approved = false
				record.UpdatedAt = now
				record.upsertTurn(fakeTurnState{
					ID:        turnID,
					Status:    "inProgress",
					Prompt:    prompt,
					Approval:  approvalRequested,
					UpdatedAt: now,
				})
				s.syncCurrentThread(*record)
			} else {
				s.Thread = threadID
				s.Turn = turnID
				s.Status = "inProgress"
				s.Final = ""
				s.Prompt = prompt
				s.Approval = approvalRequested
				s.Approved = false
				s.upsertThread(fakeThreadState{
					ID:        threadID,
					CWD:       s.CWD,
					Title:     "Project One",
					Turn:      turnID,
					Status:    "inProgress",
					Prompt:    prompt,
					Approval:  approvalRequested,
					UpdatedAt: now,
					Turns:     []fakeTurnState{{ID: turnID, Status: "inProgress", Prompt: prompt, Approval: approvalRequested, UpdatedAt: now}},
				})
			}
			if approvalRequested {
				approvalRequestID = "approval-" + turnID
			}
			s.TurnStarts = append(s.TurnStarts, threadID)
			dirty = true
			r = map[string]any{"turn": map[string]any{"id": turnID}}
		case "thread/read":
			threadID := fakeStringParam(q.Params, "threadId")
			appendFakeEvent(eventsPath, "thread/read", threadID)
			r = fakeThreadPayload(s, threadID)
		case "thread/list":
			appendFakeEvent(eventsPath, "thread/list", "")
			r = fakeThreadListPayload(s)
		case "thread/resume":
			if record, ok := s.threadByID(fakeStringParam(q.Params, "threadId")); ok {
				s.syncCurrentThread(*record)
				dirty = true
				r = fakeThreadPayload(s, record.ID)
			}
		case "serverRequest/resolve":
			completedThreadID, completedTurnID = s.resolveCurrentApprovalTurn()
			dirty = true
		}
		if dirty {
			if err := writeStateMain(path, s); err != nil {
				if q.ID != nil {
					_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": q.ID, "error": map[string]any{"code": -32000, "message": "failed to persist fake App Server state"}})
				}
				continue
			}
		}
		if approvalRequestID != "" {
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": approvalRequestID, "method": "item/commandExecution/requestApproval", "params": map[string]any{"threadId": s.Thread, "turnId": s.Turn, "command": "echo safe"}})
		}
		if completedThreadID != "" && completedTurnID != "" {
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "method": "turn/completed", "params": map[string]any{"threadId": completedThreadID, "turnId": completedTurnID, "status": "completed"}})
		}
		if q.ID != nil {
			_ = enc.Encode(map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": r})
		}
	}
}
func readState(path string) fakeState {
	deadline := time.Now().Add(2 * time.Second)
	for {
		s, err := readFakeStateFile(path)
		if err == nil {
			return s
		}
		if !fakeStateReadRetryable(err) || time.Now().After(deadline) {
			return fakeState{}
		}
		time.Sleep(10 * time.Millisecond)
	}
}
func writeStateMain(path string, s fakeState) error {
	return writeFakeStateFileWithHook(path, s, fakeStateWriteTestHook)
}
func fakeStringParam(params map[string]any, key string) string {
	value, _ := params[key].(string)
	return strings.TrimSpace(value)
}

func fakePromptParam(params map[string]any) string {
	if in, ok := params["input"].([]any); ok && len(in) > 0 {
		if p, ok := in[0].(map[string]any); ok {
			value, _ := p["text"].(string)
			return value
		}
	}
	return ""
}

func fakeThreadListPayload(s fakeState) map[string]any {
	if len(s.Threads) == 0 {
		if s.Thread == "" {
			return map[string]any{"data": []any{}}
		}
		return map[string]any{"data": []any{fakeThreadListItem(fakeThreadState{
			ID:        s.Thread,
			CWD:       s.CWD,
			Title:     "Project One",
			Turn:      s.Turn,
			Status:    s.Status,
			Final:     s.Final,
			UpdatedAt: time.Now().UTC().Unix(),
		})}}
	}
	items := make([]any, 0, len(s.Threads))
	for _, thread := range s.Threads {
		items = append(items, fakeThreadListItem(thread))
	}
	return map[string]any{"data": items}
}

func fakeThreadListItem(thread fakeThreadState) map[string]any {
	updatedAt := thread.UpdatedAt
	if updatedAt == 0 {
		updatedAt = time.Now().UTC().Unix()
	}
	status := fakeThreadStatusFromThread(thread)
	item := map[string]any{
		"id":        thread.ID,
		"cwd":       thread.CWD,
		"name":      thread.Title,
		"status":    status,
		"updatedAt": updatedAt,
	}
	if fakeThreadHasActiveTurn(status) && thread.Turn != "" {
		item["activeTurnId"] = thread.Turn
	}
	return item
}

func fakeThreadPayload(s fakeState, threadID string) map[string]any {
	if len(s.Threads) == 0 {
		return fakeThreadPayloadFromThread(fakeThreadState{
			ID:        s.Thread,
			CWD:       s.CWD,
			Title:     "Project One",
			Turn:      s.Turn,
			Status:    s.Status,
			Final:     s.Final,
			Prompt:    s.Prompt,
			Approval:  s.Approval,
			UpdatedAt: time.Now().UTC().Unix(),
		})
	}
	if thread, ok := s.threadByID(threadID); ok {
		return fakeThreadPayloadFromThread(*thread)
	}
	return map[string]any{}
}

func fakeThreadPayloadFromThread(thread fakeThreadState) map[string]any {
	status := fakeThreadStatusFromThread(thread)
	payload := map[string]any{
		"id":        thread.ID,
		"cwd":       thread.CWD,
		"name":      thread.Title,
		"status":    status,
		"updatedAt": thread.UpdatedAt,
		"turns":     fakeTurnPayloads(thread),
	}
	if fakeThreadHasActiveTurn(status) && thread.Turn != "" {
		payload["activeTurnId"] = thread.Turn
	}
	return payload
}

func fakeTurnPayloads(thread fakeThreadState) []any {
	turns := thread.Turns
	if len(turns) == 0 {
		turns = []fakeTurnState{{
			ID:        thread.Turn,
			Status:    thread.Status,
			Final:     thread.Final,
			Prompt:    thread.Prompt,
			Approval:  thread.Approval,
			Approved:  thread.Approved,
			UpdatedAt: thread.UpdatedAt,
		}}
	}
	out := make([]any, 0, len(turns))
	for _, turn := range turns {
		status := fakeTurnStatus(turn)
		out = append(out, map[string]any{
			"id":     turn.ID,
			"status": status,
			"items":  fakeTurnItems(turn),
		})
	}
	return out
}

func fakeTurnItems(turn fakeTurnState) []any {
	userText := strings.TrimSpace(turn.Prompt)
	if userText == "" {
		userText = "fake"
	}
	items := []any{map[string]any{"id": "user-" + turn.ID, "type": "userMessage", "text": userText}}
	if fakeTurnStatus(turn) == "completed" {
		items = append(items, map[string]any{"id": "final-" + turn.ID, "type": "agentMessage", "text": turn.Final, "phase": "final"})
	}
	return items
}

func fakeThreadStatusFromThread(thread fakeThreadState) string {
	if thread.Approval {
		return "waitingOnApproval"
	}
	return thread.Status
}

func fakeTurnStatus(turn fakeTurnState) string {
	if turn.Approval {
		return "waitingOnApproval"
	}
	return turn.Status
}

func fakeThreadHasActiveTurn(status string) bool {
	return status == "inProgress" || status == "waitingOnApproval" || status == "active"
}

type fakeMessage struct {
	MessageID int64
	Text      string
	Buttons   map[string]string
}
type fakeTelegram struct {
	t                            *testing.T
	server                       *httptest.Server
	mu                           sync.Mutex
	message, inboundID, updateID int64
	messages                     []fakeMessage
	fail                         int
	failed                       int
	voiceText                    string
}

func newFakeTelegram(t *testing.T) *fakeTelegram {
	f := &fakeTelegram{t: t, message: 100}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}
func (f *fakeTelegram) close()      { f.server.Close() }
func (f *fakeTelegram) url() string { return f.server.URL }
func (f *fakeTelegram) inbound() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inboundID++
	return f.inboundID
}
func (f *fakeTelegram) update() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateID++
	return f.updateID
}
func (f *fakeTelegram) count() int       { f.mu.Lock(); defer f.mu.Unlock(); return len(f.messages) }
func (f *fakeTelegram) failNext(n int)   { f.mu.Lock(); defer f.mu.Unlock(); f.fail = n }
func (f *fakeTelegram) failedSends() int { f.mu.Lock(); defer f.mu.Unlock(); return f.failed }
func (f *fakeTelegram) voice(v string)   { f.mu.Lock(); defer f.mu.Unlock(); f.voiceText = v }
func (h *bridgeHarness) waitFailedSends(want int) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.tg.failedSends() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("failed sends = %d, want at least %d; state=%+v messages=%q daemon_logs=%q", h.tg.failedSends(), want, h.app.get(), h.tg.texts(), h.daemonLogs.String())
}
func (f *fakeTelegram) waitAfter(n int) fakeMessage {
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		if len(f.messages) > n {
			m := f.messages[len(f.messages)-1]
			f.mu.Unlock()
			return m
		}
		f.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatal("no direct response")
	return fakeMessage{}
}
func (f *fakeTelegram) find(text string) (fakeMessage, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.messages) - 1; i >= 0; i-- {
		if strings.Contains(f.messages[i].Text, text) {
			return f.messages[i], true
		}
	}
	return fakeMessage{}, false
}
func (f *fakeTelegram) findAll(text string) []fakeMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeMessage
	for _, m := range f.messages {
		if strings.Contains(m.Text, text) {
			out = append(out, m)
		}
	}
	return out
}
func (f *fakeTelegram) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.messages))
	for _, m := range f.messages {
		out = append(out, m.Text)
	}
	return out
}
func (f *fakeTelegram) after(id int64) []fakeMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []fakeMessage
	for _, m := range f.messages {
		if m.MessageID >= id {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeTelegram) terminalGroup(first fakeMessage, total int) []fakeMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	start := -1
	for i, message := range f.messages {
		if message.MessageID == first.MessageID {
			start = i
			break
		}
	}
	if start < 0 || start+total > len(f.messages) {
		return nil
	}
	group := make([]fakeMessage, 0, total)
	group = append(group, first)
	for offset := 1; offset < total; offset++ {
		message := f.messages[start+offset]
		index, count, ok := terminalChunkLabel(message.Text)
		if !ok || count != total || index != offset+1 {
			return nil
		}
		group = append(group, message)
	}
	return group
}

func (f *fakeTelegram) button(id int64, label string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range f.messages {
		if m.MessageID == id {
			return m.Buttons[label]
		}
	}
	return ""
}
func (f *fakeTelegram) handle(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/file/") {
		f.mu.Lock()
		v := f.voiceText
		f.mu.Unlock()
		_, _ = io.WriteString(w, v)
		return
	}
	method := filepath.Base(r.URL.Path)
	if method == "sendMessage" {
		f.mu.Lock()
		fail := f.fail
		f.fail = 0
		f.mu.Unlock()
		if fail != 0 {
			f.mu.Lock()
			f.failed++
			f.mu.Unlock()
			w.WriteHeader(fail)
			return
		}
		var b struct {
			Text        string `json:"text"`
			ReplyMarkup struct {
				InlineKeyboard [][]struct {
					Text         string `json:"text"`
					CallbackData string `json:"callback_data"`
				} `json:"inline_keyboard"`
			} `json:"reply_markup"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		f.message++
		m := fakeMessage{f.message, b.Text, map[string]string{}}
		for _, row := range b.ReplyMarkup.InlineKeyboard {
			for _, button := range row {
				m.Buttons[button.Text] = button.CallbackData
			}
		}
		f.messages = append(f.messages, m)
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": m.MessageID}})
		return
	}
	if method == "editMessageText" {
		var b struct {
			MessageID   int64  `json:"message_id"`
			Text        string `json:"text"`
			ReplyMarkup struct {
				InlineKeyboard [][]struct {
					Text         string `json:"text"`
					CallbackData string `json:"callback_data"`
				} `json:"inline_keyboard"`
			} `json:"reply_markup"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.mu.Lock()
		for i := range f.messages {
			if f.messages[i].MessageID == b.MessageID {
				f.messages[i].Text = b.Text
				f.messages[i].Buttons = map[string]string{}
				for _, row := range b.ReplyMarkup.InlineKeyboard {
					for _, button := range row {
						f.messages[i].Buttons[button.Text] = button.CallbackData
					}
				}
			}
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": b.MessageID}})
		return
	}
	if method == "getFile" {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_id": "voice-1", "file_path": "voice/file.ogg"}})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
}
