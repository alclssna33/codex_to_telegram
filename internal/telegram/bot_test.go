package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alclssna33/codex_to_telegram/internal/config"
	"github.com/alclssna33/codex_to_telegram/internal/daemon"
	"github.com/alclssna33/codex_to_telegram/internal/model"
	"github.com/alclssna33/codex_to_telegram/internal/transcription"
)

type botVoiceTranscriber struct {
	text  string
	data  []byte
	calls int
}

func (f *botVoiceTranscriber) Transcribe(_ context.Context, audio io.Reader, _ transcription.Meta) (string, error) {
	f.calls++
	f.data, _ = io.ReadAll(audio)
	return f.text, nil
}

func TestBotVoiceDownloadsAfterValidationAndDeliversSilentBoundPreview(t *testing.T) {
	t.Parallel()

	var paths []string
	var sent sendMessageRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/getFile":
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"voice-file","file_path":"voice/clip.oga"}}`))
		case "/file/voice/clip.oga":
			_, _ = w.Write([]byte("telegram ogg"))
		case "/sendMessage":
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":777,"chat":{"id":7,"type":"private"}}}`))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	cfg, service := newMinimalBotService(t)
	transcriber := &botVoiceTranscriber{text: "테스트를 실행해줘"}
	service.SetVoiceTranscriber(transcriber)
	picker, err := service.HandleInboundText(context.Background(), model.InboundText{ChatID: 7, UserID: 7, Text: "/start", ReceivedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.HandleCallback(context.Background(), 7, 0, 100, 7, picker.Buttons[0][0].CallbackData); err != nil {
		t.Fatal(err)
	}
	client := NewClient("token")
	client.baseURL = server.URL
	client.fileBaseURL = server.URL + "/file"
	bot := &Bot{cfg: cfg, client: client, service: service, logger: log.New(io.Discard, "", 0)}
	if err := bot.handleMessage(context.Background(), Message{
		MessageID: 10, Date: time.Now().Unix(), From: &User{ID: 7}, Chat: Chat{ID: 7, Type: "private"},
		Voice: &Voice{FileID: "voice-file", FileUniqueID: "voice-unique", Duration: 2, FileSize: 12, MimeType: "audio/ogg"},
	}); err != nil {
		t.Fatalf("handleMessage failed: %v paths=%#v sent=%#v", err, paths, sent)
	}
	if got, want := strings.Join(paths, ","), "/getFile,/file/voice/clip.oga,/sendMessage"; got != want {
		t.Fatalf("paths = %q, want %q", got, want)
	}
	if transcriber.calls != 1 || string(transcriber.data) != "telegram ogg" {
		t.Fatalf("transcriber calls=%d data=%q", transcriber.calls, transcriber.data)
	}
	if !strings.Contains(sent.Text, "[음성 인식]") || !strings.Contains(sent.Text, transcriber.text) || !sent.DisableNotification {
		t.Fatalf("sent preview = %#v", sent)
	}
	if sent.ReplyMarkup == nil || len(sent.ReplyMarkup.InlineKeyboard) != 1 || len(sent.ReplyMarkup.InlineKeyboard[0]) != 2 ||
		sent.ReplyMarkup.InlineKeyboard[0][0].Text != "실행" || sent.ReplyMarkup.InlineKeyboard[0][1].Text != "취소" {
		t.Fatalf("preview keyboard = %#v", sent.ReplyMarkup)
	}
	cancelToken := sent.ReplyMarkup.InlineKeyboard[0][1].CallbackData
	if response, err := service.HandleCallback(context.Background(), 7, 0, 777, 7, cancelToken); err != nil || response == nil || !strings.Contains(response.Text, "취소") {
		t.Fatalf("bound cancel response=%#v err=%v", response, err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{cfg.Paths.DBPath, cfg.Paths.DBPath + "-wal", cfg.Paths.DBPath + "-shm"} {
		data, err := os.ReadFile(path)
		if err != nil && os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, private := range []string{"voice-file", "voice-unique", "voice/clip.oga", "telegram ogg", transcriber.text} {
			if strings.Contains(string(data), private) {
				t.Fatalf("SQLite artifact %s contains private voice material %q", filepath.Base(path), private)
			}
		}
	}
}

func TestBotVoiceRejectsUnauthorizedAndStaleBeforeGetFile(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		user       int64
		date       int64
		wantSend   bool
		configured bool
	}{
		{name: "unauthorized", user: 999, date: time.Now().Unix(), configured: true},
		{name: "stale", user: 7, date: time.Now().Add(-11 * time.Minute).Unix(), wantSend: true, configured: true},
		{name: "missing key", user: 7, date: time.Now().Unix(), wantSend: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":778,"chat":{"id":7,"type":"private"}}}`))
			}))
			defer server.Close()
			cfg, service := newMinimalBotService(t)
			if test.configured {
				service.SetVoiceTranscriber(&botVoiceTranscriber{text: "unused"})
			}
			client := NewClient("token")
			client.baseURL = server.URL
			client.fileBaseURL = server.URL + "/file"
			bot := &Bot{cfg: cfg, client: client, service: service, logger: log.New(io.Discard, "", 0)}
			if err := bot.handleMessage(context.Background(), Message{
				MessageID: 11, Date: test.date, From: &User{ID: test.user}, Chat: Chat{ID: 7, Type: "private"},
				Voice: &Voice{FileID: "must-not-fetch", Duration: 1, FileSize: 1},
			}); err != nil {
				t.Fatal(err)
			}
			wantPaths := 0
			if test.wantSend {
				wantPaths = 1
			}
			if len(paths) != wantPaths || len(paths) == 1 && paths[0] != "/sendMessage" {
				t.Fatalf("paths = %#v, want only optional response delivery", paths)
			}
		})
	}
}

