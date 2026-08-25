package transcription

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestOpenAITranscribePostsMultipartAudioAndModel(t *testing.T) {
	t.Parallel()

	const apiKey = "PRIVATE_OPENAI_KEY_d38c17"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Method, http.MethodPost; got != want {
			t.Fatalf("method = %q, want %q", got, want)
		}
		if got, want := r.URL.Path, "/v1/audio/transcriptions"; got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
		if got, want := r.Header.Get("Authorization"), "Bearer "+apiKey; got != want {
			t.Fatalf("authorization header mismatch")
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatal(err)
		}
		if got, want := r.FormValue("model"), "gpt-transcribe"; got != want {
			t.Fatalf("model = %q, want %q", got, want)
		}
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		data, err := io.ReadAll(file)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := string(data), "converted audio"; got != want {
			t.Fatalf("audio = %q, want %q", got, want)
		}
		if got, want := header.Filename, "voice.mp3"; got != want {
			t.Fatalf("filename = %q, want %q", got, want)
		}
		if got, want := header.Header.Get("Content-Type"), "audio/mpeg"; got != want {
			t.Fatalf("content type = %q, want %q", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"테스트를 실행해줘"}`))
	}))
	defer server.Close()

	client := NewOpenAI(apiKey, "gpt-transcribe")
	if got, want := client.http.Timeout, 90*time.Second; got != want {
		t.Fatalf("HTTP timeout = %s, want %s", got, want)
	}
	client.baseURL = server.URL
	transcript, err := client.Transcribe(context.Background(), strings.NewReader("converted audio"), Meta{
		FileName: "voice.mp3", ContentType: "audio/mpeg",
	})
	if err != nil {
		t.Fatalf("Transcribe failed: %v", err)
	}
	if got, want := transcript, "테스트를 실행해줘"; got != want {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
}

func TestOpenAITranscribeRejectsHTTPFailuresWithoutResponseBody(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		status := status
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Parallel()
			const privateMarker = "PRIVATE_OPENAI_RESPONSE_751ea9"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, privateMarker, status)
			}))
			defer server.Close()

			client := NewOpenAI("private-key", "gpt-transcribe")
			client.baseURL = server.URL
			_, err := client.Transcribe(context.Background(), strings.NewReader("audio"), Meta{FileName: "voice.mp3", ContentType: "audio/mpeg"})
			if err == nil {
				t.Fatalf("Transcribe succeeded for HTTP %d", status)
			}
			if strings.Contains(err.Error(), privateMarker) || strings.Contains(err.Error(), "private-key") {
				t.Fatalf("error leaked private material: %v", err)
			}
			if !strings.Contains(err.Error(), http.StatusText(status)) {
				t.Fatalf("error = %v, want sanitized status", err)
			}
		})
	}
}

func TestOpenAITranscribeTimesOut(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"text":"late"}`))
	}))
	defer server.Close()

	client := NewOpenAI("private-key", "gpt-transcribe")
	client.baseURL = server.URL
	client.http.Timeout = 20 * time.Millisecond
	_, err := client.Transcribe(context.Background(), strings.NewReader("audio"), Meta{FileName: "voice.mp3", ContentType: "audio/mpeg"})
	if err == nil || !strings.Contains(err.Error(), "transcription request failed") {
		t.Fatalf("error = %v, want sanitized timeout", err)
	}
}

func TestOpenAITranscribeValidatesTranscript(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{name: "empty", body: `{"text":"  "}`},
		{name: "too long", body: `{"text":"` + strings.Repeat("가", MaxTranscriptRunes+1) + `"}`},
		{name: "invalid utf8", body: "{\"text\":\"\xff\"}"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()

			client := NewOpenAI("private-key", "gpt-transcribe")
			client.baseURL = server.URL
			if _, err := client.Transcribe(context.Background(), strings.NewReader("audio"), Meta{FileName: "voice.mp3", ContentType: "audio/mpeg"}); err == nil {
				t.Fatal("Transcribe accepted invalid transcript")
			}
		})
	}
}

func TestOpenAITranscribeRequiresAPIKeyWithoutSending(t *testing.T) {
	t.Parallel()

	client := NewOpenAI("", "gpt-transcribe")
	if _, err := client.Transcribe(context.Background(), strings.NewReader("audio"), Meta{FileName: "voice.mp3", ContentType: "audio/mpeg"}); err == nil {
		t.Fatal("Transcribe succeeded without an API key")
	}
}
