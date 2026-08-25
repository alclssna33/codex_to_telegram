package telegram

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
	"net/url"
	pathpkg "path"
	"strings"
	"time"
	"unicode"

	"github.com/alclssna33/codex_to_telegram/internal/model"
)

type Client struct {
	baseURL     string
	fileBaseURL string
	http        *http.Client
}

var ErrFileDownload = errors.New("telegram file download failed")

func NewClient(token string) *Client {
	return NewClientWithBaseURLs(token, "https://api.telegram.org", "https://api.telegram.org")
}

// NewClientWithBaseURLs creates a client using explicit Bot API origins. It is
// primarily useful for deterministic local HTTP integration tests.
func NewClientWithBaseURLs(token, apiBaseURL, fileBaseURL string) *Client {
	apiBaseURL = strings.TrimRight(strings.TrimSpace(apiBaseURL), "/")
	fileBaseURL = strings.TrimRight(strings.TrimSpace(fileBaseURL), "/")
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
	}
	return &Client{
		baseURL:     fmt.Sprintf("%s/bot%s", apiBaseURL, token),
		fileBaseURL: fmt.Sprintf("%s/file/bot%s", fileBaseURL, token),
		http: &http.Client{
			Transport: transport,
			Timeout:   70 * time.Second,
		},
	}
}

type apiResponse[T any] struct {
	OK          bool   `json:"ok"`
	Result      T      `json:"result"`
	Description string `json:"description"`
	ErrorCode   int    `json:"error_code"`
}

type User struct {
	ID       int64  `json:"id"`
	IsBot    bool   `json:"is_bot"`
	Username string `json:"username"`
}

type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

type Message struct {
	MessageID       int64           `json:"message_id"`
	MessageThreadID int64           `json:"message_thread_id,omitempty"`
	Date            int64           `json:"date"`
	From            *User           `json:"from"`
	Chat            Chat            `json:"chat"`
	Text            string          `json:"text"`
	Voice           *Voice          `json:"voice,omitempty"`
	Document        *Document       `json:"document,omitempty"`
	Entities        []MessageEntity `json:"entities,omitempty"`
	ReplyToMessage  *Message        `json:"reply_to_message"`
}

type Voice struct {
	FileID       string `json:"file_id"`
	FileUniqueID string `json:"file_unique_id"`
	Duration     int    `json:"duration"`
	FileSize     int64  `json:"file_size"`
	MimeType     string `json:"mime_type"`
}

type File struct {
	FileID   string `json:"file_id"`
	FilePath string `json:"file_path"`
}

type CallbackQuery struct {
	ID      string   `json:"id"`
	From    *User    `json:"from"`
	Message *Message `json:"message"`
	Data    string   `json:"data"`
}

type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message"`
	EditedMessage *Message       `json:"edited_message"`
	CallbackQuery *CallbackQuery `json:"callback_query"`
}

type BotCommand struct {
	Command     string `json:"command"`
	Description string `json:"description"`
}

type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data,omitempty"`
}

type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

type MessageEntity struct {
	Type     string `json:"type"`
	Offset   int    `json:"offset"`
	Length   int    `json:"length"`
	URL      string `json:"url,omitempty"`
	Language string `json:"language,omitempty"`
}

type sendMessageRequest struct {
	ChatID              int64                 `json:"chat_id"`
	Text                string                `json:"text"`
	MessageThreadID     int64                 `json:"message_thread_id,omitempty"`
	ReplyMarkup         *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
	DisablePreview      bool                  `json:"disable_web_page_preview,omitempty"`
	DisableNotification bool                  `json:"disable_notification,omitempty"`
	ParseMode           string                `json:"parse_mode,omitempty"`
	Entities            []MessageEntity       `json:"entities,omitempty"`
}

type editMessageTextRequest struct {
	ChatID         int64                 `json:"chat_id"`
	MessageID      int64                 `json:"message_id"`
	Text           string                `json:"text"`
	ReplyMarkup    *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
	DisablePreview bool                  `json:"disable_web_page_preview,omitempty"`
	ParseMode      string                `json:"parse_mode,omitempty"`
	Entities       []MessageEntity       `json:"entities,omitempty"`
}

type deleteMessageRequest struct {
	ChatID    int64 `json:"chat_id"`
	MessageID int64 `json:"message_id"`
}

type getUpdatesRequest struct {
	Offset         int64    `json:"offset,omitempty"`
	Timeout        int      `json:"timeout,omitempty"`
	AllowedUpdates []string `json:"allowed_updates,omitempty"`
}

type setMyCommandsRequest struct {
	Commands []BotCommand `json:"commands"`
}

type answerCallbackQueryRequest struct {
	CallbackQueryID string `json:"callback_query_id"`
	Text            string `json:"text,omitempty"`
	ShowAlert       bool   `json:"show_alert,omitempty"`
}

type getFileRequest struct {
	FileID string `json:"file_id"`
}

type DocumentFile struct {
	Name        string
	ContentType string
	Data        []byte
}

