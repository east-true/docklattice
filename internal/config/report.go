package config

// DefaultsReport is the stable, machine-readable representation emitted by
// `dockpilot defaults`. Duration fields are strings and byte fields name their
// unit so release evidence never has to infer Go's duration or size encoding.
type DefaultsReport struct {
	SchemaVersion int                        `json:"schema_version"`
	WAL           WALDefaultsReport          `json:"wal"`
	Operations    OperationDefaultsReport    `json:"operations"`
	Storage       StorageDefaultsReport      `json:"storage"`
	Discovery     DiscoveryDefaultsReport    `json:"discovery"`
	LiveMetrics   LiveMetricsDefaultsReport  `json:"live_metrics"`
	Connectivity  ConnectivityDefaultsReport `json:"connectivity"`
	Audit         AuditDefaultsReport        `json:"audit"`
	Memory        MemoryDefaultsReport       `json:"memory"`
	Credentials   CredentialDefaultsReport   `json:"credentials"`
}

type WALDefaultsReport struct {
	MaxBytes      int64  `json:"max_bytes"`
	Retention     string `json:"retention"`
	FsyncInterval string `json:"fsync_interval"`
	FsyncBytes    int64  `json:"fsync_bytes"`
}

type OperationDefaultsReport struct {
	ResultRetention string                  `json:"result_retention"`
	ResultMax       int                     `json:"result_max"`
	OutputTailBytes int64                   `json:"output_tail_bytes"`
	CancelGrace     string                  `json:"cancel_grace"`
	StalledWarning  string                  `json:"stalled_warning"`
	Timeouts        OperationTimeoutsReport `json:"timeouts"`
}

type OperationTimeoutsReport struct {
	Container       string `json:"container"`
	ComposeUp       string `json:"compose_up"`
	ComposeRestart  string `json:"compose_restart"`
	ComposeDown     string `json:"compose_down"`
	ComposePull     string `json:"compose_pull"`
	FileWrite       string `json:"file_write"`
	BackupCreate    string `json:"backup_create"`
	BackupRestore   string `json:"backup_restore"`
	DiscoveryRescan string `json:"discovery_rescan"`
}

type StorageDefaultsReport struct {
	AgentStateMaxBytes           int64 `json:"agent_state_max_bytes"`
	FilesystemFreeMinBytes       int64 `json:"filesystem_free_min_bytes"`
	FilesystemFreeMinPercent     int   `json:"filesystem_free_min_percent"`
	EmergencyReserveBytes        int64 `json:"emergency_reserve_bytes"`
	AutomaticSnapshotsPerProject int   `json:"automatic_snapshots_per_project"`
	EditableFileMaxBytes         int64 `json:"editable_file_max_bytes"`
}

type DiscoveryDefaultsReport struct {
	Interval             string `json:"interval"`
	MaxDirectories       int    `json:"max_directories"`
	MaxDuration          string `json:"max_duration"`
	DirectoriesPerSecond int    `json:"directories_per_second"`
}

type LiveMetricsDefaultsReport struct {
	StatsSampleInterval  string `json:"stats_sample_interval"`
	FrameInterval        string `json:"frame_interval"`
	BrowserSparklineSize int    `json:"browser_sparkline_samples"`
}

type ConnectivityDefaultsReport struct {
	HeartbeatInterval string `json:"heartbeat_interval"`
	OfflineAfter      string `json:"offline_after"`
}

type AuditDefaultsReport struct {
	EventCoalescingWindow    string `json:"event_coalescing_window"`
	ObservedMaxPerSecond     int    `json:"observed_max_per_second"`
	ServerOperationRetention string `json:"server_operation_retention"`
	ServerRetention          string `json:"server_retention"`
	ServerMaxBytes           int64  `json:"server_max_bytes"`
	ServerWarnPercent        int    `json:"server_warn_percent"`
	ServerAggressivePercent  int    `json:"server_aggressive_percent"`
	ACKStallWarning          string `json:"ack_stall_warning"`
}

type MemoryDefaultsReport struct {
	AgentRSSTargetBytes        int64 `json:"agent_rss_target_bytes"`
	AgentMemoryHardLimitBytes  int64 `json:"agent_memory_hard_limit_bytes"`
	ServerRSSTargetBytes       int64 `json:"server_rss_target_bytes"`
	ServerMemoryHardLimitBytes int64 `json:"server_memory_hard_limit_bytes"`
}

