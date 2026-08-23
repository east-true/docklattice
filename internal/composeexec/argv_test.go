package composeexec

import (
	"errors"
	"reflect"
	"testing"
)

func validSpec(operation Operation) Spec {
	return Spec{
		Operation: operation,
		Project: Project{
			WorkingDir: "/srv/stacks/demo",
			Files:      []string{"/srv/stacks/demo/compose.yaml", "/srv/stacks/demo/compose.override.yaml"},
			Name:       "demo_project",
		},
	}
}

func TestBuildArgsUsesFixedComposeArgvAndAllowlistedFlags(t *testing.T) {
	spec := validSpec(OperationUp)
	spec.Services = []string{"web", "worker-1"}
	spec.Flags = Flags{RemoveOrphans: true, ForceRecreate: true, Pull: PullPolicyAlways}
	got, err := BuildArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"compose", "--progress", "plain",
		"--project-directory", "/srv/stacks/demo",
		"--file", "/srv/stacks/demo/compose.yaml",
		"--file", "/srv/stacks/demo/compose.override.yaml",
		"--project-name", "demo_project",
		"up", "--detach", "--no-build", "--remove-orphans", "--force-recreate", "--pull", "always",
		"web", "worker-1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BuildArgs() = %#v\nwant %#v", got, want)
	}
	for _, forbidden := range []string{"sh", "-c", "bash"} {
		for _, arg := range got {
			if arg == forbidden {
				t.Fatalf("shell token %q escaped into argv", forbidden)
			}
		}
	}
}

func TestBuildArgsAlwaysDisablesComposeBuildForUp(t *testing.T) {
	spec := validSpec(OperationUp)
	args, err := BuildArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	wantSuffix := []string{"up", "--detach", "--no-build"}
	if !reflect.DeepEqual(args[len(args)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("up suffix = %#v, want %#v", args, wantSuffix)
	}
}

func TestBuildArgsSupportsOnlyNamedOperationsAndTheirTargets(t *testing.T) {
	for _, operation := range []Operation{
		OperationPS, OperationPull, OperationUp, OperationDown, OperationStart,
		OperationStop, OperationRestart, OperationLogs, OperationConfig,
	} {
		t.Run(string(operation), func(t *testing.T) {
			spec := validSpec(operation)
			if operation != OperationDown {
				spec.Services = []string{"api"}
			}
			if _, err := BuildArgs(spec); err != nil {
				t.Fatal(err)
			}
		})
	}

	logs := validSpec(OperationLogs)
	logs.Flags = Flags{LogsFollow: true, LogsTail: 200, LogsTimestamps: true}
	logs.Services = []string{"api"}
	args, err := BuildArgs(logs)
	if err != nil {
		t.Fatal(err)
	}
	wantSuffix := []string{"logs", "--follow", "--tail", "200", "--timestamps", "api"}
	if !reflect.DeepEqual(args[len(args)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("logs suffix = %v", args)
	}

	for mode, suffix := range map[ConfigOutput][]string{
		ConfigOutputJSON:  {"config", "--format", "json"},
		ConfigOutputQuiet: {"config", "--quiet"},
	} {
		config := validSpec(OperationConfig)
		config.Flags.ConfigOutput = mode
		args, err := BuildArgs(config)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(args[len(args)-len(suffix):], suffix) {
			t.Fatalf("config %q suffix = %v", mode, args)
		}
	}
	config := validSpec(OperationConfig)
	config.Services = []string{"api"}
	config.Flags.ConfigNoInterpolate = true
	args, err = BuildArgs(config)
	if err != nil {
		t.Fatal(err)
	}
	wantSuffix = []string{"config", "--no-interpolate", "api"}
	if !reflect.DeepEqual(args[len(args)-len(wantSuffix):], wantSuffix) {
		t.Fatalf("no-interpolate config suffix = %v", args)
	}
}

func TestBuildArgsSupportsAgentResolvedEnvFileForConfigValidation(t *testing.T) {
	spec := validSpec(OperationConfig)
	spec.Project.EnvFile = "/proc/123/fd/7/.dockpilot-stage-env"
	spec.Flags.ConfigOutput = ConfigOutputQuiet
	args, err := BuildArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := []string{"compose", "--progress", "plain", "--project-directory", "/srv/stacks/demo", "--env-file", spec.Project.EnvFile}
	if !reflect.DeepEqual(args[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("env validation prefix = %#v", args)
	}
	spec.Project.EnvFile = "../../etc/passwd"
	if _, err := BuildArgs(spec); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("relative env file error = %v", err)
	}
}

func TestBuildArgsRejectsInjectionAndCrossOperationFlags(t *testing.T) {
	tests := map[string]func(*Spec){
		"operation":            func(spec *Spec) { spec.Operation = Operation("up; rm -rf /tmp/x") },
		"working dir":          func(spec *Spec) { spec.Project.WorkingDir = "relative;sh" },
		"working nul":          func(spec *Spec) { spec.Project.WorkingDir = "/srv/demo\x00--help" },
		"file":                 func(spec *Spec) { spec.Project.Files[0] = "--file=/tmp/evil" },
		"project":              func(spec *Spec) { spec.Project.Name = "demo;touch-pwned" },
		"service option":       func(spec *Spec) { spec.Services = []string{"--help"} },
		"service shell":        func(spec *Spec) { spec.Services = []string{"web;id"} },
		"too many files":       func(spec *Spec) { spec.Project.Files = make([]string, 33) },
		"down service":         func(spec *Spec) { spec.Operation = OperationDown; spec.Services = []string{"web"} },
		"wrong orphan":         func(spec *Spec) { spec.Flags.RemoveOrphans = true },
		"wrong recreate":       func(spec *Spec) { spec.Flags.ForceRecreate = true },
		"wrong pull":           func(spec *Spec) { spec.Flags.Pull = PullPolicyAlways },
		"unknown pull":         func(spec *Spec) { spec.Operation = OperationUp; spec.Flags.Pull = PullPolicy("shell") },
		"wrong log follow":     func(spec *Spec) { spec.Flags.LogsFollow = true },
		"wrong log timestamps": func(spec *Spec) { spec.Flags.LogsTimestamps = true },
		"negative tail":        func(spec *Spec) { spec.Operation = OperationLogs; spec.Flags.LogsTail = -1 },
		"wrong ps all":         func(spec *Spec) { spec.Flags.PSAll = true },
		"wrong config mode":    func(spec *Spec) { spec.Flags.ConfigOutput = ConfigOutputJSON },
		"unknown config":       func(spec *Spec) { spec.Operation = OperationConfig; spec.Flags.ConfigOutput = ConfigOutput("yaml") },
		"wrong no interpolate": func(spec *Spec) { spec.Flags.ConfigNoInterpolate = true },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			spec := validSpec(OperationPull)
			mutate(&spec)
			if _, err := BuildArgs(spec); !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("BuildArgs error = %v", err)
			}
		})
	}
}
