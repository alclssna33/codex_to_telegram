package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/alclssna33/codex_to_telegram/internal/model"
	"github.com/alclssna33/codex_to_telegram/internal/transcription"
)

type VoiceTranscriber interface {
	Transcribe(ctx context.Context, telegramFile io.Reader, meta transcription.Meta) (string, error)
}

type VoiceInput struct {
	ChatID           int64
	TopicID          int64
	UserID           int64
	MessageID        int64
	ReplyToMessageID int64
	ReceivedAt       time.Time
	FileSize         int64
	Duration         int
	MimeType         string
	Open             func(context.Context) (io.ReadCloser, error)
}

func (s *Service) HandleVoice(ctx context.Context, input VoiceInput) (*DirectResponse, error) {
	if !s.IsAllowed(input.UserID, input.ChatID) {
		if s.cfg.Profile == "minimal" {
			return nil, ErrUnauthorized
		}
		return nil, nil
	}
	if s.cfg.Profile != "minimal" {
		return nil, nil
	}
	if s.minimalInboundExpired(input.ReceivedAt) {
		return &DirectResponse{Text: "음성 요청이 만료되었습니다. 다시 보내주세요."}, nil
	}
	if input.FileSize > transcription.MaxAudioBytes {
		return &DirectResponse{Text: "음성 파일은 25MB 이하만 처리할 수 있습니다. 텍스트로 보내주세요."}, nil
	}
	if input.Duration > transcription.MaxAudioDuration {
		return &DirectResponse{Text: "음성 메시지는 10분 이하만 처리할 수 있습니다. 텍스트로 보내주세요."}, nil
	}
	s.mu.RLock()
	voiceTranscriber := s.voiceTranscriber
	s.mu.RUnlock()
	if voiceTranscriber == nil {
		return &DirectResponse{Text: "음성 인식을 사용할 수 없습니다. 텍스트로 보내주세요."}, nil
	}
	target, project, picker, err := s.resolveVoiceTarget(ctx, input)
	if err != nil {
		return nil, err
	}
	if picker != nil {
		return picker, nil
	}
	if input.Open == nil {
		return &DirectResponse{Text: "음성 파일을 내려받지 못했습니다. 다시 녹음하거나 텍스트로 보내주세요."}, nil
	}
	body, err := input.Open(ctx)
	if err != nil || body == nil {
		return &DirectResponse{Text: "음성 파일을 내려받지 못했습니다. 다시 녹음하거나 텍스트로 보내주세요."}, nil
	}
	defer body.Close()
	transcript, err := voiceTranscriber.Transcribe(ctx, body, transcription.Meta{
		FileName: "voice.ogg", ContentType: strings.TrimSpace(input.MimeType), Size: input.FileSize, Duration: input.Duration,
	})
	if err != nil {
		return &DirectResponse{Text: "음성 인식에 실패했습니다. 다시 녹음하거나 텍스트로 보내주세요."}, nil
	}
	executeToken, err := newMinimalApprovalToken()
	if err != nil {
		return nil, errors.New("create voice callback failed")
	}
	cancelToken, err := newMinimalApprovalToken()
	if err != nil {
		return nil, errors.New("create voice callback failed")
	}
	expiresAt := s.now().UTC().Add(minimalCallbackTTL)
	voiceID, err := s.store.CreateVoiceConfirmation(ctx, model.VoiceConfirmation{
		ProjectID:       project.ID,
		TargetKind:      target.TargetKind,
		ThreadID:        target.ThreadID,
		SourceTurnID:    target.SourceTurnID,
		Transcript:      transcript,
		SessionIdentity: s.voiceSessionIdentity,
		ExpiresAt:       model.TimeString(expiresAt.Format(time.RFC3339Nano)),
	}, executeToken, cancelToken)
	if err != nil {
		return nil, err
	}
	targetText := "새 작업"
	if target.TargetKind == model.VoiceTargetThread {
		targetText = "기존 Thread " + shortVoiceThreadID(target.ThreadID)
	}
	return &DirectResponse{
		Text: fmt.Sprintf("[음성 인식]\n작업폴더: %s\n대상: %s\n\n%s", project.DisplayName, targetText, transcript),
		Buttons: [][]model.ButtonSpec{{
			{Text: "실행", CallbackData: executeToken},
			{Text: "취소", CallbackData: cancelToken},
		}},
		ThreadID:            target.ThreadID,
		TurnID:              target.SourceTurnID,
		VoiceConfirmationID: voiceID,
	}, nil
}

func (s *Service) SetVoiceTranscriber(transcriber VoiceTranscriber) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.voiceTranscriber = transcriber
}

func (s *Service) resolveVoiceTarget(ctx context.Context, input VoiceInput) (ResolvedTarget, model.Project, *DirectResponse, error) {
	if input.ReplyToMessageID != 0 {
		route, err := s.store.ResolveMessageRoute(ctx, input.ChatID, input.TopicID, input.ReplyToMessageID)
		if err != nil {
			return ResolvedTarget{}, model.Project{}, nil, err
		}
		if route == nil || strings.TrimSpace(route.ThreadID) == "" {
			return ResolvedTarget{}, model.Project{}, nil, errors.New("voice reply target is not routed to a Codex thread")
		}
		thread, err := s.store.GetThread(ctx, route.ThreadID)
		if err != nil {
			return ResolvedTarget{}, model.Project{}, nil, err
		}
		if thread == nil {
			return ResolvedTarget{}, model.Project{}, nil, errors.New("routed Codex thread is unavailable")
		}
		project, _, err := s.minimalRouter.projectForThread(thread)
		if err != nil {
			return ResolvedTarget{}, model.Project{}, nil, err
		}
		return ResolvedTarget{ProjectID: project.ID, TargetKind: model.VoiceTargetThread, ThreadID: thread.ID, SourceTurnID: route.TurnID}, project, nil, nil
	}
	projectID, err := s.store.GetSelectedProject(ctx, input.ChatID, input.TopicID)
	if err != nil {
		return ResolvedTarget{}, model.Project{}, nil, err
	}
	if strings.TrimSpace(projectID) == "" {
		picker, err := s.minimalProjectPicker(ctx)
		return ResolvedTarget{}, model.Project{}, picker, err
	}
	project, err := s.projectRegistry.Resolve(projectID)
	if err != nil {
		return ResolvedTarget{}, model.Project{}, nil, err
	}
	return ResolvedTarget{ProjectID: project.ID, TargetKind: model.VoiceTargetNew}, project, nil, nil
}

