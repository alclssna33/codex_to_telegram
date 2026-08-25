//go:build windows

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/alclssna33/codex_to_telegram/internal/config"
)

func promptInitTelegramToken(reader *bufio.Reader, in io.Reader, out io.Writer) (string, error) {
	value, err := readSecretInput(reader, in, out)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(value) == "" {
		return "", errors.New("Telegram bot token is required")
	}
	return value, nil
}

func persistInitTelegramToken(token string) (bool, error) {
	target, _, err := config.EffectiveCredentialTargets()
	if err != nil {
		return false, err
	}
	if err := secretStoreFactory().Write(target, []byte(token)); err != nil {
		return false, fmt.Errorf("store Telegram credential: %w", err)
	}
	return true, nil
}
