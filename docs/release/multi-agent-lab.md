# Multi-Agent lab

Status: PASS

Every gate before this one drives a single Agent against a single Docker Engine.
That arrangement cannot answer the question this lab exists for: **when one host
dies, stalls, fills up, or restarts its daemon, does anything happen to the
others?**

Each Agent host here is a Docker-in-Docker container with its own `dockerd`, its
own storage, its own container namespace and its own filesystem. The Agent runs
as a container *of that inner daemon*, so self-protection, discovery and Compose
all see exactly what they would see on a separate machine. Cutting a host off
the lab network is a real partition; killing its daemon is a real daemon
restart; killing the host is a real abrupt loss.

## Where it runs, and why not here

Privileged Docker-in-Docker is not isolated from the host kernel. The inner
`dockerd` loads kernel modules into the *host* module table, sets bridge sysctls
that apply to every bridge on the machine, and rewrites host-visible netfilter
state - none of which is undone when the containers are removed.

On this project's own workstation that cost the operator remote access twice,
and a reboot each time. The mechanism was never proved, and the guard in the
harness deliberately does not encode a theory about it. A harness that has twice
taken a working machine off the network does not get to argue about why: it runs
on a disposable `dp-vm-*` guest, or it refuses to start. `LAB_ALLOW_DIND_ON_THIS_HOST=1`
overrides that on a machine the operator is willing to lose.

Provision a guest with
[`../../scripts/vm-lab-provision.sh`](../../scripts/vm-lab-provision.sh).

## Cases

| Case | What it isolates |
| --- | --- |
| `registration` | N Agents register at once, each with a distinct identity and independent capabilities |
| `reconnect-storm` | every Agent loses the Server simultaneously and returns with its identity intact |
| `server-restart` | graceful stop and `SIGKILL`, with N Agents attached |
| `partition-one` | one host is cut off; the others must not notice, and the fleet view must still answer |
| `daemon-restart` | one host's `dockerd` restarts under a live Agent |
| `host-poweroff` | one host is killed outright and brought back |
| `bulk-isolation` | one Agent floods log and stats streams while another does durable work |
| `identity-crossover` | an operation naming one Agent and another Agent's project |
| `operation-flood` | parallel operations per Agent, with per-Agent locks staying independent |
| `catchup-fairness` | unequal backlogs reconnect together; the largest must not be starved |
| `disk-pressure` | one host fills up; the others stay healthy |

Two of these are worth explaining.

**`partition-one`** is the case that found the defect this lab was built to
find. Cutting a single Agent off the network left `GET /api/v1/dashboard`
returning nothing at all, for every Agent, for more than twenty seconds, while
the other two were healthy. A partitioned Agent does not reset its connection -
its packets are dropped - so the per-host heartbeat the dashboard performs hung
until something above it gave up, and the dashboard waited for every host before
answering. The Server's liveness loop already bounded a heartbeat; the
dashboard's did not. The fix and its regression tests are in
`internal/serverapi/dashboard_isolation_test.go`.

**`identity-crossover`** exists because a project UID is
`sha256(agent_id || NUL || canonical working directory)` - it already names its
owner. A crossed pair (Agent A's id, Agent B's project) is therefore a
misconfiguration the Server can detect without asking anyone: an orchestrator
templating the wrong host, a UID pasted from the wrong dashboard row, a client
that cached a project across a re-registration. Executing it would either write
A's content into B's directory or run Compose on a host that does not own the
project. Both are silent cross-host damage.

## Safety

The physical host's Docker is never mutated. Every container the lab creates
carries the run's own label; every inner container lives inside a throwaway dind
host; every project target is checked against the fixture identity the run
derived, and a target that cannot be proved is a failure rather than a guess.
The lab network is pinned to `198.18.0.0/15`, which RFC 2544 reserves for
benchmarking and nothing routes.

## Recorded execution

    started_at        2026-08-20T10:30:10Z
    finished_at       2026-08-20T10:43:48Z
    guest             dp-vm-lab, Linux 6.8.0-137-generic x86_64, Docker 29.1.3
    agents            3
    release_revision  c6366b83dc31c712b58ace47fe384bffb15a2a32
    server_image_id   sha256:0c05818885eb56673b95608de83bb2b0ea7401ad8ed23c9018809ad87c4de6ee
    agent_image_id    sha256:0d221f24ed5cb744e9b3b785bdbdf738cb3b950827951b4856e09acb9fda99f2
    dind_image_id     sha256:12e683a161823b2a839aeea999b9d960e6e1f9a97b1679ad6b441982e2d9cf07

| Assertion | Result |
| --- | --- |
| `fixture_identity_verified` | PASS |
| `registration_agents_distinct` | PASS |
| `registration_capabilities_ready` | PASS |
| `reconnect_storm_all_returned` | PASS |
| `reconnect_storm_identities_stable` | PASS |
| `server_restart_graceful` | PASS |
| `server_restart_sigkill` | PASS |
| `server_restart_identities_preserved` | PASS |
| `partition_one_victim_offline` | PASS |
| `partition_one_others_unaffected` | PASS |
| `partition_one_victim_operation_refused` | 503 |
| `partition_one_victim_recovered` | PASS |
| `daemon_restart_signalled` | PASS |
| `daemon_restart_isolated` | PASS |
| `daemon_restart_recovered` | PASS |
| `daemon_restart_rediscovered` | PASS |
| `host_poweroff_isolated` | PASS |
| `host_poweroff_incarnation_advanced` | 2 -> 3 |
| `host_poweroff_recovered` | PASS |
| `bulk_isolation_control_ops_completed` | PASS |
| `identity_crossover_refused_at_dispatch` | 404 |
| `identity_crossover_orphan_refused` | 404 |
| `identity_crossover_victim_untouched` | PASS |
| `identity_crossover_fleet_healthy` | PASS |
| `operation_flood_no_server_error` | PASS |
| `operation_flood_fleet_stable` | PASS |
| `disk_pressure_reason_reported` | PASS |
| `disk_pressure_others_unaffected` | PASS |
| `disk_pressure_recovered` | PASS |
| `final_invariants` | PASS |

`catchup-fairness` and `bulk-isolation` were re-run afterwards under
strengthened assertions - the originals recorded a stronger claim than they
checked. With backlogs of 6, 12 and 18 container restarts generated while
partitioned, the three Agents' archive heads reached 14, 20 and 28, each fully
acknowledged:

| Assertion | Result |
| --- | --- |
| `catchup_agent_1_ack` | 14 -> 14 (archive head 14, backlog 6 restarts) |
| `catchup_agent_2_ack` | 20 -> 20 (archive head 20, backlog 12 restarts) |
| `catchup_agent_3_ack` | 28 -> 28 (archive head 28, backlog 18 restarts) |
| `catchup_no_cursor_regression` | PASS |
| `catchup_every_backlog_delivered` | PASS |
| `bulk_isolation_durable_cursor_not_regressed` | 0 -> 0 (the bystander does no work by design) |

## What this does not measure

- **One kernel.** A guest kernel panic, a hypervisor-level power cut, and
  anything depending on separate kernel state are outside what dind can show.
  The [power-cut gate](power-cut.md) covers the abrupt-loss case on real
  hardware boundaries; a real `systemctl restart docker` is covered by the
  hardening matrix's `docker-daemon-restart` case, run in a VM.
- **Three Agents.** `LAB_AGENTS` raises it, but the recorded run is three.
- **One run per case.** These are isolation assertions, not statistics.
