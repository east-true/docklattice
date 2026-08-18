package serverapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/producttransport"
	"github.com/east-true/dockpilot/internal/projectmodel"
	"github.com/east-true/dockpilot/internal/serverstore"
	"github.com/east-true/dockpilot/internal/webui"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestDashboardCombinesOnlyAllowedCacheWithLiveSession(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-b", "Offline", `{}`)
	insertAgent(t, ctx, store, "agent-a", "Active", `{
		"fs_read": true,
		"fs_write": false,
		"fs_write_reason": "read-only root"
	}`)
	insertAgent(t, ctx, store, "retired", "Retired", `{}`)
	if _, err := store.DB().ExecContext(ctx, `UPDATE agents SET retired_at = ? WHERE id = 'retired'`, dbTime(time.Now())); err != nil {
		t.Fatal(err)
	}
	insertProject(t, ctx, store, "project-b", "agent-b", "offline-project", `{}`)
	insertProject(t, ctx, store, "project-a", "agent-a", "active-project", `{"collision":true}`)

	session := newFakeSession("agent-a")
	session.capability = producttransport.Capability{
		ConnectionReady: true,
		DockerReady:     true,
		ComposeReady:    false,
		Reason:          "Compose plugin unavailable",
	}
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}

	dashboard, err := backend.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dashboard.Hosts) != 2 || dashboard.Hosts[0].ID != "agent-a" || dashboard.Hosts[1].ID != "agent-b" {
		t.Fatalf("hosts = %+v", dashboard.Hosts)
	}
	active := dashboard.Hosts[0]
	if active.State != "ACTIVE" || !active.Capabilities.Connection.Enabled || !active.Capabilities.Docker.Enabled ||
		active.Capabilities.Compose.Enabled || active.Capabilities.Compose.Reason != "Compose plugin unavailable" ||
		!active.Capabilities.FSRead.Enabled || active.Capabilities.FSWrite.Enabled || active.Capabilities.FSWrite.Reason != "read-only root" {
		t.Fatalf("active host = %+v", active)
	}
	if dashboard.Hosts[1].State != "OFFLINE" || dashboard.Hosts[1].Capabilities.Connection.Enabled {
		t.Fatalf("offline host = %+v", dashboard.Hosts[1])
	}
	if len(dashboard.Projects) != 2 || dashboard.Projects[0].UID != "project-a" ||
		!dashboard.Projects[0].Collision || !dashboard.Projects[0].ReadOnly {
		t.Fatalf("projects = %+v", dashboard.Projects)
	}
	if session.heartbeatCalls() != 1 {
		t.Fatalf("heartbeat calls = %d", session.heartbeatCalls())
	}

	host, err := backend.Host(ctx, "agent-a")
	if err != nil || host.ID != "agent-a" {
		t.Fatalf("Host = %+v, %v", host, err)
	}
	if _, err := backend.Host(ctx, "retired"); !errors.Is(err, webui.ErrNotFound) {
		t.Fatalf("retired Host error = %v", err)
	}
}

func TestDashboardPreservesEnabledCapabilityWarning(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "Agent", `{}`)
	warning := "DEGRADED_STORAGE: FILESYSTEM_FREE_LOW"
	session := newFakeSession("agent-a")
	session.capability = producttransport.Capability{
		ConnectionReady: true,
		DockerReady:     true,
		ComposeReady:    true,
		Reason:          warning,
	}
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}

	host, err := backend.Host(ctx, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	for name, capability := range map[string]webui.Capability{
		"connection": host.Capabilities.Connection,
		"docker":     host.Capabilities.Docker,
		"compose":    host.Capabilities.Compose,
	} {
		if !capability.Enabled || capability.Reason != warning {
			t.Fatalf("%s capability = %+v, want enabled warning %q", name, capability, warning)
		}
	}
}

func TestDashboardReconcilesVerifiedProjectsAndTierOneDriftWithoutRawFacts(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	agentID := "11111111-1111-4111-8111-111111111111"
	insertAgent(t, ctx, store, agentID, "Agent", `{"fs_read":true,"fs_write":true}`)
	at := time.Date(2026, 8, 15, 1, 2, 3, 0, time.UTC)
	secret := "project-service-secret-never-persist"
	project := testAgentProject(t, agentID, "/srv", "/srv/app", "app", strings.Repeat("a", 64), []string{secret})
	session := newFakeSession(agentID)
	session.setProjectListPayload(projectListPayload(t, at, false, project))
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}

	dashboard, err := backend.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dashboard.Projects) != 1 {
		t.Fatalf("projects = %+v", dashboard.Projects)
	}
	got := dashboard.Projects[0]
	if got.UID != project.UID || got.WorkingDir != project.WorkingDir || got.Name != "app" || !got.Present ||
		got.Stale || got.ReadOnly || !got.ComposeExecutable || !got.FilesystemWritable ||
		got.CurrentFingerprint != project.CurrentFingerprint || got.LastVerifiedFingerprint != project.CurrentFingerprint ||
		got.AppliedFingerprint != "" || got.Drift != string(projectmodel.DriftNoBaseline) ||
		got.LastVerifiedAt == nil || !got.LastVerifiedAt.Equal(at) {
		t.Fatalf("project = %+v", got)
	}
	if len(dashboard.Hosts) != 1 || dashboard.Hosts[0].ProjectScan == nil ||
		!dashboard.Hosts[0].ProjectScan.ScannedAt.Equal(at) || dashboard.Hosts[0].ProjectScan.Truncated {
		t.Fatalf("host project scan = %+v", dashboard.Hosts)
	}
	var scannedAt, statusJSON, flagsJSON string
	if err := store.DB().QueryRowContext(ctx, `
		SELECT agents.projects_scanned_at, agents.project_scan_status_json, projects.flags_json
		FROM agents JOIN projects ON projects.agent_id = agents.id WHERE agents.id = ?
	`, agentID).Scan(&scannedAt, &statusJSON, &flagsJSON); err != nil {
		t.Fatal(err)
	}
	if scannedAt != "2026-08-15T01:02:03.000000000Z" || !strings.Contains(flagsJSON, project.CurrentFingerprint) ||
		strings.Contains(statusJSON, secret) || strings.Contains(flagsJSON, secret) {
		t.Fatalf("stored scan=%q status=%q flags=%q", scannedAt, statusJSON, flagsJSON)
	}
	var secretMatches int
	if err := store.DB().QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM agents WHERE instr(metadata_json, ?) > 0 OR instr(capabilities_json, ?) > 0 OR instr(project_scan_status_json, ?) > 0) +
			(SELECT COUNT(*) FROM projects WHERE instr(working_dir, ?) > 0 OR instr(name, ?) > 0 OR instr(applied_fingerprints_json, ?) > 0 OR instr(flags_json, ?) > 0)
	`, secret, secret, secret, secret, secret, secret, secret).Scan(&secretMatches); err != nil {
		t.Fatal(err)
	}
	if secretMatches != 0 {
		t.Fatalf("raw project facts leaked secret into Server state: %d", secretMatches)
	}

	appliedJSON := fmt.Sprintf(`{"version":1,"fingerprint":%q}`, project.CurrentFingerprint)
	if _, err := store.DB().ExecContext(ctx, `UPDATE projects SET applied_fingerprints_json = ? WHERE project_uid = ?`, appliedJSON, project.UID); err != nil {
		t.Fatal(err)
	}
	session.setProjectListPayload(projectListPayload(t, at.Add(time.Second), false, project))
	dashboard, err = backend.Dashboard(ctx)
	if err != nil || dashboard.Projects[0].Drift != string(projectmodel.DriftInSync) {
		t.Fatalf("in-sync dashboard = %+v, %v", dashboard.Projects, err)
	}
	project = testAgentProject(t, agentID, "/srv", "/srv/app", "app", strings.Repeat("b", 64), []string{"web"})
	session.setProjectListPayload(projectListPayload(t, at.Add(2*time.Second), false, project))
	dashboard, err = backend.Dashboard(ctx)
	if err != nil || dashboard.Projects[0].Drift != string(projectmodel.DriftChanged) {
		t.Fatalf("changed dashboard = %+v, %v", dashboard.Projects, err)
	}
	project.Name = ""
	project.ComposeExecutable = false
	project.FilesystemWritable = false
	project.CapabilityReason = "discovery root is safety-degraded"
	session.setProjectListPayload(projectListPayload(t, at.Add(3*time.Second), false, project))
	dashboard, err = backend.Dashboard(ctx)
	if err != nil || dashboard.Projects[0].Name != "app" || !dashboard.Projects[0].ReadOnly ||
		dashboard.Projects[0].ComposeExecutable || dashboard.Projects[0].FilesystemWritable ||
		dashboard.Projects[0].CapabilityReason != project.CapabilityReason {
		t.Fatalf("read-only capability dashboard = %+v, %v", dashboard.Projects, err)
	}
}

func TestDashboardMergesDockerFactsAndBlocksUnmanagedNameCollision(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	agentID := "11111111-1111-4111-8111-111111111111"
	insertAgent(t, ctx, store, agentID, "Agent", `{"fs_read":true,"fs_write":true}`)
	filesystem := testAgentProject(t, agentID, "/srv", "/srv/app", "shared", strings.Repeat("a", 64), []string{"secret-from-compose-config"})
	payload, err := json.Marshal(agentProjectList{
		Projects: []agentProjectSnapshot{filesystem},
		DockerFacts: []agentDockerProjectFact{
			{
				ContainerID: strings.Repeat("b", 64), ProjectName: "shared", WorkingDir: "/srv/app", Service: "api",
				ConfigFiles: []string{"/srv/app/compose.yaml"}, ConfigHash: "opaque-compose-config-hash",
			},
			{
				ContainerID: strings.Repeat("c", 64), ProjectName: "shared", WorkingDir: "/opt/legacy", Service: "legacy",
				ConfigFiles: []string{"/opt/legacy/compose.yaml"}, ConfigHash: "opaque-compose-config-hash",
			},
			{ContainerID: strings.Repeat("d", 64), ProjectName: "no-working-dir", Service: "worker"},
		},
		Status: agentProjectScanStatus{ScannedAt: time.Date(2026, 8, 16, 2, 0, 0, 0, time.UTC), DirectoriesSeen: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	session := newFakeSession(agentID)
	session.setProjectListPayload(payload)
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}

	dashboard, err := backend.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(dashboard.Projects) != 2 {
		t.Fatalf("projects = %+v", dashboard.Projects)
	}
	byDir := make(map[string]webui.Project, len(dashboard.Projects))
	for _, project := range dashboard.Projects {
		byDir[project.WorkingDir] = project
	}
	managed := byDir["/srv/app"]
	unmanaged := byDir["/opt/legacy"]
	if !managed.Managed || !managed.Collision || !managed.ReadOnly || len(managed.ContainerIDs) != 1 ||
		managed.ContainerIDs[0] != strings.Repeat("b", 64) || len(managed.Services) != 1 || managed.Services[0] != "api" {
		t.Fatalf("managed project = %+v", managed)
	}
	if unmanaged.Managed || unmanaged.UnmanagedReason == "" || !unmanaged.Collision || !unmanaged.ReadOnly ||
		len(unmanaged.ContainerIDs) != 1 || unmanaged.ContainerIDs[0] != strings.Repeat("c", 64) ||
		len(unmanaged.Services) != 1 || unmanaged.Services[0] != "legacy" {
		t.Fatalf("unmanaged project = %+v", unmanaged)
	}
	if _, err := backend.WriteProjectFile(ctx, webui.FileWriteRequest{
		ID: "collision-write", ProjectUID: managed.UID, RelativePath: ".env",
		ExpectedSHA256: strings.Repeat("a", 64), Content: "A=B\n",
	}); !errors.Is(err, webui.ErrConflict) {
		t.Fatalf("collision write error = %v", err)
	}
	var flags string
	if err := store.DB().QueryRowContext(ctx, `SELECT flags_json FROM projects WHERE project_uid = ?`, managed.UID).Scan(&flags); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(flags, "secret-from-compose-config") || strings.Contains(flags, "opaque-compose-config-hash") {
		t.Fatalf("raw Compose data persisted in flags: %s", flags)
	}
}

func TestDashboardCarriesValidatedSourceProvenanceAndIncludeParent(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	agentID := "11111111-1111-4111-8111-111111111111"
	insertAgent(t, ctx, store, agentID, "Agent", `{"fs_read":true,"fs_write":true}`)
	parent := testAgentProject(t, agentID, "/srv", "/srv/parent", "parent", strings.Repeat("a", 64), []string{"app"})
	child := testAgentProject(t, agentID, "/srv", "/srv/child", "child", strings.Repeat("b", 64), []string{"app"})
	parent.IncludedWorkDirs = []string{child.WorkingDir}
	parent.SourceGraphComplete = true
	parent.SourceReferences = []agentSourceReference{
		{Kind: "extends", Path: "/srv/parent/shared/base.yaml", Accessible: true, ReadOnly: true},
		{Kind: "include", Path: "/srv/child/compose.yaml", Accessible: true},
	}
	session := newFakeSession(agentID)
	session.setProjectListPayload(projectListPayload(t, time.Date(2026, 8, 16, 1, 2, 3, 0, time.UTC), false, parent, child))
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	dashboard, err := backend.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var gotParent, gotChild *webui.Project
	for index := range dashboard.Projects {
		if dashboard.Projects[index].UID == parent.UID {
			gotParent = &dashboard.Projects[index]
		}
		if dashboard.Projects[index].UID == child.UID {
			gotChild = &dashboard.Projects[index]
		}
	}
	if gotParent == nil || gotChild == nil || !gotParent.SourceGraphComplete || len(gotParent.SourceReferences) != 2 ||
		gotParent.SourceReferences[0].Path != "/srv/parent/shared/base.yaml" || gotParent.SourceReferences[1].ReadOnly ||
		fmt.Sprint(gotChild.IncludedBy) != fmt.Sprint([]string{parent.UID}) {
		t.Fatalf("dashboard source provenance parent=%+v child=%+v", gotParent, gotChild)
	}
}

func TestTargetedProjectSnapshotSetsComposeUpBaselineAndRetainsItForConfigWrites(t *testing.T) {
	ctx, backend, store, _ := newTestBackend(t)
	agentID := "55555555-5555-4555-8555-555555555555"
	insertAgent(t, ctx, store, agentID, "Agent", `{"fs_read":true,"fs_write":true}`)
	initial := testAgentProject(t, agentID, "/srv", "/srv/app", "app", strings.Repeat("a", 64), []string{"web"})
	validated, err := validateProjectSnapshot(agentID, agentProjectList{
		Projects: []agentProjectSnapshot{initial},
		Status:   agentProjectScanStatus{ScannedAt: time.Date(2026, 8, 16, 1, 0, 0, 0, time.UTC), DirectoriesSeen: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.mergeProjectSnapshot(ctx, agentID, agentProjectScanStatus{ScannedAt: time.Date(2026, 8, 16, 1, 0, 0, 0, time.UTC), DirectoriesSeen: 1}, validated); err != nil {
		t.Fatal(err)
	}

	applied := testAgentProject(t, agentID, "/srv", "/srv/app", "app", strings.Repeat("b", 64), []string{"web"})
	validatedTarget, err := validateTargetedProjectSnapshot(agentID, applied)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.mergeTargetedProjectSnapshot(ctx, agentID, validatedTarget, true); err != nil {
		t.Fatal(err)
	}
	var rawApplied, rawFlags string
	if err := store.DB().QueryRowContext(ctx, `SELECT applied_fingerprints_json, flags_json FROM projects WHERE project_uid = ?`, initial.UID).Scan(&rawApplied, &rawFlags); err != nil {
		t.Fatal(err)
	}
	gotApplied, err := appliedFingerprint(rawApplied)
	if err != nil || gotApplied != applied.CurrentFingerprint || !strings.Contains(rawFlags, `"drift":"in-sync"`) {
		t.Fatalf("compose.up baseline applied=%q flags=%s err=%v", gotApplied, rawFlags, err)
	}

	changed := testAgentProject(t, agentID, "/srv", "/srv/app", "app", strings.Repeat("c", 64), []string{"web"})
	validatedTarget, err = validateTargetedProjectSnapshot(agentID, changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.mergeTargetedProjectSnapshot(ctx, agentID, validatedTarget, false); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT applied_fingerprints_json, flags_json FROM projects WHERE project_uid = ?`, initial.UID).Scan(&rawApplied, &rawFlags); err != nil {
		t.Fatal(err)
	}
	gotApplied, err = appliedFingerprint(rawApplied)
	if err != nil || gotApplied != applied.CurrentFingerprint || !strings.Contains(rawFlags, `"drift":"changed"`) || !strings.Contains(rawFlags, changed.CurrentFingerprint) {
		t.Fatalf("config mutation applied=%q flags=%s err=%v", gotApplied, rawFlags, err)
	}
}

