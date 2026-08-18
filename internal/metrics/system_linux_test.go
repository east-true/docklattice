//go:build linux

package metrics

import "testing"

func TestCPUListCount(t *testing.T) {
	for input, want := range map[string]int{"0": 1, "0-3": 4, "0-1,4,6-7": 5} {
		got, ok := cpuListCount(input)
		if !ok || got != want {
			t.Errorf("cpuListCount(%q) = %d, %t; want %d, true", input, got, ok, want)
		}
	}
	if _, ok := cpuListCount("3-1"); ok {
		t.Fatal("descending CPU range must be rejected")
	}
}
