package daemon

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/appserver"
	"github.com/alclssna33/codex_to_telegram/internal/model"
	"github.com/alclssna33/codex_to_telegram/internal/transcription"
)

type fakeVoiceTranscriber struct {
	text  string
	err   error
	calls int
}

func (f *fakeVoiceTranscriber) Transcribe(_ context.Context, audio io.Reader, _ transcription.Meta) (string, error) {
	f.calls++
	_, _ = io.ReadAll(audio)
	return f.text, f.err
}

type trackingVoiceBody struct {
	*strings.Reader
	closed bool
}

func (r *trackingVoiceBody) Close() error {
	r.closed = true
	return nil
}

func TestVoicePreviewDoesNotRunUntilExecuteAndUsesFrozenProject(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession("voice-thread")
	useRouterSession(svc, app)
	transcriber := &fakeVoiceTranscriber{text: "테스트를 실행해줘"}
	svc.voiceTranscriber = transcriber
	mustSelect(t, svc, 100, "bridge")
	body := &trackingVoiceBody{Reader: strings.NewReader("ogg")}
	response, err := svc.HandleVoice(context.Background(), VoiceInput{
		ChatID: 100, UserID: 7, MessageID: 50, ReceivedAt: svc.now(),
		FileSize: 3, Duration: 1, MimeType: "audio/ogg",
		Open: func(context.Context) (io.ReadCloser, error) { return body, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(app.turnCalls()) != 0 {
		t.Fatal("voice auto-ran")
	}
	if !body.closed {
		t.Fatal("download body was not closed")
	}
	wantPrefix := "[음성 인식]\n작업폴더: Bridge\n대상: 새 작업\n\n테스트를 실행해줘"
	if response == nil || response.Text != wantPrefix {
		t.Fatalf("preview = %#v, want %q", response, wantPrefix)
	}
	if got := buttonLabels(response.Buttons); len(got) != 1 || len(got[0]) != 2 || got[0][0] != "실행" || got[0][1] != "취소" {
		t.Fatalf("buttons = %#v", got)
	}
	if response.VoiceConfirmationID == 0 {
		t.Fatal("voice confirmation id is zero")
	}
	if err := svc.RegisterDirectDelivery(context.Background(), 100, 0, 700, response); err != nil {
		t.Fatal(err)
	}
	mustSelect(t, svc, 100, "second")
	executeToken := callbackTokenForVoiceButton(t, response, "실행")
	callback, err := svc.HandleCallback(context.Background(), 100, 0, 700, 7, executeToken)
	if err != nil {
		t.Fatal(err)
	}
	if callback == nil || callback.CallbackText == "" {
		t.Fatalf("callback = %#v", callback)
	}
	calls := app.turnCalls()
	if len(calls) != 1 || calls[0].message != "테스트를 실행해줘" || calls[0].cwd != svc.cfg.Projects[0].CanonicalPath {
		t.Fatalf("turn calls = %#v", calls)
	}
	if again, err := svc.HandleCallback(context.Background(), 100, 0, 700, 7, executeToken); err != nil || again == nil || len(app.turnCalls()) != 1 {
		t.Fatalf("double execute response=%#v err=%v calls=%#v", again, err, app.turnCalls())
	}
}

func TestVoiceReplyFreezesExactThreadBeforeDownload(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	svc.voiceTranscriber = &fakeVoiceTranscriber{text: "후속 작업"}
	original := model.Thread{ID: "thread-original-1234", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), original); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: original.ID, TurnID: "turn-original", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{
		original.ID: threadReadPayload(original.ID, "Original", original.CWD, "turn-original", "completed", "done"),
	}
	response, err := svc.HandleVoice(context.Background(), VoiceInput{
		ChatID: 100, UserID: 7, MessageID: 51, ReplyToMessageID: 501, ReceivedAt: svc.now(), FileSize: 3, Duration: 1,
		Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("ogg")), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Text, "대상: 기존 Thread thread-o") {
		t.Fatalf("preview = %q", response.Text)
	}
	if err := svc.RegisterDirectDelivery(context.Background(), 100, 0, 701, response); err != nil {
		t.Fatal(err)
	}
	other := model.Thread{ID: "thread-other-9999", CWD: svc.cfg.Projects[1].CanonicalPath, Status: "completed"}
	seedReply(t, svc, 100, 501, other)
	_, err = svc.HandleCallback(context.Background(), 100, 0, 701, 7, callbackTokenForVoiceButton(t, response, "실행"))
	if err != nil {
		t.Fatal(err)
	}
	calls := app.turnCalls()
	if len(calls) != 1 || calls[0].cwd != svc.cfg.Projects[0].CanonicalPath {
		t.Fatalf("turn calls = %#v, want original frozen project/thread", calls)
	}
	if len(app.threadResumeCalls) != 1 || app.threadResumeCalls[0].threadID != original.ID {
		t.Fatalf("resume calls = %#v, want %s", app.threadResumeCalls, original.ID)
	}
}

func TestVoiceReplyConfirmationPreservesSourceTurn(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	svc.voiceTranscriber = &fakeVoiceTranscriber{text: "후속 작업"}
	parent := model.Thread{ID: "parent", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "turn-2"}
	seedReply(t, svc, 100, 501, parent)
	app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "active", "activeTurnId": "turn-2",
		"turns": []any{map[string]any{"id": "turn-1", "status": "completed"}, map[string]any{"id": "turn-2", "status": "inProgress"}},
	}}}
	preview, err := svc.HandleVoice(context.Background(), VoiceInput{
		ChatID: 100, UserID: 7, MessageID: 51, ReplyToMessageID: 501, ReceivedAt: svc.now(), FileSize: 3, Duration: 1,
		Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("ogg")), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterDirectDelivery(context.Background(), 100, 0, 701, preview); err != nil {
		t.Fatal(err)
	}
	other := model.Thread{ID: "other", CWD: svc.cfg.Projects[1].CanonicalPath, Status: "completed"}
	seedReply(t, svc, 100, 501, other)

	if _, err := svc.HandleCallback(context.Background(), 100, 0, 701, 7, callbackTokenForVoiceButton(t, preview, "실행")); err != nil {
		t.Fatal(err)
	}

	queued, err := svc.store.ClaimPendingCommand(context.Background(), parent.ID)
	if err != nil || queued == nil || queued.SourceThreadID != parent.ID || queued.SourceTurnID != "turn-2" {
		t.Fatalf("queued=%#v err=%v", queued, err)
	}
}

