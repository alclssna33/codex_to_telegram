package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadProjectsReadsFixedRegistryJSON(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	path := filepath.Join(root, "projects.json")
	data := `{"projects":[{"id":"bridge","display_name":"Codex Bridge","path":"` + jsonPath(root) + `"}]}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	projects, err := LoadProjects(path)
	if err != nil {
		t.Fatalf("LoadProjects failed: %v", err)
	}
	if got, want := len(projects), 1; got != want {
		t.Fatalf("projects = %d, want %d", got, want)
	}
	if got, want := projects[0].ID, "bridge"; got != want {
		t.Fatalf("project id = %q, want %q", got, want)
	}
	if got, want := projects[0].CanonicalPath, root; got != want {
		t.Fatalf("project path = %q, want %q", got, want)
	}
}

func TestLoadProjectsRejectsAmbiguousJSON(t *testing.T) {
	t.Parallel()

	for name, data := range map[string]string{
		"unknown field":            `{"projects":[],"editable":true}`,
		"trailing value":           `{"projects":[]} {"projects":[]}`,
		"duplicate projects key":   `{"projects":[],"projects":[]}`,
		"duplicate project key":    `{"projects":[{"id":"one","id":"two","display_name":"One","path":"x"}]}`,
		"case folded projects key": `{"projects":[],"Projects":[]}`,
		"case folded project key":  `{"projects":[{"id":"one","ID":"two","display_name":"One","path":"x"}]}`,
		"unicode folded key":       `{"projects":[],"projectſ":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "projects.json")
			if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
				t.Fatalf("WriteFile failed: %v", err)
			}
			if _, err := LoadProjects(path); err == nil {
				t.Fatal("LoadProjects succeeded, want strict JSON error")
			}
		})
	}
}

func jsonPath(path string) string {
	return strings.ReplaceAll(path, `\`, `\\`)
}
