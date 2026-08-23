//go:build linux

package composeconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/composeexec"
)

const evaluatorHelper = "DOCKPILOT_COMPOSECONFIG_HELPER"

func TestMain(main *testing.M) {
	if os.Getenv(evaluatorHelper) == "1" {
		switch os.Getenv("DOCKPILOT_COMPOSECONFIG_MODE") {
		case "json":
			_, _ = os.Stdout.WriteString(`{"name":"demo","services":{"web":{"environment":{"SECRET":"not-returned"}},"db":{}}}`)
			os.Exit(0)
		case "env-files":
			if !hasComposeConfigArgs(os.Args[1:]) {
				os.Exit(9)
			}
			_, _ = os.Stdout.WriteString(`{"name":"demo","services":{"web":{"env_file":["configs/web.env",{"path":"shared/common.env","required":true}],"environment":{"SECRET":"not-returned"}},"worker":{"env_file":{"path":"configs/web.env"}}}}`)
			os.Exit(0)
		case "models":
			if hasArg(os.Args[1:], "--profile", "*") {
				_, _ = os.Stdout.WriteString(`{"name":"demo","services":{"api":{"image":"company/api:1.8","build":{"context":"."},"profiles":["prod"],"depends_on":{"db":{"condition":"service_started"}}},"db":{"image":"postgres:18"},"worker":{"build":{"context":"worker"},"profiles":["tools"]},"builder":{"image":"company/builder:1","build":{"context":"builder"},"pull_policy":"build"}},"secrets":{"token":{"file":"./token.txt"},"external_token":{"name":"shared-token","external":true}},"configs":{"settings":{"environment":"SETTINGS_FILE"}}}`)
			} else {
				_, _ = os.Stdout.WriteString(`{"name":"demo","services":{"api":{"image":"company/api:1.8","build":{"context":"."},"profiles":["prod"],"depends_on":{"db":{"condition":"service_started"}}},"db":{"image":"postgres:18"}}}`)
			}
			os.Exit(0)
		case "oversize":
			_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), 4096))
			os.Exit(0)
		case "fail":
			_, _ = os.Stderr.WriteString("SECRET_FROM_COMPOSE=do-not-disclose\n")
			os.Exit(7)
		case "hang":
			for {
				time.Sleep(time.Hour)
			}
		}
		os.Exit(2)
	}
	os.Exit(main.Run())
}

