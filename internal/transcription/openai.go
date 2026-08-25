package transcription

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const maxTranscriptionResponseBytes = 2 * 1024 * 1024

type OpenAI struct {
	apiKey  string
	model   string
	baseURL string
	http    *http.Client
}

func NewOpenAI(apiKey, model string) *OpenAI {
	model = strings.TrimSpace(model)
	if model == "" {
		model = "gpt-transcribe"
	}
	return &OpenAI{
		apiKey:  strings.TrimSpace(apiKey),
		model:   model,
		baseURL: "https://api.openai.com",
		http:    &http.Client{Timeout: 90 * time.Second},
	}
}

func (c *OpenAI) Transcribe(ctx context.Context, audio io.Reader, meta Meta) (string, error) {
	if c == nil || strings.TrimSpace(c.apiKey) == "" {
		return "", errors.New("voice transcription is unavailable")
	}
	if audio == nil {
		return "", errors.New("converted audio is required")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", c.model); err != nil {
		return "", errors.New("prepare transcription request failed")
	}
	fileName := filepath.Base(strings.TrimSpace(meta.FileName))
	if fileName == "" || fileName == "." {
		fileName = "voice.mp3"
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, escapeMultipartValue(fileName)))
	contentType := strings.TrimSpace(meta.ContentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return "", errors.New("prepare transcription request failed")
	}
	if _, err := io.Copy(part, audio); err != nil {
		return "", errors.New("prepare transcription request failed")
	}
	if err := writer.Close(); err != nil {
		return "", errors.New("prepare transcription request failed")
	}

	endpoint := strings.TrimRight(c.baseURL, "/") + "/v1/audio/transcriptions"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body.Bytes()))
	if err != nil {
		return "", errors.New("prepare transcription request failed")
	}
	request.Header.Set("Authorization", "Bearer "+c.apiKey)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := c.http.Do(request)
	if err != nil {
		return "", errors.New("transcription request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("transcription request failed: %s", http.StatusText(response.StatusCode))
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxTranscriptionResponseBytes+1))
	if err != nil || len(data) > maxTranscriptionResponseBytes || !utf8.Valid(data) {
		return "", errors.New("invalid transcription response")
	}
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", errors.New("invalid transcription response")
	}
	transcript := strings.TrimSpace(payload.Text)
	if transcript == "" {
		return "", errors.New("transcription returned empty text")
	}
	if !utf8.ValidString(transcript) || utf8.RuneCountInString(transcript) > MaxTranscriptRunes {
		return "", errors.New("transcription text is invalid or too long")
	}
	return transcript, nil
}

func escapeMultipartValue(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
}
