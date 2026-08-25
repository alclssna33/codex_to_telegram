package daemon

import (
	"strings"
	"unicode"

	"github.com/alclssna33/codex_to_telegram/internal/appserver"
	"github.com/alclssna33/codex_to_telegram/internal/model"
)

const notifierSummaryRuneLimit = 300

func notifierSummary(text string) string {
	paragraph := firstNotifierParagraph(text)
	if paragraph == "" {
		return ""
	}
	return truncateNotifierRunes(paragraph, notifierSummaryRuneLimit)
}

func renderNotifierTerminal(snapshot appserver.ThreadReadSnapshot) model.RenderedMessage {
	status := canonicalMinimalTerminalStatus(snapshot.LatestTurnStatus)
	header := "✅ 작업 완료"
	if status == "failed" || status == "interrupted" {
		header = "❌ 작업 실패"
	}
	lines := []string{header, ""}
	_, folder := model.ProjectNameFromCWD(snapshot.Thread.CWD)
	if folder = cleanNotifierField(folder); folder != "" {
		lines = append(lines, "폴더: "+folder)
	}
	if title := cleanNotifierField(snapshot.Thread.Title); title != "" && title != snapshot.Thread.ID {
		lines = append(lines, "대화: "+title)
	}
	if len(lines) > 2 {
		lines = append(lines, "")
	}
	if status == "failed" || status == "interrupted" {
		lines = append(lines, "내용: 작업이 실패하거나 중단되었습니다.")
	} else if summary := notifierSummary(snapshot.LatestFinalText); summary != "" {
		lines = append(lines, "요약: "+summary)
	} else {
		lines = append(lines, "작업이 완료되었습니다.")
	}
	return model.RenderedMessage{Text: strings.Join(lines, "\n")}
}

func firstNotifierParagraph(text string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	var paragraph []string
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		cleaned := cleanNotifierMarkdownLine(trimmed)
		if cleaned == "" {
			if len(paragraph) > 0 {
				break
			}
			continue
		}
		paragraph = append(paragraph, cleaned)
	}
	return cleanNotifierField(strings.Join(paragraph, " "))
}

func cleanNotifierMarkdownLine(line string) string {
	line = strings.TrimSpace(line)
	for line != "" {
		original := line
		line = strings.TrimSpace(strings.TrimLeft(line, ">"))
		line = trimNotifierHeadingMarker(line)
		line = trimNotifierListMarker(line)
		if line == original {
			break
		}
	}
	line = strings.NewReplacer("**", "", "__", "", "`", "", "*", "", "_", "").Replace(line)
	return cleanNotifierField(line)
}

func trimNotifierHeadingMarker(line string) string {
	i := 0
	for i < len(line) && line[i] == '#' {
		i++
	}
	if i == 0 {
		return line
	}
	if i < len(line) && unicode.IsSpace(rune(line[i])) {
		return strings.TrimSpace(line[i+1:])
	}
	return line
}

func trimNotifierListMarker(line string) string {
	if len(line) >= 2 {
		switch line[0] {
		case '-', '+', '*':
			if unicode.IsSpace(rune(line[1])) {
				return strings.TrimSpace(line[2:])
			}
		}
	}
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i > 0 && i+1 < len(line) && line[i] == '.' && unicode.IsSpace(rune(line[i+1])) {
		return strings.TrimSpace(line[i+2:])
	}
	return line
}

func cleanNotifierField(field string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(field)), " ")
}

func truncateNotifierRunes(text string, limit int) string {
	runes := []rune(text)
	if limit <= 0 || len(runes) <= limit {
		return text
	}
	cutoff := limit - 1
	sentenceEnd := -1
	for i := 0; i < cutoff; i++ {
		if i < 120 {
			continue
		}
		if isNotifierSentenceEndingRune(runes[i]) {
			sentenceEnd = i + 1
		}
	}
	if sentenceEnd > 0 {
		return string(runes[:sentenceEnd]) + "…"
	}
	return string(runes[:cutoff]) + "…"
}

func isNotifierSentenceEndingRune(r rune) bool {
	switch r {
	case '.', '!', '?', '。', '！', '？', '…':
		return true
	default:
		return false
	}
}
