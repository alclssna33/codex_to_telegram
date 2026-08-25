package projectregistry_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alclssna33/codex_to_telegram/internal/model"
	"github.com/alclssna33/codex_to_telegram/internal/projectregistry"
)

func TestRegistryResolvesCanonicalProjectAndMatchesOnlyRealDescendants(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root := filepath.Join(base, "project")
	child := filepath.Join(root, "child")
	sibling := filepath.Join(base, "project-copy")
	for _, dir := range []string{child, sibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) failed: %v", dir, err)
		}
	}
	registry, err := projectregistry.New([]model.Project{{
		ID:            "p1",
		DisplayName:   "Project 1",
		CanonicalPath: filepath.Join(root, "."),
	}})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	resolved, err := registry.Resolve("p1")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if got, want := resolved.CanonicalPath, filepath.Clean(root); got != want {
		t.Fatalf("canonical path = %q, want %q", got, want)
	}
	if got, ok := registry.MatchCWD(root); !ok || got.ID != "p1" {
		t.Fatalf("root match = %#v, %t; want p1", got, ok)
	}
	if got, ok := registry.MatchCWD(child); !ok || got.ID != "p1" {
		t.Fatalf("child match = %#v, %t; want p1", got, ok)
	}
	if got, ok := registry.MatchCWD(sibling); ok {
		t.Fatalf("sibling matched: %#v", got)
	}
	if got, ok := registry.MatchCWD(filepath.Join(root, "..", "project-copy")); ok {
		t.Fatalf("outside path matched: %#v", got)
	}
}

func TestRegistryMatchExactCWDAcceptsOnlyCanonicalRoot(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	root := filepath.Join(base, "project")
	child := filepath.Join(root, "child")
	sibling := filepath.Join(base, "project-copy")
	for _, dir := range []string{child, sibling} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%s) failed: %v", dir, err)
		}
	}
	registry, err := projectregistry.New([]model.Project{{
		ID:            "p1",
		DisplayName:   "Project 1",
		CanonicalPath: filepath.Join(root, "."),
	}})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}

	tests := []struct {
		name string
		cwd  string
		want bool
	}{
		{name: "root", cwd: root, want: true},
		{name: "canonical equivalent", cwd: filepath.Join(root, "."), want: true},
		{name: "child", cwd: child},
		{name: "sibling", cwd: sibling},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := registry.MatchExactCWD(test.cwd)
			if ok != test.want {
				t.Fatalf("MatchExactCWD(%q) ok=%t, want %t; project=%#v", test.cwd, ok, test.want, got)
			}
			if ok && got.ID != "p1" {
				t.Fatalf("MatchExactCWD(%q) project=%#v, want p1", test.cwd, got)
			}
		})
	}
	if got, ok := registry.MatchCWD(child); !ok || got.ID != "p1" {
		t.Fatalf("MatchCWD child = %#v, %t; want descendant behavior preserved", got, ok)
	}
}

func TestRegistryRejectsInvalidAndDuplicateProjects(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	other := t.TempDir()
	tests := map[string][]model.Project{
		"empty id":            {{DisplayName: "One", CanonicalPath: root}},
		"duplicate id":        {{ID: "one", DisplayName: "One", CanonicalPath: root}, {ID: "one", DisplayName: "Two", CanonicalPath: other}},
		"duplicate display":   {{ID: "one", DisplayName: "Same", CanonicalPath: root}, {ID: "two", DisplayName: "Same", CanonicalPath: other}},
		"missing path":        {{ID: "one", DisplayName: "One"}},
		"nonexistent path":    {{ID: "one", DisplayName: "One", CanonicalPath: filepath.Join(root, "missing")}},
		"canonical collision": {{ID: "one", DisplayName: "One", CanonicalPath: root}, {ID: "two", DisplayName: "Two", CanonicalPath: filepath.Join(root, ".")}},
	}
	for name, projects := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := projectregistry.New(projects); err == nil {
				t.Fatal("New succeeded, want validation error")
			}
		})
	}
}

func TestRegistryResolveRevalidatesCanonicalPath(t *testing.T) {
	t.Parallel()

	root := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}
	registry, err := projectregistry.New([]model.Project{{ID: "p1", DisplayName: "Project 1", CanonicalPath: root}})
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	if _, err := registry.Resolve("p1"); err == nil || !strings.Contains(err.Error(), "revalidate") {
		t.Fatalf("Resolve error = %v, want revalidation failure", err)
	}
}
