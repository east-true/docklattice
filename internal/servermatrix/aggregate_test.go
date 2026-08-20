package servermatrix

import (
	"context"
	"testing"
	"time"

	"github.com/east-true/dockpilot/internal/producttransport"
)

func bounded(id string, sample producttransport.StatsSample, context ContainerContext) ContainerRow {
	sample.ContainerID = id
	row := ContainerRow{ContainerID: id, Sample: sample, ContainerContext: context}
	markMemoryLimit(&row, 16<<30)
	return row
}

// The metrics that add up, add up. Every one of them comes from the same frame,
// so a row is a projection of its members rather than a second measurement.
func TestAggregateSumsTheMetricsThatAddUp(t *testing.T) {
	rows := []ContainerRow{
		bounded("a", producttransport.StatsSample{
			CPUPercent: 12.5, MemoryUsage: 100, MemoryLimit: 400, NetworkRX: 10, NetworkTX: 20,
			BlockRead: 30, BlockWrite: 40, RestartCount: 1,
		}, ContainerContext{}),
		bounded("b", producttransport.StatsSample{
			CPUPercent: 7.5, MemoryUsage: 300, MemoryLimit: 600, NetworkRX: 1, NetworkTX: 2,
			BlockRead: 3, BlockWrite: 4, RestartCount: 2,
		}, ContainerContext{}),
	}
	total := aggregate(rows)
	if total.CPUPercent != 20 || total.MemoryUsage != 400 || total.NetworkRX != 11 || total.NetworkTX != 22 {
		t.Fatalf("sums are %+v", total)
	}
	if total.BlockRead != 33 || total.BlockWrite != 44 || total.Restarts != 3 {
		t.Fatalf("block and restart sums are %+v", total)
	}
	if total.ContainerCount != 2 || total.PendingCount != 0 {
		t.Fatalf("row covers %d containers (%d pending)", total.ContainerCount, total.PendingCount)
	}
	if total.MemoryLimitUnbounded || total.MemoryLimit != 1000 {
		t.Fatalf("memory limit is %d (unbounded=%v), want 1000 bounded", total.MemoryLimit, total.MemoryLimitUnbounded)
	}
	if !total.MemoryPercentKnown || total.MemoryPercent != 40 {
		t.Fatalf("memory percent is %v (known=%v), want 40", total.MemoryPercent, total.MemoryPercentKnown)
	}
}

// One unlimited member makes the whole row's limit unbounded, and the percent
// is withheld rather than computed against a number that does not bound
// anything. Unbounded is a different kind of answer from a large one.
func TestOneUnlimitedMemberMakesTheRowUnbounded(t *testing.T) {
	rows := []ContainerRow{
		bounded("a", producttransport.StatsSample{MemoryUsage: 100, MemoryLimit: 400}, ContainerContext{}),
		bounded("b", producttransport.StatsSample{MemoryUsage: 900, MemoryLimit: 16 << 30}, ContainerContext{}),
	}
	total := aggregate(rows)
	if !total.MemoryLimitUnbounded {
		t.Fatal("a row with an unlimited member reported a bounded limit")
	}
	if total.MemoryLimit != 0 {
		t.Fatalf("an unbounded row still carried the number %d, which would be read as a limit", total.MemoryLimit)
	}
	if total.MemoryPercentKnown || total.MemoryPercent != 0 {
		t.Fatalf("memory percent was computed against an unbounded limit: %v", total.MemoryPercent)
	}
	if total.MemoryUsage != 1000 {
		t.Fatalf("usage still adds up regardless of limits: %d", total.MemoryUsage)
	}
}

// Health is the worst of the members, and "no healthcheck" outranks "healthy":
// healthy is the only claim that needs every member to make it.
func TestHealthIsTheWorstOfTheMembers(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		members []string
		want    string
	}{
		{"all healthy", []string{"healthy", "healthy"}, "healthy"},
		{"one unhealthy", []string{"healthy", "unhealthy"}, "unhealthy"},
		{"one starting", []string{"healthy", "starting"}, "starting"},
		{"unhealthy beats starting", []string{"starting", "unhealthy"}, "unhealthy"},
		{"one without a healthcheck", []string{"healthy", ""}, "none"},
		{"unhealthy beats no healthcheck", []string{"", "unhealthy"}, "unhealthy"},
		{"an unrecognised status is no information", []string{"healthy", "restarting-soon"}, "none"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var rows []ContainerRow
			for index, status := range testCase.members {
				rows = append(rows, bounded(string(rune('a'+index)),
					producttransport.StatsSample{Health: status, MemoryLimit: 100}, ContainerContext{}))
			}
			if got := aggregate(rows).Health; got != testCase.want {
				t.Fatalf("health is %q, want %q", got, testCase.want)
			}
		})
	}
}

