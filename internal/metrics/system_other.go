//go:build !linux

package metrics

func collectLinuxProcess(*Registry) {}
func collectCgroupV2(*Registry)     {}