func TestSuccessfulComposeUpLookupRefreshesOnlyTargetProjectAndSetsBaseline(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	agentID := "56565656-5656-4565-8565-565656565656"
	insertAgent(t, ctx, store, agentID, "Agent", `{"fs_read":true,"fs_write":true}`)
	initial := testAgentProject(t, agentID, "/srv", "/srv/app", "app", strings.Repeat("a", 64), []string{"web"})
	refreshed := testAgentProject(t, agentID, "/srv", "/srv/app", "app", strings.Repeat("b", 64), []string{"web"})
	session := newFakeSession(agentID)
	session.operation = producttransport.OperationResponse{Status: "requested", Phase: "PREPARING", Revision: 1}
	session.setProjectListPayload(projectListPayload(t, time.Date(2026, 8, 16, 2, 0, 0, 0, time.UTC), false, initial))
	payload, err := json.Marshal(agentProjectSnapshotResponse{Project: refreshed})
	if err != nil {
		t.Fatal(err)
	}
	session.setProjectSnapshotPayload(payload)
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Dashboard(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.StartOperation(ctx, webui.OperationRequest{
		ID: "compose-up-refresh", AgentID: agentID, ProjectUID: initial.UID, Kind: "compose.up",
	}); err != nil {
		t.Fatal(err)
	}
	session.getOperation = producttransport.GetOperationResponse{Found: true, Operation: producttransport.OperationResponse{
		Status: "success", Phase: "FINALIZING", Revision: 4,
	}}
	if _, err := backend.GetOperation(ctx, agentID, "compose-up-refresh"); err != nil {
		t.Fatal(err)
	}
	if request := session.lastQuery(); request.Kind != "project.snapshot" || request.Target != initial.UID || len(request.Payload) != 0 {
		t.Fatalf("targeted query = %#v", request)
	}
	var rawApplied string
	if err := store.DB().QueryRowContext(ctx, `SELECT applied_fingerprints_json FROM projects WHERE project_uid = ?`, initial.UID).Scan(&rawApplied); err != nil {
		t.Fatal(err)
	}
	applied, err := appliedFingerprint(rawApplied)
	if err != nil || applied != refreshed.CurrentFingerprint {
		t.Fatalf("applied fingerprint=%q err=%v", applied, err)
	}
}

func TestLateFullProjectSnapshotCannotOverwriteTargetedPostOperationRefresh(t *testing.T) {
	ctx, backend, store, _ := newTestBackend(t)
	agentID := "57575757-5757-4575-8575-575757575757"
	insertAgent(t, ctx, store, agentID, "Agent", `{}`)
	initial := testAgentProject(t, agentID, "/srv", "/srv/app", "app", strings.Repeat("a", 64), []string{"web"})
	refreshed := testAgentProject(t, agentID, "/srv", "/srv/app", "app", strings.Repeat("b", 64), []string{"web"})
	scanOne := agentProjectScanStatus{ScannedAt: time.Date(2026, 8, 16, 3, 0, 0, 0, time.UTC), DirectoriesSeen: 1}
	scanTwo := agentProjectScanStatus{ScannedAt: scanOne.ScannedAt.Add(time.Second), DirectoriesSeen: 1}
	firstObserved := time.Date(2026, 8, 16, 3, 1, 0, 0, time.UTC)
	targetObserved := firstObserved.Add(2 * time.Second)
	lateObserved := firstObserved.Add(time.Second)
	validatedInitial, err := validateProjectSnapshot(agentID, agentProjectList{Projects: []agentProjectSnapshot{initial}, Status: scanOne})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.mergeProjectSnapshotObserved(ctx, agentID, scanOne, validatedInitial, firstObserved); err != nil {
		t.Fatal(err)
	}
	validatedRefreshed, err := validateTargetedProjectSnapshot(agentID, refreshed)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.mergeTargetedProjectSnapshotObserved(ctx, agentID, validatedRefreshed, true, targetObserved); err != nil {
		t.Fatal(err)
	}
	// This complete scan was requested before the targeted refresh, but its
	// response reached the Server later. Its agent scan watermark is newer than
	// the prior full scan, so per-project server observation ordering is needed.
	if err := backend.mergeProjectSnapshotObserved(ctx, agentID, scanTwo, validatedInitial, lateObserved); err != nil {
		t.Fatal(err)
	}
	var rawApplied, rawFlags string
	if err := store.DB().QueryRowContext(ctx, `SELECT applied_fingerprints_json, flags_json FROM projects WHERE project_uid = ?`, initial.UID).Scan(&rawApplied, &rawFlags); err != nil {
		t.Fatal(err)
	}
	applied, err := appliedFingerprint(rawApplied)
	if err != nil || applied != refreshed.CurrentFingerprint || !strings.Contains(rawFlags, refreshed.CurrentFingerprint) || !strings.Contains(rawFlags, `"drift":"in-sync"`) {
		t.Fatalf("late snapshot overwrote targeted state applied=%q flags=%s err=%v", applied, rawFlags, err)
	}
}

func TestProjectSnapshotWatermarkRenameCollisionAndMissingAreConservative(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	agentID := "22222222-2222-4222-8222-222222222222"
	insertAgent(t, ctx, store, agentID, "Agent", `{}`)
	at := time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC)
	first := testAgentProject(t, agentID, "/srv", "/srv/a", "alpha", strings.Repeat("a", 64), []string{"web"})
	second := testAgentProject(t, agentID, "/srv", "/srv/b", "beta", strings.Repeat("b", 64), []string{"worker"})
	session := newFakeSession(agentID)
	session.setProjectListPayload(projectListPayload(t, at, false, first, second))
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.Dashboard(ctx); err != nil {
		t.Fatal(err)
	}

	// A sub-second snapshot newer than an exact-second watermark must advance;
	// fixed-width storage prevents RFC3339Nano lexical ordering bugs.
	first.Name, second.Name = "shared", "shared"
	session.setProjectListPayload(projectListPayload(t, at.Add(100*time.Millisecond), false, first, second))
	dashboard, err := backend.Dashboard(ctx)
	if err != nil || len(dashboard.Projects) != 2 || !dashboard.Projects[0].Collision || !dashboard.Projects[1].Collision ||
		!dashboard.Projects[0].ReadOnly || !dashboard.Projects[1].ReadOnly {
		t.Fatalf("collision dashboard = %+v, %v", dashboard.Projects, err)
	}

	// An older response cannot undo the accepted rename/collision state.
	first.Name, second.Name = "stale-alpha", "stale-beta"
	session.setProjectListPayload(projectListPayload(t, at.Add(50*time.Millisecond), false, first, second))
	dashboard, err = backend.Dashboard(ctx)
	if err != nil || dashboard.Projects[0].Name != "shared" || dashboard.Projects[1].Name != "shared" {
		t.Fatalf("stale snapshot changed mirror = %+v, %v", dashboard.Projects, err)
	}

	// A truncated scan cannot declare unseen projects missing.
	session.setProjectListPayload(projectListPayload(t, at.Add(200*time.Millisecond), true))
	dashboard, err = backend.Dashboard(ctx)
	if err != nil || !dashboard.Projects[0].Present || !dashboard.Projects[1].Present {
		t.Fatalf("truncated scan removed projects = %+v, %v", dashboard.Projects, err)
	}

	// A later complete empty scan retains history but marks it stale/read-only.
	session.setProjectListPayload(projectListPayload(t, at.Add(300*time.Millisecond), false))
	dashboard, err = backend.Dashboard(ctx)
	if err != nil || len(dashboard.Projects) != 2 {
		t.Fatalf("complete empty scan = %+v, %v", dashboard.Projects, err)
	}
	for _, project := range dashboard.Projects {
		if project.Present || !project.Stale || !project.ReadOnly || project.CapabilityReason == "" {
			t.Fatalf("missing project history = %+v", project)
		}
	}
}