// A service is only as old as its youngest container.
func TestUptimeIsTheYoungestMember(t *testing.T) {
	rows := []ContainerRow{
		bounded("a", producttransport.StatsSample{Uptime: time.Hour, MemoryLimit: 100}, ContainerContext{}),
		bounded("b", producttransport.StatsSample{Uptime: 90 * time.Second, MemoryLimit: 100}, ContainerContext{}),
	}
	total := aggregate(rows)
	if !total.UptimeKnown || total.Uptime != 90*time.Second {
		t.Fatalf("uptime is %v (known=%v), want the youngest member's 90s", total.Uptime, total.UptimeKnown)
	}
}

// A container that has not reported yet is counted but contributes nothing. It
// cannot be counted as idle, because it has not said anything at all.
func TestPendingMembersCountButDoNotContribute(t *testing.T) {
	rows := []ContainerRow{
		bounded("a", producttransport.StatsSample{CPUPercent: 5, MemoryUsage: 100, MemoryLimit: 200, Uptime: time.Minute}, ContainerContext{}),
		{ContainerID: "b", Pending: true},
	}
	total := aggregate(rows)
	if total.ContainerCount != 2 || total.PendingCount != 1 {
		t.Fatalf("row covers %d containers (%d pending), want 2 and 1", total.ContainerCount, total.PendingCount)
	}
	if total.CPUPercent != 5 || total.MemoryUsage != 100 || total.MemoryLimit != 200 {
		t.Fatalf("a pending member changed the sums: %+v", total)
	}
	if total.Uptime != time.Minute {
		t.Fatalf("a pending member was treated as a zero-second-old container: %v", total.Uptime)
	}

	empty := aggregate([]ContainerRow{{ContainerID: "a", Pending: true}})
	if empty.UptimeKnown || empty.Health != "" || empty.MemoryPercentKnown {
		t.Fatalf("a row with nothing reported claimed something: %+v", empty)
	}
}

// Docker reports the machine's memory as the limit for a container that has
// none, so the number alone cannot be read as a limit.
func TestAMemoryLimitAtTheMachineSizeIsNotALimit(t *testing.T) {
	for _, testCase := range []struct {
		name          string
		limit         uint64
		capacity      uint64
		wantUnbounded bool
	}{
		{"a real limit", 512 << 20, 16 << 30, false},
		{"the whole machine", 16 << 30, 16 << 30, true},
		{"more than the machine", 32 << 30, 16 << 30, true},
		{"no limit reported at all", 0, 16 << 30, true},
		{"unknown capacity leaves a plausible limit alone", 512 << 20, 0, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			row := ContainerRow{ContainerID: "a", Sample: producttransport.StatsSample{MemoryUsage: 128 << 20, MemoryLimit: testCase.limit}}
			markMemoryLimit(&row, testCase.capacity)
			if row.MemoryLimitUnbounded != testCase.wantUnbounded {
				t.Fatalf("unbounded=%v, want %v", row.MemoryLimitUnbounded, testCase.wantUnbounded)
			}
			if row.MemoryPercentKnown == testCase.wantUnbounded {
				t.Fatalf("percent known=%v alongside unbounded=%v", row.MemoryPercentKnown, row.MemoryLimitUnbounded)
			}
		})
	}
}

