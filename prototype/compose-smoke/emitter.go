// Command compose-smoke-emitter is a disposable fixture for architecture A.5.
// It approximates the simulated Operation's stdout cadence inside a real
// container started by Docker Compose.
package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	linesPerSecond = 50
	lineBytes      = 200
)

func main() {
	duration := 120 * time.Second
	if raw := os.Getenv("COMPOSE_SMOKE_DURATION"); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed <= 0 {
			fmt.Fprintf(os.Stderr, "invalid COMPOSE_SMOKE_DURATION %q\n", raw)
			os.Exit(2)
		}
		duration = parsed
	}
	w := bufio.NewWriterSize(os.Stdout, 64<<10)
	defer w.Flush()
	interval := time.Second / linesPerSecond
	lines := int(duration / interval)
	started := time.Now()
	for i := 1; i <= lines; i++ {
		prefix := "compose-smoke line=" + strconv.Itoa(i) + " "
		padding := lineBytes - 1 - len(prefix)
		if padding < 0 {
			fmt.Fprintln(os.Stderr, "line prefix exceeds controlled line size")
			os.Exit(2)
		}
		fmt.Fprint(w, prefix, strings.Repeat("x", padding), "\n")
		if err := w.Flush(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		target := started.Add(time.Duration(i) * interval)
		if delay := time.Until(target); delay > 0 {
			time.Sleep(delay)
		}
	}
	fmt.Fprintf(os.Stderr, "COMPOSE_SMOKE_COMPLETE lines=%d line_bytes=%d duration_ms=%d\n", lines, lineBytes, time.Since(started).Milliseconds())
}
