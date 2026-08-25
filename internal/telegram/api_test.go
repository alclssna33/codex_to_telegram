package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/alclssna33/codex_to_telegram/internal/model"
)

func TestUpdateMessageDecodesVoiceMetadata(t *testing.T) {
	t.Parallel()

	var update Update
	if err := json.Unmarshal([]byte(`{"update_id":1,"message":{"message_id":2,"date":1787198400,"from":{"id":7},"chat":{"id":7,"type":"private"},"voice":{"file_id":"voice-file","file_unique_id":"voice-unique","duration":42,"file_size":1234,"mime_type":"audio/ogg"}}}`), &update); err != nil {
		t.Fatal(err)
	}
	if update.Message == nil || update.Message.Voice == nil {
		t.Fatalf("message = %#v, want voice metadata", update.Message)
	}
	if got, want := *update.Message.Voice, (Voice{FileID: "voice-file", FileUniqueID: "voice-unique", Duration: 42, FileSize: 1234, MimeType: "audio/ogg"}); got != want {
		t.Fatalf("voice = %#v, want %#v", got, want)
	}
}

func TestClientGetFileAndDownloadFileStream(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/getFile":
			var request map[string]string
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if got, want := request["file_id"], "voice-file"; got != want {
				t.Fatalf("file_id = %q, want %q", got, want)
			}
			_, _ = w.Write([]byte(`{"ok":true,"result":{"file_id":"voice-file","file_path":"voice/clip.oga"}}`))
		case "/file/voice/clip.oga":
			w.Header().Set("Content-Type", "audio/ogg")
			_, _ = w.Write([]byte("audio bytes"))
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient("token")
	client.baseURL = server.URL
	client.fileBaseURL = server.URL + "/file"
	file, err := client.GetFile(context.Background(), "voice-file")
	if err != nil {
		t.Fatalf("GetFile failed: %v", err)
	}
	if file == nil || file.FilePath != "voice/clip.oga" {
		t.Fatalf("file = %#v", file)
	}
	body, err := client.DownloadFile(context.Background(), file.FilePath)
	if err != nil {
		t.Fatalf("DownloadFile failed: %v", err)
	}
	data, readErr := io.ReadAll(body)
	closeErr := body.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read=%v close=%v", readErr, closeErr)
	}
	if got, want := string(data), "audio bytes"; got != want {
		t.Fatalf("audio = %q, want %q", got, want)
	}
}

func TestClientDownloadFileRejectsHTTPErrorWithoutPrivateResponse(t *testing.T) {
	t.Parallel()

	const privateMarker = "PRIVATE_FILE_PATH_OR_TOKEN_4cb893"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, privateMarker, http.StatusUnauthorized)
	}))
	defer server.Close()

	client := NewClient("token")
	client.fileBaseURL = server.URL
	body, err := client.DownloadFile(context.Background(), "voice/private.oga")
	if body != nil {
		_ = body.Close()
		t.Fatal("DownloadFile returned a body for non-2xx")
	}
	if err == nil {
		t.Fatal("DownloadFile succeeded for 401")
	}
	if strings.Contains(err.Error(), privateMarker) || strings.Contains(err.Error(), "voice/private.oga") || strings.Contains(err.Error(), "token") {
		t.Fatalf("error leaked private response or path: %v", err)
	}
	if !errors.Is(err, ErrFileDownload) {
		t.Fatalf("error = %v, want ErrFileDownload", err)
	}
}

