package projectregistry

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/alclssna33/codex_to_telegram/internal/model"
)

var ErrUnknownProject = errors.New("unknown project")

// Registry holds only validated copies of the local project allowlist.
type Registry struct {
	projects []model.Project
	byID     map[string]int
}

func New(projects []model.Project) (*Registry, error) {
	registry := &Registry{
		projects: make([]model.Project, 0, len(projects)),
		byID:     make(map[string]int, len(projects)),
	}
	ids := make(map[string]struct{}, len(projects))
	displayNames := make(map[string]struct{}, len(projects))
	paths := make(map[string]struct{}, len(projects))

	for index, raw := range projects {
		project := model.Project{
			ID:            strings.TrimSpace(raw.ID),
			DisplayName:   strings.TrimSpace(raw.DisplayName),
			CanonicalPath: strings.TrimSpace(raw.CanonicalPath),
		}
		if project.ID == "" {
			return nil, fmt.Errorf("project %d: id is required", index+1)
		}
		if project.DisplayName == "" {
			return nil, fmt.Errorf("project %q: display_name is required", project.ID)
		}
		if project.CanonicalPath == "" {
			return nil, fmt.Errorf("project %q: path is required", project.ID)
		}

		idKey := comparisonKey(project.ID)
		if _, exists := ids[idKey]; exists {
			return nil, fmt.Errorf("duplicate project id %q", project.ID)
		}
		displayKey := comparisonKey(project.DisplayName)
		if _, exists := displayNames[displayKey]; exists {
			return nil, fmt.Errorf("duplicate project display_name %q", project.DisplayName)
		}
		canonical, err := canonicalDirectory(project.CanonicalPath)
		if err != nil {
			return nil, fmt.Errorf("project %q: %w", project.ID, err)
		}
		pathKey := pathComparisonKey(canonical)
		if _, exists := paths[pathKey]; exists {
			return nil, fmt.Errorf("project %q: canonical path collision at %q", project.ID, canonical)
		}

		project.CanonicalPath = canonical
		ids[idKey] = struct{}{}
		displayNames[displayKey] = struct{}{}
		paths[pathKey] = struct{}{}
		registry.byID[idKey] = len(registry.projects)
		registry.projects = append(registry.projects, project)
	}
	return registry, nil
}

// Projects returns a copy so callers cannot mutate the registry's cached roots.
func (r *Registry) Projects() []model.Project {
	if r == nil {
		return nil
	}
	return append([]model.Project(nil), r.projects...)
}

// Resolve returns a value copy and revalidates the cached root immediately.
func (r *Registry) Resolve(id string) (model.Project, error) {
	if r == nil {
		return model.Project{}, ErrUnknownProject
	}
	index, ok := r.byID[comparisonKey(strings.TrimSpace(id))]
	if !ok {
		return model.Project{}, fmt.Errorf("%w: %q", ErrUnknownProject, id)
	}
	project := r.projects[index]
	canonical, err := canonicalDirectory(project.CanonicalPath)
	if err != nil {
		return model.Project{}, fmt.Errorf("revalidate project %q: %w", project.ID, err)
	}
	if pathComparisonKey(canonical) != pathComparisonKey(project.CanonicalPath) {
		return model.Project{}, fmt.Errorf("revalidate project %q: canonical path changed from %q to %q", project.ID, project.CanonicalPath, canonical)
	}
	return project, nil
}

// MatchCWD returns the most specific registered project containing cwd.
func (r *Registry) MatchCWD(cwd string) (model.Project, bool) {
	if r == nil {
		return model.Project{}, false
	}
	canonicalCWD, err := canonicalDirectory(cwd)
	if err != nil {
		return model.Project{}, false
	}
	best := model.Project{}
	bestLength := -1
	for _, cached := range r.projects {
		project, err := r.Resolve(cached.ID)
		if err != nil || !containsPath(project.CanonicalPath, canonicalCWD) {
			continue
		}
		if len(project.CanonicalPath) > bestLength {
			best = project
			bestLength = len(project.CanonicalPath)
		}
	}
	return best, bestLength >= 0
}

// MatchExactCWD returns the registered project whose canonical root is exactly cwd.
func (r *Registry) MatchExactCWD(cwd string) (model.Project, bool) {
	if r == nil {
		return model.Project{}, false
	}
	canonicalCWD, err := canonicalDirectory(cwd)
	if err != nil {
		return model.Project{}, false
	}
	cwdKey := pathComparisonKey(canonicalCWD)
	for _, cached := range r.projects {
		project, err := r.Resolve(cached.ID)
		if err != nil {
			continue
		}
		if pathComparisonKey(project.CanonicalPath) == cwdKey {
			return project, true
		}
	}
	return model.Project{}, false
}

func canonicalDirectory(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", errors.New("path is required")
	}
	absolute, err := filepath.Abs(filepath.Clean(trimmed))
	if err != nil {
		return "", fmt.Errorf("make path absolute: %w", err)
	}
	evaluated, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", fmt.Errorf("canonicalize path %q: %w", absolute, err)
	}
	info, err := os.Stat(evaluated)
	if err != nil {
		return "", fmt.Errorf("stat path %q: %w", evaluated, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("path %q is not a directory", evaluated)
	}
	return filepath.Clean(evaluated), nil
}

func containsPath(root, candidate string) bool {
	relative, err := filepath.Rel(root, candidate)
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func comparisonKey(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func pathComparisonKey(value string) string {
	cleaned := filepath.Clean(value)
	if runtime.GOOS == "windows" {
		return strings.ToLower(cleaned)
	}
	return cleaned
}
