//go:build windows

package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/alclssna33/codex_to_telegram/internal/config"
	secretpkg "github.com/alclssna33/codex_to_telegram/internal/secrets"
)

type secretStore = secretpkg.Store

var secretStoreFactory = secretpkg.NewStore

func runSecrets(args []string, in io.Reader, out io.Writer) error {
	if len(args) < 2 {
		return errors.New("usage: ctr-go secrets set telegram|openai | delete telegram|openai")
	}
	target, err := credentialTarget(args[1])
	if err != nil {
		return err
	}
	store := secretStoreFactory()
	switch args[0] {
	case "set":
		if len(args) != 2 {
			return errors.New("usage: ctr-go secrets set telegram|openai")
		}
		value, err := readSecretInput(bufio.NewReader(in), in, out)
		if err != nil {
			return err
		}
		if value == "" {
			return errors.New("secret value is required")
		}
		if err := store.Write(target, []byte(value)); err != nil {
			return err
		}
		_, _ = fmt.Fprintf(out, "%s credential stored.\n", args[1])
		return nil
	case "delete":
		if len(args) != 2 {
			return errors.New("usage: ctr-go secrets delete telegram|openai")
		}
		if err := store.Delete(target); err != nil && !errors.Is(err, secretpkg.ErrNotFound) {
			return err
		}
		_, _ = fmt.Fprintf(out, "%s credential removed.\n", args[1])
		return nil
	default:
		return errors.New("usage: ctr-go secrets set telegram|openai | delete telegram|openai")
	}
}

func credentialTarget(name string) (string, error) {
	telegramTarget, openAITarget, err := config.EffectiveCredentialTargets()
	if err != nil {
		return "", err
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "telegram":
		return telegramTarget, nil
	case "openai":
		return openAITarget, nil
	default:
		return "", fmt.Errorf("unknown credential name %q: use telegram or openai", name)
	}
}

func readSecretInput(reader *bufio.Reader, in io.Reader, out io.Writer) (string, error) {
	_, _ = fmt.Fprint(out, "Secret: ")
	if file, ok := in.(*os.File); ok && file == os.Stdin && term.IsTerminal(int(file.Fd())) {
		data, err := term.ReadPassword(int(file.Fd()))
		_, _ = fmt.Fprintln(out)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(data)), nil
	}
	line, err := reader.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	return strings.TrimSpace(line), nil
}
