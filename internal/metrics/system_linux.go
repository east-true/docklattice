//go:build linux

package metrics

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func collectLinuxProcess(r *Registry) {
	if b, err := os.ReadFile("/proc/self/statm"); err == nil {
		fields := strings.Fields(string(b))
		if len(fields) > 1 {
			if pages, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				r.Set(ProcessRSSBytes, float64(pages*uint64(os.Getpagesize())))
			}
		}
	}
	if entries, err := os.ReadDir("/proc/self/fd"); err == nil {
		r.Set(OpenFDs, float64(len(entries)))
	}
	if status, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "Cpus_allowed_list:") {
				if count, ok := cpuListCount(strings.TrimSpace(strings.TrimPrefix(line, "Cpus_allowed_list:"))); ok {
					r.Set(ProcessCPUAffinityCores, float64(count))
				}
				break
			}
		}
	}
	var nofile syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &nofile); err == nil {
		r.Set(RlimitNoFileSoft, float64(nofile.Cur))
		r.Set(RlimitNoFileHard, float64(nofile.Max))
	}
}

func cpuListCount(list string) (int, bool) {
	total := 0
	for _, item := range strings.Split(list, ",") {
		bounds := strings.SplitN(item, "-", 2)
		first, err := strconv.Atoi(bounds[0])
		if err != nil {
			return 0, false
		}
		last := first
		if len(bounds) == 2 {
			last, err = strconv.Atoi(bounds[1])
			if err != nil || last < first {
				return 0, false
			}
		}
		total += last - first + 1
	}
	return total, total > 0
}

func collectCgroupV2(r *Registry) {
	root := currentCgroupV2Path()
	current := readUintFile(filepath.Join(root, "memory.current"))
	peak := readUintFile(filepath.Join(root, "memory.peak"))
	max := readUintFile(filepath.Join(root, "memory.max"))
	high := readUintFile(filepath.Join(root, "memory.high"))
	swapMax := readUintFile(filepath.Join(root, "memory.swap.max"))
	r.Set(CgroupMemoryCurrentBytes, float64(current))
	r.Set(CgroupMemoryPeakBytes, float64(peak))
	if max > 0 {
		r.Set(CgroupMemoryMaxBytes, float64(max))
	}
	r.Set(CgroupMemoryHighBytes, float64(high))
	r.Set(CgroupMemorySwapMaxBytes, float64(swapMax))
	cpuMax, _ := os.ReadFile(filepath.Join(root, "cpu.max"))
	parts := strings.Fields(string(cpuMax))
	if len(parts) == 2 && parts[0] != "max" {
		quota, quotaErr := strconv.ParseUint(parts[0], 10, 64)
		period, periodErr := strconv.ParseUint(parts[1], 10, 64)
		if quotaErr == nil && periodErr == nil && period > 0 {
			r.Set(CgroupCPUQuotaCores, float64(quota)/float64(period))
		}
	}
	stat := readKVFile(filepath.Join(root, "memory.stat"))
	anon, file, inactive := stat["anon"], stat["file"], stat["inactive_file"]
	r.Set(CgroupMemoryAnonBytes, float64(anon))
	r.Set(CgroupMemoryFileBytes, float64(file))
	r.Set(CgroupMemoryInactiveFileBytes, float64(inactive))
	r.Set(CgroupMemoryKernelBytes, float64(stat["kernel"]))
	r.Set(CgroupMemoryKernelStackBytes, float64(stat["kernel_stack"]))
	r.Set(CgroupMemoryPageTablesBytes, float64(stat["pagetables"]))
	r.Set(CgroupMemorySlabBytes, float64(stat["slab"]))
	r.Set(CgroupMemorySlabUnreclaimableBytes, float64(stat["slab_unreclaimable"]))
	r.Set(CgroupMemorySockBytes, float64(stat["sock"]))
	r.Set(CgroupMemoryFileDirtyBytes, float64(stat["file_dirty"]))
	r.Set(CgroupMemoryFileWritebackBytes, float64(stat["file_writeback"]))
	working := current
	if inactive < working {
		working -= inactive
	} else {
		working = 0
	}
	r.Set(CgroupMemoryWorkingSetApproxBytes, float64(working))
	events := readKVFile(filepath.Join(root, "memory.events.local"))
	r.Set(CgroupMemoryEventsMaxTotal, float64(events["max"]))
	r.Set(CgroupMemoryEventsOOMTotal, float64(events["oom"]))
	r.Set(CgroupMemoryEventsOOMKillTotal, float64(events["oom_kill"]))
	pressure, _ := os.ReadFile(filepath.Join(root, "memory.pressure"))
	for _, line := range strings.Split(string(pressure), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 0 {
			continue
		}
		for _, field := range parts[1:] {
			if !strings.HasPrefix(field, "total=") {
				continue
			}
			v, _ := strconv.ParseUint(strings.TrimPrefix(field, "total="), 10, 64)
			if parts[0] == "some" {
				r.Set(CgroupMemoryPressureSomeTotalMicros, float64(v))
			} else if parts[0] == "full" {
				r.Set(CgroupMemoryPressureFullTotalMicros, float64(v))
			}
		}
	}
}

func currentCgroupV2Path() string {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "/sys/fs/cgroup"
	}
	for _, line := range strings.Split(string(b), "\n") {
		// Unified hierarchy format: 0::<relative path>.
		if strings.HasPrefix(line, "0::") {
			rel := strings.TrimPrefix(line, "0::")
			return filepath.Join("/sys/fs/cgroup", filepath.Clean("/"+rel))
		}
	}
	return "/sys/fs/cgroup"
}

func readUintFile(path string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil || strings.TrimSpace(string(b)) == "max" {
		return 0
	}
	v, _ := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	return v
}

func readKVFile(path string) map[string]uint64 {
	out := make(map[string]uint64)
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		out[fields[0]], _ = strconv.ParseUint(fields[1], 10, 64)
	}
	return out
}
