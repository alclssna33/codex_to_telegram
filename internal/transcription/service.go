package transcription

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const (
	MaxAudioBytes      int64 = 25 * 1024 * 1024
	MaxAudioDuration         = 600
	MaxTranscriptRunes       = 20_000
)

type Meta struct {
	FileName    string
	ContentType string
	Size        int64
	Duration    int
}

type AudioTranscriber interface {
	Transcribe(ctx context.Context, audio io.Reader, meta Meta) (string, error)
}

type commandRunner func(ctx context.Context, bin string, args []string) error

type Service struct {
	ffmpegBin string
	client    AudioTranscriber
	tempRoot  string
	run       commandRunner
}

func NewService(ffmpegBin string, client AudioTranscriber) *Service {
	ffmpegBin = strings.TrimSpace(ffmpegBin)
	if ffmpegBin == "" {
		ffmpegBin = "ffmpeg"
	}
	return &Service{
		ffmpegBin: ffmpegBin,
		client:    client,
		tempRoot:  os.TempDir(),
		run:       runFFmpeg,
	}
}

func (s *Service) Transcribe(ctx context.Context, telegramFile io.Reader, meta Meta) (string, error) {
	if s == nil || s.client == nil {
		return "", errors.New("voice transcription is unavailable")
	}
	if telegramFile == nil {
		return "", errors.New("telegram audio is required")
	}
	if meta.Size > MaxAudioBytes {
		return "", errors.New("voice message exceeds the 25 MB limit")
	}
	if meta.Duration > MaxAudioDuration {
		return "", errors.New("voice message exceeds the 10 minute limit")
	}

	tempDir, err := makePrivateTempDir(s.tempRoot)
	if err != nil {
		return "", errors.New("prepare voice conversion failed")
	}
	defer os.RemoveAll(tempDir)

	inputPath := filepath.Join(tempDir, "voice.ogg")
	input, err := os.OpenFile(inputPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", errors.New("prepare voice conversion failed")
	}
	limited := &io.LimitedReader{R: telegramFile, N: MaxAudioBytes + 1}
	written, copyErr := io.Copy(input, limited)
	closeErr := input.Close()
	if copyErr != nil || closeErr != nil {
		return "", errors.New("read telegram audio failed")
	}
	if written > MaxAudioBytes {
		return "", errors.New("voice message exceeds the 25 MB limit")
	}

	outputPath := filepath.Join(tempDir, "voice.mp3")
	args := []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-i", inputPath,
		"-vn", "-ac", "1", "-ar", "16000", "-b:a", "64k",
		outputPath,
	}
	if err := s.run(ctx, s.ffmpegBin, args); err != nil {
		return "", errors.New("ffmpeg conversion failed")
	}
	output, err := os.Open(outputPath)
	if err != nil {
		return "", errors.New("ffmpeg conversion failed")
	}
	defer output.Close()
	info, err := output.Stat()
	if err != nil || info.Size() <= 0 || info.Size() > MaxAudioBytes {
		return "", errors.New("ffmpeg conversion produced invalid audio")
	}
	transcript, err := s.client.Transcribe(ctx, output, Meta{
		FileName: "voice.mp3", ContentType: "audio/mpeg", Size: info.Size(), Duration: meta.Duration,
	})
	if err != nil {
		return "", errors.New("audio transcription failed")
	}
	transcript = strings.TrimSpace(transcript)
	if transcript == "" {
		return "", errors.New("transcription returned empty text")
	}
	if !utf8.ValidString(transcript) || utf8.RuneCountInString(transcript) > MaxTranscriptRunes {
		return "", errors.New("transcription text is invalid or too long")
	}
	return transcript, nil
}

func makePrivateTempDir(root string) (string, error) {
	tempDir, err := os.MkdirTemp(root, "codex-tg-voice-")
	if err != nil {
		return "", err
	}
	if err := restrictPrivateTempDir(tempDir); err != nil {
		_ = os.RemoveAll(tempDir)
		return "", err
	}
	return tempDir, nil
}

func runFFmpeg(ctx context.Context, bin string, args []string) error {
	command := exec.CommandContext(ctx, bin, args...)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Run()
}
