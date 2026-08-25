//go:build !windows

package main

import (
	"bufio"
	"io"
)

func promptInitTelegramToken(reader *bufio.Reader, _ io.Reader, out io.Writer) (string, error) {
	return promptRequired(reader, out, "Telegram bot token")
}

func persistInitTelegramToken(string) (bool, error) { return false, nil }
