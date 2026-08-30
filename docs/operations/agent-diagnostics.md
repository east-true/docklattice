# Agent diagnostics

The Agent writes bounded structured records to stderr. In the documented
Container deployment, inspect them with:

```sh
docker logs --tail 200 docklattice-agent
```

Each record is one line with `time`, `level`, `component=agent`, and `event`.
Values are quoted so spaces remain unambiguous. A record is capped at 1 KiB;
newlines are flattened, common `token=`, `credential=`, `authorization=`,
`secret=`, and Bearer values are redacted, and repeating problems of the same
event type are emitted at most once per minute.

## Events

| Event | Meaning |
|---|---|
| `boot_started`, `boot_ready`, `boot_failed` | Agent state, WAL, Docker, and product handlers are being assembled, became ready, or failed before readiness. |
| `registration_started`, `registration_complete` | A new or expired-credential Agent entered and completed the explicit enrollment path. |
| `credential_loaded` | A restart loaded its durable credential without reading a Join Token. |
| `credential_renewal_started`, `credential_renewal_complete` | The 50%-remaining renewal path began or committed. |
| `connection_maintenance_started`, `connection_maintenance_stopped` | The outbound Server reconnect loop started or stopped. |
| `connection_failed`, `connection_established`, `connection_ended` | A dial/handshake failed, a session was established, or an established session ended. |
| `docker_unavailable`, `docker_probe_failed`, `docker_inventory_failed` | The local Docker Engine could not be opened, probed, or listed. |
| `self_identification_failed` | The Agent could not prove which Container is itself and disabled mutations fail-closed. |
| `docker_ready` | Docker probing and Agent self-identification succeeded. |
| `docker_snapshot_failed`, `docker_event_stream_ended` | Observed-Audit reconciliation or the Docker event stream failed and will be retried. |
| `discovery_scan_failed` | A discovery scan published only its verified partial result or failed before publication. Inspect the host's `project_scan` stop reason and last scanned path; unrelated Docker inventory, logs, and metrics remain available after a published partial scan. |
| `audit_archive_rebound` | The Agent durably accepted a strictly newer Server Audit Archive generation. |
| `audit_archive_refused` | The Server announced an unsafe Archive identity or generation. `ARCHIVE_ROLLBACK_DETECTED` includes the bound and presented generations. |
| `shutdown_started`, `shutdown_complete`, `shutdown_failed` | Ordered Agent shutdown began, completed, or failed. |

## Archive rollback

If the Server database and Identity State were restored together, the Agent may
hold a newer Archive generation than the Server presents. A representative
record is:

```text
level=WARN component=agent event=audit_archive_refused presented_generation="2" bound_generation="3" error="agentruntime: bind announced Archive: ARCHIVE_ROLLBACK_DETECTED: bound generation 3, presented generation 2"
```

Do not delete Agent state or edit either generation. Follow the
[identity recovery runbook](recovery.md), restore a matching newer Server pair,
or advance the Server trust state through the documented recovery procedure.

Diagnostics are an operator signal, not an Audit archive. They are not parsed
by DockLattice, synchronized to the Server, or retained beyond the Container log
driver policy.