func TestVoiceReplyExecuteUsesContinuationNotice(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	svc.voiceTranscriber = &fakeVoiceTranscriber{text: "후속 작업"}
	parent := model.Thread{ID: "parent-12345678", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-1", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "completed", "source": "vscode", "originator": "Codex Desktop",
		"turns": []any{map[string]any{"id": "turn-1", "status": "completed"}},
	}}}
	app.forkIDs = []string{"child-87654321"}
	preview, err := svc.HandleVoice(context.Background(), VoiceInput{
		ChatID: 100, UserID: 7, MessageID: 51, ReplyToMessageID: 501, ReceivedAt: svc.now(), FileSize: 3, Duration: 1,
		Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("ogg")), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.ThreadID != parent.ID || preview.TurnID != "turn-1" {
		t.Fatalf("preview route thread=%q turn=%q, want frozen parent turn", preview.ThreadID, preview.TurnID)
	}
	if err := svc.RegisterDirectDelivery(context.Background(), 100, 0, 701, preview); err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{}
	svc.SetSender(sender)

	callback, err := svc.HandleCallback(context.Background(), 100, 0, 701, 7, callbackTokenForVoiceButton(t, preview, "실행"))

	if err != nil {
		t.Fatal(err)
	}
	if callback == nil || callback.CallbackText == "" {
		t.Fatalf("callback=%#v", callback)
	}
	if len(sender.edits) != 1 {
		t.Fatalf("edits=%#v, want voice preview edited once", sender.edits)
	}
	want := "PC Codex가 원본 대화를 사용 중이어서, 답변 시점의 문맥을 이어받은 새 대화에서 작업을 시작했습니다.\n\n원본: parent-1 · 이어받은 대화: child-87"
	if !strings.Contains(sender.edits[0].text, want) {
		t.Fatalf("voice execute edit=%q, want continuation notice %q", sender.edits[0].text, want)
	}
	if got := app.callSequence(); !reflect.DeepEqual(got, []string{"thread/fork:" + parent.ID + ":turn-1", "turn/start:child-87654321"}) {
		t.Fatalf("sequence=%v", got)
	}
}