func TestClientDownloadFileRejectsUntrustedPathsWithoutRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
	}{
		{name: "empty", path: ""},
		{name: "root slash", path: "/"},
		{name: "leading slash", path: "/voice/clip.oga"},
		{name: "dot", path: "."},
		{name: "dot dot", path: ".."},
		{name: "dot segment", path: "voice/./clip.oga"},
		{name: "dot dot segment", path: "voice/../clip.oga"},
		{name: "leading dot segment", path: "./voice.oga"},
		{name: "leading dot dot segment", path: "../voice.oga"},
		{name: "encoded dot", path: "%2e/voice.oga"},
		{name: "encoded dot dot", path: "%2E%2E/voice.oga"},
		{name: "nested encoded dot dot", path: "voice/%2e%2e/clip.oga"},
		{name: "double encoded dot dot", path: "%252e%252e/voice.oga"},
		{name: "escaped ordinary byte", path: "voice/%41.oga"},
		{name: "absolute URL", path: "https://evil.example/voice.oga"},
		{name: "host form", path: "//evil.example/voice.oga"},
		{name: "scheme form", path: "file:voice.oga"},
		{name: "backslash", path: `voice\clip.oga`},
		{name: "query", path: "voice/clip.oga?download=1"},
		{name: "fragment", path: "voice/clip.oga#private"},
		{name: "control NUL", path: "voice/clip\x00.oga"},
		{name: "control newline", path: "voice/clip\n.oga"},
		{name: "empty segment", path: "voice//clip.oga"},
		{name: "trailing slash", path: "voice/"},
		{name: "surrounding whitespace", path: " voice/clip.oga "},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			client := NewClient("PRIVATE_TOKEN_must_not_leak")
			client.fileBaseURL = server.URL + "/file"
			body, err := client.DownloadFile(context.Background(), test.path)
			if body != nil {
				_ = body.Close()
				t.Fatal("DownloadFile returned a body for an untrusted path")
			}
			if !errors.Is(err, ErrFileDownload) {
				t.Fatalf("error = %v, want ErrFileDownload", err)
			}
			if err != nil && (test.path != "" && strings.Contains(err.Error(), test.path) || strings.Contains(err.Error(), "PRIVATE_TOKEN")) {
				t.Fatalf("error leaked untrusted path or token: %v", err)
			}
			if requests != 0 {
				t.Fatalf("untrusted path made %d HTTP requests", requests)
			}
		})
	}
}

func TestClientDownloadFileEscapesValidRelativePathSegments(t *testing.T) {
	t.Parallel()

	var requestURI string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestURI = r.RequestURI
		_, _ = w.Write([]byte("audio"))
	}))
	defer server.Close()

	client := NewClient("token")
	client.fileBaseURL = server.URL + "/file"
	body, err := client.DownloadFile(context.Background(), "voice clips/안녕.oga")
	if err != nil {
		t.Fatal(err)
	}
	if closeErr := body.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if got, want := requestURI, "/file/voice%20clips/%EC%95%88%EB%85%95.oga"; got != want {
		t.Fatalf("request URI = %q, want %q", got, want)
	}
}

func TestUpdateMessageDecodesTelegramServerDate(t *testing.T) {
	t.Parallel()

	var update Update
	if err := json.Unmarshal([]byte(`{"update_id":1,"message":{"message_id":2,"date":1787198400,"from":{"id":7},"chat":{"id":7,"type":"private"},"text":"run tests"}}`), &update); err != nil {
		t.Fatal(err)
	}
	if update.Message == nil || update.Message.Date != 1787198400 {
		t.Fatalf("message = %#v, want Telegram date 1787198400", update.Message)
	}
}

func TestClientEditMessageTextSendsExpectedJSON(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/editMessageText" {
			t.Fatalf("path = %q, want /editMessageText", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("json.Unmarshal failed: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":77,"chat":{"id":42,"type":"private"},"text":"updated"}}`))
	}))
	defer server.Close()

	client := NewClient("token")
	client.baseURL = server.URL
	message, err := client.EditMessageText(context.Background(), 42, 77, "updated", &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{{{Text: "Open", CallbackData: "cb-1"}}},
	})
	if err != nil {
		t.Fatalf("EditMessageText failed: %v", err)
	}
	if message == nil || message.MessageID != 77 {
		t.Fatalf("message = %#v, want message_id=77", message)
	}
	if got, want := captured["chat_id"], float64(42); got != want {
		t.Fatalf("chat_id = %#v, want %#v", got, want)
	}
	if got, want := captured["message_id"], float64(77); got != want {
		t.Fatalf("message_id = %#v, want %#v", got, want)
	}
	if got, want := captured["text"], "updated"; got != want {
		t.Fatalf("text = %#v, want %q", got, want)
	}
	if got, ok := captured["disable_web_page_preview"].(bool); !ok || !got {
		t.Fatalf("disable_web_page_preview = %#v, want true", captured["disable_web_page_preview"])
	}
	if _, ok := captured["parse_mode"]; ok {
		t.Fatalf("parse_mode = %#v, want omitted for plain text", captured["parse_mode"])
	}
}

func TestClientSendMessageUsesHTMLParseModeForMixedCodeBlock(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sendMessage" {
			t.Fatalf("path = %q, want /sendMessage", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("json.Unmarshal failed: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":78,"chat":{"id":42,"type":"private"},"text":"[Tool]"}}`))
	}))
	defer server.Close()

	client := NewClient("token")
	client.baseURL = server.URL
	message, err := client.SendMessage(context.Background(), 42, 0, `[Tool]
<pre><code class="language-powershell">Status: completed</code></pre>`, nil, model.SendOptions{})
	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}
	if message == nil || message.MessageID != 78 {
		t.Fatalf("message = %#v, want message_id=78", message)
	}
	if got, want := captured["parse_mode"], "HTML"; got != want {
		t.Fatalf("parse_mode = %#v, want %q", got, want)
	}
	if _, ok := captured["disable_notification"]; ok {
		t.Fatalf("disable_notification = %#v, want omitted for audible message", captured["disable_notification"])
	}
}

