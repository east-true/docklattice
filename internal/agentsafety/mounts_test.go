package agentsafety

import "testing"

func TestAssessDiscoveryRoot(t *testing.T) {
	tests := []struct {
		name        string
		root        string
		mounts      []Mount
		wantReason  MountReason
		wantMount   string
		wantHost    string
		wantWrite   bool
		wantCompose bool
	}{
		{
			name: "most specific identical bind",
			root: "/srv/apps/project",
			mounts: []Mount{
				{Type: "bind", Source: "/srv", Destination: "/srv", RW: true},
				{Type: "bind", Source: "/srv/apps", Destination: "/srv/apps", RW: true},
			},
			wantReason: MountReady, wantMount: "/srv/apps", wantHost: "/srv/apps/project", wantWrite: true, wantCompose: true,
		},
		{
			name:       "component boundary is not string prefix",
			root:       "/srv/application/project",
			mounts:     []Mount{{Type: "bind", Source: "/srv/app", Destination: "/srv/app", RW: true}},
			wantReason: MountNotFound,
		},
		{
			name: "non-bind child overlays valid parent",
			root: "/srv/apps/project",
			mounts: []Mount{
				{Type: "bind", Source: "/srv", Destination: "/srv", RW: true},
				{Type: "volume", Source: "named", Destination: "/srv/apps", RW: true},
			},
			wantReason: MountNotBind, wantMount: "/srv/apps",
		},
		{
			name:       "host path differs",
			root:       "/srv/apps/project",
			mounts:     []Mount{{Type: "bind", Source: "/host/apps", Destination: "/srv/apps", RW: true}},
			wantReason: MountIdentityMismatch, wantMount: "/srv/apps", wantHost: "/host/apps/project",
		},
		{
			name:       "read only keeps compose path identity",
			root:       "/srv/apps",
			mounts:     []Mount{{Type: "bind", Source: "/srv/apps", Destination: "/srv/apps", RW: false}},
			wantReason: MountReadOnly, wantMount: "/srv/apps", wantHost: "/srv/apps", wantCompose: true,
		},
		{
			name:       "linux traversal is normalized before comparison",
			root:       "/srv/apps/../project",
			mounts:     []Mount{{Type: "bind", Source: "/srv", Destination: "/srv/./", RW: true}},
			wantReason: MountReady, wantMount: "/srv", wantHost: "/srv/project", wantWrite: true, wantCompose: true,
		},
		{
			name:       "root mount",
			root:       "/srv/project",
			mounts:     []Mount{{Type: "bind", Source: "/", Destination: "/", RW: true}},
			wantReason: MountReady, wantMount: "/", wantHost: "/srv/project", wantWrite: true, wantCompose: true,
		},
		{
			name:       "relative root rejected",
			root:       "srv/project",
			mounts:     []Mount{{Type: "bind", Source: "/srv", Destination: "/srv", RW: true}},
			wantReason: MountInvalidRoot,
		},
		{
			name:       "nul root rejected",
			root:       "/srv/project\x00/escape",
			mounts:     []Mount{{Type: "bind", Source: "/srv", Destination: "/srv", RW: true}},
			wantReason: MountInvalidRoot,
		},
		{
			name: "duplicate most specific mounts fail closed",
			root: "/srv/project",
			mounts: []Mount{
				{Type: "bind", Source: "/srv", Destination: "/srv", RW: true},
				{Type: "bind", Source: "/other", Destination: "/srv", RW: true},
			},
			wantReason: MountAmbiguous, wantMount: "/srv",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AssessDiscoveryRoot(tt.root, tt.mounts)
			if got.Reason != tt.wantReason || got.Mount.Destination != tt.wantMount || got.HostPath != tt.wantHost || got.FSWrite != tt.wantWrite || got.ComposeExec != tt.wantCompose {
				t.Fatalf("assessment = %#v", got)
			}
		})
	}
}

func TestInvalidBindSourceDisablesCapabilities(t *testing.T) {
	got := AssessDiscoveryRoot("/srv/project", []Mount{{Type: "bind", Source: "relative", Destination: "/srv", RW: true}})
	if got.Reason != MountInvalid || got.FSWrite || got.ComposeExec {
		t.Fatalf("assessment = %#v", got)
	}
}
