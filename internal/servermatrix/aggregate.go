package servermatrix

import (
	"sort"
	"time"
)

// Docker's health vocabulary, plus the empty string a container with no
// healthcheck reports. Anything unrecognised is treated as no information
// rather than as a new severity, because guessing where an unknown status
// ranks is worse than saying nothing about it.
const (
	healthHealthy   = "healthy"
	healthStarting  = "starting"
	healthUnhealthy = "unhealthy"
	healthNone      = "none"
)

// healthRank orders the three statuses a container can actually report. It
// deliberately has no entry for "no healthcheck", because that is not a
// severity: see Aggregate.Health.
func healthRank(status string) int {
	switch status {
	case healthUnhealthy:
		return 3
	case healthStarting:
		return 2
	case healthHealthy:
		return 1
	default:
		return 0
	}
}

// reportsHealth says whether a container is answering the health question at
// all. An unrecognised status counts as not answering: a Docker release that
// adds a status must not have it silently ranked as a severity this code
// invented.
func reportsHealth(status string) bool {
	switch status {
	case healthUnhealthy, healthStarting, healthHealthy:
		return true
	default:
		return false
	}
}

// Aggregate is what a group of containers adds up to. Every level uses it -
// host, project, service - because they are all projections of the same
// container samples in the same frame, and there is never a second measurement
// of the same thing to disagree with the first.
//
// Not every metric aggregates by adding. The fields that do not say so.
type Aggregate struct {
	// ContainerCount is how many containers this row covers, and PendingCount
	// how many of those have not reported a sample yet. A row whose numbers
	// come from fewer containers than it covers is saying so with these two.
	ContainerCount uint32
	PendingCount   uint32

	CPUPercent  float64
	MemoryUsage uint64
	NetworkRX   uint64
	NetworkTX   uint64
	BlockRead   uint64
	BlockWrite  uint64
	Restarts    uint64

	// MemoryLimit is the sum of the members' limits, and is only a number when
	// every member has one. MemoryLimitUnbounded means at least one member runs
	// without a limit, which is not a large number - it is a different kind of
	// answer, and is presented as one. A limit labelled "partial" would still
	// be read as a limit and divided into.
	MemoryLimit          uint64
	MemoryLimitUnbounded bool
	// MemoryPercent is withheld rather than approximated when the limit is
	// unbounded or unknown, which is what MemoryPercentKnown reports.
	MemoryPercent      float64
	MemoryPercentKnown bool

	// Health is the worst status the members actually reported, and is exactly
	// one of: "unhealthy" if any member is unhealthy; "starting" if none is
	// unhealthy and any is starting; "healthy" if every member that has a
	// healthcheck reports healthy; "none" if the members exist and not one of
	// them has a healthcheck; and empty when there is nothing to report at all,
	// which is a row whose every member is still pending.
	//
	// A container without a healthcheck is not a bad health status - it is the
	// absence of one, which is a difference in what can be observed rather than
	// in how worrying the answer is. Ranking it against healthy on one scale
	// would force the row to either overstate its confidence or understate a
	// real healthy result, so it is counted instead, in HealthUnreported. A row
	// reading "healthy" with HealthUnreported above zero is saying precisely
	// what it knows: every container that answers is healthy, and this many did
	// not answer.
	Health string
	// HealthUnreported counts members with no healthcheck, or with a status
	// this build does not recognise. Pending members are not counted here; they
	// are in PendingCount, because not having reported yet and not having a
	// healthcheck are different states.
	HealthUnreported uint32
	// Uptime is the youngest member's, because a service is only as old as its
	// newest container. UptimeKnown is false when no member has reported yet.
	Uptime      time.Duration
	UptimeKnown bool
}

// aggregate sums one group of container rows.
//
// Pending members contribute no numbers, because they have none: a container
// that has not reported cannot be counted as idle. They are still counted, in
// ContainerCount and PendingCount, so the row says what it is missing.
func aggregate(rows []ContainerRow) Aggregate {
	total := Aggregate{ContainerCount: uint32(len(rows))}
	sampled := 0
	for _, row := range rows {
		if row.Pending {
			total.PendingCount++
			continue
		}
		sampled++
		sample := row.Sample
		total.CPUPercent += sample.CPUPercent
		total.MemoryUsage += sample.MemoryUsage
		total.NetworkRX += sample.NetworkRX
		total.NetworkTX += sample.NetworkTX
		total.BlockRead += sample.BlockRead
		total.BlockWrite += sample.BlockWrite
		total.Restarts += sample.RestartCount

		if row.MemoryLimitUnbounded {
			total.MemoryLimitUnbounded = true
		} else {
			total.MemoryLimit += sample.MemoryLimit
		}
		if reportsHealth(sample.Health) {
			if healthRank(sample.Health) > healthRank(total.Health) {
				total.Health = sample.Health
			}
		} else {
			total.HealthUnreported++
		}
		if !total.UptimeKnown || sample.Uptime < total.Uptime {
			total.Uptime, total.UptimeKnown = sample.Uptime, true
		}
	}
	if sampled > 0 && total.Health == "" {
		// Members exist and not one of them has a healthcheck. That is a real
		// answer about the configuration, and a different one from a row that
		// has nothing to report because nothing has reported yet.
		total.Health = healthNone
	}
	if total.MemoryLimitUnbounded {
		total.MemoryLimit = 0
	}
	if !total.MemoryLimitUnbounded && total.MemoryLimit > 0 {
		total.MemoryPercent = float64(total.MemoryUsage) / float64(total.MemoryLimit) * 100
		total.MemoryPercentKnown = true
	}
	return total
}