func TestClientSendMessageSilentSetsDisableNotification(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sendMessage" {
			t.Fatalf("path = %q, want /sendMessage", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("json.Unmarshal failed: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":79,"chat":{"id":42,"type":"private"},"text":"silent"}}`))
	}))
	defer server.Close()

	client := NewClient("token")
	client.baseURL = server.URL
	if _, err := client.SendMessage(context.Background(), 42, 0, "silent", nil, model.SendOptions{Silent: true}); err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}
	if got, ok := captured["disable_notification"].(bool); !ok || !got {
		t.Fatalf("disable_notification = %#v, want true", captured["disable_notification"])
	}
}

func TestClientDeleteMessageSendsExpectedJSON(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/deleteMessage" {
			t.Fatalf("path = %q, want /deleteMessage", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("json.Unmarshal failed: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":true}`))
	}))
	defer server.Close()

	client := NewClient("token")
	client.baseURL = server.URL
	if err := client.DeleteMessage(context.Background(), 42, 77); err != nil {
		t.Fatalf("DeleteMessage failed: %v", err)
	}
	if got, want := captured["chat_id"], float64(42); got != want {
		t.Fatalf("chat_id = %#v, want %#v", got, want)
	}
	if got, want := captured["message_id"], float64(77); got != want {
		t.Fatalf("message_id = %#v, want %#v", got, want)
	}
}

func TestClientSendRenderedMessageUsesEntitiesWithoutParseMode(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sendMessage" {
			t.Fatalf("path = %q, want /sendMessage", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("json.Unmarshal failed: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":80,"chat":{"id":42,"type":"private"},"text":"formatted"}}`))
	}))
	defer server.Close()

	client := NewClient("token")
	client.baseURL = server.URL
	message, err := client.SendRenderedMessage(context.Background(), 42, 0, model.RenderedMessage{
		Text: "formatted",
		Entities: []model.MessageEntity{{
			Type:     "pre",
			Offset:   0,
			Length:   9,
			Language: "bash",
		}},
	}, nil, model.SendOptions{})
	if err != nil {
		t.Fatalf("SendRenderedMessage failed: %v", err)
	}
	if message == nil || message.MessageID != 80 {
		t.Fatalf("message = %#v, want message_id=80", message)
	}
	if _, ok := captured["parse_mode"]; ok {
		t.Fatalf("parse_mode = %#v, want omitted when entities are supplied", captured["parse_mode"])
	}
	entities, ok := captured["entities"].([]any)
	if !ok || len(entities) != 1 {
		t.Fatalf("entities = %#v, want one entity", captured["entities"])
	}
	entity, ok := entities[0].(map[string]any)
	if !ok {
		t.Fatalf("entity = %#v, want object", entities[0])
	}
	if got, want := entity["type"], "pre"; got != want {
		t.Fatalf("entity.type = %#v, want %q", got, want)
	}
	if got, want := entity["language"], "bash"; got != want {
		t.Fatalf("entity.language = %#v, want %q", got, want)
	}
}

func TestClientEditMessageTextUsesHTMLParseModeForMixedCodeBlock(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/editMessageText" {
			t.Fatalf("path = %q, want /editMessageText", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("json.Unmarshal failed: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":79,"chat":{"id":42,"type":"private"},"text":"[Output]"}}`))
	}))
	defer server.Close()

	client := NewClient("token")
	client.baseURL = server.URL
	message, err := client.EditMessageText(context.Background(), 42, 79, `[Output]
<pre><code class="language-bash">hello</code></pre>`, nil)
	if err != nil {
		t.Fatalf("EditMessageText failed: %v", err)
	}
	if message == nil || message.MessageID != 79 {
		t.Fatalf("message = %#v, want message_id=79", message)
	}
	if got, want := captured["parse_mode"], "HTML"; got != want {
		t.Fatalf("parse_mode = %#v, want %q", got, want)
	}
}

