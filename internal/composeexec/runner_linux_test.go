//go:build linux

package composeexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const helperEnabled = "DOCKLATTICE_COMPOSEEXEC_HELPER"

func TestMain(main *testing.M) {
	if os.Getenv(helperEnabled) == "1" {
		os.Exit(runHelper())
	}
	os.Exit(main.Run())
}

func TestRunnerDrainsOutputKeepsTailAndUsesExitCodeOnly(t *testing.T) {
	relay := make(chan OutputChunk, 1) // deliberately never consumed while running
	runner := helperRunner("output")
	started := time.Now()
	result, err := runner.Run(context.Background(), validSpec(OperationPull), relay)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("slow relay blocked process drain for %s", elapsed)
	}
	if result.ExitCode != 7 || result.Success() {
		t.Fatalf("exit result = %+v", result)
	}
	if len(result.Tail) != DefaultTailBytes || !result.TailTruncated {
		t.Fatalf("tail = len %d truncated %t", len(result.Tail), result.TailTruncated)
	}
	if result.RelayDroppedBytes == 0 {
		t.Fatal("bounded relay did not report dropped output")
	}
	if len(result.Args) == 0 || result.Args[0] != "compose" {
		t.Fatalf("runner args = %v", result.Args)
	}
}

func TestRunnerCancellationSignalsEntireProcessGroup(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	markers := filepath.Join(dir, "markers")
	runner := helperRunner("term-tree")
	runner.Env = append(runner.Env, "DOCKLATTICE_HELPER_READY="+ready, "DOCKLATTICE_HELPER_MARKERS="+markers)
	ctx, cancel := context.WithCancel(context.Background())
	type outcome struct {
		result Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := runner.Run(ctx, validSpec(OperationUp), nil)
		done <- outcome{result, err}
	}()
	waitForFileContains(t, ready, "child-ready", 3*time.Second)
	cancel()
	completed := <-done
	if completed.err != nil {
		t.Fatal(completed.err)
	}
	if !completed.result.Canceled || completed.result.TimedOut || completed.result.EscalatedToKill {
		t.Fatalf("cancel result = %+v", completed.result)
	}
	waitForFileContains(t, markers, "parent-term", time.Second)
	waitForFileContains(t, markers, "child-term", time.Second)
}

func TestRunnerTimeoutUsesSameTermThenKillPath(t *testing.T) {
	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	runner := helperRunner("ignore-term-tree")
	runner.CancelGrace = 50 * time.Millisecond
	runner.Env = append(runner.Env, "DOCKLATTICE_HELPER_READY="+ready)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	result, err := runner.Run(ctx, validSpec(OperationRestart), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Canceled || !result.TimedOut || !result.EscalatedToKill || result.ExitCode != -1 {
		t.Fatalf("timeout result = %+v", result)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatalf("helper did not start before timeout: %v", err)
	}
}

func TestRunnerRejectsInvalidSpecBeforeStartingAndReportsStartFailure(t *testing.T) {
	invalid := validSpec(OperationUp)
	invalid.Services = []string{"--help"}
	if _, err := (Runner{DockerPath: "/definitely/not/a/binary"}).Run(context.Background(), invalid, nil); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("invalid spec error = %v", err)
	}
	if _, err := (Runner{DockerPath: "/definitely/not/a/binary"}).Run(context.Background(), validSpec(OperationPS), nil); err == nil || !strings.Contains(err.Error(), "start docker compose") {
		t.Fatalf("start failure = %v", err)
	}
}

func helperRunner(mode string) Runner {
	return Runner{
		DockerPath:  os.Args[0],
		CancelGrace: 2 * time.Second,
		Env: append(os.Environ(),
			helperEnabled+"=1",
			"DOCKLATTICE_HELPER_MODE="+mode,
		),
	}
}

func runHelper() int {
	switch os.Getenv("DOCKLATTICE_HELPER_MODE") {
	case "output":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("o"), 100<<10))
		_, _ = os.Stderr.Write([]byte("stderr-final\n"))
		return 7
	case "term-tree":
		return runSignalTree(false)
	case "term-child":
		return runSignalChild(false)
	case "ignore-term-tree":
		return runSignalTree(true)
	case "ignore-term-child":
		return runSignalChild(true)
	default:
		_, _ = fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", os.Getenv("DOCKLATTICE_HELPER_MODE"))
		return 2
	}
}

func runSignalTree(ignore bool) int {
	var signals chan os.Signal
	if ignore {
		signal.Ignore(syscall.SIGTERM)
	} else {
		signals = make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
	}
	childMode := "term-child"
	if ignore {
		childMode = "ignore-term-child"
	}
	child := exec.Command(os.Args[0])
	child.Env = replaceEnv(os.Environ(), "DOCKLATTICE_HELPER_MODE", childMode)
	if err := child.Start(); err != nil {
		return 3
	}
	appendLine(os.Getenv("DOCKLATTICE_HELPER_READY"), "parent-ready "+strconv.Itoa(os.Getpid()))
	if ignore {
		appendLine(os.Getenv("DOCKLATTICE_HELPER_READY"), "child-spawned "+strconv.Itoa(child.Process.Pid))
		for {
			time.Sleep(time.Hour)
		}
	}
	<-signals
	appendLine(os.Getenv("DOCKLATTICE_HELPER_MARKERS"), "parent-term")
	return 0
}

func runSignalChild(ignore bool) int {
	if ignore {
		signal.Ignore(syscall.SIGTERM)
		appendLine(os.Getenv("DOCKLATTICE_HELPER_READY"), "child-ready "+strconv.Itoa(os.Getpid()))
		for {
			time.Sleep(time.Hour)
		}
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM)
	appendLine(os.Getenv("DOCKLATTICE_HELPER_READY"), "child-ready "+strconv.Itoa(os.Getpid()))
	<-signals
	appendLine(os.Getenv("DOCKLATTICE_HELPER_MARKERS"), "child-term")
	return 0
}

func appendLine(path, line string) {
	if path == "" {
		return
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	_, _ = file.WriteString(line + "\n")
	_ = file.Close()
}

func replaceEnv(environment []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}

func waitForFileContains(t *testing.T, path, substring string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		payload, err := os.ReadFile(path)
		if err == nil && bytes.Contains(payload, []byte(substring)) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	payload, _ := os.ReadFile(path)
	t.Fatalf("%s did not contain %q before timeout; content=%q", path, substring, payload)
}
