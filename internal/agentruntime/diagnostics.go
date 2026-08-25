package agentruntime

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	agentDiagnosticLineLimit = 1024
	diagnosticRepeatInterval = time.Minute
)

var (
	diagnosticAssignment = regexp.MustCompile(`(?i)(token|credential|authorization|secret)=([^\s,;]+)`)
	diagnosticBearer     = regexp.MustCompile(`(?i)bearer\s+[^\s,;]+`)
)

type diagnosticField struct {
	key   string
	value string
}

type agentDiagnostics struct {
	mu          sync.Mutex
	writer      io.Writer
	now         func() time.Time
	lastProblem map[string]time.Time
}

func newAgentDiagnostics(writer io.Writer, now func() time.Time) *agentDiagnostics {
	if now == nil {
		now = time.Now
	}
	return &agentDiagnostics{
		writer:      writer,
		now:         now,
		lastProblem: make(map[string]time.Time),
	}
}

func (diagnostics *agentDiagnostics) info(event string, fields ...diagnosticField) {
	diagnostics.write("INFO", event, nil, fields...)
}

func (diagnostics *agentDiagnostics) failure(event string, err error, fields ...diagnosticField) {
	if err == nil {
		return
	}
	diagnostics.write("ERROR", event, err, fields...)
}

// problem rate-limits a repeating failure by event name. Reconnect and Docker
// event loops must remain observable without turning an outage into an
// unbounded log stream.
func (diagnostics *agentDiagnostics) problem(event string, err error, fields ...diagnosticField) {
	if diagnostics == nil || diagnostics.writer == nil || err == nil {
		return
	}
	diagnostics.mu.Lock()
	now := diagnostics.now().UTC()
	if previous, exists := diagnostics.lastProblem[event]; exists && now.Sub(previous) < diagnosticRepeatInterval {
		diagnostics.mu.Unlock()
		return
	}
	diagnostics.lastProblem[event] = now
	diagnostics.writeLocked(now, "WARN", event, err, fields...)
	diagnostics.mu.Unlock()
}

func (diagnostics *agentDiagnostics) resolved(event string) {
	if diagnostics == nil {
		return
	}
	diagnostics.mu.Lock()
	delete(diagnostics.lastProblem, event)
	diagnostics.mu.Unlock()
}

func (diagnostics *agentDiagnostics) write(level, event string, err error, fields ...diagnosticField) {
	if diagnostics == nil || diagnostics.writer == nil {
		return
	}
	diagnostics.mu.Lock()
	diagnostics.writeLocked(diagnostics.now().UTC(), level, event, err, fields...)
	diagnostics.mu.Unlock()
}

func (diagnostics *agentDiagnostics) writeLocked(
	now time.Time,
	level string,
	event string,
	err error,
	fields ...diagnosticField,
) {
	var line strings.Builder
	fmt.Fprintf(
		&line,
		"time=%s level=%s component=agent event=%s",
		now.Format(time.RFC3339Nano),
		level,
		event,
	)
	for _, field := range fields {
		if field.key == "" {
			continue
		}
		fmt.Fprintf(&line, " %s=%s", field.key, strconv.Quote(safeDiagnosticText(field.value)))
	}
	if err != nil {
		fmt.Fprintf(&line, " error=%s", strconv.Quote(safeDiagnosticText(err.Error())))
	}
	line.WriteByte('\n')
	_, _ = io.WriteString(diagnostics.writer, boundedDiagnosticLine(line.String()))
}

func safeDiagnosticText(value string) string {
	value = strings.Map(func(char rune) rune {
		if char == '\n' || char == '\r' {
			return ' '
		}
		return char
	}, value)
	value = diagnosticAssignment.ReplaceAllString(value, `${1}=[REDACTED]`)
	return diagnosticBearer.ReplaceAllString(value, "Bearer [REDACTED]")
}

func boundedDiagnosticLine(line string) string {
	if len(line) <= agentDiagnosticLineLimit {
		return line
	}
	limit := agentDiagnosticLineLimit - len("...\n")
	for limit > 0 && !utf8.RuneStart(line[limit]) {
		limit--
	}
	return line[:limit] + "...\n"
}