func TestVoicePreviewTextReplyBeforeExecuteStaysFrozenToOriginalTurn(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	svc.voiceTranscriber = &fakeVoiceTranscriber{text: "미리보기 음성"}
	parent := model.Thread{ID: "parent-12345678", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "turn-2"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-1", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "active", "activeTurnId": "turn-2",
		"turns": []any{
			map[string]any{"id": "turn-1", "status": "completed"},
			map[string]any{"id": "turn-2", "status": "inProgress"},
		},
	}}}
	preview, err := svc.HandleVoice(context.Background(), VoiceInput{
		ChatID: 100, UserID: 7, MessageID: 51, ReplyToMessageID: 501, ReceivedAt: svc.now(), FileSize: 3, Duration: 1,
		Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("ogg")), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.ThreadID != parent.ID || preview.TurnID != "turn-1" {
		t.Fatalf("preview route thread=%q turn=%q, want frozen parent turn", preview.ThreadID, preview.TurnID)
	}
	if err := svc.RegisterDirectDelivery(context.Background(), 100, 0, 701, preview); err != nil {
		t.Fatal(err)
	}

	response := submitMinimal(t, svc, model.InboundText{ChatID: 100, UserID: 7, ReplyToMessageID: 701, Text: "텍스트 후속", ReceivedAt: svc.now()})

	if response.ThreadID != parent.ID || response.TurnID == "turn-2" {
		t.Fatalf("response=%#v, want route anchored to completed turn-1 instead of active turn-2", response)
	}
	calls := app.turnCalls()
	if len(calls) != 1 || calls[0].message != "텍스트 후속" {
		t.Fatalf("turn calls=%#v", calls)
	}
}