func TestBotVoiceDownloadFailureReturnsGuidanceWithoutPrivateLeak(t *testing.T) {
	t.Parallel()

	const privateMarker = "PRIVATE_TELEGRAM_DOWNLOAD_BODY_ea82"
	var sent sendMessageRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getFile":
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"voice-file","file_path":"voice/private.oga"}}`))
		case "/file/voice/private.oga":
			http.Error(w, privateMarker, http.StatusUnauthorized)
		case "/sendMessage":
			if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":779,"chat":{"id":7,"type":"private"}}}`))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	cfg, service := newMinimalBotService(t)
	service.SetVoiceTranscriber(&botVoiceTranscriber{text: "unused"})
	picker, err := service.HandleInboundText(context.Background(), model.InboundText{ChatID: 7, UserID: 7, Text: "/start", ReceivedAt: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.HandleCallback(context.Background(), 7, 0, 100, 7, picker.Buttons[0][0].CallbackData); err != nil {
		t.Fatal(err)
	}
	var logs strings.Builder
	client := NewClient("token")
	client.baseURL = server.URL
	client.fileBaseURL = server.URL + "/file"
	bot := &Bot{cfg: cfg, client: client, service: service, logger: log.New(&logs, "", 0)}
	if err := bot.handleMessage(context.Background(), Message{
		MessageID: 12, Date: time.Now().Unix(), From: &User{ID: 7}, Chat: Chat{ID: 7, Type: "private"},
		Voice: &Voice{FileID: "voice-file", FileUniqueID: "voice-unique", Duration: 1, FileSize: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent.Text, "텍스트") || strings.Contains(sent.Text, privateMarker) || strings.Contains(logs.String(), privateMarker) || strings.Contains(logs.String(), "voice/private.oga") {
		t.Fatalf("sent=%q logs=%q", sent.Text, logs.String())
	}
}

func TestBotLegacyProfileContinuesIgnoringVoiceMessages(t *testing.T) {
	t.Parallel()

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	root := t.TempDir()
	cfg := config.Config{Profile: "legacy", AllowedUserIDs: []int64{7}, Paths: config.Paths{
		Home: root, DataDir: filepath.Join(root, "data"), LogDir: filepath.Join(root, "logs"), DBPath: filepath.Join(root, "data", "state.sqlite"),
	}}
	service, err := daemon.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	client := NewClient("token")
	client.baseURL = server.URL
	client.fileBaseURL = server.URL + "/file"
	bot := &Bot{cfg: cfg, client: client, service: service, logger: log.New(io.Discard, "", 0)}
	if err := bot.handleMessage(context.Background(), Message{
		MessageID: 13, Date: time.Now().Unix(), From: &User{ID: 7}, Chat: Chat{ID: 7, Type: "private"},
		Voice: &Voice{FileID: "legacy-voice", Duration: 1, FileSize: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("legacy voice made %d Telegram requests", requests)
	}
}

func TestBotStartSetsOnlyStartCommandForMinimalProfile(t *testing.T) {
	t.Parallel()

	var commands []BotCommand
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getMe":
			_, _ = w.Write([]byte(`{"ok":true,"result":{"id":1,"is_bot":true,"username":"minimal_bot"}}`))
		case "/setMyCommands":
			var request setMyCommandsRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			commands = request.Commands
			_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
		default:
			t.Fatalf("unexpected Telegram method %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient("token")
	client.baseURL = server.URL
	bot := &Bot{cfg: config.Config{Profile: "minimal"}, client: client, logger: log.New(io.Discard, "", 0)}
	if err := bot.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || commands[0].Command != "start" {
		t.Fatalf("minimal commands = %#v, want only /start", commands)
	}
}

func TestNotifierCommandsExposeManagementOnly(t *testing.T) {
	got := commandsForProfile("notifier")
	want := []BotCommand{
		{Command: "start", Description: "완료 알림 시작"},
		{Command: "help", Description: "관리 명령 안내"},
		{Command: "status", Description: "감시 상태 확인"},
		{Command: "repair", Description: "읽기 연결 재시작"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("commands=%#v", got)
	}
}

func TestNotifierVoiceDoesNotDownloadOrTranscribeAudio(t *testing.T) {
	t.Parallel()

	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":778,"chat":{"id":7,"type":"private"}}}`))
	}))
	defer server.Close()
	cfg, service := newNotifierBotService(t)
	service.SetVoiceTranscriber(&botVoiceTranscriber{text: "must not run"})
	client := NewClient("token")
	client.baseURL = server.URL
	client.fileBaseURL = server.URL + "/file"
	bot := &Bot{cfg: cfg, client: client, service: service, logger: log.New(io.Discard, "", 0)}

	if err := bot.handleMessage(context.Background(), Message{
		MessageID: 14, Date: time.Now().Unix(), From: &User{ID: 7}, Chat: Chat{ID: 7, Type: "private"},
		Voice: &Voice{FileID: "must-not-fetch", Duration: 1, FileSize: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("notifier voice made Telegram requests: %#v", paths)
	}
}

func TestNotifierCallbackAnswersWithoutConsumingCallbackRoutes(t *testing.T) {
	t.Parallel()

	var paths []string
	var captured answerCallbackQueryRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.URL.Path != "/answerCallbackQuery" {
			t.Fatalf("path = %q, want only /answerCallbackQuery", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()
	cfg, service := newNotifierBotService(t)
	client := NewClient("token")
	client.baseURL = server.URL
	bot := &Bot{cfg: cfg, client: client, service: service, logger: log.New(io.Discard, "", 0)}

	if err := bot.handleCallback(context.Background(), CallbackQuery{
		ID:   "notifier-callback",
		From: &User{ID: 7},
		Message: &Message{
			MessageID: 5,
			Chat:      Chat{ID: 7, Type: "private"},
		},
		Data: "untrusted-token",
	}); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(paths, ","), "/answerCallbackQuery"; got != want {
		t.Fatalf("paths = %q, want %q", got, want)
	}
	if !strings.Contains(captured.Text, "알림 전용") {
		t.Fatalf("callback acknowledgement = %#v, want notifier inactive guidance", captured)
	}
}

func TestBotMinimalMessageUsesTelegramServerTimestamp(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":777,"chat":{"id":7,"type":"private"}}}`))
	}))
	defer server.Close()

	cfg, service := newMinimalBotService(t)
	client := NewClient("token")
	client.baseURL = server.URL
	bot := &Bot{cfg: cfg, client: client, service: service, logger: log.New(io.Discard, "", 0)}
	messageDate := time.Now().Add(-11 * time.Minute).Unix()
	if err := bot.handleMessage(context.Background(), Message{
		MessageID: 1,
		Date:      messageDate,
		From:      &User{ID: 7},
		Chat:      Chat{ID: 7, Type: "private"},
		Text:      "run tests",
	}); err != nil {
		t.Fatal(err)
	}
	if text, _ := captured["text"].(string); !strings.Contains(text, "만료") {
		t.Fatalf("sent text = %#v, want expiry notice from Telegram timestamp", captured["text"])
	}
	if silent, _ := captured["disable_notification"].(bool); !silent {
		t.Fatalf("disable_notification = %#v, want silent expiry response", captured["disable_notification"])
	}
}

func TestBotAuthenticatesMinimalCallbackBeforeInspectingData(t *testing.T) {
	t.Parallel()

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":778,"chat":{"id":7,"type":"private"}}}`))
	}))
	defer server.Close()

	cfg, service := newMinimalBotService(t)
	client := NewClient("token")
	client.baseURL = server.URL
	bot := &Bot{cfg: cfg, client: client, service: service}
	if err := bot.handleCallback(context.Background(), CallbackQuery{
		ID:   "callback-1",
		From: &User{ID: 999},
		Message: &Message{
			MessageID: 5,
			Chat:      Chat{ID: 7, Type: "private"},
		},
		Data: "arbitrary untrusted content",
	}); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("unauthorized callback made %d Telegram requests", requests)
	}
}

func TestBotAuthenticatesMinimalMessageBeforeInspectingText(t *testing.T) {
	t.Parallel()

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":778,"chat":{"id":7,"type":"private"}}}`))
	}))
	defer server.Close()

	cfg, service := newMinimalBotService(t)
	client := NewClient("token")
	client.baseURL = server.URL
	bot := &Bot{cfg: cfg, client: client, service: service, logger: log.New(io.Discard, "", 0)}
	if err := bot.handleMessage(context.Background(), Message{
		MessageID: 6,
		Date:      time.Now().Unix(),
		From:      &User{ID: 999},
		Chat:      Chat{ID: 7, Type: "private"},
		Text:      "arbitrary untrusted content",
	}); err != nil {
		t.Fatal(err)
	}
	if requests != 0 {
		t.Fatalf("unauthorized message made %d Telegram requests", requests)
	}
}

