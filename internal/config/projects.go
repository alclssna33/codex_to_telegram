package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/alclssna33/codex_to_telegram/internal/model"
)

type projectFile struct {
	Projects []model.Project `json:"projects"`
}

func LoadProjects(path string) ([]model.Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read projects file: %w", err)
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, fmt.Errorf("decode projects file: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var file projectFile
	if err := decoder.Decode(&file); err != nil {
		return nil, fmt.Errorf("decode projects file: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return nil, fmt.Errorf("decode projects file: unexpected trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode projects file trailing data: %w", err)
	}
	return append([]model.Project(nil), file.Projects...), nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := scanJSONValue(decoder, "$"); err != nil {
		return err
	}
	if token, err := decoder.Token(); err == nil {
		return fmt.Errorf("unexpected trailing JSON token %v", token)
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make([]string, 0)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("expected object key at %s", path)
			}
			for _, existing := range seen {
				if strings.EqualFold(existing, key) {
					return fmt.Errorf("duplicate object key %q at %s", key, path)
				}
			}
			seen = append(seen, key)
			if err := scanJSONValue(decoder, path+"."+key); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim('}') {
			return fmt.Errorf("expected object end at %s", path)
		}
	case '[':
		for index := 0; decoder.More(); index++ {
			if err := scanJSONValue(decoder, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil {
			return err
		}
		if closing != json.Delim(']') {
			return fmt.Errorf("expected array end at %s", path)
		}
	default:
		return fmt.Errorf("unexpected delimiter %q at %s", delim, path)
	}
	return nil
}