func TestVoiceReplyReusesContinuationWithNormalNoticeAndUpdatesEditedRoute(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession()
	useRouterSession(svc, app)
	svc.voiceTranscriber = &fakeVoiceTranscriber{text: "첫 음성"}
	parent := model.Thread{ID: "parent-12345678", CWD: svc.cfg.Projects[0].CanonicalPath, Status: "inProgress", ActiveTurnID: "turn-3"}
	if err := svc.store.UpsertThread(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 100, MessageID: 501, ThreadID: parent.ID, TurnID: "turn-2", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	app.threadReads = map[string]map[string]any{parent.ID: {"thread": map[string]any{
		"id": parent.ID, "cwd": parent.CWD, "status": "active", "activeTurnId": "turn-3",
		"turns": []any{
			map[string]any{"id": "turn-2", "status": "completed"},
			map[string]any{"id": "turn-3", "status": "inProgress"},
		},
	}}}
	app.resumeErrByThread = map[string]error{parent.ID: &appserver.RPCError{Code: -32600, Message: "thread " + parent.ID + " already has an active writer"}}
	app.forkIDs = []string{"child-87654321"}
	firstPreview, err := svc.HandleVoice(context.Background(), VoiceInput{
		ChatID: 100, UserID: 7, MessageID: 51, ReplyToMessageID: 501, ReceivedAt: svc.now(), FileSize: 3, Duration: 1,
		Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("ogg")), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterDirectDelivery(context.Background(), 100, 0, 701, firstPreview); err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{}
	svc.SetSender(sender)
	if _, err := svc.HandleCallback(context.Background(), 100, 0, 701, 7, callbackTokenForVoiceButton(t, firstPreview, "실행")); err != nil {
		t.Fatal(err)
	}
	child := model.Thread{ID: "child-87654321", CWD: parent.CWD, Status: "completed"}
	if err := svc.store.UpsertThread(context.Background(), child); err != nil {
		t.Fatal(err)
	}
	app.threadReads[child.ID] = map[string]any{"thread": map[string]any{
		"id": child.ID, "cwd": child.CWD, "status": "completed",
		"turns": []any{map[string]any{"id": "turn-1", "status": "completed"}},
	}}

	svc.voiceTranscriber = &fakeVoiceTranscriber{text: "두 번째 음성"}
	secondPreview, err := svc.HandleVoice(context.Background(), VoiceInput{
		ChatID: 100, UserID: 7, MessageID: 52, ReplyToMessageID: 501, ReceivedAt: svc.now(), FileSize: 3, Duration: 1,
		Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("ogg")), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.RegisterDirectDelivery(context.Background(), 100, 0, 702, secondPreview); err != nil {
		t.Fatal(err)
	}

	callback, err := svc.HandleCallback(context.Background(), 100, 0, 702, 7, callbackTokenForVoiceButton(t, secondPreview, "실행"))

	if err != nil {
		t.Fatal(err)
	}
	if callback == nil || callback.Text != "" || callback.CallbackText == "" {
		t.Fatalf("callback=%#v, want callback-only response after edit", callback)
	}
	if len(sender.messages) != 0 {
		t.Fatalf("voice execution sent duplicate messages: %#v", sender.messages)
	}
	if len(sender.edits) != 2 {
		t.Fatalf("edits=%#v, want first and second previews edited", sender.edits)
	}
	secondEdit := sender.edits[1].text
	if !strings.Contains(secondEdit, "작업을 시작했습니다.") || strings.Contains(secondEdit, "새 대화") || strings.Contains(secondEdit, "이어받은 대화") {
		t.Fatalf("second voice edit=%q, want normal reused start text", secondEdit)
	}
	route, err := svc.store.ResolveMessageRoute(context.Background(), 100, 0, 702)
	if err != nil {
		t.Fatal(err)
	}
	if route == nil || route.ThreadID != child.ID || route.TurnID != "turn-2" {
		t.Fatalf("edited route=%#v, want fork child turn-2", route)
	}
}

func TestVoiceLinkedActiveWriterConflictRetainsNoRejectedTranscriptOrPrompt(t *testing.T) {
	svc, _ := newMinimalService(t)
	live := newRouterSession()
	useRouterSession(svc, live)
	workers := installWorkerFactory(svc, func(index int, worker *routerSession) {
		worker.resumeErrByThread = map[string]error{"linked-1": &appserver.RPCError{Code: -32600, Message: "thread linked-1 already has an active writer"}}
	})
	seedReadyLinkedThread(t, svc, 7, 0, "source-1", "source-turn-1", "linked-1")
	if err := svc.store.PutMessageRoute(context.Background(), model.MessageRoute{ChatID: 7, MessageID: 501, ThreadID: "source-1", TurnID: "source-turn-1", CreatedAt: model.NowString()}); err != nil {
		t.Fatal(err)
	}
	live.threadReads = map[string]map[string]any{"source-1": {"thread": map[string]any{
		"id": "source-1", "cwd": svc.cfg.Projects[0].CanonicalPath, "status": "completed", "source": "vscode", "originator": "Codex Desktop",
		"turns": []any{map[string]any{"id": "source-turn-1", "status": "completed"}},
	}}}
	svc.voiceTranscriber = &fakeVoiceTranscriber{text: "거절된 음성 원문"}
	preview, err := svc.HandleVoice(context.Background(), VoiceInput{
		ChatID: 7, UserID: 7, MessageID: 51, ReplyToMessageID: 501, ReceivedAt: svc.now(), FileSize: 3, Duration: 1,
		Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("ogg")), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if preview.ThreadID != "source-1" || preview.TurnID != "source-turn-1" {
		t.Fatalf("preview route thread=%q turn=%q, want source route", preview.ThreadID, preview.TurnID)
	}
	if err := svc.RegisterDirectDelivery(context.Background(), 7, 0, 701, preview); err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{}
	svc.SetSender(sender)

	callback, err := svc.HandleCallback(context.Background(), 7, 0, 701, 7, callbackTokenForVoiceButton(t, preview, "실행"))
	if err != nil {
		t.Fatal(err)
	}
	if callback == nil || callback.CallbackText == "" {
		t.Fatalf("callback=%#v, want callback acknowledgement", callback)
	}
	if len(sender.edits) != 1 || !strings.Contains(sender.edits[0].text, "Codex에서") || !strings.Contains(sender.edits[0].text, "Codex 앱을 종료") || !strings.Contains(sender.edits[0].text, "다시 시도") {
		t.Fatalf("voice conflict edit=%#v", sender.edits)
	}
	worker := workers.Single(t)
	if got := worker.callSequence(); !reflect.DeepEqual(got, []string{"thread/resume:linked-1"}) {
		t.Fatalf("worker calls=%#v, want linked resume only", got)
	}
	if !worker.Closed() {
		t.Fatal("blocked voice worker was not closed")
	}
	if queued, err := svc.store.ClaimPendingCommand(context.Background(), "linked-1"); err != nil || queued != nil {
		t.Fatalf("linked queue after rejected voice=%#v err=%v", queued, err)
	}
	db, err := sql.Open("sqlite", svc.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var transcript sql.NullString
	var status string
	if err := db.QueryRow(`SELECT transcript_payload,status FROM voice_confirmations WHERE id=?`, preview.VoiceConfirmationID).Scan(&transcript, &status); err != nil {
		t.Fatal(err)
	}
	if transcript.Valid && transcript.String != "" {
		t.Fatal("rejected voice transcript remained in voice confirmation storage")
	}
	if status != model.VoiceStatusExecuted {
		t.Fatalf("voice confirmation status=%q, want executed consumed state", status)
	}
}

func TestVoiceCancelNeverRunsAndRemovesButtons(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession("unused")
	useRouterSession(svc, app)
	svc.voiceTranscriber = &fakeVoiceTranscriber{text: "취소할 작업"}
	mustSelect(t, svc, 100, "bridge")
	response := previewVoice(t, svc, 100, 52)
	if err := svc.RegisterDirectDelivery(context.Background(), 100, 0, 702, response); err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{}
	svc.SetSender(sender)
	callback, err := svc.HandleCallback(context.Background(), 100, 0, 702, 7, callbackTokenForVoiceButton(t, response, "취소"))
	if err != nil {
		t.Fatal(err)
	}
	if len(app.turnCalls()) != 0 || len(sender.edits) != 1 || len(sender.edits[0].buttons) != 0 {
		t.Fatalf("calls=%#v edits=%#v", app.turnCalls(), sender.edits)
	}
	if callback == nil || callback.Text != "" || callback.CallbackText == "" {
		t.Fatalf("callback = %#v", callback)
	}
}

func TestVoiceRejectsUnauthorizedStaleAndBoundsBeforeDownload(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name     string
		userID   int64
		received time.Time
		fileSize int64
		duration int
		wantAuth bool
	}{
		{name: "unauthorized", userID: 999, received: minimalTestNow, fileSize: 1, duration: 1, wantAuth: true},
		{name: "stale", userID: 7, received: minimalTestNow.Add(-11 * time.Minute), fileSize: 1, duration: 1},
		{name: "size", userID: 7, received: minimalTestNow, fileSize: transcription.MaxAudioBytes + 1, duration: 1},
		{name: "duration", userID: 7, received: minimalTestNow, fileSize: 1, duration: transcription.MaxAudioDuration + 1},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			svc, _ := newMinimalService(t)
			svc.voiceTranscriber = &fakeVoiceTranscriber{text: "unused"}
			mustSelect(t, svc, 100, "bridge")
			opens := 0
			response, err := svc.HandleVoice(context.Background(), VoiceInput{
				ChatID: 100, UserID: test.userID, ReceivedAt: test.received, FileSize: test.fileSize, Duration: test.duration,
				Open: func(context.Context) (io.ReadCloser, error) {
					opens++
					return io.NopCloser(strings.NewReader("ogg")), nil
				},
			})
			if test.wantAuth && !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("err = %v, want unauthorized", err)
			}
			if !test.wantAuth && (err != nil || response == nil) {
				t.Fatalf("response=%#v err=%v", response, err)
			}
			if opens != 0 {
				t.Fatalf("download opened %d times", opens)
			}
		})
	}
}

func TestVoiceDownloadAndTranscriptionFailuresCloseAndNeverRun(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name          string
		openErr       error
		transcribeErr error
	}{
		{name: "download", openErr: errors.New("download failed")},
		{name: "transcription", transcribeErr: errors.New("transcribe failed")},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			svc, _ := newMinimalService(t)
			app := newRouterSession("unused")
			useRouterSession(svc, app)
			svc.voiceTranscriber = &fakeVoiceTranscriber{text: "unused", err: test.transcribeErr}
			mustSelect(t, svc, 100, "bridge")
			body := &trackingVoiceBody{Reader: strings.NewReader("ogg")}
			response, err := svc.HandleVoice(context.Background(), VoiceInput{
				ChatID: 100, UserID: 7, ReceivedAt: svc.now(), FileSize: 3, Duration: 1,
				Open: func(context.Context) (io.ReadCloser, error) {
					if test.openErr != nil {
						return nil, test.openErr
					}
					return body, nil
				},
			})
			if err != nil || response == nil || !strings.Contains(response.Text, "텍스트") {
				t.Fatalf("response=%#v err=%v, want safe retry guidance", response, err)
			}
			if test.openErr == nil && !body.closed {
				t.Fatal("download body was not closed")
			}
			if len(app.turnCalls()) != 0 {
				t.Fatal("failed voice ran Codex")
			}
		})
	}
}