func TestBotLegacyUnauthorizedCallbackStillGetsIgnoredAcknowledgement(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/answerCallbackQuery" {
			t.Fatalf("path = %q, want /answerCallbackQuery", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()

	root := t.TempDir()
	cfg := config.Config{
		Profile:        "legacy",
		AllowedUserIDs: []int64{7},
		Paths: config.Paths{
			Home:    root,
			DataDir: filepath.Join(root, "data"),
			LogDir:  filepath.Join(root, "logs"),
			DBPath:  filepath.Join(root, "data", "state.sqlite"),
		},
	}
	service, err := daemon.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	client := NewClient("token")
	client.baseURL = server.URL
	bot := &Bot{cfg: cfg, client: client, service: service, logger: log.New(io.Discard, "", 0)}
	if err := bot.handleCallback(context.Background(), CallbackQuery{
		ID:   "legacy-unauthorized",
		From: &User{ID: 999},
		Message: &Message{
			MessageID: 9,
			Chat:      Chat{ID: 7, Type: "private"},
		},
		Data: "legacy-opaque",
	}); err != nil {
		t.Fatal(err)
	}
	if captured["text"] != "Ignored." {
		t.Fatalf("callback acknowledgement = %#v, want Ignored.", captured)
	}
}

func newMinimalBotService(t *testing.T) (config.Config, *daemon.Service) {
	t.Helper()
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Profile:          "minimal",
		CommandMaxAge:    10 * time.Minute,
		TelegramBotToken: "fake-test-token",
		AllowedUserIDs:   []int64{7},
		Projects:         []model.Project{{ID: "project", DisplayName: "Project", CanonicalPath: project}},
		Paths: config.Paths{
			Home:    root,
			DataDir: filepath.Join(root, "data"),
			LogDir:  filepath.Join(root, "logs"),
			DBPath:  filepath.Join(root, "data", "state.sqlite"),
		},
	}
	service, err := daemon.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return cfg, service
}