func TestProjectReconciliationIsolatesInvalidUnavailableAndOfflineAgents(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	agentA := "33333333-3333-4333-8333-333333333333"
	agentB := "44444444-4444-4444-8444-444444444444"
	agentC := "55555555-5555-4555-8555-555555555555"
	for _, id := range []string{agentA, agentB, agentC} {
		insertAgent(t, ctx, store, id, id, `{}`)
	}
	at := time.Date(2026, 8, 15, 3, 0, 0, 0, time.UTC)
	projectA := testAgentProject(t, agentA, "/srv", "/srv/a", "a", strings.Repeat("a", 64), nil)
	sessionA := newFakeSession(agentA)
	sessionA.setProjectListPayload(projectListPayload(t, at, false, projectA))
	sessionB := newFakeSession(agentB)
	sessionB.setProjectListPayload([]byte(`{"projects":`))
	sessionC := newFakeSession(agentC)
	sessionC.queryErr = errors.New("temporary query outage")
	for _, session := range []*fakeSession{sessionA, sessionB, sessionC} {
		if err := registry.Register(session); err != nil {
			t.Fatal(err)
		}
	}
	dashboard, err := backend.Dashboard(ctx)
	if err != nil || len(dashboard.Projects) != 1 || dashboard.Projects[0].UID != projectA.UID {
		t.Fatalf("isolated dashboard = %+v, %v", dashboard, err)
	}
	hosts := make(map[string]webui.Host)
	for _, host := range dashboard.Hosts {
		hosts[host.ID] = host
	}
	if !hosts[agentA].Capabilities.Discovery.Enabled || hosts[agentB].Capabilities.Discovery.Enabled ||
		!strings.Contains(hosts[agentB].Capabilities.Discovery.Reason, "invalid") || hosts[agentC].Capabilities.Discovery.Enabled ||
		!strings.Contains(hosts[agentC].Capabilities.Discovery.Reason, "unavailable") {
		t.Fatalf("discovery capabilities = A:%+v B:%+v C:%+v", hosts[agentA].Capabilities.Discovery, hosts[agentB].Capabilities.Discovery, hosts[agentC].Capabilities.Discovery)
	}
	var bWatermark, cWatermark *string
	if err := store.DB().QueryRowContext(ctx, `SELECT projects_scanned_at FROM agents WHERE id = ?`, agentB).Scan(&bWatermark); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT projects_scanned_at FROM agents WHERE id = ?`, agentC).Scan(&cWatermark); err != nil {
		t.Fatal(err)
	}
	if bWatermark != nil || cWatermark != nil {
		t.Fatalf("failed snapshots advanced watermarks: invalid=%v unavailable=%v", bWatermark, cWatermark)
	}

	registry.SessionClosed(agentA, sessionA.info.SessionID)
	sessionA.setProjectListPayload(projectListPayload(t, at.Add(time.Hour), false))
	dashboard, err = backend.Dashboard(ctx)
	if err != nil || len(dashboard.Projects) != 1 || !dashboard.Projects[0].Present {
		t.Fatalf("offline Agent changed project history = %+v, %v", dashboard.Projects, err)
	}
}

func TestProjectEnvironmentIsLiveStrictAndSecretIsNotPersisted(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "Agent", `{"fs_read":true}`)
	insertProject(t, ctx, store, "project-a", "agent-a", "project", `{}`)
	entries := []webui.EnvironmentEntry{
		{Name: "PUBLIC", Value: "visible", Secret: true},
		{Name: "PASSWORD", Value: "serverapi-secret-value", Secret: true},
	}
	payload, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	session := newFakeSession("agent-a")
	session.queryPayload = payload
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}

	got, err := backend.ProjectEnvironment(ctx, "project-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Value != "serverapi-secret-value" || !got[1].Secret {
		t.Fatalf("environment = %+v", got)
	}
	request := session.lastQuery()
	if request.Kind != QueryProjectEnvironment || request.Target != "project-a" || len(request.Payload) != 0 {
		t.Fatalf("query request = %+v", request)
	}
	assertNoPersistentSecret(t, ctx, store, "serverapi-secret-value")

	session.setQueryPayload([]byte(`[{"name":"A","value":"B","secret":false,"unknown":1}]`))
	if _, err := backend.ProjectEnvironment(ctx, "project-a"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("unknown Agent JSON field error = %v", err)
	}
	session.setQueryPayload([]byte(`[] {}`))
	if _, err := backend.ProjectEnvironment(ctx, "project-a"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("trailing Agent JSON error = %v", err)
	}
}

func TestProjectComposeQueriesAreLiveProjectScopedAndBounded(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "Agent", `{}`)
	insertProject(t, ctx, store, "project-a", "agent-a", "project", `{"compose_executable":true}`)
	session := newFakeSession("agent-a")
	session.queryPayload = []byte(`{"output":"NAME STATE\nweb running\n"}`)
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}

	output, err := backend.ProjectComposePS(ctx, "project-a", webui.ComposeQuery{Services: []string{"web"}, All: true})
	if err != nil || output.Output != "NAME STATE\nweb running\n" {
		t.Fatalf("compose ps = %+v, %v", output, err)
	}
	request := session.lastQuery()
	if request.Kind != QueryComposePS || request.Target != "project-a" || string(request.Payload) != `{"services":["web"],"all":true}` {
		t.Fatalf("compose ps request = %+v", request)
	}

	session.setQueryPayload([]byte(`{"output":"services:\n  web: {}\n"}`))
	output, err = backend.ProjectComposeConfig(ctx, "project-a", webui.ComposeQuery{Services: []string{"web"}})
	if err != nil || output.Output == "" {
		t.Fatalf("compose config = %+v, %v", output, err)
	}
	request = session.lastQuery()
	if request.Kind != QueryComposeConfig || string(request.Payload) != `{"services":["web"]}` {
		t.Fatalf("compose config request = %+v", request)
	}

	if _, err := backend.ProjectComposeConfig(ctx, "project-a", webui.ComposeQuery{All: true}); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("config all error = %v", err)
	}
	if _, err := backend.ProjectComposePS(ctx, "project-a", webui.ComposeQuery{Services: []string{"web", "web"}}); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("duplicate service error = %v", err)
	}
	session.setQueryPayload([]byte(`{"output":"` + strings.Repeat("x", maxComposeOutputBytes+1) + `"}`))
	if _, err := backend.ProjectComposePS(ctx, "project-a", webui.ComposeQuery{}); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("oversized output error = %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE projects SET flags_json = '{"compose_executable":false,"capability_reason":"unsafe mount"}' WHERE project_uid = 'project-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ProjectComposePS(ctx, "project-a", webui.ComposeQuery{}); !errors.Is(err, webui.ErrUnavailable) {
		t.Fatalf("unavailable project error = %v", err)
	}
}

func TestProjectComposeLogsAreLiveProjectScopedAndTyped(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "Agent", `{}`)
	insertProject(t, ctx, store, "project-a", "agent-a", "project", `{"compose_executable":true}`)
	session := newFakeSession("agent-a")
	stream := &fakeLogReceiveStream{events: []producttransport.LogEvent{{Data: []byte("web | ready\n")}, {Terminal: true}}}
	session.logStream = stream
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}

	logs, err := backend.OpenProjectLogs(ctx, "project-a", webui.ProjectLogRequest{
		Services: []string{"web"}, Follow: true, TailLines: 100, Timestamps: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.logRequest.ProjectUID != "project-a" || session.logRequest.ContainerID != "" ||
		len(session.logRequest.Services) != 1 || session.logRequest.Services[0] != "web" || !session.logRequest.Follow ||
		session.logRequest.TailLines != 100 || !session.logRequest.ShowStdout || !session.logRequest.ShowStderr || !session.logRequest.Timestamps {
		t.Fatalf("Compose log request = %+v", session.logRequest)
	}
	event, err := logs.Recv(ctx)
	if err != nil || string(event.Data) != "web | ready\n" {
		t.Fatalf("Compose log event = %+v, %v", event, err)
	}
	if err := logs.Close(); err != nil || !stream.closed {
		t.Fatalf("Compose log close = %v closed=%v", err, stream.closed)
	}
	if _, err := backend.OpenProjectLogs(ctx, "project-a", webui.ProjectLogRequest{Services: []string{"web", "web"}}); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("duplicate Compose service error = %v", err)
	}
	if _, err := backend.OpenProjectLogs(ctx, "project-a", webui.ProjectLogRequest{TailLines: maxComposeLogTail + 1}); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("large Compose tail error = %v", err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE projects SET flags_json = '{"compose_executable":false}' WHERE project_uid = 'project-a'`); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.OpenProjectLogs(ctx, "project-a", webui.ProjectLogRequest{}); !errors.Is(err, webui.ErrUnavailable) {
		t.Fatalf("unavailable Compose log error = %v", err)
	}
}