func TestVoiceExecuteRouterFailureStaysConsumedAndInactive(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession("voice-fail")
	app.fail["will fail"] = errors.New("remote outcome unknown")
	useRouterSession(svc, app)
	svc.voiceTranscriber = &fakeVoiceTranscriber{text: "will fail"}
	mustSelect(t, svc, 100, "bridge")
	response := previewVoice(t, svc, 100, 53)
	if err := svc.RegisterDirectDelivery(context.Background(), 100, 0, 703, response); err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{}
	svc.SetSender(sender)
	token := callbackTokenForVoiceButton(t, response, "실행")
	callback, err := svc.HandleCallback(context.Background(), 100, 0, 703, 7, token)
	if err != nil {
		t.Fatal(err)
	}
	if callback == nil || callback.CallbackText == "" || len(sender.edits) != 1 || len(sender.edits[0].buttons) != 0 || !strings.Contains(sender.edits[0].text, "다시") {
		t.Fatalf("callback=%#v edits=%#v", callback, sender.edits)
	}
	if _, err := svc.HandleCallback(context.Background(), 100, 0, 703, 7, token); err != nil {
		t.Fatal(err)
	}
	if len(app.turnCalls()) != 1 {
		t.Fatalf("turn calls = %#v, want one ambiguous start only", app.turnCalls())
	}
}

