// Package agentsafety contains the pure policy decisions used to keep the
// containerized Agent from operating outside its verified host path identity
// or mutating itself.
package agentsafety

import (
	"path"
	"strings"
)

// Mount is the subset of a Docker container mount needed by the path identity
// check. Paths use Linux container/host semantics on every build host.
type Mount struct {
	Type        string
	Source      string
	Destination string
	RW          bool
}

// MountReason is a stable, machine-readable path identity outcome.
type MountReason string

const (
	MountReady            MountReason = "READY"
	MountReadOnly         MountReason = "READ_ONLY"
	MountInvalidRoot      MountReason = "INVALID_ROOT"
	MountNotFound         MountReason = "MOUNT_NOT_FOUND"
	MountAmbiguous        MountReason = "AMBIGUOUS_MOUNT"
	MountInvalid          MountReason = "INVALID_MOUNT"
	MountNotBind          MountReason = "MOUNT_NOT_BIND"
	MountIdentityMismatch MountReason = "PATH_IDENTITY_MISMATCH"
)

// RootAssessment is the capability result for one discovery root. A failed
// identity check always disables both filesystem writes and Compose execution.
type RootAssessment struct {
	Root        string
	Mount       Mount
	HostPath    string
	Matched     bool
	FSWrite     bool
	ComposeExec bool
	Reason      MountReason
}

// AssessDiscoveryRoot selects the most-specific mount containing root, then
// verifies that it is a bind mount whose reconstructed host path is identical
// to the container path. A read-only identical bind remains valid for Compose
// execution, but cannot support file writes.
func AssessDiscoveryRoot(root string, mounts []Mount) RootAssessment {
	cleanRoot, ok := cleanAbsolute(root)
	if !ok {
		return RootAssessment{Root: root, Reason: MountInvalidRoot}
	}
	result := RootAssessment{Root: cleanRoot, Reason: MountNotFound}

	type candidate struct {
		mount Mount
		depth int
	}
	var selected *candidate
	ambiguous := false
	for _, mount := range mounts {
		destination, valid := cleanAbsolute(mount.Destination)
		if !valid || !containsPath(destination, cleanRoot) {
			continue
		}
		mount.Destination = destination
		current := candidate{mount: mount, depth: pathDepth(destination)}
		if selected == nil || current.depth > selected.depth {
			selected = &current
			ambiguous = false
			continue
		}
		if current.depth == selected.depth {
			// Two mounts cannot safely own the same most-specific target.
			ambiguous = true
		}
	}
	if selected == nil {
		return result
	}
	result.Matched = true
	result.Mount = selected.mount
	if ambiguous {
		result.Reason = MountAmbiguous
		return result
	}
	if !strings.EqualFold(selected.mount.Type, "bind") {
		result.Reason = MountNotBind
		return result
	}
	source, valid := cleanAbsolute(selected.mount.Source)
	if !valid {
		result.Reason = MountInvalid
		return result
	}
	result.Mount.Source = source
	relative := strings.TrimPrefix(cleanRoot, selected.mount.Destination)
	relative = strings.TrimPrefix(relative, "/")
	hostPath := source
	if relative != "" {
		hostPath = path.Join(source, relative)
	}
	result.HostPath = hostPath
	if hostPath != cleanRoot {
		result.Reason = MountIdentityMismatch
		return result
	}
	result.ComposeExec = true
	if !selected.mount.RW {
		result.Reason = MountReadOnly
		return result
	}
	result.FSWrite = true
	result.Reason = MountReady
	return result
}

func cleanAbsolute(value string) (string, bool) {
	if value == "" || strings.IndexByte(value, 0) >= 0 || !strings.HasPrefix(value, "/") {
		return "", false
	}
	return path.Clean(value), true
}

func containsPath(parent, child string) bool {
	if parent == "/" {
		return true
	}
	return child == parent || strings.HasPrefix(child, parent+"/")
}

func pathDepth(value string) int {
	if value == "/" {
		return 0
	}
	return strings.Count(strings.Trim(value, "/"), "/") + 1
}