// The tree is project, then service, then container. A project Dockpilot does
// not manage is still a project; containers belonging to no project at all are
// a bucket of their own and sort last, but they are never hidden.
func TestGroupingKeepsEveryContainerSomewhere(t *testing.T) {
	rows := []ContainerRow{
		bounded("a", producttransport.StatsSample{CPUPercent: 1, MemoryLimit: 100}, ContainerContext{ProjectUID: "uid-shop", ProjectName: "shop", Service: "web"}),
		bounded("b", producttransport.StatsSample{CPUPercent: 2, MemoryLimit: 100}, ContainerContext{ProjectUID: "uid-shop", ProjectName: "shop", Service: "web"}),
		bounded("c", producttransport.StatsSample{CPUPercent: 4, MemoryLimit: 100}, ContainerContext{ProjectUID: "uid-shop", ProjectName: "shop", Service: "db"}),
		bounded("d", producttransport.StatsSample{CPUPercent: 8, MemoryLimit: 100}, ContainerContext{ProjectName: "someone-elses-stack", Service: "api"}),
		{ContainerID: "e", Unmapped: true, Sample: producttransport.StatsSample{ContainerID: "e", CPUPercent: 16, MemoryLimit: 100}},
	}
	projects := groupProjects(rows)
	if len(projects) != 3 {
		t.Fatalf("grouped into %d projects, want shop, the foreign stack, and the unmapped bucket", len(projects))
	}
	if projects[0].ProjectName != "shop" || projects[0].ProjectUID != "uid-shop" {
		t.Fatalf("first project is %+v", projects[0])
	}
	if projects[1].ProjectName != "someone-elses-stack" || projects[1].ProjectUID != "" || projects[1].Unmapped {
		t.Fatalf("a Compose project Dockpilot does not manage was not shown as itself: %+v", projects[1])
	}
	if !projects[2].Unmapped {
		t.Fatalf("the containers belonging to no project did not sort last: %+v", projects[2])
	}

	shop := projects[0]
	if len(shop.Services) != 2 || shop.Services[0].Service != "db" || shop.Services[1].Service != "web" {
		t.Fatalf("services are %+v, want db then web", shop.Services)
	}
	if shop.Services[1].Totals.ContainerCount != 2 || shop.Services[1].Totals.CPUPercent != 3 {
		t.Fatalf("the web service row is %+v, want two containers summing to 3%%", shop.Services[1].Totals)
	}
	if shop.Totals.ContainerCount != 3 || shop.Totals.CPUPercent != 7 {
		t.Fatalf("the project row is %+v, want three containers summing to 7%%", shop.Totals)
	}
	if unmapped := projects[2].Services[0]; !unmapped.Unmapped || len(unmapped.Containers) != 1 || unmapped.Totals.CPUPercent != 16 {
		t.Fatalf("the unmapped container lost its metrics on the way into the tree: %+v", unmapped)
	}
}

// A project's containers can carry a project label and no service label. That
// is a service-shaped hole inside a real project, not a reason to move the
// container out of it.
func TestAServiceLessContainerStaysInsideItsProject(t *testing.T) {
	rows := []ContainerRow{
		bounded("a", producttransport.StatsSample{MemoryLimit: 100}, ContainerContext{ProjectUID: "uid-shop", ProjectName: "shop", Service: "web"}),
		bounded("b", producttransport.StatsSample{MemoryLimit: 100}, ContainerContext{ProjectUID: "uid-shop", ProjectName: "shop"}),
	}
	projects := groupProjects(rows)
	if len(projects) != 1 || projects[0].Unmapped {
		t.Fatalf("grouped into %+v, want one real project", projects)
	}
	if len(projects[0].Services) != 2 {
		t.Fatalf("services are %+v, want web and the unnamed one", projects[0].Services)
	}
	if last := projects[0].Services[1]; !last.Unmapped || last.Containers[0].ContainerID != "b" {
		t.Fatalf("the service-less container is %+v, want it last and marked unmapped", last)
	}
}

// End to end: a frame becomes a tree with the host row summing all of it.
func TestViewCarriesTheWholeHostAsATree(t *testing.T) {
	sessions := &fakeSessions{}
	hub, source, _ := newContextHub(t, sessions)
	source.set(map[string]ContainerContext{
		"a": {ProjectUID: "uid-shop", ProjectName: "shop", Service: "web", Image: "nginx:1"},
		"b": {ProjectUID: "uid-shop", ProjectName: "shop", Service: "web", Image: "nginx:1"},
	})
	viewer, err := hub.Subscribe(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	frame := sampleFrame("a", "b", "c")
	for index := range frame.Containers {
		frame.Containers[index].MemoryUsage = 100
		frame.Containers[index].MemoryLimit = 400
	}
	sessions.current().push(frame)

	view := nextView(t, viewer)
	if len(view.Projects) != 2 {
		t.Fatalf("view has %d projects, want shop and the unmapped bucket", len(view.Projects))
	}
	if view.Host.Totals.ContainerCount != 3 || view.Host.Totals.MemoryUsage != 300 {
		t.Fatalf("the host row is %+v, want all three containers", view.Host.Totals)
	}
	if view.Host.MemoryCapacity != 8<<30 {
		t.Fatalf("the host row lost the Engine's capacity: %d", view.Host.MemoryCapacity)
	}
	if !view.Host.Totals.MemoryPercentKnown || view.Host.Totals.MemoryPercent != 25 {
		t.Fatalf("host memory percent is %v (known=%v), want 25 against the summed limits",
			view.Host.Totals.MemoryPercent, view.Host.Totals.MemoryPercentKnown)
	}
	web := view.Projects[0].Services[0]
	if web.Service != "web" || web.Totals.ContainerCount != 2 || web.Containers[0].Image != "nginx:1" {
		t.Fatalf("the web service row is %+v", web)
	}
}