func (c *Client) GetMe(ctx context.Context) (*User, error) {
	var user User
	if err := c.callJSON(ctx, "getMe", nil, &user); err != nil {
		return nil, err
	}
	return &user, nil
}

func (c *Client) SetMyCommands(ctx context.Context, commands []BotCommand) error {
	return c.callJSON(ctx, "setMyCommands", setMyCommandsRequest{Commands: commands}, nil)
}

func (c *Client) GetUpdates(ctx context.Context, offset int64, timeoutSeconds int) ([]Update, error) {
	request := getUpdatesRequest{
		Offset:         offset,
		Timeout:        timeoutSeconds,
		AllowedUpdates: []string{"message", "edited_message", "callback_query"},
	}
	var updates []Update
	if err := c.callJSON(ctx, "getUpdates", request, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

func (c *Client) GetFile(ctx context.Context, fileID string) (*File, error) {
	if strings.TrimSpace(fileID) == "" {
		return nil, errors.New("telegram file id is required")
	}
	var file File
	if err := c.callJSON(ctx, "getFile", getFileRequest{FileID: fileID}, &file); err != nil {
		return nil, err
	}
	if strings.TrimSpace(file.FilePath) == "" {
		return nil, errors.New("telegram getFile returned no file path")
	}
	return &file, nil
}

func (c *Client) DownloadFile(ctx context.Context, filePath string) (io.ReadCloser, error) {
	endpoint, err := telegramFileEndpoint(c.fileBaseURL, filePath)
	if err != nil {
		return nil, ErrFileDownload
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, ErrFileDownload
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, ErrFileDownload
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_ = response.Body.Close()
		return nil, ErrFileDownload
	}
	return response.Body, nil
}

// telegramFileEndpoint accepts only a raw, clean relative Bot API file path.
// Percent escapes are rejected instead of decoded so encoded traversal cannot
// become meaningful before URL.Path performs the outbound escaping.
func telegramFileEndpoint(baseURL, filePath string) (string, error) {
	if filePath == "" || strings.TrimSpace(filePath) != filePath ||
		strings.HasPrefix(filePath, "/") || strings.ContainsAny(filePath, `\%?#`) {
		return "", ErrFileDownload
	}
	for _, value := range filePath {
		if unicode.IsControl(value) {
			return "", ErrFileDownload
		}
	}
	parsedPath, err := url.Parse(filePath)
	if err != nil || parsedPath.IsAbs() || parsedPath.Host != "" || parsedPath.User != nil || parsedPath.Opaque != "" ||
		parsedPath.RawQuery != "" || parsedPath.Fragment != "" || parsedPath.Path != filePath || pathpkg.Clean(filePath) != filePath {
		return "", ErrFileDownload
	}
	for _, segment := range strings.Split(filePath, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return "", ErrFileDownload
		}
	}

	endpoint, err := url.Parse(baseURL)
	if err != nil || endpoint.Scheme != "http" && endpoint.Scheme != "https" || endpoint.Host == "" ||
		endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return "", ErrFileDownload
	}
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + "/" + filePath
	endpoint.RawPath = ""
	return endpoint.String(), nil
}

func (c *Client) SendMessage(ctx context.Context, chatID, topicID int64, text string, markup *InlineKeyboardMarkup, options model.SendOptions) (*Message, error) {
	request := sendMessageRequest{
		ChatID:              chatID,
		Text:                text,
		MessageThreadID:     topicID,
		ReplyMarkup:         markup,
		DisablePreview:      true,
		DisableNotification: options.Silent,
	}
	if topicID == 0 {
		request.MessageThreadID = 0
	}
	request.ParseMode = parseModeForText(text)
	var message Message
	if err := c.callJSON(ctx, "sendMessage", request, &message); err != nil {
		return nil, err
	}
	return &message, nil
}

func (c *Client) SendRenderedMessage(ctx context.Context, chatID, topicID int64, rendered model.RenderedMessage, markup *InlineKeyboardMarkup, options model.SendOptions) (*Message, error) {
	request := sendMessageRequest{
		ChatID:              chatID,
		Text:                rendered.Text,
		MessageThreadID:     topicID,
		ReplyMarkup:         markup,
		DisablePreview:      true,
		DisableNotification: options.Silent,
		Entities:            toAPIEntities(rendered.Entities),
	}
	if topicID == 0 {
		request.MessageThreadID = 0
	}
	var message Message
	if err := c.callJSON(ctx, "sendMessage", request, &message); err != nil {
		return nil, err
	}
	return &message, nil
}

func (c *Client) EditMessageText(ctx context.Context, chatID, messageID int64, text string, markup *InlineKeyboardMarkup) (*Message, error) {
	request := editMessageTextRequest{
		ChatID:         chatID,
		MessageID:      messageID,
		Text:           text,
		ReplyMarkup:    markup,
		DisablePreview: true,
	}
	request.ParseMode = parseModeForText(text)
	var message Message
	if err := c.callJSON(ctx, "editMessageText", request, &message); err != nil {
		return nil, err
	}
	return &message, nil
}

