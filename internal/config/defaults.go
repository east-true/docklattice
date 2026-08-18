// Package config owns product configuration defaults and their invariants.
package config

import (
	"fmt"
	"time"
)

const (
	KiB int64 = 1 << 10
	MiB int64 = 1 << 20
	GiB int64 = 1 << 30
)

// OperationTimeouts are the v1 operation deadlines from architecture section 19.
type OperationTimeouts struct {
	Container       time.Duration
	ComposeUp       time.Duration
	ComposeRestart  time.Duration
	ComposeDown     time.Duration
	ComposePull     time.Duration
	FileWrite       time.Duration
	BackupCreate    time.Duration
	BackupRestore   time.Duration
	DiscoveryRescan time.Duration
}

// Defaults contains the provisional v1 operational defaults. Values whose
// fitness depends on real I/O and workload remain subject to Appendix A.15's
// Integration Resource Gate; their behavioral meaning is already fixed.
type Defaults struct {
	WALMaxBytes                       int64
	WALRetention                      time.Duration
	WALFsyncInterval                  time.Duration
	WALFsyncBytes                     int64
	OperationResultRetention          time.Duration
	OperationResultMax                int
	OperationOutputTailBytes          int64
	AgentStateMaxBytes                int64
	FilesystemFreeMinBytes            int64
	FilesystemFreeMinPercent          int
	EmergencyReserveBytes             int64
	AutomaticSnapshotsPerProject      int
	EditableFileMaxBytes              int64
	OperationTimeout                  OperationTimeouts
	CancelGracePeriod                 time.Duration
	StalledWarningAfter               time.Duration
	DiscoveryInterval                 time.Duration
	DiscoveryMaxDirectories           int
	DiscoveryMaxDuration              time.Duration
	DiscoveryDirectoriesPerSecond     int
	StatsSampleInterval               time.Duration
	BrowserSparklineSamples           int
	HeartbeatInterval                 time.Duration
	OfflineAfter                      time.Duration
	EventCoalescingWindow             time.Duration
	ObservedAuditMaxPerSecond         int
	ServerOperationRetention          time.Duration
	ServerAuditRetention              time.Duration
	ServerAuditMaxBytes               int64
	ServerAuditWarnPercent            int
	ServerAuditAggressivePercent      int
	AuditACKStallWarningAfter         time.Duration
	AgentRSSTargetBytes               int64
	AgentMemoryHardLimitBytes         int64
	ServerRSSTargetBytes              int64
	ServerMemoryHardLimitBytes        int64
	CredentialLifetime                time.Duration
	CredentialRenewalRemainingPercent int
}

// V1Defaults returns a fresh copy of the architecture's v1 defaults.
func V1Defaults() Defaults {
	return Defaults{
		WALMaxBytes:                  256 * MiB,
		WALRetention:                 14 * 24 * time.Hour,
		WALFsyncInterval:             time.Second,
		WALFsyncBytes:                64 * KiB,
		OperationResultRetention:     24 * time.Hour,
		OperationResultMax:           500,
		OperationOutputTailBytes:     64 * KiB,
		AgentStateMaxBytes:           2 * GiB,
		FilesystemFreeMinBytes:       GiB,
		FilesystemFreeMinPercent:     5,
		EmergencyReserveBytes:        64 * MiB,
		AutomaticSnapshotsPerProject: 20,
		EditableFileMaxBytes:         MiB,
		OperationTimeout: OperationTimeouts{
			Container:       time.Minute,
			ComposeUp:       15 * time.Minute,
			ComposeRestart:  10 * time.Minute,
			ComposeDown:     5 * time.Minute,
			ComposePull:     45 * time.Minute,
			FileWrite:       30 * time.Second,
			BackupCreate:    5 * time.Minute,
			BackupRestore:   5 * time.Minute,
			DiscoveryRescan: 10 * time.Minute,
		},
		CancelGracePeriod:                 10 * time.Second,
		StalledWarningAfter:               5 * time.Minute,
		DiscoveryInterval:                 5 * time.Minute,
		DiscoveryMaxDirectories:           200_000,
		DiscoveryMaxDuration:              time.Minute,
		DiscoveryDirectoriesPerSecond:     1_000,
		StatsSampleInterval:               2 * time.Second,
		BrowserSparklineSamples:           120,
		HeartbeatInterval:                 30 * time.Second,
		OfflineAfter:                      90 * time.Second,
		EventCoalescingWindow:             5 * time.Second,
		ObservedAuditMaxPerSecond:         20,
		ServerOperationRetention:          90 * 24 * time.Hour,
		ServerAuditRetention:              365 * 24 * time.Hour,
		ServerAuditMaxBytes:               10 * GiB,
		ServerAuditWarnPercent:            80,
		ServerAuditAggressivePercent:      95,
		AuditACKStallWarningAfter:         5 * time.Minute,
		AgentRSSTargetBytes:               256 * MiB,
		AgentMemoryHardLimitBytes:         512 * MiB,
		ServerRSSTargetBytes:              512 * MiB,
		ServerMemoryHardLimitBytes:        GiB,
		CredentialLifetime:                90 * 24 * time.Hour,
		CredentialRenewalRemainingPercent: 50,
	}
}

