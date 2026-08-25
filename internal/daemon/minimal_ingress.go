package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/model"
	"github.com/alclssna33/codex_to_telegram/internal/storage"
)

const (
	minimalProjectsAction    = "minimal_projects"
	minimalProjectPickAction = "minimal_project_pick"
	minimalCallbackTTL       = 10 * time.Minute
	minimalDefaultMaxAge     = 10 * time.Minute
)

var ErrUnauthorized = errors.New("unauthorized telegram user")

func (s *Service) HandleInboundText(ctx context.Context, inbound model.InboundText) (*DirectResponse, error) {
	if !s.IsAllowed(inbound.UserID, inbound.ChatID) {
		if s.cfg.Profile == "minimal" || isNotifierProfile(s.cfg.Profile) {
			return nil, ErrUnauthorized
		}
		return nil, nil
	}
	if isNotifierProfile(s.cfg.Profile) {
		return s.handleNotifierInboundText(ctx, inbound)
	}
	if s.cfg.Profile != "minimal" {
		return s.HandleMessage(ctx, inbound.ChatID, inbound.TopicID, inbound.UserID, inbound.Text, inbound.ReplyToMessageID)
	}
	if s.minimalInboundExpired(inbound.ReceivedAt) {
		return &DirectResponse{Text: "요청이 만료되었습니다. 다시 보내주세요."}, nil
	}

	text := strings.TrimSpace(inbound.Text)
	if minimalCommandName(text) == "start" {
		if inbound.ChatID == inbound.UserID && inbound.TopicID == 0 {
			if err := s.store.SetGlobalObserverTarget(ctx, inbound.ChatID, inbound.TopicID, true); err != nil {
				return nil, err
			}
		}
		return s.minimalProjectPicker(ctx)
	}
	switch minimalCommandName(text) {
	case "status", "repair":
		return s.handleCommand(ctx, inbound.ChatID, inbound.TopicID, text, inbound.ReplyToMessageID)
	}
	if s.minimalRouter == nil {
		return nil, errors.New("minimal router is unavailable")
	}
	return s.minimalRouter.Submit(ctx, inbound)
}

func (s *Service) minimalInboundExpired(receivedAt time.Time) bool {
	if receivedAt.IsZero() {
		return true
	}
	maxAge := s.cfg.CommandMaxAge
	if maxAge <= 0 {
		maxAge = minimalDefaultMaxAge
	}
	return s.now().Sub(receivedAt) > maxAge
}

func minimalCommandName(text string) string {
	head := strings.Fields(strings.TrimSpace(text))
	if len(head) == 0 || !strings.HasPrefix(head[0], "/") {
		return ""
	}
	command := strings.TrimPrefix(head[0], "/")
	if index := strings.IndexByte(command, '@'); index >= 0 {
		command = command[:index]
	}
	return strings.ToLower(command)
}

func (s *Service) minimalProjectPicker(ctx context.Context) (*DirectResponse, error) {
	projects := s.projectRegistry.Projects()
	buttons := make([][]model.ButtonSpec, 0, len(projects))
	for _, project := range projects {
		button, err := s.minimalCallbackButton(ctx, project.DisplayName, minimalProjectPickAction, project.ID)
		if err != nil {
			return nil, err
		}
		buttons = append(buttons, []model.ButtonSpec{button})
	}
	return &DirectResponse{Text: "작업폴더를 선택하세요.", Buttons: buttons}, nil
}

func (s *Service) minimalCallbackButton(ctx context.Context, label, action, projectID string) (model.ButtonSpec, error) {
	token := randomToken()
	payload := map[string]string{
		"action":     action,
		"project_id": strings.TrimSpace(projectID),
	}
	route := model.CallbackRoute{
		Token:       token,
		Action:      action,
		Status:      model.CallbackStatusActive,
		ExpiresAt:   s.now().UTC().Add(minimalCallbackTTL).Format(time.RFC3339Nano),
		PayloadJSON: storage.MustJSON(payload),
		CreatedAt:   model.TimeString(s.now().UTC().Format(time.RFC3339Nano)),
	}
	if err := s.store.PutCallbackRoute(ctx, route); err != nil {
		return model.ButtonSpec{}, err
	}
	return model.ButtonSpec{Text: label, CallbackData: token}, nil
}

