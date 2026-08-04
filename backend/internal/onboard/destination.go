package onboard

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"unicode"
)

var knownDeploymentFiles = []string{
	".github/workflows/deploy.yml",
	"CHECKLIST.md",
	"deploy/values.yaml",
	"deploy/README.md",
}

type destinationConflictError struct {
	paths []string
}

func (e *destinationConflictError) Error() string {
	return fmt.Sprintf("refusing to replace existing scaffold files without --update-existing: %s", strings.Join(e.paths, ", "))
}

func inspectFileDestination(outDir string, files map[string]string) ([]DestinationFilePlan, []string, error) {
	var err error
	outDir, err = normalizeDashboardConsumerDir(outDir)
	if err != nil {
		return nil, nil, err
	}
	if err := inspectDestinationRoot(outDir); err != nil {
		return nil, nil, err
	}

	actions := make([]DestinationFilePlan, 0, len(files))
	for _, rel := range sortedFilePaths(files) {
		if err := validateDestinationFilePath(rel); err != nil {
			return nil, nil, err
		}
		if err := inspectDestinationParents(outDir, rel); err != nil {
			return nil, nil, err
		}
		full := filepath.Join(outDir, filepath.FromSlash(rel))
		info, err := os.Lstat(full)
		switch {
		case err == nil:
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return nil, nil, fmt.Errorf("generated file path %s conflicts with a non-regular filesystem entry", full)
			}
			actions = append(actions, DestinationFilePlan{Path: rel, Action: destinationActionReplace})
		case os.IsNotExist(err):
			actions = append(actions, DestinationFilePlan{Path: rel, Action: destinationActionCreate})
		default:
			return nil, nil, fmt.Errorf("checking %s: %w", full, err)
		}
	}

	planned := make(map[string]struct{}, len(files))
	for rel := range files {
		planned[rel] = struct{}{}
	}
	var stale []string
	for _, rel := range knownDeploymentFiles {
		if _, ok := planned[rel]; ok {
			continue
		}
		if _, err := os.Lstat(filepath.Join(outDir, filepath.FromSlash(rel))); err == nil {
			stale = append(stale, rel)
		} else if !os.IsNotExist(err) {
			return nil, nil, fmt.Errorf("checking stale scaffold file %s: %w", rel, err)
		}
	}
	return actions, stale, nil
}

func normalizeDashboardConsumerDir(outDir string) (string, error) {
	outDir = strings.TrimSpace(outDir)
	if outDir == "" {
		return "", fmt.Errorf("dashboard consumer directory is required")
	}
	if strings.IndexFunc(outDir, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("dashboard consumer directory must not contain control characters")
	}
	return filepath.Clean(outDir), nil
}

func inspectDestinationRoot(outDir string) error {
	info, err := os.Lstat(outDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking dashboard consumer directory %s: %w", outDir, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("dashboard consumer directory %s conflicts with a non-directory filesystem entry", outDir)
	}
	return nil
}

func inspectDestinationParents(outDir, rel string) error {
	current := filepath.Clean(outDir)
	parent := path.Dir(rel)
	if parent == "." {
		return nil
	}
	for _, part := range strings.Split(parent, "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("checking generated file parent %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("generated file parent %s conflicts with a non-directory filesystem entry", current)
		}
	}
	return nil
}

func validateDestinationFilePath(rel string) error {
	if rel == "" || rel == ".." || strings.HasPrefix(rel, "../") || path.IsAbs(rel) || path.Clean(rel) != rel || strings.Contains(rel, "\\") || strings.IndexFunc(rel, unicode.IsControl) >= 0 {
		return fmt.Errorf("generated file path %q is not a safe repo-relative path", rel)
	}
	return nil
}

func hasDestinationReplacements(files []DestinationFilePlan) bool {
	for _, file := range files {
		if file.Action == destinationActionReplace {
			return true
		}
	}
	return false
}

func destinationReplacementPaths(files []DestinationFilePlan) []string {
	var paths []string
	for _, file := range files {
		if file.Action == destinationActionReplace {
			paths = append(paths, file.Path)
		}
	}
	return paths
}

func writeFiles(outDir string, files map[string]string, updateExisting bool, expected []DestinationFilePlan) error {
	var err error
	outDir, err = normalizeDashboardConsumerDir(outDir)
	if err != nil {
		return err
	}
	actions, _, err := inspectFileDestination(outDir, files)
	if err != nil {
		return err
	}
	if len(expected) > 0 && !reflect.DeepEqual(actions, expected) {
		return fmt.Errorf("dashboard consumer directory changed after review; rerun onboarding before writing")
	}
	if replacements := destinationReplacementPaths(actions); len(replacements) > 0 && !updateExisting {
		return &destinationConflictError{paths: replacements}
	}
	for _, action := range actions {
		full := filepath.Join(outDir, filepath.FromSlash(action.Path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("creating directory for %s: %w", action.Path, err)
		}
		if err := inspectDestinationRoot(outDir); err != nil {
			return err
		}
		if err := inspectDestinationParents(outDir, action.Path); err != nil {
			return err
		}
		if action.Action == destinationActionReplace {
			if err := replaceFileAtomic(full, []byte(files[action.Path])); err != nil {
				return fmt.Errorf("replacing %s: %w", action.Path, err)
			}
			continue
		}
		if err := createFileExclusive(full, []byte(files[action.Path])); err != nil {
			return fmt.Errorf("creating %s: %w", action.Path, err)
		}
	}
	return nil
}

func createFileExclusive(filename string, content []byte) error {
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	created, _ := file.Stat()
	complete := false
	defer func() {
		_ = file.Close()
		if complete || created == nil {
			return
		}
		current, err := os.Lstat(filename)
		if err == nil && os.SameFile(created, current) {
			_ = os.Remove(filename)
		}
	}()
	if _, err := file.Write(content); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}

func replaceFileAtomic(filename string, content []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(filename), ".prow-ai-dashboard-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	cleanup := true
	defer func() {
		_ = temp.Close()
		if cleanup {
			_ = os.Remove(tempName)
		}
	}()
	if err := temp.Chmod(0o644); err != nil {
		return err
	}
	if _, err := temp.Write(content); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, filename); err != nil {
		return err
	}
	cleanup = false
	return nil
}