func newNotifierBotService(t *testing.T) (config.Config, *daemon.Service) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Config{
		Profile:          "notifier",
		CommandMaxAge:    10 * time.Minute,
		TelegramBotToken: "fake-test-token",
		AllowedUserIDs:   []int64{7},
		Paths: config.Paths{
			Home:    root,
			DataDir: filepath.Join(root, "data"),
			LogDir:  filepath.Join(root, "logs"),
			DBPath:  filepath.Join(root, "data", "state.sqlite"),
		},
	}
	service, err := daemon.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	return cfg, service
}

func TestBotEditMessageRejectsMultiChunkPayload(t *testing.T) {
	t.Parallel()

	bot := &Bot{client: NewClient("token")}
	err := bot.EditMessage(context.Background(), 42, 0, 77, strings.Repeat("x", telegramMessageLimit+10), nil)
	if err == nil {
		t.Fatal("EditMessage must reject multi-chunk payloads")
	}
}

func TestSanitizeTelegramLogErrorRedactsBotTokenURL(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf(`Post "https://api.telegram.org/bot123456789:AAF_secret-token/getUpdates": context deadline exceeded`)
	got := sanitizeTelegramLogError(err)
	if strings.Contains(got, "123456789:AAF_secret-token") {
		t.Fatalf("sanitizeTelegramLogError leaked token: %q", got)
	}
	if !strings.Contains(got, "bot<redacted>") {
		t.Fatalf("sanitizeTelegramLogError = %q, want redacted marker", got)
	}
}

