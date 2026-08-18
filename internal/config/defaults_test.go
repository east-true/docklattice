package config

import (
	"strings"
	"testing"
	"time"
)

func TestV1DefaultsMatchArchitecture(t *testing.T) {
	d := V1Defaults()

	checks := map[string]bool{
		"wal bounds":        d.WALMaxBytes == 256*MiB && d.WALRetention == 14*24*time.Hour,
		"wal durability":    d.WALFsyncInterval == time.Second && d.WALFsyncBytes == 64*KiB,
		"operation result":  d.OperationResultRetention == 24*time.Hour && d.OperationResultMax == 500,
		"operation output":  d.OperationOutputTailBytes == 64*KiB,
		"agent storage":     d.AgentStateMaxBytes == 2*GiB && d.EmergencyReserveBytes == 64*MiB,
		"free floor":        d.FilesystemFreeMinBytes == GiB && d.FilesystemFreeMinPercent == 5,
		"snapshot and edit": d.AutomaticSnapshotsPerProject == 20 && d.EditableFileMaxBytes == MiB,
		"discovery":         d.DiscoveryInterval == 5*time.Minute && d.DiscoveryMaxDirectories == 200_000 && d.DiscoveryMaxDuration == time.Minute && d.DiscoveryDirectoriesPerSecond == 1_000,
		"stats":             d.StatsSampleInterval == 2*time.Second && d.BrowserSparklineSamples == 120,
		"liveness":          d.HeartbeatInterval == 30*time.Second && d.OfflineAfter == 90*time.Second,
		"audit rates":       d.EventCoalescingWindow == 5*time.Second && d.ObservedAuditMaxPerSecond == 20,
		"server retention":  d.ServerOperationRetention == 90*24*time.Hour && d.ServerAuditRetention == 365*24*time.Hour && d.ServerAuditMaxBytes == 10*GiB,
		"server watermarks": d.ServerAuditWarnPercent == 80 && d.ServerAuditAggressivePercent == 95,
		"memory":            d.AgentRSSTargetBytes == 256*MiB && d.AgentMemoryHardLimitBytes == 512*MiB && d.ServerRSSTargetBytes == 512*MiB && d.ServerMemoryHardLimitBytes == GiB,
		"credential":        d.CredentialLifetime == 90*24*time.Hour && d.CredentialRenewalRemainingPercent == 50,
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("%s default differs from architecture", name)
		}
	}

	wantTimeouts := OperationTimeouts{
		Container: time.Minute, ComposeUp: 15 * time.Minute,
		ComposeRestart: 10 * time.Minute, ComposeDown: 5 * time.Minute,
		ComposePull: 45 * time.Minute, FileWrite: 30 * time.Second,
		BackupCreate: 5 * time.Minute, BackupRestore: 5 * time.Minute,
		DiscoveryRescan: 10 * time.Minute,
	}
	if d.OperationTimeout != wantTimeouts {
		t.Fatalf("operation timeouts = %#v, want %#v", d.OperationTimeout, wantTimeouts)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("defaults do not validate: %v", err)
	}
}

func TestDefaultsValidation(t *testing.T) {
	tests := []struct {
		name string
		edit func(*Defaults)
		want string
	}{
		{"offline ordering", func(d *Defaults) { d.OfflineAfter = d.HeartbeatInterval }, "offline threshold"},
		{"audit watermark ordering", func(d *Defaults) { d.ServerAuditWarnPercent = d.ServerAuditAggressivePercent }, "audit watermarks"},
		{"renewal range", func(d *Defaults) { d.CredentialRenewalRemainingPercent = 100 }, "credential renewal"},
		{"agent memory", func(d *Defaults) { d.AgentRSSTargetBytes = d.AgentMemoryHardLimitBytes }, "agent RSS"},
		{"server memory", func(d *Defaults) { d.ServerRSSTargetBytes = d.ServerMemoryHardLimitBytes }, "server RSS"},
		{"positive count", func(d *Defaults) { d.DiscoveryMaxDirectories = 0 }, "discovery max directories"},
		{"positive scan rate", func(d *Defaults) { d.DiscoveryDirectoriesPerSecond = 0 }, "discovery directories/sec"},
		{"positive duration", func(d *Defaults) { d.CredentialLifetime = 0 }, "credential lifetime"},
		{"operation timeout", func(d *Defaults) { d.OperationTimeout.ComposePull = 0 }, "compose pull timeout"},
		{"filesystem percent", func(d *Defaults) { d.FilesystemFreeMinPercent = 100 }, "filesystem free minimum"},
		{"reserve bound", func(d *Defaults) { d.EmergencyReserveBytes = d.AgentStateMaxBytes }, "emergency reserve"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := V1Defaults()
			tt.edit(&d)
			err := d.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}
