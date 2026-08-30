//go:build !linux

package agentstorage

import (
	"context"
	"errors"

	"github.com/east-true/docklattice/internal/diskbudget"
)

func observeFilesystem(context.Context, string) (diskbudget.Observation, error) {
	return diskbudget.Observation{}, errors.New("Agent storage observation requires Linux")
}
