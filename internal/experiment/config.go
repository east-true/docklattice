// Package experiment is the one-shot Transport Prototype load driver from ADR
// Appendix A. It is not production DockLattice code.
package experiment

import "time"

type Config struct {
	Scenario          int     `json:"scenario"`
	TimeScale         float64 `json:"time_scale"`
	ControlledHarness bool    `json:"controlled_harness"`
	AuditRate         int     `json:"audit_records_per_second"`
	AuditPayloadBytes int     `json:"audit_payload_bytes"`
	AuditMode         string  `json:"audit_mode"`
	Agents            int     `json:"agents"`
	PauseLog          bool    `json:"pause_log_consumer"`
	EchoPayload       int     `json:"echo_payload_bytes"`
	StatsTargets      int     `json:"stats_targets"`
	LogStreams        int     `json:"log_streams"`
	LogBytesSecond    int     `json:"log_bytes_per_second"`
	LogLineBytes      int     `json:"log_line_bytes"`
}

func DefaultConfig(scenario int) Config {
	c := Config{Scenario: scenario, TimeScale: 1, AuditRate: 20, AuditPayloadBytes: 512, AuditMode: "managed-like", Agents: 1, PauseLog: true, EchoPayload: 1024, StatsTargets: 6, LogStreams: 4, LogBytesSecond: 200 << 10, LogLineBytes: 200}
	switch scenario {
	case 2:
		// A rate is selected by the invocation: 20, 50, or 100.
		c.PauseLog = true
	case 3:
		c.Agents = 20
		c.AuditRate = 5
		c.LogStreams = 1
		c.StatsTargets = 1
		c.PauseLog = false
	case 4:
		c.AuditRate = 0
		c.LogStreams = 0
		c.StatsTargets = 0
		c.PauseLog = false
	}
	return c
}

func (c Config) scaled(d time.Duration) time.Duration {
	scale := c.TimeScale
	if scale <= 0 {
		scale = 1
	}
	out := time.Duration(float64(d) * scale)
	if out < time.Millisecond {
		return time.Millisecond
	}
	return out
}

func (c Config) Duration() time.Duration {
	switch c.Scenario {
	case 1:
		return c.scaled(10 * time.Minute)
	case 2:
		return c.scaled(5 * time.Minute)
	case 3:
		return c.scaled(10 * time.Minute)
	case 4:
		// 200 stream cycles and 50 operation cycles, five seconds apart.
		return c.scaled(1250 * time.Second)
	default:
		return 0
	}
}

func (c Config) OperationDuration() time.Duration {
	if c.Scenario == 1 || c.Scenario == 2 {
		return c.scaled(120 * time.Second)
	}
	return c.scaled(30 * time.Second)
}
