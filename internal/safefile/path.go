//go:build linux

package safefile

import (
	"fmt"
	"path/filepath"
	"strings"
)

func normalizeRelative(path string) (string, error) {
	if path == "" {
		return "", &PathError{Path: path, Reason: "empty relative path"}
	}
	if strings.IndexByte(path, 0) >= 0 {
		return "", &PathError{Path: path, Reason: "NUL byte"}
	}
	if filepath.IsAbs(path) {
		return "", &PathError{Path: path, Reason: "absolute path"}
	}
	for _, component := range strings.Split(filepath.ToSlash(path), "/") {
		if component == ".." {
			return "", &PathError{Path: path, Reason: "parent traversal"}
		}
	}
	clean := filepath.Clean(path)
	if clean == "." || clean == string(filepath.Separator) || filepath.IsAbs(clean) {
		return "", &PathError{Path: path, Reason: "path does not name a file"}
	}
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", &PathError{Path: path, Reason: "path escapes project root"}
	}
	return filepath.ToSlash(clean), nil
}

func defaultAccess(path string) Access {
	if strings.Contains(path, "/") {
		return 0
	}
	switch path {
	case "compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml", ".env":
		return ReadWrite
	}
	if strings.HasPrefix(path, ".env.") ||
		strings.HasPrefix(path, "compose.override.") ||
		strings.HasPrefix(path, "docker-compose.override.") ||
		(strings.HasPrefix(path, "compose.") && strings.HasSuffix(path, ".yaml")) {
		return ReadWrite
	}
	return 0
}

func (r *Root) authorize(path string, write bool) (string, error) {
	normalized, err := normalizeRelative(path)
	if err != nil {
		return "", err
	}
	access := defaultAccess(normalized)
	if approved, ok := r.approved[normalized]; ok && access == 0 {
		access = approved
	}
	if access == 0 || (write && access != ReadWrite) {
		action := "read"
		if write {
			action = "write"
		}
		return "", &PathError{Path: path, Reason: fmt.Sprintf("not allowlisted for %s", action)}
	}
	return normalized, nil
}