func TestProjectFileReadIsLiveStrictSecretAwareAndBounded(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "Agent", `{"fs_read":true}`)
	insertProject(t, ctx, store, "project-a", "agent-a", "project", `{}`)
	secret := "file-read-secret-never-persist"
	session := newFakeSession("agent-a")
	session.queryPayload = []byte(`{"relative_path":".env","content":"` + secret + `","sha256":"` + strings.Repeat("a", 64) + `","mtime":"2026-08-15T01:02:03Z","mode":384,"line_endings":"lf","secret":true}`)
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	file, err := backend.ProjectFile(ctx, "project-a", ".env")
	if err != nil {
		t.Fatal(err)
	}
	if file.Content != secret || !file.Secret || file.Mode != 0o600 || file.RelativePath != ".env" {
		t.Fatalf("file = %+v", file)
	}
	query := session.lastQuery()
	if query.Kind != QueryProjectFile || query.Target != "project-a" || string(query.Payload) != `{"relative_path":".env"}` {
		t.Fatalf("query = %+v payload=%q", query, query.Payload)
	}
	assertNoPersistentSecret(t, ctx, store, secret)

	session.setQueryPayload([]byte(`{"relative_path":".env","content":"x","sha256":"` + strings.Repeat("a", 64) + `","mtime":"2026-08-15T01:02:03Z","mode":384,"line_endings":"lf","secret":false}`))
	if _, err := backend.ProjectFile(ctx, "project-a", ".env"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("unmarked env secret error = %v", err)
	}
	session.setQueryPayload([]byte(`{"relative_path":".env","content":"x","sha256":"` + strings.Repeat("a", 64) + `","mtime":"2026-08-15T01:02:03Z","mode":384,"line_endings":"lf","secret":true,"secret":false}`))
	if _, err := backend.ProjectFile(ctx, "project-a", ".env"); !errors.Is(err, ErrCorruptData) {
		t.Fatalf("duplicate Agent field error = %v", err)
	}
	session.queryErr = status.Error(codes.Unknown, "Agent query response exceeds 1 MiB")
	if _, err := backend.ProjectFile(ctx, "project-a", ".env"); !errors.Is(err, webui.ErrTooLarge) {
		t.Fatalf("transport limit error = %v", err)
	}
}

func TestFileWriteUsesTypedIdempotentOperationWithoutPersistingContent(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "Agent", `{"fs_read":true,"fs_write":true}`)
	insertProject(t, ctx, store, "project-a", "agent-a", "project", `{}`)
	session := newFakeSession("agent-a")
	session.operation = producttransport.OperationResponse{Status: "ACCEPTED", Phase: "QUEUED", Revision: 1}
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	secret := "write-secret-never-persist"
	request := webui.FileWriteRequest{
		ID: "write-1", ProjectUID: "project-a", RelativePath: ".env",
		ExpectedSHA256: strings.Repeat("b", 64), Content: "TOKEN=" + secret + "\n",
	}
	for attempt := 0; attempt < 2; attempt++ {
		operation, err := backend.WriteProjectFile(ctx, request)
		if err != nil || operation.ID != "write-1" || operation.Status != "ACCEPTED" {
			t.Fatalf("write attempt %d = %+v, %v", attempt, operation, err)
		}
	}
	dispatched := session.lastOperation()
	if dispatched.OperationID != request.ID || dispatched.ProjectKey != request.ProjectUID || dispatched.Type != "env.write" || dispatched.Target != ".env" {
		t.Fatalf("operation request = %+v", dispatched)
	}
	var payload struct {
		Version        int    `json:"version"`
		ExpectedSHA256 string `json:"expected_sha256"`
		Content        string `json:"content"`
	}
	if err := json.Unmarshal(dispatched.Payload, &payload); err != nil || payload.Version != 1 || payload.Content != request.Content || payload.ExpectedSHA256 != request.ExpectedSHA256 {
		t.Fatalf("operation payload = %+v, %v", payload, err)
	}
	assertNoPersistentSecret(t, ctx, store, secret)

	tooLarge := request
	tooLarge.ID = "write-large"
	tooLarge.Content = strings.Repeat("\n", maxProjectFileBytes)
	if _, err := backend.WriteProjectFile(ctx, tooLarge); !errors.Is(err, webui.ErrTooLarge) {
		t.Fatalf("escaped transport overflow error = %v", err)
	}
	if session.operationCalls() != 2 {
		t.Fatalf("oversized write reached Agent; calls=%d", session.operationCalls())
	}
}