// ServiceRow is one Compose service, or the containers in a project that carry
// no service label. Unmapped says which.
type ServiceRow struct {
	Service    string
	Unmapped   bool
	Totals     Aggregate
	Containers []ContainerRow
}

// ProjectRow is one Compose project. A project Dockpilot does not manage still
// appears - somebody else's stack on this host is part of what the host is
// doing - and so does the bucket for containers belonging to no project at all,
// which is what Unmapped marks.
type ProjectRow struct {
	ProjectUID  string
	ProjectName string
	Unmapped    bool
	Totals      Aggregate
	Services    []ServiceRow
}

// groupProjects builds the project → service → container tree from one frame's
// rows.
//
// Projects are keyed by UID where there is one and by name otherwise. The
// second case is not a fallback for missing data: a Compose project on this
// host that Dockpilot has never been told to manage has real labels and no UID,
// and grouping it by name shows it as the project it is rather than dumping it
// in with unrelated containers.
func groupProjects(rows []ContainerRow) []ProjectRow {
	type projectGroup struct {
		row      ProjectRow
		services map[string][]ContainerRow
		order    []string
	}
	groups := make(map[string]*projectGroup)
	var order []string
	for _, row := range rows {
		key, project := projectKey(row)
		group, present := groups[key]
		if !present {
			group = &projectGroup{row: project, services: make(map[string][]ContainerRow)}
			groups[key] = group
			order = append(order, key)
		}
		if _, seen := group.services[row.Service]; !seen {
			group.order = append(group.order, row.Service)
		}
		group.services[row.Service] = append(group.services[row.Service], row)
	}

	projects := make([]ProjectRow, 0, len(groups))
	for _, key := range order {
		group := groups[key]
		services := make([]ServiceRow, 0, len(group.services))
		var members []ContainerRow
		for _, name := range group.order {
			containers := group.services[name]
			members = append(members, containers...)
			services = append(services, ServiceRow{
				Service: name, Unmapped: name == "", Totals: aggregate(containers), Containers: containers,
			})
		}
		sortServiceRows(services)
		group.row.Services = services
		group.row.Totals = aggregate(members)
		projects = append(projects, group.row)
	}
	sortProjectRows(projects)
	return projects
}

func projectKey(row ContainerRow) (string, ProjectRow) {
	switch {
	case row.ProjectUID != "":
		return "uid:" + row.ProjectUID, ProjectRow{ProjectUID: row.ProjectUID, ProjectName: row.ProjectName}
	case row.ProjectName != "":
		return "name:" + row.ProjectName, ProjectRow{ProjectName: row.ProjectName}
	default:
		return "", ProjectRow{Unmapped: true}
	}
}

// sortProjectRows puts the containers belonging to no project last. They are
// not a project and must not sort as one, but they are still shown.
func sortProjectRows(rows []ProjectRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Unmapped != rows[j].Unmapped {
			return rows[j].Unmapped
		}
		if rows[i].ProjectName != rows[j].ProjectName {
			return rows[i].ProjectName < rows[j].ProjectName
		}
		return rows[i].ProjectUID < rows[j].ProjectUID
	})
}

func sortServiceRows(rows []ServiceRow) {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Unmapped != rows[j].Unmapped {
			return rows[j].Unmapped
		}
		return rows[i].Service < rows[j].Service
	})
}

// markMemoryLimit decides whether a container's reported limit is a limit.
//
// Docker reports the machine's memory as the limit for a container that has
// none, so the number alone cannot be read as one. A limit at or above what the
// Engine says the host has does not bound anything, and zero is not a limit
// either. This is the only place that judgement is made; every aggregate above
// reads the flag rather than the number.
func markMemoryLimit(row *ContainerRow, memoryCapacity uint64) {
	if row.Pending {
		return
	}
	limit := row.Sample.MemoryLimit
	if limit == 0 || (memoryCapacity > 0 && limit >= memoryCapacity) {
		row.MemoryLimitUnbounded = true
		return
	}
	row.MemoryPercent = float64(row.Sample.MemoryUsage) / float64(limit) * 100
	row.MemoryPercentKnown = true
}