func TestDefaultCommandsExposeNewChatMenuCommand(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool)
	for _, command := range defaultCommands() {
		if seen[command.Command] {
			t.Fatalf("defaultCommands contains duplicate command %q", command.Command)
		}
		seen[command.Command] = true
	}
	for _, command := range []string{"newchat", "newthread"} {
		if !seen[command] {
			t.Fatalf("defaultCommands must expose /%s in the Telegram command menu", command)
		}
	}
	if seen["default"] {
		t.Fatal("defaultCommands must not expose hidden /default fallback in the Telegram command menu")
	}
}

func TestBotSendMessageChunksAndReturnsLastMessageID(t *testing.T) {
	t.Parallel()

	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = fmt.Fprintf(w, `{"ok":true,"result":{"message_id":%d,"chat":{"id":42,"type":"private"}}}`, 100+calls)
	}))
	defer server.Close()

	client := NewClient("token")
	client.baseURL = server.URL
	bot := &Bot{client: client}

	messageID, err := bot.SendMessage(context.Background(), 42, 0, strings.Repeat("line\n", telegramMessageLimit/4), nil, model.SendOptions{})
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}
	if calls < 2 {
		t.Fatalf("calls = %d, want at least 2 chunked requests", calls)
	}
	if got, want := messageID, int64(100+calls); got != want {
		t.Fatalf("messageID = %d, want %d", got, want)
	}
}

