# Degraded storage recovery

This is the operator procedure for `DEGRADED_STORAGE`. It restates
architecture sections 14.1-14.3 and the values in
`internal/config.V1Defaults` and `internal/diskbudget`; the architecture record
remains authoritative if they ever disagree.

## What DEGRADED_STORAGE means

It is not an outage and not a failure of the Agent. It is a declared state in
which Dockpilot stops creating new durable bytes so that it can still close the
recovery boundary of work already in flight and keep the minimum record needed
to explain what happened.

`DEGRADED_STORAGE` **does not turn permitted capabilities false.** The Agent
keeps reporting Compose and the other allowed capabilities as enabled and
attaches the degraded state as a capability reason. The Server API and the Web
UI preserve that reason and show it as a warning, so operation buttons stay
usable and the remaining storage pressure stays visible.

## Entry and exit

Entry is an OR; exit is an AND. The gap between them is deliberate hysteresis
that prevents flapping around the threshold and immediate re-entry right after
a cleanup.

```
entry (OR)    filesystem free       <  max(1 GiB, 5%)
              Agent state usage     >  state budget (default 2 GiB)

exit  (AND)   filesystem free       >= max(1.2 GiB, 6%)
              Agent state usage     <= 90% of the state budget
```

Recovering to the entry floor is not enough. Free space must reach the higher
exit floor before the state clears.

## Read the reason first

The two causes need different operator actions, so the Agent always
distinguishes them:

```
storage_degraded_reason: FILESYSTEM_FREE_LOW
                       | AGENT_STATE_BUDGET_EXCEEDED
                       | BOTH
```

- **`AGENT_STATE_BUDGET_EXCEEDED`** - Dockpilot's own state under
  `/var/lib/dockpilot` exceeded its budget. Dockpilot's automatic eviction is
  the primary remedy; raising the budget is the deliberate alternative.
- **`FILESYSTEM_FREE_LOW`** - the host filesystem is short of space. This can
  be entirely unrelated to Dockpilot. **If files outside Dockpilot filled the
  disk, the Agent cleaning its own state may not clear the state, and that is
  intended behaviour.** The remedy is host filesystem cleanup. Dockpilot never
  deletes arbitrary host files.
- **`BOTH`** - resolve both; exit requires both conditions.

## What Dockpilot evicts on its own

Before declaring the state, the Agent evicts in an order that discards
irreversible data last:

```
1. abandoned temp/staging files
2. WAL segments the Server has already acknowledged
3. operation results and output tails past retention
4. automatic snapshots beyond the retention count
5. older automatic snapshots, keeping at least one newest snapshot per project
6. unacknowledged WAL, recording AUDIT_GAP(DISK_PRESSURE)
7. nothing further is deleted automatically -> enter DEGRADED_STORAGE
```

**Manual backups are never deleted automatically.** A backup the user created
explicitly is not something Dockpilot removes silently. Deleting one is an
operator action.

Operation output tails are evictable under pressure, but the minimum result
(`operation_id`, `status`, `phase`, `error_code`, `output_truncated:true`) is
always preserved.

## What still works while degraded

This list is authoritative. Do not read a broader "reject new write
operations" phrase elsewhere as overriding it.

**Queries and streams, all allowed**: Docker and Compose queries, file read,
logs, live metrics, Audit sync, operation status and result queries, backup
listing, and manual deletion of an existing backup.

**Mutating operations, allowed**: `compose.up`, `compose.down`,
`compose.start`, `compose.stop`, `compose.restart`, `container.start`,
`container.stop`, `container.restart`, `container.remove`.

These are allowed because they write no new Agent configuration file, need no
automatic snapshot, may themselves be what frees space or restores service, and
can still record minimal operation state and Managed Audit from the 64 MiB
emergency reserve. Blocking the ability to restart a service on a full disk
would make Dockpilot an obstacle to incident response.

Each one must still pass the **Durable Admission Check** before acceptance: is
there room to record the minimal operation record, the Managed Audit record,
and the terminal or interrupted status? If even that room is gone, the
operation is rejected.

**Rejected**: `compose.pull`, file writes, `backup.create`, `backup.restore`,
new manual backups, any change requiring an automatic snapshot, and anything
requiring large staging.

`compose.pull` is rejected because pulling a large image adds data to Docker
storage and worsens the shortage. `compose.up` is *not* transformed to avoid
its image pulls - Dockpilot shows the storage warning and returns the Docker or
Compose result unchanged.

## Procedure

1. **Read the reason** from the host capability warning in the UI or from the
   Agent's capability reason in the API.
2. **If `AGENT_STATE_BUDGET_EXCEEDED`**: list backups for the affected host and
   delete manual backups that are no longer needed. Automatic eviction has
   already removed everything it is permitted to remove, so remaining Dockpilot
   state is either protected (newest snapshot per project, manual backups) or
   durable metadata. Raising `AgentStateMaxBytes` is the alternative and takes
   effect on Agent restart.
3. **If `FILESYSTEM_FREE_LOW`**: free host filesystem space outside Dockpilot.
   Use `docker system df` to see what Docker itself holds. Dockpilot will not
   delete host files for you.
4. **Bring free space past the exit floor**, not merely past the entry floor:
   `max(1.2 GiB, 6%)` free and Agent state at or below 90% of the budget.
5. **Confirm recovery.** The Agent re-evaluates on its own; the capability
   reason clears once both exit conditions hold. No restart is required.

## If Audit gaps were recorded

Step 6 of the eviction order creates `AUDIT_GAP(DISK_PRESSURE)` coverage
entries when unacknowledged WAL had to be dropped. Those gaps are permanent
facts about coverage, not errors to clear: the Server's Coverage Ledger records
the exact range so later queries report honestly that the range is missing
rather than presenting an unbroken history. See
[`recovery.md`](recovery.md) for how coverage is reconciled after the Server
reconnects to the Agent.