func hasArg(args []string, pair ...string) bool {
	for index := 0; index+len(pair) <= len(args); index++ {
		matched := true
		for offset := range pair {
			if args[index+offset] != pair[offset] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func hasComposeConfigArgs(args []string) bool {
	want := []string{"config", "--format", "json", "--no-env-resolution"}
	for index := 0; index+len(want) <= len(args); index++ {
		matched := true
		for offset := range want {
			if args[index+offset] != want[offset] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func helperEvaluator(mode string) Evaluator {
	return Evaluator{
		DockerPath: os.Args[0], CancelGrace: 20 * time.Millisecond,
		Env: append(os.Environ(), evaluatorHelper+"=1", "DOCKPILOT_COMPOSECONFIG_MODE="+mode),
	}
}

func TestEvaluateDelegatesToComposeAndReturnsOnlyIdentity(t *testing.T) {
	result, err := helperEvaluator("json").Evaluate(context.Background(), "/srv/demo", []string{"/srv/demo/compose.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Project.Name != "demo" || fmt.Sprint(result.Services) != "[db web]" {
		t.Fatalf("result = %#v", result)
	}
	if strings.Contains(fmt.Sprintf("%#v", result), "not-returned") {
		t.Fatalf("resolved secret escaped result: %#v", result)
	}
}

func TestEvaluateCollectsOnlyDistinctServiceEnvFilePaths(t *testing.T) {
	result, err := helperEvaluator("env-files").Evaluate(context.Background(), "/srv/demo", []string{"/srv/demo/compose.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(result.EnvFiles); got != "[configs/web.env shared/common.env]" {
		t.Fatalf("env files = %s", got)
	}
	if strings.Contains(fmt.Sprintf("%#v", result), "not-returned") {
		t.Fatalf("resolved secret escaped result: %#v", result)
	}
}

func TestEvaluateReturnsEffectiveServiceBuildProfileAndResourceMetadata(t *testing.T) {
	result, err := helperEvaluator("models").Evaluate(context.Background(), "/srv/demo", []string{"/srv/demo/compose.yml"})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(result.Services); got != "[api builder db worker]" {
		t.Fatalf("services = %s", got)
	}
	if got := fmt.Sprint(result.ActiveProfiles); got != "[prod]" {
		t.Fatalf("active profiles = %s", got)
	}
	byName := make(map[string]composeexec.Service)
	for _, service := range result.ServiceModels {
		byName[service.Name] = service
	}
	if !byName["api"].Active || !byName["api"].ImageBacked() || fmt.Sprint(byName["api"].DependsOn) != "[db]" {
		t.Fatalf("api model = %#v", byName["api"])
	}
	if byName["worker"].Active || !byName["worker"].BuildRequired() {
		t.Fatalf("worker model = %#v", byName["worker"])
	}
	if !byName["builder"].BuildRequired() {
		t.Fatalf("builder model = %#v", byName["builder"])
	}
	if got := fmt.Sprint(result.Secrets); !strings.Contains(got, "token file ./token.txt") || !strings.Contains(got, "external_token external shared-token") {
		t.Fatalf("secrets = %#v", result.Secrets)
	}
	if got := fmt.Sprint(result.Configs); !strings.Contains(got, "settings environment SETTINGS_FILE") {
		t.Fatalf("configs = %#v", result.Configs)
	}
}

func TestParseEnvFileFieldRejectsInvalidEntriesWithoutEchoingPayload(t *testing.T) {
	_, err := parseEnvFileField(json.RawMessage(`[{"path":"\u0000TOP_SECRET=do-not-disclose"}]`))
	if err == nil || strings.Contains(err.Error(), "TOP_SECRET") {
		t.Fatalf("env_file parse error = %v", err)
	}
}

func TestEvaluateBoundsOutputAndRedactsFailure(t *testing.T) {
	evaluator := helperEvaluator("oversize")
	evaluator.MaxOutput = 32
	if _, err := evaluator.Evaluate(context.Background(), "/srv/demo", []string{"/srv/demo/compose.yml"}); !errors.Is(err, ErrOutputTooLarge) {
		t.Fatalf("oversize error = %v", err)
	}
	_, err := helperEvaluator("fail").Evaluate(context.Background(), "/srv/demo", []string{"/srv/demo/compose.yml"})
	if err == nil || strings.Contains(err.Error(), "SECRET_FROM_COMPOSE") || !strings.Contains(err.Error(), "status 7") {
		t.Fatalf("failure error = %v", err)
	}
}

func TestEvaluateCancellationTerminatesProcessGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := helperEvaluator("hang").Evaluate(ctx, "/srv/demo", []string{"/srv/demo/compose.yml"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestEvaluateRejectsUnsafeInputsBeforeExec(t *testing.T) {
	evaluator := Evaluator{DockerPath: "/definitely/missing"}
	for _, tt := range []struct {
		dir   string
		files []string
	}{
		{"relative", []string{"/srv/compose.yml"}},
		{"/srv", nil},
		{"/srv", []string{"/outside/compose.yml"}},
		{"/srv", []string{"relative.yml"}},
	} {
		if _, err := evaluator.Evaluate(context.Background(), tt.dir, tt.files); !errors.Is(err, ErrInvalidProject) {
			t.Fatalf("Evaluate(%q,%v) error = %v", tt.dir, tt.files, err)
		}
	}
}