type CredentialDefaultsReport struct {
	Lifetime                string `json:"lifetime"`
	RenewalRemainingPercent int    `json:"renewal_remaining_percent"`
}

func (defaults Defaults) Report() DefaultsReport {
	return DefaultsReport{
		SchemaVersion: 1,
		WAL: WALDefaultsReport{
			MaxBytes:      defaults.WALMaxBytes,
			Retention:     defaults.WALRetention.String(),
			FsyncInterval: defaults.WALFsyncInterval.String(),
			FsyncBytes:    defaults.WALFsyncBytes,
		},
		Operations: OperationDefaultsReport{
			ResultRetention: defaults.OperationResultRetention.String(),
			ResultMax:       defaults.OperationResultMax,
			OutputTailBytes: defaults.OperationOutputTailBytes,
			CancelGrace:     defaults.CancelGracePeriod.String(),
			StalledWarning:  defaults.StalledWarningAfter.String(),
			Timeouts: OperationTimeoutsReport{
				Container:       defaults.OperationTimeout.Container.String(),
				ComposeUp:       defaults.OperationTimeout.ComposeUp.String(),
				ComposeRestart:  defaults.OperationTimeout.ComposeRestart.String(),
				ComposeDown:     defaults.OperationTimeout.ComposeDown.String(),
				ComposePull:     defaults.OperationTimeout.ComposePull.String(),
				FileWrite:       defaults.OperationTimeout.FileWrite.String(),
				BackupCreate:    defaults.OperationTimeout.BackupCreate.String(),
				BackupRestore:   defaults.OperationTimeout.BackupRestore.String(),
				DiscoveryRescan: defaults.OperationTimeout.DiscoveryRescan.String(),
			},
		},
		Storage: StorageDefaultsReport{
			AgentStateMaxBytes:           defaults.AgentStateMaxBytes,
			FilesystemFreeMinBytes:       defaults.FilesystemFreeMinBytes,
			FilesystemFreeMinPercent:     defaults.FilesystemFreeMinPercent,
			EmergencyReserveBytes:        defaults.EmergencyReserveBytes,
			AutomaticSnapshotsPerProject: defaults.AutomaticSnapshotsPerProject,
			EditableFileMaxBytes:         defaults.EditableFileMaxBytes,
		},
		Discovery: DiscoveryDefaultsReport{
			Interval:             defaults.DiscoveryInterval.String(),
			MaxDirectories:       defaults.DiscoveryMaxDirectories,
			MaxDuration:          defaults.DiscoveryMaxDuration.String(),
			DirectoriesPerSecond: defaults.DiscoveryDirectoriesPerSecond,
		},
		LiveMetrics: LiveMetricsDefaultsReport{
			StatsSampleInterval:  defaults.StatsSampleInterval.String(),
			FrameInterval:        defaults.MetricsFrameInterval.String(),
			BrowserSparklineSize: defaults.BrowserSparklineSamples,
		},
		Connectivity: ConnectivityDefaultsReport{
			HeartbeatInterval: defaults.HeartbeatInterval.String(),
			OfflineAfter:      defaults.OfflineAfter.String(),
		},
		Audit: AuditDefaultsReport{
			EventCoalescingWindow:    defaults.EventCoalescingWindow.String(),
			ObservedMaxPerSecond:     defaults.ObservedAuditMaxPerSecond,
			ServerOperationRetention: defaults.ServerOperationRetention.String(),
			ServerRetention:          defaults.ServerAuditRetention.String(),
			ServerMaxBytes:           defaults.ServerAuditMaxBytes,
			ServerWarnPercent:        defaults.ServerAuditWarnPercent,
			ServerAggressivePercent:  defaults.ServerAuditAggressivePercent,
			ACKStallWarning:          defaults.AuditACKStallWarningAfter.String(),
		},
		Memory: MemoryDefaultsReport{
			AgentRSSTargetBytes:        defaults.AgentRSSTargetBytes,
			AgentMemoryHardLimitBytes:  defaults.AgentMemoryHardLimitBytes,
			ServerRSSTargetBytes:       defaults.ServerRSSTargetBytes,
			ServerMemoryHardLimitBytes: defaults.ServerMemoryHardLimitBytes,
		},
		Credentials: CredentialDefaultsReport{
			Lifetime:                defaults.CredentialLifetime.String(),
			RenewalRemainingPercent: defaults.CredentialRenewalRemainingPercent,
		},
	}
}