func TestBotSendRenderedMessagesFallsBackToPlainEntities(t *testing.T) {
	t.Parallel()

	var calls int
	var second map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"ok":false,"error_code":400,"description":"Bad Request: entities are invalid"}`))
			return
		}
		if err := json.Unmarshal(body, &second); err != nil {
			t.Fatalf("json.Unmarshal failed: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":202,"chat":{"id":42,"type":"private"}}}`))
	}))
	defer server.Close()

	client := NewClient("token")
	client.baseURL = server.URL
	bot := &Bot{client: client}
	ids, err := bot.SendRenderedMessages(context.Background(), 42, 0, []model.RenderedMessage{{
		Text:     "formatted",
		Entities: []model.MessageEntity{{Type: "code", Offset: 0, Length: 9}},
	}}, nil, model.SendOptions{})
	if err != nil {
		t.Fatalf("SendRenderedMessages failed: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if len(ids) != 1 || ids[0] != 202 {
		t.Fatalf("ids = %#v, want [202]", ids)
	}
	if _, ok := second["entities"]; ok {
		t.Fatalf("fallback entities = %#v, want omitted", second["entities"])
	}
	if _, ok := second["parse_mode"]; ok {
		t.Fatalf("fallback parse_mode = %#v, want omitted", second["parse_mode"])
	}
	if got, want := second["text"], "formatted"; got != want {
		t.Fatalf("fallback text = %#v, want identical %q", got, want)
	}
}

func TestSplitTextIsUnicodeSafe(t *testing.T) {
	chunks := splitText(strings.Repeat("가", 9000), telegramMessageLimit)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3", len(chunks))
	}
	if strings.Join(chunks, "") != strings.Repeat("가", 9000) {
		t.Fatal("unicode text was split or changed")
	}
	for _, chunk := range chunks {
		if utf8.RuneCountInString(chunk) > telegramMessageLimit {
			t.Fatal("oversized chunk")
		}
	}
}

func TestBotSendDocumentReturnsTelegramMessageID(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":555,"chat":{"id":42,"type":"private"}}}`))
	}))
	defer server.Close()

	client := NewClient("token")
	client.baseURL = server.URL
	bot := &Bot{client: client}

	dir := t.TempDir()
	path := filepath.Join(dir, "trace.log")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("WriteFile(trace.log) failed: %v", err)
	}

	messageID, err := bot.SendDocument(context.Background(), 42, 0, "trace.log", path, "trace", model.SendOptions{})
	if err != nil {
		t.Fatalf("SendDocument failed: %v", err)
	}
	if got, want := messageID, int64(555); got != want {
		t.Fatalf("messageID = %d, want %d", got, want)
	}
}

func TestBotDeliverDirectResponseSendsSilentMessage(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("json.Unmarshal failed: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":777,"chat":{"id":42,"type":"private"},"text":"menu"}}`))
	}))
	defer server.Close()

	root := t.TempDir()
	service, err := daemon.New(config.Config{Paths: config.Paths{
		Home:    root,
		DataDir: filepath.Join(root, "data"),
		LogDir:  filepath.Join(root, "logs"),
		DBPath:  filepath.Join(root, "data", "state.sqlite"),
	}})
	if err != nil {
		t.Fatalf("daemon.New failed: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })

	client := NewClient("token")
	client.baseURL = server.URL
	bot := &Bot{client: client, service: service}
	if err := bot.deliverDirectResponse(context.Background(), 42, 0, &daemon.DirectResponse{Text: "menu"}); err != nil {
		t.Fatalf("deliverDirectResponse failed: %v", err)
	}
	if got, ok := captured["disable_notification"].(bool); !ok || !got {
		t.Fatalf("disable_notification = %#v, want true", captured["disable_notification"])
	}
}