// Validate checks relationships that must remain true when defaults are made
// configurable. It deliberately does not judge workload fitness; that belongs
// to the Integration Resource Gate.
func (d Defaults) Validate() error {
	positiveDurations := map[string]time.Duration{
		"wal retention":              d.WALRetention,
		"wal fsync interval":         d.WALFsyncInterval,
		"operation result retention": d.OperationResultRetention,
		"cancel grace period":        d.CancelGracePeriod,
		"stalled warning":            d.StalledWarningAfter,
		"discovery interval":         d.DiscoveryInterval,
		"discovery max duration":     d.DiscoveryMaxDuration,
		"stats sample interval":      d.StatsSampleInterval,
		"heartbeat interval":         d.HeartbeatInterval,
		"offline threshold":          d.OfflineAfter,
		"credential lifetime":        d.CredentialLifetime,
		"container timeout":          d.OperationTimeout.Container,
		"compose up timeout":         d.OperationTimeout.ComposeUp,
		"compose restart timeout":    d.OperationTimeout.ComposeRestart,
		"compose down timeout":       d.OperationTimeout.ComposeDown,
		"compose pull timeout":       d.OperationTimeout.ComposePull,
		"file write timeout":         d.OperationTimeout.FileWrite,
		"backup create timeout":      d.OperationTimeout.BackupCreate,
		"backup restore timeout":     d.OperationTimeout.BackupRestore,
		"discovery rescan timeout":   d.OperationTimeout.DiscoveryRescan,
	}
	for name, value := range positiveDurations {
		if value <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	if d.OfflineAfter <= d.HeartbeatInterval {
		return fmt.Errorf("offline threshold must exceed heartbeat interval")
	}
	if d.ServerAuditWarnPercent <= 0 ||
		d.ServerAuditWarnPercent >= d.ServerAuditAggressivePercent ||
		d.ServerAuditAggressivePercent >= 100 {
		return fmt.Errorf("audit watermarks must satisfy 0 < warn < aggressive < 100")
	}
	if d.CredentialRenewalRemainingPercent <= 0 || d.CredentialRenewalRemainingPercent >= 100 {
		return fmt.Errorf("credential renewal remaining percent must be between 0 and 100")
	}
	if d.FilesystemFreeMinPercent <= 0 || d.FilesystemFreeMinPercent >= 100 {
		return fmt.Errorf("filesystem free minimum percent must be between 0 and 100")
	}
	if d.AgentRSSTargetBytes >= d.AgentMemoryHardLimitBytes {
		return fmt.Errorf("agent RSS target must be below its hard limit")
	}
	if d.ServerRSSTargetBytes >= d.ServerMemoryHardLimitBytes {
		return fmt.Errorf("server RSS target must be below its hard limit")
	}
	positiveCounts := map[string]int64{
		"wal max bytes":             d.WALMaxBytes,
		"wal fsync bytes":           d.WALFsyncBytes,
		"operation result max":      int64(d.OperationResultMax),
		"operation output tail":     d.OperationOutputTailBytes,
		"agent state max":           d.AgentStateMaxBytes,
		"filesystem free minimum":   d.FilesystemFreeMinBytes,
		"emergency reserve":         d.EmergencyReserveBytes,
		"automatic snapshots":       int64(d.AutomaticSnapshotsPerProject),
		"editable file max":         d.EditableFileMaxBytes,
		"discovery max directories": int64(d.DiscoveryMaxDirectories),
		"discovery directories/sec": int64(d.DiscoveryDirectoriesPerSecond),
		"browser sparkline samples": int64(d.BrowserSparklineSamples),
		"observed audit rate":       int64(d.ObservedAuditMaxPerSecond),
		"server audit max":          d.ServerAuditMaxBytes,
		"agent RSS target":          d.AgentRSSTargetBytes,
		"agent memory hard limit":   d.AgentMemoryHardLimitBytes,
		"server RSS target":         d.ServerRSSTargetBytes,
		"server memory hard limit":  d.ServerMemoryHardLimitBytes,
	}
	for name, value := range positiveCounts {
		if value <= 0 {
			return fmt.Errorf("%s must be positive", name)
		}
	}
	if d.EmergencyReserveBytes >= d.AgentStateMaxBytes {
		return fmt.Errorf("emergency reserve must be below agent state maximum")
	}
	return nil
}
