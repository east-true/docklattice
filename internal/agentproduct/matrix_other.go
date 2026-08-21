//go:build !linux

package agentproduct

import "errors"

func probeFilesystem(string) (filesystemUsage, error) {
	return filesystemUsage{}, errors.New("managed filesystem capacity requires Linux")
}