func (s *Service) handleMinimalVoiceCallback(ctx context.Context, chatID, topicID, messageID int64, route *model.VoiceCallbackRoute) (*DirectResponse, error) {
	if route == nil {
		return &DirectResponse{Text: "이 음성 버튼은 더 이상 사용할 수 없습니다.", CallbackText: "만료된 음성 버튼입니다."}, nil
	}
	exactMessage := route.ChatID != 0 && route.MessageID != 0 && route.ChatID == chatID && route.TopicID == topicID && route.MessageID == messageID
	if route.Status != model.VoiceRouteStatusActive {
		if exactMessage {
			return s.voiceTerminalResponse(ctx, chatID, topicID, messageID, "[음성 인식]\n이 확인 요청은 만료되었습니다. 음성을 다시 보내주세요.", "만료된 음성 버튼입니다."), nil
		}
		return &DirectResponse{Text: "이 음성 버튼은 더 이상 사용할 수 없습니다.", CallbackText: "만료된 음성 버튼입니다."}, nil
	}
	confirmation, err := s.store.ConsumeVoiceConfirmation(ctx, model.VoiceClaim{
		Token: route.Token, Action: route.Action, SessionIdentity: s.voiceSessionIdentity,
		ChatID: chatID, TopicID: topicID, MessageID: messageID, Now: s.now().UTC(),
	})
	if err != nil {
		return nil, err
	}
	if confirmation == nil {
		if exactMessage {
			return s.voiceTerminalResponse(ctx, chatID, topicID, messageID, "[음성 인식]\n이 확인 요청은 만료되었습니다. 음성을 다시 보내주세요.", "만료된 음성 버튼입니다."), nil
		}
		return &DirectResponse{Text: "이 음성 버튼은 더 이상 사용할 수 없습니다.", CallbackText: "만료된 음성 버튼입니다."}, nil
	}
	if route.Action == model.VoiceActionCancel {
		return s.voiceTerminalResponse(ctx, chatID, topicID, messageID, "[음성 인식]\n음성 작업을 취소했습니다.", "취소했습니다."), nil
	}
	if route.Action != model.VoiceActionExecute {
		return s.voiceTerminalResponse(ctx, chatID, topicID, messageID, "[음성 인식]\n이 확인 요청은 유효하지 않습니다.", "유효하지 않은 음성 버튼입니다."), nil
	}
	routerResponse, routerErr := s.minimalRouter.SubmitResolved(ctx, model.InboundText{
		ChatID: chatID, TopicID: topicID, ReceivedAt: s.now().UTC(),
	}, ResolvedTarget{ProjectID: confirmation.ProjectID, TargetKind: confirmation.TargetKind, ThreadID: confirmation.ThreadID, SourceTurnID: confirmation.SourceTurnID}, confirmation.Transcript)
	if routerErr != nil {
		return s.voiceTerminalResponse(ctx, chatID, topicID, messageID, "[음성 인식]\n실행 여부를 확인할 수 없습니다. 음성을 다시 보내주세요.", "실행하지 못했습니다."), nil
	}
	text := "[음성 인식]\n음성 명령을 실행했습니다."
	if routerResponse != nil && strings.TrimSpace(routerResponse.Text) != "" {
		text += "\n\n" + strings.TrimSpace(routerResponse.Text)
	}
	response := s.voiceTerminalResponse(ctx, chatID, topicID, messageID, text, "실행했습니다.")
	if response != nil && routerResponse != nil {
		response.ThreadID = routerResponse.ThreadID
		response.TurnID = routerResponse.TurnID
		if response.Text == "" && response.ThreadID != "" && messageID != 0 {
			if err := s.store.PutMessageRoute(ctx, model.MessageRoute{
				ChatID:    chatID,
				TopicID:   topicID,
				MessageID: messageID,
				ThreadID:  response.ThreadID,
				TurnID:    response.TurnID,
				ItemID:    response.ItemID,
				EventID:   response.EventID,
				CreatedAt: model.NowString(),
			}); err != nil {
				return nil, err
			}
		}
	}
	return response, nil
}

func (s *Service) voiceTerminalResponse(ctx context.Context, chatID, topicID, messageID int64, text, callbackText string) *DirectResponse {
	s.mu.RLock()
	sender := s.sender
	s.mu.RUnlock()
	if sender != nil && messageID != 0 {
		if err := sender.EditMessage(ctx, chatID, topicID, messageID, text, nil); err == nil {
			return &DirectResponse{CallbackText: callbackText}
		}
	}
	return &DirectResponse{Text: text, CallbackText: callbackText}
}

func (s *Service) AbandonVoiceConfirmation(ctx context.Context, voiceID int64) error {
	if voiceID == 0 {
		return nil
	}
	return s.store.AbandonVoiceConfirmation(ctx, voiceID)
}

func shortVoiceThreadID(threadID string) string {
	threadID = strings.TrimSpace(threadID)
	if len(threadID) <= 8 {
		return threadID
	}
	return threadID[:8]
}
