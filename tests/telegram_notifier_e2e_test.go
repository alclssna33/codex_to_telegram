package tests

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/config"
	"github.com/alclssna33/codex_to_telegram/internal/daemon"
	"github.com/alclssna33/codex_to_telegram/internal/securestore"
	"github.com/alclssna33/codex_to_telegram/internal/storage"
	"github.com/alclssna33/codex_to_telegram/internal/telegram"
	"github.com/alclssna33/codex_to_telegram/internal/transcription"
)

const (
	task9FakeAppServerEnv       = "CTR_GO_TASK9_FAKE_APPSERVER"
	task9FakeAppServerStateEnv  = "CTR_GO_TASK9_FAKE_APPSERVER_STATE"
	task9FakeAppServerEventsEnv = "CTR_GO_TASK9_FAKE_APPSERVER_EVENTS"
	task9PromptMarker           = "task9-prompt-marker-19e8"
	task9FinalMarker            = "task9-final-marker-a1f4"
	task9TitleMarker            = "task9-title-marker-7bb2"
	task9FullCWDMarker          = "D:\\task9-full-cwd-marker\\workspace"
	task9OwnerMarker            = "900000000001"
)

func init() {
	if os.Getenv(task9FakeAppServerEnv) == "1" {
		task9FakeAppServerMain()
		os.Exit(0)
	}
}