func TestBackupListCreateAndRestoreUseMetadataAndTypedOperations(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "Agent", `{"fs_read":true,"fs_write":true}`)
	insertProject(t, ctx, store, "project-a", "agent-a", "project", `{}`)
	backupID := "20260815T010203.000000000Z-0123456789abcdef"
	session := newFakeSession("agent-a")
	session.queryPayload = []byte(`[{"backup_id":"` + backupID + `","project_uid":"project-a","created_at":"2026-08-15T01:02:03Z","trigger":"manual","file_count":2,"size_bytes":100,"manifest_sha256":"` + strings.Repeat("c", 64) + `"}]`)
	session.operation = producttransport.OperationResponse{Status: "ACCEPTED", Phase: "QUEUED"}
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	backups, err := backend.ProjectBackups(ctx, "project-a")
	if err != nil || len(backups) != 1 || backups[0].ID != backupID {
		t.Fatalf("backups = %+v, %v", backups, err)
	}
	if request := session.lastQuery(); request.Kind != QueryProjectBackups || len(request.Payload) != 0 {
		t.Fatalf("backup query = %+v", request)
	}

	create := webui.BackupCreateRequest{ID: "backup-create-1", ProjectUID: "project-a", RelativePaths: []string{"compose.yaml", ".env"}}
	if _, err := backend.CreateBackup(ctx, create); err != nil {
		t.Fatal(err)
	}
	dispatched := session.lastOperation()
	if dispatched.Type != "backup.create" || dispatched.Target != "" || !strings.Contains(string(dispatched.Payload), `"version":1`) {
		t.Fatalf("backup create request = %+v payload=%s", dispatched, dispatched.Payload)
	}
	if _, err := backend.RestoreBackup(ctx, webui.BackupRestoreRequest{ID: "backup-restore-1", ProjectUID: "project-a", BackupID: backupID}); err != nil {
		t.Fatal(err)
	}
	dispatched = session.lastOperation()
	if dispatched.Type != "backup.restore" || dispatched.Target != backupID || string(dispatched.Payload) != `{"version":1}` {
		t.Fatalf("backup restore request = %+v payload=%s", dispatched, dispatched.Payload)
	}
	var indexedAgent, indexedProject, kind, storagePath, flags string
	if err := store.DB().QueryRowContext(ctx, `
		SELECT agent_id, project_uid, kind, storage_path, flags_json FROM backup_index WHERE id = ?
	`, backupID).Scan(&indexedAgent, &indexedProject, &kind, &storagePath, &flags); err != nil {
		t.Fatal(err)
	}
	if indexedAgent != "agent-a" || indexedProject != "project-a" || kind != "configuration" || storagePath != "" ||
		!strings.Contains(flags, `"file_count":2`) || strings.Contains(flags, "working_dir") || strings.Contains(flags, "files.tar") {
		t.Fatalf("backup index leaked or lost metadata: agent=%q project=%q kind=%q path=%q flags=%q", indexedAgent, indexedProject, kind, storagePath, flags)
	}
	session.setQueryPayload([]byte(`[]`))
	if listed, err := backend.ProjectBackups(ctx, "project-a"); err != nil || len(listed) != 0 {
		t.Fatalf("empty reconciled backup list = %+v, %v", listed, err)
	}
	var remaining int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM backup_index WHERE project_uid = 'project-a'`).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("stale backup index count=%d err=%v", remaining, err)
	}
}

func TestFileAndRestoreCapabilitiesFailClosedBeforeAgentDispatch(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "Agent", `{"fs_read":false,"fs_write":false,"fs_read_reason":"root unavailable","fs_write_reason":"read-only root"}`)
	insertProject(t, ctx, store, "project-a", "agent-a", "project", `{"read_only":true}`)
	session := newFakeSession("agent-a")
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.ProjectFile(ctx, "project-a", "compose.yaml"); !errors.Is(err, webui.ErrUnavailable) {
		t.Fatalf("file read capability error = %v", err)
	}
	write := webui.FileWriteRequest{
		ID: "write-1", ProjectUID: "project-a", RelativePath: "compose.yaml",
		ExpectedSHA256: strings.Repeat("a", 64), Content: "services: {}\n",
	}
	if _, err := backend.WriteProjectFile(ctx, write); !errors.Is(err, webui.ErrConflict) {
		t.Fatalf("file write read-only error = %v", err)
	}
	if _, err := backend.RestoreBackup(ctx, webui.BackupRestoreRequest{ID: "restore-1", ProjectUID: "project-a", BackupID: "backup-1"}); !errors.Is(err, webui.ErrConflict) {
		t.Fatalf("restore read-only error = %v", err)
	}
	if session.operationCalls() != 0 {
		t.Fatalf("unsafe requests reached Agent; operations=%d", session.operationCalls())
	}
}

func TestOfflineIsTypedBeforeLiveQueryOrOperation(t *testing.T) {
	ctx, backend, store, _ := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "Agent", `{"fs_read":true}`)
	insertProject(t, ctx, store, "project-a", "agent-a", "project", `{}`)

	_, err := backend.ProjectEnvironment(ctx, "project-a")
	var offline *OfflineError
	if !errors.As(err, &offline) || !errors.Is(err, ErrAgentOffline) || !errors.Is(err, webui.ErrUnavailable) || offline.AgentID != "agent-a" {
		t.Fatalf("environment offline error = %#v", err)
	}
	_, err = backend.StartOperation(ctx, webui.OperationRequest{ID: "op", AgentID: "agent-a", ProjectUID: "project-a", Kind: "compose.up"})
	if !errors.Is(err, ErrAgentOffline) || !errors.Is(err, webui.ErrUnavailable) {
		t.Fatalf("operation offline error = %v", err)
	}
}

func TestLiveTransportFailureDoesNotExposeCauseToWebClient(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "Agent", `{"fs_read":true}`)
	insertProject(t, ctx, store, "project-a", "agent-a", "project", `{}`)
	session := newFakeSession("agent-a")
	session.queryErr = errors.New("rpc endpoint credential=do-not-expose")
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	_, err := backend.ProjectEnvironment(ctx, "project-a")
	if !errors.Is(err, webui.ErrUnavailable) || !errors.Is(err, session.queryErr) {
		t.Fatalf("live error chain = %v", err)
	}
	if strings.Contains(err.Error(), "credential") || strings.Contains(err.Error(), "do-not-expose") {
		t.Fatalf("client-visible error exposes transport cause: %v", err)
	}
}

func TestStartOperationPersistsImmediateCanonicalAgentRecord(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "Agent", `{}`)
	insertProject(t, ctx, store, "project-a", "agent-a", "project", `{}`)
	session := newFakeSession("agent-a")
	session.operation = producttransport.OperationResponse{
		Status: "ACCEPTED", Phase: "QUEUED", Revision: 7, PartialEffectsPossible: true,
		Error: "current error", OutputTail: []byte("current output"), OutputTruncated: true,
	}
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	request := webui.OperationRequest{ID: "op-1", AgentID: "agent-a", ProjectUID: "project-a", Kind: "compose.up", Target: "api"}
	got, err := backend.StartOperation(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	want := webui.Operation{
		ID: "op-1", Status: "ACCEPTED", Phase: "QUEUED", Revision: 7, PartialEffectsPossible: true,
		Error: "current error", OutputTail: "current output", OutputTruncated: true,
	}
	if got != want {
		t.Fatalf("operation = %+v, want %+v", got, want)
	}
	liveRequest := session.lastOperation()
	if liveRequest.OperationID != request.ID || liveRequest.Type != request.Kind || liveRequest.ProjectKey != request.ProjectUID || liveRequest.Target != request.Target {
		t.Fatalf("live request = %+v", liveRequest)
	}
	var agentID, projectUID, kind, status, phase, summary, output string
	var actor *string
	var revision int64
	var truncated bool
	if err := store.DB().QueryRowContext(ctx, `
		SELECT agent_id, project_uid, kind, status, phase, revision, actor,
		       summary_json, CAST(output_tail AS TEXT), output_truncated
		FROM operations WHERE id = 'op-1'
	`).Scan(&agentID, &projectUID, &kind, &status, &phase, &revision, &actor, &summary, &output, &truncated); err != nil {
		t.Fatal(err)
	}
	if agentID != "agent-a" || projectUID != "project-a" || kind != "compose.up" || status != "ACCEPTED" ||
		phase != "QUEUED" || revision != 7 || actor != nil || output != "current output" || !truncated ||
		!strings.Contains(summary, `"target":"api"`) || !strings.Contains(summary, `"error":"current error"`) {
		t.Fatalf("stored operation = agent=%q project=%q kind=%q status=%q phase=%q rev=%d actor=%v summary=%q output=%q truncated=%v",
			agentID, projectUID, kind, status, phase, revision, actor, summary, output, truncated)
	}

	_, err = backend.StartOperation(ctx, webui.OperationRequest{ID: "op-2", AgentID: "agent-a", ProjectUID: "unknown", Kind: "compose.up"})
	if !errors.Is(err, webui.ErrNotFound) {
		t.Fatalf("unknown project error = %v", err)
	}
	if session.operationCalls() != 1 {
		t.Fatalf("operation calls = %d, want 1", session.operationCalls())
	}
	_, err = backend.StartOperation(ctx, webui.OperationRequest{ID: "write-bypass", AgentID: "agent-a", ProjectUID: "project-a", Kind: "env.write", Target: ".env"})
	if !errors.Is(err, webui.ErrInvalidRequest) || session.operationCalls() != 1 {
		t.Fatalf("generic file mutation bypass error=%v calls=%d", err, session.operationCalls())
	}
}

func TestOperationLookupPersistsOnlyMonotonicAgentRevisions(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "Agent", `{}`)
	session := newFakeSession("agent-a")
	session.operation = producttransport.OperationResponse{Status: "running", Phase: "EXECUTING", Revision: 7, OutputTail: []byte("newer")}
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.StartOperation(ctx, webui.OperationRequest{ID: "op-1", AgentID: "agent-a", Kind: "docker.prune", Target: "host"}); err != nil {
		t.Fatal(err)
	}

	session.getOperation = producttransport.GetOperationResponse{Found: true, Operation: producttransport.OperationResponse{
		Status: "requested", Phase: "PREPARING", Revision: 6, OutputTail: []byte("stale"),
	}}
	got, err := backend.GetOperation(ctx, "agent-a", "op-1")
	if err != nil || got.Revision != 7 || got.OutputTail != "newer" {
		t.Fatalf("stale lookup = %+v, %v", got, err)
	}

	session.getOperation = producttransport.GetOperationResponse{Found: true, Operation: producttransport.OperationResponse{
		Status: "success", Phase: "FINALIZING", Revision: 8, OutputTail: []byte("complete"), OutputTruncated: true,
	}}
	got, err = backend.GetOperation(ctx, "agent-a", "op-1")
	if err != nil || got.Revision != 8 || got.Status != "success" || got.OutputTail != "complete" || !got.OutputTruncated {
		t.Fatalf("advanced lookup = %+v, %v", got, err)
	}
	var revision int64
	if err := store.DB().QueryRowContext(ctx, `SELECT revision FROM operations WHERE id = 'op-1'`).Scan(&revision); err != nil || revision != 8 {
		t.Fatalf("stored revision = %d, %v", revision, err)
	}

	session.getOperation.Operation.Status = "failed"
	if _, err := backend.GetOperation(ctx, "agent-a", "op-1"); !errors.Is(err, webui.ErrConflict) {
		t.Fatalf("same-revision mutation error = %v", err)
	}
}

func TestOperationMergeAtomicallyGuardsImmutableIdentityAndSpec(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "Agent A", `{}`)
	insertAgent(t, ctx, store, "agent-b", "Agent B", `{}`)
	session := newFakeSession("agent-a")
	session.operation = producttransport.OperationResponse{Status: "requested", Phase: "PREPARING", Revision: 1}
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	base := operationSpec{ID: "op-1", AgentID: "agent-a", ProjectUID: "", Kind: "docker.prune", Target: "host"}
	if _, err := backend.dispatchOperation(ctx, base.AgentID, base.ID, base.ProjectUID, base.Kind, base.Target, nil); err != nil {
		t.Fatal(err)
	}
	incoming := webui.Operation{ID: base.ID, Status: "running", Phase: "EXECUTING", Revision: 2}
	conflicts := []operationSpec{
		{ID: base.ID, AgentID: "agent-b", ProjectUID: base.ProjectUID, Kind: base.Kind, Target: base.Target},
		{ID: base.ID, AgentID: base.AgentID, ProjectUID: "different-project", Kind: base.Kind, Target: base.Target},
		{ID: base.ID, AgentID: base.AgentID, ProjectUID: base.ProjectUID, Kind: "compose.up", Target: base.Target},
		{ID: base.ID, AgentID: base.AgentID, ProjectUID: base.ProjectUID, Kind: base.Kind, Target: "different-target"},
	}
	for _, conflict := range conflicts {
		if _, err := backend.mergeOperation(ctx, conflict, incoming, true); !errors.Is(err, webui.ErrConflict) {
			t.Errorf("merge conflict %+v error = %v", conflict, err)
		}
	}
	stored, storedSpec, err := backend.loadStoredOperation(ctx, base.ID)
	if err != nil || stored.Revision != 1 || storedSpec != base {
		t.Fatalf("immutable row changed: operation=%+v spec=%+v err=%v", stored, storedSpec, err)
	}
}

func TestOperationLookupNotFoundConflictAndUnavailable(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "Agent A", `{}`)
	insertAgent(t, ctx, store, "agent-b", "Agent B", `{}`)
	session := newFakeSession("agent-a")
	session.operation = producttransport.OperationResponse{Status: "requested", Phase: "PREPARING", Revision: 1}
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.StartOperation(ctx, webui.OperationRequest{ID: "op-1", AgentID: "agent-a", Kind: "docker.prune"}); err != nil {
		t.Fatal(err)
	}

	session.getOperation = producttransport.GetOperationResponse{Found: false}
	if _, err := backend.GetOperation(ctx, "agent-a", "op-1"); !errors.Is(err, webui.ErrNotFound) {
		t.Fatalf("Agent not-found error = %v", err)
	}
	if _, err := backend.GetOperation(ctx, "agent-b", "op-1"); !errors.Is(err, webui.ErrConflict) {
		t.Fatalf("identity conflict error = %v", err)
	}
	if _, err := backend.GetOperation(ctx, "agent-a", "missing"); !errors.Is(err, webui.ErrNotFound) {
		t.Fatalf("unknown operation error = %v", err)
	}

	_ = session.Close(nil)
	if _, err := backend.GetOperation(ctx, "agent-a", "op-1"); !errors.Is(err, ErrAgentOffline) || !errors.Is(err, webui.ErrUnavailable) {
		t.Fatalf("offline lookup error = %v", err)
	}
}

func TestCancelOperationUsesUserReasonAndPersistsAllOutcomes(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "Agent", `{}`)
	session := newFakeSession("agent-a")
	session.operation = producttransport.OperationResponse{Status: "running", Phase: "EXECUTING", Revision: 2}
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.StartOperation(ctx, webui.OperationRequest{ID: "op-1", AgentID: "agent-a", Kind: "docker.prune"}); err != nil {
		t.Fatal(err)
	}

	session.cancelOperation = producttransport.CancelOperationResponse{Outcome: "ACCEPTED", Operation: producttransport.OperationResponse{
		Status: "canceled", Phase: "EXECUTING", Revision: 3, PartialEffectsPossible: true,
	}}
	got, err := backend.CancelOperation(ctx, "agent-a", "op-1")
	if err != nil || got.Outcome != "ACCEPTED" || got.Operation.Revision != 3 || !got.Operation.PartialEffectsPossible {
		t.Fatalf("cancel = %+v, %v", got, err)
	}
	if session.cancelRequest.OperationID != "op-1" || session.cancelRequest.Reason != "USER" {
		t.Fatalf("cancel request = %+v", session.cancelRequest)
	}

	session.cancelOperation = producttransport.CancelOperationResponse{Outcome: "ALREADY_TERMINAL", Operation: producttransport.OperationResponse{
		Status: "success", Phase: "FINALIZING", Revision: 4,
	}}
	got, err = backend.CancelOperation(ctx, "agent-a", "op-1")
	if err != nil || got.Outcome != "ALREADY_TERMINAL" || got.Operation.Status != "success" {
		t.Fatalf("terminal cancel = %+v, %v", got, err)
	}

	session.cancelOperation = producttransport.CancelOperationResponse{Outcome: "NOT_FOUND"}
	if _, err := backend.CancelOperation(ctx, "agent-a", "op-1"); !errors.Is(err, webui.ErrNotFound) {
		t.Fatalf("cancel not found error = %v", err)
	}
}

func TestOperationPersistenceNeverStoresRawSecretPayloadAndRejectsSpecReuse(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "Agent", `{"fs_write":true}`)
	insertProject(t, ctx, store, "project-a", "agent-a", "Project", `{}`)
	session := newFakeSession("agent-a")
	session.operation = producttransport.OperationResponse{Status: "requested", Phase: "PREPARING", Revision: 1}
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	secret := "operation-payload-secret-value"
	_, err := backend.WriteProjectFile(ctx, webui.FileWriteRequest{
		ID: "op-secret", ProjectUID: "project-a", RelativePath: ".env",
		ExpectedSHA256: strings.Repeat("a", 64), Content: "TOKEN=" + secret,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertNoPersistentSecret(t, ctx, store, secret)
	if _, err := backend.WriteProjectFile(ctx, webui.FileWriteRequest{
		ID: "op-secret", ProjectUID: "project-a", RelativePath: ".env.production",
		ExpectedSHA256: strings.Repeat("a", 64), Content: "safe",
	}); !errors.Is(err, webui.ErrConflict) {
		t.Fatalf("operation spec reuse error = %v", err)
	}
	if session.operationCalls() != 1 {
		t.Fatalf("conflicting spec reached Agent; calls=%d", session.operationCalls())
	}
}

func TestBrowserDisconnectAfterAgentAcceptanceStillPersistsWithoutCancel(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "Agent", `{}`)
	session := newFakeSession("agent-a")
	session.operation = producttransport.OperationResponse{Status: "requested", Phase: "PREPARING", Revision: 1}
	requestCtx, cancel := context.WithCancel(ctx)
	session.operationHook = cancel
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	got, err := backend.StartOperation(requestCtx, webui.OperationRequest{ID: "op-1", AgentID: "agent-a", Kind: "docker.prune"})
	if err != nil || got.ID != "op-1" {
		t.Fatalf("StartOperation after disconnect = %+v, %v", got, err)
	}
	var count int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM operations WHERE id = 'op-1'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("persisted rows = %d, %v", count, err)
	}
	if session.cancels != 0 {
		t.Fatalf("browser disconnect sent %d cancellation requests", session.cancels)
	}
}

func TestActiveOperationRecoveryRestoresMissingRowAcrossServerRestartAndNeverInfersAbsence(t *testing.T) {
	ctx, _, store, _ := newTestBackend(t)
	agentID := "agent-a"
	insertAgent(t, ctx, store, agentID, "Agent", `{}`)
	insertProject(t, ctx, store, "project-a", agentID, "Project", `{}`)
	active := producttransport.ActiveOperation{
		OperationID: "op-recovered", Type: "compose.up", ProjectKey: "project-a", Target: "api",
		Operation: producttransport.OperationResponse{
			Status: "running", Phase: "EXECUTING", Revision: 3, PartialEffectsPossible: true,
			OutputTail: []byte("recovering"), OutputTruncated: true,
		},
	}
	newRestartedBackend := func(active []producttransport.ActiveOperation) (*Backend, *fakeRecoverySession) {
		registry := producttransport.NewSessionRegistry()
		backend, err := New(store, registry)
		if err != nil {
			t.Fatal(err)
		}
		session := &fakeRecoverySession{fakeSession: newFakeSession(agentID), active: active}
		if err := registry.Register(session); err != nil {
			t.Fatal(err)
		}
		return backend, session
	}

	backend, session := newRestartedBackend([]producttransport.ActiveOperation{active})
	dashboard, err := backend.Dashboard(ctx)
	if err != nil || !dashboard.Hosts[0].Capabilities.OperationRecovery.Enabled {
		t.Fatalf("restart recovery dashboard = %+v, %v", dashboard, err)
	}
	var storedAgent, projectUID, kind, status, phase, summary, output string
	var revision int64
	var requestedAt, actor *string
	if err := store.DB().QueryRowContext(ctx, `
		SELECT agent_id, project_uid, kind, status, phase, revision, requested_at, actor,
		       summary_json, CAST(output_tail AS TEXT)
		FROM operations WHERE id = 'op-recovered'
	`).Scan(&storedAgent, &projectUID, &kind, &status, &phase, &revision, &requestedAt, &actor, &summary, &output); err != nil {
		t.Fatal(err)
	}
	if storedAgent != agentID || projectUID != "project-a" || kind != "compose.up" || status != "running" ||
		phase != "EXECUTING" || revision != 3 || requestedAt != nil || actor != nil || output != "recovering" ||
		!strings.Contains(summary, `"target":"api"`) {
		t.Fatalf("recovered row agent=%q project=%q kind=%q status=%q phase=%q rev=%d requested=%v actor=%v summary=%q output=%q",
			storedAgent, projectUID, kind, status, phase, revision, requestedAt, actor, summary, output)
	}
	if session.listCalls != 1 || session.cancels != 0 {
		t.Fatalf("recovery calls=%d cancellations=%d", session.listCalls, session.cancels)
	}

	active.Operation.Status = "success"
	active.Operation.Phase = "FINALIZING"
	active.Operation.Revision = 4
	backend, _ = newRestartedBackend([]producttransport.ActiveOperation{active})
	if _, err := backend.Dashboard(ctx); err != nil {
		t.Fatal(err)
	}
	active.Operation.Status = "running"
	active.Operation.Phase = "EXECUTING"
	active.Operation.Revision = 2
	backend, _ = newRestartedBackend([]producttransport.ActiveOperation{active})
	if _, err := backend.Dashboard(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := backend.mergeOperation(ctx, operationSpec{
		ID: "op-unknown", AgentID: agentID, Kind: "docker.prune", Target: "host",
	}, webui.Operation{ID: "op-unknown", Status: "unknown", Phase: "EXECUTING", Revision: 7}, true); err != nil {
		t.Fatal(err)
	}
	backend, session = newRestartedBackend(nil)
	if _, err := backend.Dashboard(ctx); err != nil {
		t.Fatal(err)
	}
	stored, storedSpec, err := backend.loadStoredOperation(ctx, active.OperationID)
	if err != nil || stored.Revision != 4 || stored.Status != "success" || storedSpec.Target != "api" {
		t.Fatalf("operation absent from active list was changed: operation=%+v spec=%+v err=%v", stored, storedSpec, err)
	}
	unknown, _, err := backend.loadStoredOperation(ctx, "op-unknown")
	if err != nil || unknown.Status != "unknown" || unknown.Revision != 7 {
		t.Fatalf("unknown operation absent from active list was inferred: %+v, %v", unknown, err)
	}
	if session.cancels != 0 {
		t.Fatalf("empty active list sent %d cancellations", session.cancels)
	}
}

func TestActiveOperationRecoveryIsolatesNMinusOneDuplicateAndSpecConflict(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	agentA, agentB, agentC, agentD := "agent-a", "agent-b", "agent-c", "agent-d"
	for _, id := range []string{agentA, agentB, agentC, agentD} {
		insertAgent(t, ctx, store, id, id, `{}`)
	}
	insertProject(t, ctx, store, "project-a", agentA, "Project", `{}`)
	if _, err := backend.mergeOperation(ctx, operationSpec{
		ID: "op-z", AgentID: agentA, ProjectUID: "project-a", Kind: "compose.up", Target: "original",
	}, webui.Operation{ID: "op-z", Status: "running", Phase: "EXECUTING", Revision: 2}, true); err != nil {
		t.Fatal(err)
	}

	nMinusOne := newFakeSession(agentA)
	conflicting := &fakeRecoverySession{fakeSession: newFakeSession(agentA), active: []producttransport.ActiveOperation{
		{OperationID: "op-a", Type: "compose.up", ProjectKey: "project-a", Target: "new", Operation: producttransport.OperationResponse{Status: "running", Phase: "EXECUTING", Revision: 1}},
		{OperationID: "op-z", Type: "compose.up", ProjectKey: "project-a", Target: "conflict", Operation: producttransport.OperationResponse{Status: "running", Phase: "EXECUTING", Revision: 3}},
	}}
	valid := &fakeRecoverySession{fakeSession: newFakeSession(agentB), active: []producttransport.ActiveOperation{
		{OperationID: "op-b", Type: "docker.prune", Target: "host", Operation: producttransport.OperationResponse{Status: "running", Phase: "EXECUTING", Revision: 1}},
	}}
	duplicate := &fakeRecoverySession{fakeSession: newFakeSession(agentC), active: []producttransport.ActiveOperation{
		{OperationID: "op-c", Type: "docker.prune", Operation: producttransport.OperationResponse{Status: "running", Phase: "EXECUTING", Revision: 1}},
		{OperationID: "op-c", Type: "docker.prune", Operation: producttransport.OperationResponse{Status: "running", Phase: "EXECUTING", Revision: 1}},
	}}
	projectInvalid := &fakeRecoverySession{fakeSession: newFakeSession(agentD), active: []producttransport.ActiveOperation{
		{OperationID: "op-d", Type: "docker.prune", Operation: producttransport.OperationResponse{Status: "running", Phase: "EXECUTING", Revision: 1}},
	}}
	projectInvalid.setProjectListPayload([]byte(`{"projects":`))
	// First verify a concrete N-1 session is a supported degraded case.
	if err := registry.Register(nMinusOne); err != nil {
		t.Fatal(err)
	}
	dashboard, err := backend.Dashboard(ctx)
	if err != nil || dashboard.Hosts[0].Capabilities.OperationRecovery.Enabled ||
		!strings.Contains(dashboard.Hosts[0].Capabilities.OperationRecovery.Reason, "unsupported") {
		t.Fatalf("N-1 dashboard = %+v, %v", dashboard.Hosts, err)
	}
	nMinusOneRPC := &fakeRecoverySession{fakeSession: newFakeSession(agentA), listErr: producttransport.ErrHandlerUnavailable}
	if err := registry.Register(nMinusOneRPC); err != nil {
		t.Fatal(err)
	}
	dashboard, err = backend.Dashboard(ctx)
	if err != nil || dashboard.Hosts[0].Capabilities.OperationRecovery.Enabled ||
		!strings.Contains(dashboard.Hosts[0].Capabilities.OperationRecovery.Reason, "unsupported") || nMinusOneRPC.listCalls != 1 {
		t.Fatalf("N-1 typed-unavailable dashboard = %+v calls=%d err=%v", dashboard.Hosts, nMinusOneRPC.listCalls, err)
	}

	// Replacement and unrelated Agents recover independently.
	for _, session := range []producttransport.ControlSession{conflicting, valid, duplicate, projectInvalid} {
		if err := registry.Register(session); err != nil {
			t.Fatal(err)
		}
	}
	dashboard, err = backend.Dashboard(ctx)
	if err != nil {
		t.Fatal(err)
	}
	hosts := make(map[string]webui.Host)
	for _, host := range dashboard.Hosts {
		hosts[host.ID] = host
	}
	if hosts[agentA].Capabilities.OperationRecovery.Enabled || !strings.Contains(hosts[agentA].Capabilities.OperationRecovery.Reason, "conflicts") ||
		!hosts[agentB].Capabilities.OperationRecovery.Enabled || hosts[agentC].Capabilities.OperationRecovery.Enabled ||
		!strings.Contains(hosts[agentC].Capabilities.OperationRecovery.Reason, "invalid") ||
		hosts[agentD].Capabilities.OperationRecovery.Enabled || !strings.Contains(hosts[agentD].Capabilities.OperationRecovery.Reason, "conflicts") ||
		projectInvalid.listCalls != 0 {
		t.Fatalf("recovery capabilities A=%+v B=%+v C=%+v D=%+v D-calls=%d", hosts[agentA].Capabilities.OperationRecovery, hosts[agentB].Capabilities.OperationRecovery, hosts[agentC].Capabilities.OperationRecovery, hosts[agentD].Capabilities.OperationRecovery, projectInvalid.listCalls)
	}
	var countA, countB, countD int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM operations WHERE id = 'op-a'`).Scan(&countA); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM operations WHERE id = 'op-b' AND agent_id = ?`, agentB).Scan(&countB); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM operations WHERE id = 'op-d'`).Scan(&countD); err != nil {
		t.Fatal(err)
	}
	stored, _, err := backend.loadStoredOperation(ctx, "op-z")
	if err != nil || countA != 0 || countB != 1 || countD != 0 || stored.Revision != 2 {
		t.Fatalf("atomic isolation countA=%d countB=%d countD=%d op-z=%+v err=%v", countA, countB, countD, stored, err)
	}
	if conflicting.cancels != 0 || valid.cancels != 0 || duplicate.cancels != 0 || projectInvalid.cancels != 0 {
		t.Fatalf("recovery synthesized cancellation: A=%d B=%d C=%d D=%d", conflicting.cancels, valid.cancels, duplicate.cancels, projectInvalid.cancels)
	}
}