func (s *Service) handleMinimalCallback(ctx context.Context, chatID, topicID, messageID int64, route *model.CallbackRoute) (*DirectResponse, error) {
	if minimalCallbackExpired(route, s.now()) {
		_ = s.store.ExpireCallbackRoute(ctx, route.Token)
		return &DirectResponse{Text: "이 버튼은 만료되었습니다. /start를 다시 보내주세요.", CallbackText: "만료된 버튼입니다."}, nil
	}

	var payload struct {
		Action    string `json:"action"`
		ProjectID string `json:"project_id"`
	}
	if err := json.Unmarshal([]byte(route.PayloadJSON), &payload); err != nil || payload.Action != route.Action {
		return &DirectResponse{Text: "이 버튼은 유효하지 않습니다. /start를 다시 보내주세요.", CallbackText: "유효하지 않은 버튼입니다."}, nil
	}

	switch route.Action {
	case minimalProjectsAction:
		_ = s.store.ExpireCallbackRoute(ctx, route.Token)
		return s.editOrSendMinimalResponse(ctx, chatID, topicID, messageID, "작업폴더를 선택하세요.", s.minimalProjectPicker)
	case minimalProjectPickAction:
		project, ok := s.minimalProjectByID(payload.ProjectID)
		if !ok {
			return &DirectResponse{Text: "등록되지 않은 작업폴더입니다. /start를 다시 보내주세요.", CallbackText: "등록되지 않은 작업폴더입니다."}, nil
		}
		unlockSelection := minimalSelectionLock(chatID, topicID)
		defer unlockSelection()
		if err := s.store.SetSelectedProject(ctx, chatID, topicID, project.ID); err != nil {
			return nil, err
		}
		_ = s.store.ExpireCallbackRoute(ctx, route.Token)
		return s.editOrSendMinimalResponse(ctx, chatID, topicID, messageID, "작업폴더를 선택했습니다.", func(context.Context) (*DirectResponse, error) {
			return s.minimalProjectActions(ctx, chatID, topicID, project)
		})
	default:
		return nil, nil
	}
}

func (s *Service) minimalProjectByID(projectID string) (model.Project, bool) {
	projectID = strings.TrimSpace(projectID)
	for _, project := range s.projectRegistry.Projects() {
		if project.ID == projectID {
			return project, true
		}
	}
	return model.Project{}, false
}

func (s *Service) editOrSendMinimalResponse(ctx context.Context, chatID, topicID, messageID int64, callbackText string, renderer func(context.Context) (*DirectResponse, error)) (*DirectResponse, error) {
	response, err := renderer(ctx)
	if err != nil {
		return nil, err
	}
	if response == nil {
		return &DirectResponse{CallbackText: callbackText}, nil
	}
	response.CallbackText = callbackText
	s.mu.RLock()
	sender := s.sender
	s.mu.RUnlock()
	if sender != nil && messageID != 0 && strings.TrimSpace(response.Text) != "" {
		if err := sender.EditMessage(ctx, chatID, topicID, messageID, response.Text, response.Buttons); err == nil {
			return &DirectResponse{CallbackText: callbackText}, nil
		}
	}
	return response, nil
}

func minimalCallbackExpired(route *model.CallbackRoute, now time.Time) bool {
	if route == nil || route.Status != model.CallbackStatusActive || strings.TrimSpace(route.ExpiresAt) == "" {
		return true
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, route.ExpiresAt)
	return err != nil || !now.Before(expiresAt)
}