func TestTelegramNotifierAllFoldersRestartAndInputIsolation(t *testing.T) {
	root := t.TempDir()
	folderA := filepath.Join(root, "unregistered-a")
	folderB := filepath.Join(root, "unregistered-b")
	if err := os.Mkdir(folderA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(folderB, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Unix()
	h := newTask9NotifierHarness(t, root, task9FakeState{
		Threads: []task9FakeThread{
			{ID: "thread-a", CWD: folderA, Title: "Alpha", Turn: "turn-a", Status: "inProgress", UpdatedAt: now - 5, Final: task9FinalMarker},
			{ID: "thread-b", CWD: folderB, Turn: "turn-b", Status: "inProgress", UpdatedAt: now - 4},
		},
	})

	h.startNotifier()
	h.waitAppReady()
	h.sendText("/start", 0)
	h.waitOne("Codex 완료 알림을 감시합니다.")
	h.waitNotifierActivation()
	h.app.touch("thread-a")
	h.app.touch("thread-b")
	h.app.waitThreadRead("thread-a")
	h.app.waitThreadRead("thread-b")
	h.restart()

	h.app.complete("thread-a", "turn-a", "결과 요약입니다.")
	h.app.complete("thread-b", "turn-b", "")
	first := h.waitOne("폴더: unregistered-a")
	second := h.waitOne("폴더: unregistered-b")
	if first.Text == second.Text {
		t.Fatalf("notifier messages were not distinct: %q", first.Text)
	}
	if !strings.Contains(second.Text, "작업이 완료되었습니다.") {
		t.Fatalf("missing final/title fallback text in second notification: %q", second.Text)
	}

	h.sendText("plain text should not reach codex", 0)
	h.sendText("reply should not reach codex", first.MessageID)
	h.sendVoice()
	h.sendDocument()
	h.sendCallback(first.MessageID, "unsupported-callback")

	h.app.assertOnlyReadMethods()
	if h.tg.fileRequests() != 0 {
		t.Fatalf("voice file requests = %d, want 0", h.tg.fileRequests())
	}
	if h.transcriber.calls() != 0 {
		t.Fatalf("transcriptions = %d, want 0", h.transcriber.calls())
	}
}

func TestTelegramNotifierRetryIsOnceOnlyAfterAcknowledgement(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "retry-work")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Unix()
	h := newTask9NotifierHarness(t, root, task9FakeState{
		Threads: []task9FakeThread{{
			ID: "thread-retry", CWD: folder, Title: "Retry", Turn: "turn-retry", Status: "inProgress", UpdatedAt: now - 5,
		}},
	})

	h.startNotifier()
	h.waitAppReady()
	h.sendText("/start", 0)
	h.waitOne("Codex 완료 알림을 감시합니다.")
	h.waitNotifierActivation()
	h.app.touch("thread-retry")
	h.app.waitThreadRead("thread-retry")
	h.tg.failNextSend(http.StatusBadGateway)
	h.app.complete("thread-retry", "turn-retry", "retry final")
	h.tg.waitSendStats(task9SendStats{Attempts: 3, Failed: 1, Succeeded: 2})
	delivered := h.waitOne("폴더: retry-work")
	beforeRestart := h.tg.count()
	beforeAttempts := h.tg.sendStats()
	h.restart()
	h.waitNoAdditionalMessages(beforeRestart, 500*time.Millisecond)
	h.waitNoAdditionalSendAttempts(beforeAttempts, 500*time.Millisecond)
	if got := len(h.tg.findAll(delivered.Text)); got != 1 {
		t.Fatalf("delivered notifier replay count = %d, want 1", got)
	}
}

func TestTelegramNotifierLogsContainNoConversationContent(t *testing.T) {
	root := t.TempDir()
	folder := filepath.Join(root, "log-work")
	if err := os.Mkdir(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Unix()
	h := newTask9NotifierHarness(t, root, task9FakeState{
		Threads: []task9FakeThread{{
			ID:        "thread-log",
			CWD:       task9FullCWDMarker,
			Title:     task9TitleMarker,
			Turn:      "turn-log",
			Status:    "inProgress",
			Prompt:    task9PromptMarker,
			UpdatedAt: now - 5,
		}},
	})

	h.startNotifier()
	h.waitAppReady()
	h.sendText("/start", 0)
	h.waitOne("Codex 완료 알림을 감시합니다.")
	h.waitNotifierActivation()
	h.app.touch("thread-log")
	h.app.waitThreadRead("thread-log")
	h.app.complete("thread-log", "turn-log", task9FinalMarker)
	h.waitOne("작업 완료")
	logs := h.allLogs()
	for _, marker := range []string{task9PromptMarker, task9FinalMarker, task9TitleMarker, task9FullCWDMarker, task9OwnerMarker} {
		if strings.Contains(logs, marker) {
			t.Fatalf("sanitized logs contain private marker %q in %q", marker, logs)
		}
	}
}

type task9NotifierHarness struct {
	t           *testing.T
	ctx         context.Context
	cancel      context.CancelFunc
	cfg         config.Config
	service     *daemon.Service
	bot         *telegram.Bot
	app         task9FakeAppControl
	tg          *task9FakeTelegram
	transcriber *task9CountingTranscriber
	daemonLogs  bytes.Buffer
	botLogs     bytes.Buffer
}

func newTask9NotifierHarness(t *testing.T, root string, initial task9FakeState) *task9NotifierHarness {
	t.Helper()
	statePath := filepath.Join(root, "appserver.json")
	eventsPath := filepath.Join(root, "appserver-events.log")
	if err := task9WriteFakeState(statePath, initial); err != nil {
		t.Fatal(err)
	}
	t.Setenv(task9FakeAppServerEnv, "1")
	t.Setenv(task9FakeAppServerStateEnv, statePath)
	t.Setenv(task9FakeAppServerEventsEnv, eventsPath)
	ctx, cancel := context.WithCancel(context.Background())
	h := &task9NotifierHarness{
		t:      t,
		ctx:    ctx,
		cancel: cancel,
		cfg: config.Config{
			Paths: config.Paths{
				Home:    filepath.Join(root, "bridge"),
				DataDir: filepath.Join(root, "bridge", "data"),
				LogDir:  filepath.Join(root, "bridge", "logs"),
				DBPath:  filepath.Join(root, "bridge", "data", "state.sqlite"),
			},
			Profile:               "notifier",
			TelegramBotToken:      "test-token",
			AllowedUserIDs:        []int64{900000000001},
			AllowedChatIDs:        []int64{900000000001},
			CodexBin:              os.Args[0],
			AppServerListen:       "stdio://",
			DefaultCWD:            root,
			CommandMaxAge:         time.Minute,
			ObserverPollInterval:  25 * time.Millisecond,
			RequestTimeout:        2 * time.Second,
			IndexRefreshInterval:  time.Hour,
			AttachRefreshInterval: time.Hour,
			DeliveryRetryBase:     25 * time.Millisecond,
			DeliveryMaxAttempts:   3,
		},
		app:         task9FakeAppControl{t: t, statePath: statePath, eventsPath: eventsPath},
		tg:          newTask9FakeTelegram(t),
		transcriber: &task9CountingTranscriber{},
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

func (h *task9NotifierHarness) startNotifier() {
	h.t.Helper()
	service, err := daemon.New(h.cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	service.SetVoiceTranscriber(h.transcriber)
	service.SetLogger(log.New(&h.daemonLogs, "", 0))
	bot, err := telegram.NewBotWithClient(h.cfg, service, log.New(&h.botLogs, "", 0), telegram.NewClientWithBaseURLs(h.cfg.TelegramBotToken, h.tg.url(), h.tg.url()))
	if err != nil {
		h.t.Fatal(err)
	}
	service.SetSender(bot)
	if err = service.Start(h.ctx); err != nil {
		h.t.Fatal(err)
	}
	h.service = service
	h.bot = bot
}

func (h *task9NotifierHarness) restart() {
	h.t.Helper()
	if err := h.service.Close(); err != nil {
		h.t.Fatal(err)
	}
	h.service = nil
	h.bot = nil
	h.waitStoreReusable()
	h.startNotifier()
	h.waitAppReady()
}

func (h *task9NotifierHarness) waitStoreReusable() {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		probe, err := storage.OpenWithProtector(h.cfg.Paths.DBPath, securestore.NewDPAPIProtector())
		if err == nil {
			if closeErr := probe.Close(); closeErr == nil {
				return
			} else {
				lastErr = closeErr
			}
		} else {
			lastErr = err
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("store did not become reusable: %v", lastErr)
}

func (h *task9NotifierHarness) waitAppReady() {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.app.get().Ready {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("fake App Server did not become ready; state=%+v logs=%q", h.app.get(), h.allLogs())
}

func (h *task9NotifierHarness) waitNotifierActivation() {
	h.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		doctor, err := h.service.Doctor(h.ctx)
		if err == nil {
			if state, ok := doctor["daemon_state"].(map[string]string); ok && strings.TrimSpace(state["notifier.activation_unix"]) != "" {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatal("notifier activation was not recorded")
}

func (h *task9NotifierHarness) sendText(text string, replyTo int64) {
	h.t.Helper()
	msg := &telegram.Message{
		MessageID: h.tg.inboundID(),
		Date:      time.Now().Unix(),
		From:      &telegram.User{ID: 900000000001},
		Chat:      telegram.Chat{ID: 900000000001},
		Text:      text,
	}
	if replyTo != 0 {
		msg.ReplyToMessage = &telegram.Message{MessageID: replyTo}
	}
	h.handle(telegram.Update{UpdateID: h.tg.updateID(), Message: msg})
}

func (h *task9NotifierHarness) sendVoice() {
	h.t.Helper()
	h.handle(telegram.Update{UpdateID: h.tg.updateID(), Message: &telegram.Message{
		MessageID: h.tg.inboundID(),
		Date:      time.Now().Unix(),
		From:      &telegram.User{ID: 900000000001},
		Chat:      telegram.Chat{ID: 900000000001},
		Voice:     &telegram.Voice{FileID: "voice-task9", FileSize: 5, Duration: 1, MimeType: "audio/ogg"},
	}})
}

func (h *task9NotifierHarness) sendDocument() {
	h.t.Helper()
	h.handle(telegram.Update{UpdateID: h.tg.updateID(), Message: &telegram.Message{
		MessageID: h.tg.inboundID(),
		Date:      time.Now().Unix(),
		From:      &telegram.User{ID: 900000000001},
		Chat:      telegram.Chat{ID: 900000000001},
		Document:  &telegram.Document{FileID: "document-task9", FileName: "task9.txt", MimeType: "text/plain", FileSize: 7},
	}})
}

func (h *task9NotifierHarness) sendCallback(messageID int64, data string) {
	h.t.Helper()
	h.handle(telegram.Update{UpdateID: h.tg.updateID(), CallbackQuery: &telegram.CallbackQuery{
		ID:      "callback-task9",
		From:    &telegram.User{ID: 900000000001},
		Message: &telegram.Message{MessageID: messageID, Chat: telegram.Chat{ID: 900000000001}},
		Data:    data,
	}})
}

func (h *task9NotifierHarness) handle(update telegram.Update) {
	h.t.Helper()
	if err := h.bot.HandleUpdate(h.ctx, update); err != nil {
		h.t.Fatal(err)
	}
}

func (h *task9NotifierHarness) waitOne(text string) task9FakeMessage {
	h.t.Helper()
	deadline := time.Now().Add(7 * time.Second)
	for time.Now().Before(deadline) {
		if message, ok := h.tg.find(text); ok {
			return message
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatalf("missing Telegram text %q; messages=%q logs=%q", text, h.tg.texts(), h.allLogs())
	return task9FakeMessage{}
}

func (h *task9NotifierHarness) waitNoAdditionalMessages(count int, duration time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		if got := h.tg.count(); got != count {
			h.t.Fatalf("Telegram messages after restart = %d, want %d; messages=%q", got, count, h.tg.texts())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *task9NotifierHarness) waitNoAdditionalSendAttempts(stats task9SendStats, duration time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		if got := h.tg.sendStats(); got != stats {
			h.t.Fatalf("Telegram send stats after restart = %+v, want %+v; messages=%q", got, stats, h.tg.texts())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (h *task9NotifierHarness) allLogs() string {
	return h.daemonLogs.String() + "\n" + h.botLogs.String()
}

type task9FakeAppControl struct {
	t          *testing.T
	statePath  string
	eventsPath string
}

func (c task9FakeAppControl) get() task9FakeState {
	c.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		state, err := task9ReadFakeState(c.statePath)
		if err == nil {
			return state
		}
		lastErr = err
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatal(lastErr)
	return task9FakeState{}
}

func (c task9FakeAppControl) put(state task9FakeState) {
	c.t.Helper()
	if err := task9WriteFakeState(c.statePath, state); err != nil {
		c.t.Fatal(err)
	}
}

func (c task9FakeAppControl) complete(threadID, turnID, final string) {
	c.t.Helper()
	state := c.get()
	for i := range state.Threads {
		if state.Threads[i].ID == threadID {
			state.Threads[i].Turn = turnID
			state.Threads[i].Status = "completed"
			state.Threads[i].Final = final
			state.Threads[i].UpdatedAt = max(time.Now().UTC().Unix(), state.Threads[i].UpdatedAt+1)
			c.put(state)
			return
		}
	}
	c.t.Fatalf("missing fake thread %q", threadID)
}

func (c task9FakeAppControl) touch(threadID string) {
	c.t.Helper()
	state := c.get()
	for i := range state.Threads {
		if state.Threads[i].ID == threadID {
			state.Threads[i].UpdatedAt = max(time.Now().UTC().Unix()+1, state.Threads[i].UpdatedAt+1)
			c.put(state)
			return
		}
	}
	c.t.Fatalf("missing fake thread %q", threadID)
}

func (c task9FakeAppControl) waitThreadRead(threadID string) {
	c.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, event := range c.events() {
			if event.Method == "thread/read" && event.ThreadID == threadID {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	c.t.Fatalf("thread/read for %s not observed; events=%+v", threadID, c.events())
}

func (c task9FakeAppControl) assertOnlyReadMethods() {
	c.t.Helper()
	allowed := map[string]bool{"initialize": true, "initialized": true, "thread/list": true, "thread/read": true}
	for _, event := range c.events() {
		if !allowed[event.Method] {
			c.t.Fatalf("fake App Server method %q reached notifier; events=%+v", event.Method, c.events())
		}
	}
}

func (c task9FakeAppControl) events() []task9FakeEvent {
	c.t.Helper()
	data, err := os.ReadFile(c.eventsPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		c.t.Fatal(err)
	}
	var events []task9FakeEvent
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event task9FakeEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			c.t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

type task9FakeState struct {
	Ready   bool              `json:"ready"`
	Threads []task9FakeThread `json:"threads"`
}

type task9FakeThread struct {
	ID        string `json:"id"`
	CWD       string `json:"cwd"`
	Title     string `json:"title"`
	Turn      string `json:"turn"`
	Status    string `json:"status"`
	Prompt    string `json:"prompt"`
	Final     string `json:"final"`
	UpdatedAt int64  `json:"updated_at"`
}

type task9FakeEvent struct {
	Method   string `json:"method"`
	ThreadID string `json:"thread_id,omitempty"`
}

func task9FakeAppServerMain() {
	statePath := os.Getenv(task9FakeAppServerStateEnv)
	eventsPath := os.Getenv(task9FakeAppServerEventsEnv)
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     any            `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			continue
		}
		state := task9ReadStateMain(statePath)
		threadID, _ := request.Params["threadId"].(string)
		task9AppendFakeEvent(eventsPath, task9FakeEvent{Method: request.Method, ThreadID: threadID})
		result := map[string]any{}
		switch request.Method {
		case "initialize":
			state.Ready = true
			_ = task9WriteFakeState(statePath, state)
		case "thread/list":
			result = task9ThreadListPayload(state)
		case "thread/read":
			result = task9ThreadReadPayload(state, threadID)
		}
		if request.ID != nil {
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		}
	}
}

func task9ThreadListPayload(state task9FakeState) map[string]any {
	items := make([]any, 0, len(state.Threads))
	for _, thread := range state.Threads {
		status := thread.Status
		item := map[string]any{
			"id":         thread.ID,
			"cwd":        thread.CWD,
			"name":       thread.Title,
			"status":     status,
			"updatedAt":  thread.UpdatedAt,
			"sourceKind": "cli",
		}
		if status == "inProgress" && thread.Turn != "" {
			item["activeTurnId"] = thread.Turn
		}
		items = append(items, item)
	}
	return map[string]any{"data": items}
}

func task9ThreadReadPayload(state task9FakeState, threadID string) map[string]any {
	for _, thread := range state.Threads {
		if thread.ID == threadID {
			return map[string]any{
				"id":        thread.ID,
				"cwd":       thread.CWD,
				"name":      thread.Title,
				"status":    thread.Status,
				"updatedAt": thread.UpdatedAt,
				"turns": []any{map[string]any{
					"id":     thread.Turn,
					"status": thread.Status,
					"items":  task9ThreadItems(thread),
				}},
			}
		}
	}
	return map[string]any{}
}

func task9ThreadItems(thread task9FakeThread) []any {
	prompt := strings.TrimSpace(thread.Prompt)
	if prompt == "" {
		prompt = "safe prompt"
	}
	items := []any{map[string]any{"id": "user-" + thread.Turn, "type": "userMessage", "text": prompt}}
	if strings.EqualFold(thread.Status, "completed") {
		items = append(items, map[string]any{"id": "final-" + thread.Turn, "type": "agentMessage", "text": thread.Final, "phase": "final"})
	}
	return items
}

func task9ReadStateMain(path string) task9FakeState {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		state, err := task9ReadFakeState(path)
		if err == nil {
			return state
		}
		time.Sleep(10 * time.Millisecond)
	}
	return task9FakeState{}
}

func task9ReadFakeState(path string) (task9FakeState, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return task9FakeState{}, err
	}
	var state task9FakeState
	if err := json.Unmarshal(data, &state); err != nil {
		return task9FakeState{}, err
	}
	return state, nil
}

func task9WriteFakeState(path string, state task9FakeState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.new")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := task9RenameFakeState(tmpName, path); err != nil {
		return err
	}
	removeTmp = false
	return nil
}

func task9RenameFakeState(tmpName, path string) error {
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

func task9AppendFakeEvent(path string, event task9FakeEvent) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = f.Write(append(data, '\n'))
	_ = f.Close()
}

type task9FakeTelegram struct {
	t        *testing.T
	server   *httptest.Server
	mu       sync.Mutex
	nextMsg  int64
	nextIn   int64
	nextUp   int64
	messages []task9FakeMessage
	failSend int
	stats    task9SendStats
	files    int
}

type task9SendStats struct {
	Attempts  int
	Failed    int
	Succeeded int
}

type task9FakeMessage struct {
	MessageID int64
	Text      string
}

func newTask9FakeTelegram(t *testing.T) *task9FakeTelegram {
	fake := &task9FakeTelegram{t: t, nextMsg: 100}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.handle))
	return fake
}

func (f *task9FakeTelegram) close() { f.server.Close() }
func (f *task9FakeTelegram) url() string {
	return f.server.URL
}
func (f *task9FakeTelegram) inboundID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextIn++
	return f.nextIn
}
func (f *task9FakeTelegram) updateID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextUp++
	return f.nextUp
}
func (f *task9FakeTelegram) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.messages)
}
func (f *task9FakeTelegram) failNextSend(status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failSend = status
}
func (f *task9FakeTelegram) waitSendStats(want task9SendStats) {
	f.t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.sendStats(); got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.t.Fatalf("send stats = %+v, want %+v; messages=%q", f.sendStats(), want, f.texts())
}
func (f *task9FakeTelegram) sendStats() task9SendStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stats
}
func (f *task9FakeTelegram) fileRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.files
}
func (f *task9FakeTelegram) find(text string) (task9FakeMessage, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.messages) - 1; i >= 0; i-- {
		if strings.Contains(f.messages[i].Text, text) {
			return f.messages[i], true
		}
	}
	return task9FakeMessage{}, false
}
func (f *task9FakeTelegram) findAll(text string) []task9FakeMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []task9FakeMessage
	for _, message := range f.messages {
		if strings.Contains(message.Text, text) {
			out = append(out, message)
		}
	}
	return out
}
func (f *task9FakeTelegram) texts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.messages))
	for _, message := range f.messages {
		out = append(out, message.Text)
	}
	sort.Strings(out)
	return out
}
func (f *task9FakeTelegram) handle(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/file/") {
		f.mu.Lock()
		f.files++
		f.mu.Unlock()
		_, _ = io.WriteString(w, "voice")
		return
	}
	method := filepath.Base(r.URL.Path)
	switch method {
	case "sendMessage":
		f.mu.Lock()
		f.stats.Attempts++
		fail := f.failSend
		f.failSend = 0
		if fail != 0 {
			f.stats.Failed++
			f.mu.Unlock()
			w.WriteHeader(fail)
			return
		}
		f.mu.Unlock()
		var body struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.nextMsg++
		message := task9FakeMessage{MessageID: f.nextMsg, Text: body.Text}
		f.messages = append(f.messages, message)
		f.stats.Succeeded++
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": message.MessageID}})
	case "getFile":
		f.mu.Lock()
		f.files++
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_id": "voice-task9", "file_path": "voice/file.ogg"}})
	case "answerCallbackQuery":
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
	}
}

type task9CountingTranscriber struct {
	mu sync.Mutex
	n  int
}

func (t *task9CountingTranscriber) Transcribe(_ context.Context, _ io.Reader, _ transcription.Meta) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.n++
	return "must not run", nil
}

func (t *task9CountingTranscriber) calls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.n
}