func (c *Client) EditRenderedMessageText(ctx context.Context, chatID, messageID int64, rendered model.RenderedMessage, markup *InlineKeyboardMarkup) (*Message, error) {
	request := editMessageTextRequest{
		ChatID:         chatID,
		MessageID:      messageID,
		Text:           rendered.Text,
		ReplyMarkup:    markup,
		DisablePreview: true,
		Entities:       toAPIEntities(rendered.Entities),
	}
	var message Message
	if err := c.callJSON(ctx, "editMessageText", request, &message); err != nil {
		return nil, err
	}
	return &message, nil
}

func (c *Client) DeleteMessage(ctx context.Context, chatID, messageID int64) error {
	return c.callJSON(ctx, "deleteMessage", deleteMessageRequest{
		ChatID:    chatID,
		MessageID: messageID,
	}, nil)
}

func (c *Client) SendDocument(ctx context.Context, chatID, topicID int64, document DocumentFile, caption string, markup *InlineKeyboardMarkup, options model.SendOptions) (*Message, error) {
	if strings.TrimSpace(document.Name) == "" {
		document.Name = "document.txt"
	}
	fields := map[string]string{
		"chat_id": strconvFormatInt(chatID),
	}
	if topicID != 0 {
		fields["message_thread_id"] = strconvFormatInt(topicID)
	}
	if options.Silent {
		fields["disable_notification"] = "true"
	}
	if strings.TrimSpace(caption) != "" {
		fields["caption"] = caption
	}
	if markup != nil {
		encoded, err := json.Marshal(markup)
		if err != nil {
			return nil, err
		}
		fields["reply_markup"] = string(encoded)
	}
	var message Message
	if err := c.callMultipart(ctx, "sendDocument", fields, "document", document, &message); err != nil {
		return nil, err
	}
	return &message, nil
}

func (c *Client) AnswerCallbackQuery(ctx context.Context, callbackQueryID, text string, showAlert bool) error {
	request := answerCallbackQueryRequest{
		CallbackQueryID: callbackQueryID,
		Text:            text,
		ShowAlert:       showAlert,
	}
	return c.callJSON(ctx, "answerCallbackQuery", request, nil)
}

func (c *Client) callMultipart(ctx context.Context, method string, fields map[string]string, fileField string, document DocumentFile, out any) error {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if err := writer.WriteField(key, value); err != nil {
			return err
		}
	}
	header := textproto.MIMEHeader{}
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, fileField, escapeQuotes(document.Name)))
	contentType := strings.TrimSpace(document.ContentType)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	header.Set("Content-Type", contentType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return err
	}
	if _, err := part.Write(document.Data); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	endpoint := strings.TrimRight(c.baseURL, "/") + "/" + method
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body.Bytes()))
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("telegram %s http %d: %s", method, response.StatusCode, strings.TrimSpace(string(data)))
	}
	return decodeAPIResponse(method, data, out)
}

func (c *Client) callJSON(ctx context.Context, method string, payload any, out any) error {
	var body io.Reader = http.NoBody
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	endpoint := strings.TrimRight(c.baseURL, "/") + "/" + method
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("telegram %s http %d: %s", method, response.StatusCode, strings.TrimSpace(string(data)))
	}
	return decodeAPIResponse(method, data, out)
}

func decodeAPIResponse(method string, data []byte, out any) error {
	if out == nil {
		var envelope apiResponse[json.RawMessage]
		if err := json.Unmarshal(data, &envelope); err != nil {
			return err
		}
		if !envelope.OK {
			return apiError(method, envelope.ErrorCode, envelope.Description)
		}
		return nil
	}
	envelope := apiResponse[json.RawMessage]{}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if !envelope.OK {
		return apiError(method, envelope.ErrorCode, envelope.Description)
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return nil
	}
	return json.Unmarshal(envelope.Result, out)
}

func escapeQuotes(value string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value)
}

func parseModeForText(text string) string {
	if strings.Contains(text, "<pre><code") {
		return "HTML"
	}
	return ""
}

func toAPIEntities(entities []model.MessageEntity) []MessageEntity {
	out := make([]MessageEntity, 0, len(entities))
	for _, entity := range entities {
		out = append(out, MessageEntity{
			Type:     entity.Type,
			Offset:   entity.Offset,
			Length:   entity.Length,
			URL:      entity.URL,
			Language: entity.Language,
		})
	}
	return out
}

func strconvFormatInt(value int64) string {
	return fmt.Sprintf("%d", value)
}

func apiError(method string, code int, description string) error {
	description = strings.TrimSpace(description)
	if description == "" {
		description = "unknown telegram api error"
	}
	if code == 0 {
		return fmt.Errorf("telegram %s: %s", method, description)
	}
	return fmt.Errorf("telegram %s: %d %s", method, code, description)
}