func TestClientEditRenderedMessageTextUsesEntitiesWithoutParseMode(t *testing.T) {
	t.Parallel()

	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/editMessageText" {
			t.Fatalf("path = %q, want /editMessageText", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll failed: %v", err)
		}
		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("json.Unmarshal failed: %v", err)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":81,"chat":{"id":42,"type":"private"},"text":"updated"}}`))
	}))
	defer server.Close()

	client := NewClient("token")
	client.baseURL = server.URL
	message, err := client.EditRenderedMessageText(context.Background(), 42, 81, model.RenderedMessage{
		Text:     "updated",
		Entities: []model.MessageEntity{{Type: "code", Offset: 0, Length: 7}},
	}, nil)
	if err != nil {
		t.Fatalf("EditRenderedMessageText failed: %v", err)
	}
	if message == nil || message.MessageID != 81 {
		t.Fatalf("message = %#v, want message_id=81", message)
	}
	if _, ok := captured["parse_mode"]; ok {
		t.Fatalf("parse_mode = %#v, want omitted when entities are supplied", captured["parse_mode"])
	}
	if entities, ok := captured["entities"].([]any); !ok || len(entities) != 1 {
		t.Fatalf("entities = %#v, want one entity", captured["entities"])
	}
}

func TestClientSendDocumentUsesMultipartForm(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sendDocument" {
			t.Fatalf("path = %q, want /sendDocument", r.URL.Path)
		}
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("ParseMediaType failed: %v", err)
		}
		if mediaType != "multipart/form-data" {
			t.Fatalf("mediaType = %q, want multipart/form-data", mediaType)
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		fields := map[string]string{}
		var documentName, documentBody, documentType string
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("NextPart failed: %v", err)
			}
			data, err := io.ReadAll(part)
			if err != nil {
				t.Fatalf("ReadAll(part) failed: %v", err)
			}
			if part.FormName() == "document" {
				documentName = part.FileName()
				documentBody = string(data)
				documentType = part.Header.Get("Content-Type")
				continue
			}
			fields[part.FormName()] = string(data)
		}
		if got, want := fields["chat_id"], "42"; got != want {
			t.Fatalf("chat_id = %q, want %q", got, want)
		}
		if got, want := fields["message_thread_id"], "9"; got != want {
			t.Fatalf("message_thread_id = %q, want %q", got, want)
		}
		if got, want := fields["caption"], "observer dump"; got != want {
			t.Fatalf("caption = %q, want %q", got, want)
		}
		if !strings.Contains(fields["reply_markup"], `"callback_data":"cb-1"`) {
			t.Fatalf("reply_markup = %q, want callback data", fields["reply_markup"])
		}
		if _, ok := fields["disable_notification"]; ok {
			t.Fatalf("disable_notification = %q, want omitted for audible document", fields["disable_notification"])
		}
		if got, want := documentName, "observer.txt"; got != want {
			t.Fatalf("filename = %q, want %q", got, want)
		}
		if got, want := documentType, "text/plain"; got != want {
			t.Fatalf("content-type = %q, want %q", got, want)
		}
		if got, want := documentBody, "payload"; got != want {
			t.Fatalf("document body = %q, want %q", got, want)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":501,"chat":{"id":42,"type":"private"}}}`))
	}))
	defer server.Close()

	client := NewClient("token")
	client.baseURL = server.URL
	message, err := client.SendDocument(context.Background(), 42, 9, DocumentFile{
		Name:        "observer.txt",
		ContentType: "text/plain",
		Data:        []byte("payload"),
	}, "observer dump", &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{{{Text: "Open", CallbackData: "cb-1"}}},
	}, model.SendOptions{})
	if err != nil {
		t.Fatalf("SendDocument failed: %v", err)
	}
	if message == nil || message.MessageID != 501 {
		t.Fatalf("message = %#v, want message_id=501", message)
	}
}

func TestClientSendDocumentSilentSetsDisableNotification(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sendDocument" {
			t.Fatalf("path = %q, want /sendDocument", r.URL.Path)
		}
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("ParseMediaType failed: %v", err)
		}
		reader := multipart.NewReader(r.Body, params["boundary"])
		fields := map[string]string{}
		for {
			part, err := reader.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("NextPart failed: %v", err)
			}
			data, err := io.ReadAll(part)
			if err != nil {
				t.Fatalf("ReadAll(part) failed: %v", err)
			}
			if part.FormName() != "document" {
				fields[part.FormName()] = string(data)
			}
		}
		if got, want := fields["disable_notification"], "true"; got != want {
			t.Fatalf("disable_notification = %q, want %q", got, want)
		}
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":502,"chat":{"id":42,"type":"private"}}}`))
	}))
	defer server.Close()

	client := NewClient("token")
	client.baseURL = server.URL
	if _, err := client.SendDocument(context.Background(), 42, 0, DocumentFile{
		Name: "silent.txt",
		Data: []byte("payload"),
	}, "", nil, model.SendOptions{Silent: true}); err != nil {
		t.Fatalf("SendDocument failed: %v", err)
	}
}