func TestCorruptStoredJSONStopsAtSQLBoundary(t *testing.T) {
	t.Run("agent capabilities", func(t *testing.T) {
		ctx, backend, store, _ := newTestBackend(t)
		insertAgent(t, ctx, store, "agent-a", "Agent", `{"unexpected":true}`)
		if _, err := backend.Dashboard(ctx); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("Dashboard error = %v", err)
		}
	})
	t.Run("project flags", func(t *testing.T) {
		ctx, backend, store, _ := newTestBackend(t)
		insertAgent(t, ctx, store, "agent-a", "Agent", `{}`)
		insertProject(t, ctx, store, "project-a", "agent-a", "Project", `{"read_only":"yes"}`)
		if _, err := backend.Dashboard(ctx); !errors.Is(err, ErrCorruptData) {
			t.Fatalf("Dashboard error = %v", err)
		}
	})
}

func TestBackendConcurrentLiveReadsAndDispatch(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "Agent", `{"fs_read":true}`)
	insertProject(t, ctx, store, "project-a", "agent-a", "Project", `{}`)
	session := newFakeSession("agent-a")
	session.capability = producttransport.Capability{ConnectionReady: true, DockerReady: true, ComposeReady: true}
	session.queryPayload = []byte(`[{"name":"A","value":"B","secret":true}]`)
	session.operation = producttransport.OperationResponse{Status: "ACCEPTED", Phase: "QUEUED"}
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}

	const workers = 8
	var wait sync.WaitGroup
	errs := make(chan error, workers*3)
	for index := 0; index < workers; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, err := backend.Dashboard(ctx)
			errs <- err
			_, err = backend.ProjectEnvironment(ctx, "project-a")
			errs <- err
			_, err = backend.StartOperation(ctx, webui.OperationRequest{
				ID: "op-" + string(rune('a'+index)), AgentID: "agent-a", ProjectUID: "project-a", Kind: "compose.up",
			})
			errs <- err
		}(index)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestLiveAdaptersUseActiveSessionWithoutPersistingLogsOrStats(t *testing.T) {
	ctx, backend, store, registry := newTestBackend(t)
	insertAgent(t, ctx, store, "agent-a", "Agent", `{}`)
	containerID := strings.Repeat("a", 64)
	secret := "live-log-secret-never-persist"
	logs := &fakeLogReceiveStream{events: []producttransport.LogEvent{{
		Data: []byte(secret), Stream: "STDERR", LineCount: 1, Error: "credential=also-private",
	}}}
	stats := &fakeStatsReceiveStream{samples: []producttransport.StatsSample{{
		ContainerID: containerID, CPUPercent: 42, MemoryUsage: 100, Health: "healthy",
	}}}
	session := newFakeSession("agent-a")
	session.logStream, session.statsStream = logs, stats
	if err := registry.Register(session); err != nil {
		t.Fatal(err)
	}
	if len(backend.liveStats) != 0 || session.statsRequest.ContainerID != "" {
		t.Fatal("zero viewers created Server stats state or transport stream")
	}

	logStream, err := backend.OpenLogs(ctx, webui.LiveRequest{
		AgentID: "agent-a", ContainerID: containerID, Follow: true, TailLines: 20, ShowStderr: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	event, err := logStream.Recv(ctx)
	if err != nil || string(event.Data) != secret || event.Error != "log stream ended" || event.Stream != "STDERR" {
		t.Fatalf("live log = %+v, %v", event, err)
	}
	_ = logStream.Close()
	if session.logRequest.ContainerID != containerID || session.logRequest.TailLines != 20 || !logs.closed {
		t.Fatalf("transport log request=%+v closed=%v", session.logRequest, logs.closed)
	}
	assertNoPersistentSecret(t, ctx, store, secret)

	statsStream, err := backend.OpenStats(ctx, webui.LiveRequest{AgentID: "agent-a", ContainerID: containerID})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := backend.currentStats("agent-a", containerID); ok {
		t.Fatal("Server fabricated a stats sample before transport receive")
	}
	sample, err := statsStream.Recv(ctx)
	if err != nil || sample.CPUPercent != 42 || sample.ContainerID != containerID {
		t.Fatalf("live stats = %+v, %v", sample, err)
	}
	latest, ok := backend.currentStats("agent-a", containerID)
	if !ok || latest.CPUPercent != 42 || len(backend.liveStats) != 1 {
		t.Fatalf("Server latest stats = %+v, %v map=%d", latest, ok, len(backend.liveStats))
	}
	_ = statsStream.Close()
	if _, ok := backend.currentStats("agent-a", containerID); ok || len(backend.liveStats) != 0 || !stats.closed {
		t.Fatalf("last viewer retained stats: map=%d closed=%v", len(backend.liveStats), stats.closed)
	}

	if _, err := backend.OpenStats(ctx, webui.LiveRequest{AgentID: "agent-a", ContainerID: "short"}); !errors.Is(err, webui.ErrInvalidRequest) {
		t.Fatalf("non-canonical container error = %v", err)
	}
}

type fakeSession struct {
	mu                     sync.Mutex
	info                   producttransport.SessionInfo
	state                  producttransport.State
	done                   chan struct{}
	capability             producttransport.Capability
	queryPayload           []byte
	projectListPayload     []byte
	projectSnapshotPayload []byte
	queryErr               error
	operation              producttransport.OperationResponse
	operationErr           error
	operationHook          func()
	getOperation           producttransport.GetOperationResponse
	getOperationErr        error
	cancelOperation        producttransport.CancelOperationResponse
	cancelErr              error
	heartbeats             int
	queries                int
	operations             int
	gets                   int
	cancels                int
	queryRequest           producttransport.QueryRequest
	operationRequest       producttransport.OperationRequest
	getRequest             producttransport.GetOperationRequest
	cancelRequest          producttransport.CancelOperationRequest
	logRequest             producttransport.LogRequest
	statsRequest           producttransport.StatsRequest
	logStream              producttransport.LogReceiveStream
	statsStream            producttransport.StatsReceiveStream
}

type fakeRecoverySession struct {
	*fakeSession
	active    []producttransport.ActiveOperation
	listErr   error
	listCalls int
}

func (s *fakeRecoverySession) ListActiveOperations(context.Context, producttransport.ListActiveOperationsRequest) (producttransport.ListActiveOperationsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	operations := make([]producttransport.ActiveOperation, len(s.active))
	copy(operations, s.active)
	for index := range operations {
		operations[index].Operation.OutputTail = append([]byte(nil), operations[index].Operation.OutputTail...)
	}
	return producttransport.ListActiveOperationsResponse{Operations: operations}, s.listErr
}

func newFakeSession(agentID string) *fakeSession {
	return &fakeSession{
		info:               producttransport.SessionInfo{SessionID: "session-" + producttransport.SessionID(agentID), AgentID: agentID, Incarnation: 1},
		state:              producttransport.StateActive,
		done:               make(chan struct{}),
		projectListPayload: []byte(`{"projects":[],"status":{"scanned_at":"2026-08-15T00:00:00Z","truncated":true,"stop_reason":"MAX_DURATION","directories_seen":0}}`),
	}
}

func (s *fakeSession) Info() producttransport.SessionInfo { return s.info }
func (s *fakeSession) Done() <-chan struct{}              { return s.done }
func (s *fakeSession) Err() error                         { return nil }
func (s *fakeSession) Close(error) error {
	s.mu.Lock()
	if s.state != producttransport.StateClosed {
		s.state = producttransport.StateClosed
		close(s.done)
	}
	s.mu.Unlock()
	return nil
}
func (s *fakeSession) Heartbeat(context.Context) (producttransport.Heartbeat, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.heartbeats++
	return producttransport.Heartbeat{Capability: s.capability}, nil
}
func (s *fakeSession) Query(_ context.Context, request producttransport.QueryRequest) (producttransport.QueryResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queries++
	request.Payload = append([]byte(nil), request.Payload...)
	s.queryRequest = request
	if request.Kind == QueryProjectList {
		return producttransport.QueryResponse{Payload: append([]byte(nil), s.projectListPayload...)}, s.queryErr
	}
	if request.Kind == "project.snapshot" {
		return producttransport.QueryResponse{Payload: append([]byte(nil), s.projectSnapshotPayload...)}, s.queryErr
	}
	return producttransport.QueryResponse{Payload: append([]byte(nil), s.queryPayload...)}, s.queryErr
}
func (s *fakeSession) StartOperation(_ context.Context, request producttransport.OperationRequest) (producttransport.OperationResponse, error) {
	if s.operationHook != nil {
		s.operationHook()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.operations++
	request.Payload = append([]byte(nil), request.Payload...)
	s.operationRequest = request
	response := s.operation
	response.OutputTail = append([]byte(nil), response.OutputTail...)
	return response, s.operationErr
}
func (s *fakeSession) GetOperation(_ context.Context, request producttransport.GetOperationRequest) (producttransport.GetOperationResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets++
	s.getRequest = request
	response := s.getOperation
	response.Operation.OutputTail = append([]byte(nil), response.Operation.OutputTail...)
	return response, s.getOperationErr
}
func (s *fakeSession) CancelOperation(_ context.Context, request producttransport.CancelOperationRequest) (producttransport.CancelOperationResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cancels++
	s.cancelRequest = request
	response := s.cancelOperation
	response.Operation.OutputTail = append([]byte(nil), response.Operation.OutputTail...)
	return response, s.cancelErr
}
func (s *fakeSession) OpenLogs(_ context.Context, request producttransport.LogRequest) (producttransport.LogReceiveStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.logRequest = request
	if s.logStream == nil {
		return nil, producttransport.ErrHandlerUnavailable
	}
	return s.logStream, nil
}
func (s *fakeSession) OpenStats(_ context.Context, request producttransport.StatsRequest) (producttransport.StatsReceiveStream, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statsRequest = request
	if s.statsStream == nil {
		return nil, producttransport.ErrHandlerUnavailable
	}
	return s.statsStream, nil
}

type fakeLogReceiveStream struct {
	events []producttransport.LogEvent
	closed bool
}

func (s *fakeLogReceiveStream) Recv(context.Context) (producttransport.LogEvent, error) {
	if len(s.events) == 0 {
		return producttransport.LogEvent{}, io.EOF
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}
func (s *fakeLogReceiveStream) Close() error { s.closed = true; return nil }

type fakeStatsReceiveStream struct {
	samples []producttransport.StatsSample
	closed  bool
}

func (s *fakeStatsReceiveStream) Recv(context.Context) (producttransport.StatsSample, error) {
	if len(s.samples) == 0 {
		return producttransport.StatsSample{}, io.EOF
	}
	sample := s.samples[0]
	s.samples = s.samples[1:]
	return sample, nil
}
func (s *fakeStatsReceiveStream) Close() error { s.closed = true; return nil }
func (s *fakeSession) State() producttransport.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}
func (s *fakeSession) LastHeartbeat() time.Time { return time.Time{} }
func (s *fakeSession) Do(ctx context.Context, _ producttransport.TrafficClass, work func(context.Context) error) error {
	return work(ctx)
}
func (s *fakeSession) heartbeatCalls() int { s.mu.Lock(); defer s.mu.Unlock(); return s.heartbeats }
func (s *fakeSession) operationCalls() int { s.mu.Lock(); defer s.mu.Unlock(); return s.operations }
func (s *fakeSession) lastQuery() producttransport.QueryRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	request := s.queryRequest
	request.Payload = append([]byte(nil), request.Payload...)
	return request
}
func (s *fakeSession) lastOperation() producttransport.OperationRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	request := s.operationRequest
	request.Payload = append([]byte(nil), request.Payload...)
	return request
}
func (s *fakeSession) setQueryPayload(payload []byte) {
	s.mu.Lock()
	s.queryPayload = append([]byte(nil), payload...)
	s.mu.Unlock()
}

func (s *fakeSession) setProjectListPayload(payload []byte) {
	s.mu.Lock()
	s.projectListPayload = append([]byte(nil), payload...)
	s.mu.Unlock()
}

func (s *fakeSession) setProjectSnapshotPayload(payload []byte) {
	s.mu.Lock()
	s.projectSnapshotPayload = append([]byte(nil), payload...)
	s.mu.Unlock()
}

func testAgentProject(t *testing.T, agentID, root, workingDir, name, fileHash string, services []string) agentProjectSnapshot {
	t.Helper()
	uid, err := projectmodel.UID(agentID, workingDir)
	if err != nil {
		t.Fatal(err)
	}
	files := []agentProjectFileFact{{Path: filepath.Join(workingDir, "compose.yaml"), Size: 42, SHA256: fileHash}}
	fingerprint, err := projectmodel.Fingerprint([]projectmodel.FileFact{{Path: files[0].Path, Size: files[0].Size, SHA256: files[0].SHA256}})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(services)
	return agentProjectSnapshot{
		UID: uid, Root: root, WorkingDir: workingDir, Files: files, Name: name,
		Services: append([]string(nil), services...), CurrentFingerprint: fingerprint,
		ComposeExecutable: true, FilesystemWritable: true,
	}
}

func projectListPayload(t *testing.T, scannedAt time.Time, truncated bool, projects ...agentProjectSnapshot) []byte {
	t.Helper()
	status := agentProjectScanStatus{ScannedAt: scannedAt.UTC(), DirectoriesSeen: len(projects), Truncated: truncated}
	if truncated {
		status.StopReason = "MAX_DURATION"
	}
	payload, err := json.Marshal(agentProjectList{Projects: projects, Status: status})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func newTestBackend(t *testing.T) (context.Context, *Backend, *serverstore.Store, *producttransport.SessionRegistry) {
	t.Helper()
	ctx := context.Background()
	store, err := serverstore.Open(ctx, filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	registry := producttransport.NewSessionRegistry()
	backend, err := New(store, registry)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, backend, store, registry
}

func insertAgent(t *testing.T, ctx context.Context, store *serverstore.Store, id, displayName, capabilities string) {
	t.Helper()
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO agents(id, display_name, first_seen_at, last_seen_at, capabilities_json)
		VALUES (?, ?, ?, ?, ?)
	`, id, displayName, dbTime(time.Now()), dbTime(time.Now()), capabilities); err != nil {
		t.Fatal(err)
	}
}

func insertProject(t *testing.T, ctx context.Context, store *serverstore.Store, uid, agentID, name, flags string) {
	t.Helper()
	if _, err := store.DB().ExecContext(ctx, `
		INSERT INTO projects(project_uid, agent_id, working_dir, name, flags_json, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, uid, agentID, "/srv/"+uid, name, flags, dbTime(time.Now())); err != nil {
		t.Fatal(err)
	}
}

func assertNoPersistentSecret(t *testing.T, ctx context.Context, store *serverstore.Store, secret string) {
	t.Helper()
	var count int
	if err := store.DB().QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM agents WHERE instr(metadata_json, ?) > 0 OR instr(capabilities_json, ?) > 0) +
			(SELECT COUNT(*) FROM projects WHERE instr(flags_json, ?) > 0 OR instr(applied_fingerprints_json, ?) > 0) +
			(SELECT COUNT(*) FROM operations WHERE instr(summary_json, ?) > 0 OR instr(COALESCE(CAST(output_tail AS TEXT), ''), ?) > 0) +
			(SELECT COUNT(*) FROM settings WHERE instr(value_json, ?) > 0)
	`, secret, secret, secret, secret, secret, secret, secret).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("secret appeared in durable Server state (%d matches)", count)
	}
}

func dbTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