func TestVoiceCallbackExpiryNeverRunsAndRemovesButtons(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession("unused")
	useRouterSession(svc, app)
	svc.voiceTranscriber = &fakeVoiceTranscriber{text: "expired command"}
	mustSelect(t, svc, 100, "bridge")
	response := previewVoice(t, svc, 100, 54)
	if err := svc.RegisterDirectDelivery(context.Background(), 100, 0, 704, response); err != nil {
		t.Fatal(err)
	}
	sender := &recordingSender{}
	svc.SetSender(sender)
	svc.now = func() time.Time { return minimalTestNow.Add(11 * time.Minute) }
	callback, err := svc.HandleCallback(context.Background(), 100, 0, 704, 7, callbackTokenForVoiceButton(t, response, "실행"))
	if err != nil {
		t.Fatal(err)
	}
	if callback == nil || !strings.Contains(callback.CallbackText, "만료") || len(app.turnCalls()) != 0 {
		t.Fatalf("callback=%#v calls=%#v", callback, app.turnCalls())
	}
	if len(sender.edits) != 1 || len(sender.edits[0].buttons) != 0 || !strings.Contains(sender.edits[0].text, "만료") {
		t.Fatalf("edits = %#v", sender.edits)
	}
}

func TestVoiceRestartReplayNeverRunsAndRemovesButtons(t *testing.T) {
	svc, _ := newMinimalService(t)
	app := newRouterSession("unused")
	useRouterSession(svc, app)
	svc.voiceTranscriber = &fakeVoiceTranscriber{text: "restart command"}
	mustSelect(t, svc, 100, "bridge")
	response := previewVoice(t, svc, 100, 55)
	if err := svc.RegisterDirectDelivery(context.Background(), 100, 0, 705, response); err != nil {
		t.Fatal(err)
	}
	if recovered, err := svc.store.RecoverVoiceConfirmations(context.Background()); err != nil || recovered != 1 {
		t.Fatalf("RecoverVoiceConfirmations = %d, %v", recovered, err)
	}
	sender := &recordingSender{}
	svc.SetSender(sender)
	callback, err := svc.HandleCallback(context.Background(), 100, 0, 705, 7, callbackTokenForVoiceButton(t, response, "실행"))
	if err != nil {
		t.Fatal(err)
	}
	if callback == nil || !strings.Contains(callback.CallbackText, "만료") || len(app.turnCalls()) != 0 {
		t.Fatalf("callback=%#v calls=%#v", callback, app.turnCalls())
	}
	if len(sender.edits) != 1 || len(sender.edits[0].buttons) != 0 || !strings.Contains(sender.edits[0].text, "만료") {
		t.Fatalf("edits = %#v", sender.edits)
	}
}

func previewVoice(t *testing.T, svc *Service, chatID, messageID int64) *DirectResponse {
	t.Helper()
	response, err := svc.HandleVoice(context.Background(), VoiceInput{
		ChatID: chatID, UserID: 7, MessageID: messageID, ReceivedAt: svc.now(), FileSize: 3, Duration: 1,
		Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("ogg")), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func callbackTokenForVoiceButton(t *testing.T, response *DirectResponse, label string) string {
	t.Helper()
	for _, row := range response.Buttons {
		for _, button := range row {
			if button.Text == label {
				return button.CallbackData
			}
		}
	}
	t.Fatalf("button %q not found in %#v", label, response.Buttons)
	return ""
}
