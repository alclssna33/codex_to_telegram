package transcription

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type recordingAudioTranscriber struct {
	text string
	err  error
	data []byte
	meta Meta
}

func (f *recordingAudioTranscriber) Transcribe(_ context.Context, audio io.Reader, meta Meta) (string, error) {
	f.meta = meta
	f.data, _ = io.ReadAll(audio)
	return f.text, f.err
}

func TestServiceTranscribeConvertsWithArgvAndCleansTempDirectory(t *testing.T) {
	t.Parallel()

	tempRoot := t.TempDir()
	openai := &recordingAudioTranscriber{text: "테스트를 실행해줘"}
	service := NewService("ffmpeg", openai)
	service.tempRoot = tempRoot
	var gotBin string
	var gotArgs []string
	service.run = func(_ context.Context, bin string, args []string) error {
		gotBin = bin
		gotArgs = append([]string(nil), args...)
		inputPath := valueAfter(args, "-i")
		input, err := os.ReadFile(inputPath)
		if err != nil {
			return err
		}
		if got, want := string(input), "telegram ogg"; got != want {
			t.Fatalf("input = %q, want %q", got, want)
		}
		return os.WriteFile(args[len(args)-1], []byte("converted mp3"), 0o600)
	}

	transcript, err := service.Transcribe(context.Background(), strings.NewReader("telegram ogg"), Meta{
		FileName: "source.oga", ContentType: "audio/ogg", Size: 12, Duration: 9,
	})
	if err != nil {
		t.Fatalf("Transcribe failed: %v", err)
	}
	if got, want := transcript, openai.text; got != want {
		t.Fatalf("transcript = %q, want %q", got, want)
	}
	if gotBin != "ffmpeg" {
		t.Fatalf("bin = %q, want ffmpeg", gotBin)
	}
	if len(gotArgs) == 0 || gotArgs[0] != "-nostdin" || valueAfter(gotArgs, "-i") == "" {
		t.Fatalf("ffmpeg argv = %#v", gotArgs)
	}
	if !bytes.Equal(openai.data, []byte("converted mp3")) {
		t.Fatalf("uploaded audio = %q", openai.data)
	}
	if openai.meta.FileName != "voice.mp3" || openai.meta.ContentType != "audio/mpeg" {
		t.Fatalf("upload meta = %#v", openai.meta)
	}
	assertDirectoryEmpty(t, tempRoot)
}

func TestServiceTranscribeRejectsMetadataLimitsBeforeReading(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		meta Meta
	}{
		{name: "size", meta: Meta{Size: MaxAudioBytes + 1, Duration: 1}},
		{name: "duration", meta: Meta{Size: 1, Duration: MaxAudioDuration + 1}},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reader := &countingReader{reader: strings.NewReader("must not read")}
			service := NewService("ffmpeg", &recordingAudioTranscriber{text: "unused"})
			service.tempRoot = t.TempDir()
			if _, err := service.Transcribe(context.Background(), reader, test.meta); err == nil {
				t.Fatal("Transcribe accepted oversized metadata")
			}
			if reader.reads != 0 {
				t.Fatalf("reader calls = %d, want 0", reader.reads)
			}
		})
	}
}

func TestServiceTranscribeEnforcesStreamingSizeLimitAndCleans(t *testing.T) {
	t.Parallel()

	tempRoot := t.TempDir()
	service := NewService("ffmpeg", &recordingAudioTranscriber{text: "unused"})
	service.tempRoot = tempRoot
	runnerCalled := false
	service.run = func(context.Context, string, []string) error {
		runnerCalled = true
		return nil
	}
	reader := io.LimitReader(zeroReader{}, MaxAudioBytes+1)
	if _, err := service.Transcribe(context.Background(), reader, Meta{Size: 1, Duration: 1}); err == nil {
		t.Fatal("Transcribe accepted oversized stream")
	}
	if runnerCalled {
		t.Fatal("ffmpeg ran for oversized stream")
	}
	assertDirectoryEmpty(t, tempRoot)
}

func TestServiceTranscribeCleansOnFFmpegAndOpenAIFailures(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		runErr     error
		openaiErr  error
		writeAudio bool
	}{
		{name: "ffmpeg nonzero", runErr: errors.New("exit status 1")},
		{name: "openai", openaiErr: errors.New("upstream failed"), writeAudio: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			tempRoot := t.TempDir()
			service := NewService("ffmpeg", &recordingAudioTranscriber{text: "unused", err: test.openaiErr})
			service.tempRoot = tempRoot
			service.run = func(_ context.Context, _ string, args []string) error {
				if test.writeAudio {
					if err := os.WriteFile(args[len(args)-1], []byte("converted"), 0o600); err != nil {
						return err
					}
				}
				return test.runErr
			}
			if _, err := service.Transcribe(context.Background(), strings.NewReader("ogg"), Meta{Size: 3, Duration: 1}); err == nil {
				t.Fatal("Transcribe succeeded")
			}
			assertDirectoryEmpty(t, tempRoot)
		})
	}
}

func TestServiceTranscribeReportsMissingAndNonzeroFFmpegWithoutTempPath(t *testing.T) {
	for _, bin := range []string{filepath.Join(t.TempDir(), "missing-ffmpeg.exe"), "where.exe"} {
		t.Run(filepath.Base(bin), func(t *testing.T) {
			tempRoot := t.TempDir()
			service := NewService(bin, &recordingAudioTranscriber{text: "unused"})
			service.tempRoot = tempRoot
			_, err := service.Transcribe(context.Background(), strings.NewReader("ogg"), Meta{Size: 3, Duration: 1})
			if err == nil {
				t.Fatal("Transcribe succeeded")
			}
			if strings.Contains(err.Error(), tempRoot) {
				t.Fatalf("error leaked temp path: %v", err)
			}
			assertDirectoryEmpty(t, tempRoot)
		})
	}
}

type countingReader struct {
	reader io.Reader
	reads  int
}

func (r *countingReader) Read(p []byte) (int, error) {
	r.reads++
	return r.reader.Read(p)
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 0
	}
	return len(p), nil
}

func valueAfter(args []string, flag string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] == flag {
			return args[index+1]
		}
	}
	return ""
}

func assertDirectoryEmpty(t *testing.T, path string) {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary entries leaked: %#v", entries)
	}
}
