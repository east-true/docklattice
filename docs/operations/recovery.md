# Server and Agent identity recovery

This runbook covers loss of Dockpilot's three independent durable boundaries:

- Server Identity State: `identity/server-identity.json`
- Server operational/audit database: `server.db`
- each Agent's state directory, including its stable Agent ID and credential

Stop the affected Dockpilot process before restoring files. Preserve a copy of
every surviving file before recovery. A database and Identity State backup are
a matched trust set only when their `server_identity_id` agrees and the
Identity State archive generation is not behind the database generation.

Dockpilot never guesses an archive rebind or an existing Agent identity.

## Recovery matrix

| Loss | Automatic behavior | Exact operator action |
|---|---|---|
| Nothing; matching identity, generation, and archive ID | Normal reconnect | None |
| Operational DB lost; Identity State preserved | A new archive ID is created at exactly the next durable generation. Existing unexpired, unrevoked Agent credentials remain valid and Agents may automatically rebind. | Restore the DB from a matching backup if recovery of its history is required. Otherwise start the empty DB and allow Agents to reconnect. Do not copy the old archive ACK or cursor into the new archive. |
| Identity State lost; DB preserved | Server boot fails with an identity mismatch. Existing credentials cannot authenticate against the replacement signing key. | Restore the original Identity State backup that matches the DB. If it is unavailable, preserve the DB separately for audit/export, initialize a new Identity State and new DB as a new trust domain, and manually enroll every Agent with operator-issued normal Join Tokens. |
| Identity State and DB both lost | A new Server identity and generation-1 archive are created. Old credentials are rejected. | Prefer restoring both members of one matched backup set. If unavailable, treat the Server as a new installation and manually enroll every Agent with normal Join Tokens. Never treat this as Archive Rebind. |
| Agent state and credential both lost | The Agent has no proof of its old stable identity. A general Join Token cannot select an already registered Agent ID. | Enroll it with a normal Join Token as a new Agent ID. Keep the old row; deletion and ID mutation are forbidden. There is currently no supported retirement CLI, so leave it offline until the supported administrative retirement path is available rather than editing SQLite directly. |
| Agent credential expired but its signed credential and Agent ID survive | Normal authentication fails. Identity-preserving rejoin is allowed only with the exact expired, unrevoked credential proof. | Issue a purpose-bound Rejoin Token for that Agent ID and present it together with the expired credential. A normal Join Token cannot reuse the ID. |
| Credential revoked | Authentication fails and the credential cannot be used as rejoin proof, even after expiry. | Investigate the revocation. If the Agent must return and no supported unrevocation exists, enroll it as a new Agent identity with a normal Join Token; do not reuse the revoked identity. |
| Identity State generation restored below the DB archive generation | Server boot fails with archive rollback detected. | Restore a newer Identity State backup, or restore the DB and Identity State together from a matching backup set. Do not decrement the DB generation, edit the identity generation, or issue a guessed rebind. |

## New archive coverage

Losing the operational DB while preserving Identity State creates a new audit
archive. The old archive's Agent cursor, ACK watermark, and coverage ledger are
not inherited.

On the first Audit Sync for each Agent in the new archive, coverage starts at:

- the Agent WAL floor when retained WAL records exist; or
- the Agent next cursor when its WAL is empty.

The Server records this once as `SERVER_COVERAGE_START`; a database-loss boot
uses reason `SERVER_DATABASE_REINITIALIZED`. Records already removed from the
Agent WAL are not reconstructed or claimed as present. A transport reconnect
inside the same archive does not create another lower bound.

## Fail-closed checks before restart

Do not start normal service when any of these are true:

- the DB `server_identity_id` differs from Identity State;
- the DB archive generation is ahead of Identity State;
- a same-generation archive is being substituted with a different archive ID;
- an Agent presents a credential signed by another Server identity;
- identity-preserving rejoin lacks both the bound token and exact expired
  credential, or that credential is revoked.

## Automated evidence and remaining container E2E

The package recovery matrix exercises real identity files and SQLite files:

- `internal/serverbootstrap`: identity-only loss, DB-only loss, loss of both,
  and an actual older Identity State restore;
- `internal/registration`: state/credential loss cannot claim an existing ID,
  purpose-bound expired-credential rejoin, and revoked-proof rejection;
- `internal/identity`: signature, server identity, expiry, revocation, and
  durable generation behavior;
- `internal/serverstore`: archive coverage reason constraints and immutable
  Agent identities.

The following still require the release-gate real-container matrix:

- replacing Server volumes while real Agent containers remain connected;
- automatic reverse-gRPC reauthentication and Archive Rebind from real Agent
  WAL floors, including the resulting coverage lower bound;
- replacing an Agent state volume and completing normal-token enrollment with
  the actual Container Agent entrypoint;
- expired and revoked credentials over the production HTTPS registration and
  TLS reverse-gRPC endpoints;
- restart/crash timing around SQLite, Identity State, Agent state, and WAL
  fsync boundaries.

## Restoring the Server database

Restoring a Server database backup is a supported recovery. Two consequences are
worth knowing before you do it.

**The archive generation moves forward, never back.** It is minted from the
Server Identity State, which is a separate durability boundary and is not part
of the Audit database backup. A restored database therefore cannot lower it, and
Agents are never offered a generation they have already seen under a different
archive id.

**Audit coverage can be lost, and it is recorded as the Server's loss.** The
restored archive remembers acknowledging less than it had. Agents were not
restored, and released the records they saw acknowledged after the backup was
taken. The range between exists nowhere. The Server records it in the coverage
ledger as `SERVER_CURSOR_REGRESSION`, the Agents reconnect normally, and Audit
continues from there. The reason is recorded as `UNKNOWN`, because from the
Server's side a restore looks the same as other ways a cursor can end up behind
the Agent - it will not claim a cause it cannot establish.

That entry is **not** an Agent gap. The Agents lost nothing; the archive went
backwards. Any view that presents coverage loss should say which of the two it
is - see [`../interface-freeze.md`](../interface-freeze.md) §10 and §11.

Restoring the Server Identity State as well is a different case: the Server then
presents an archive generation below what Agents hold, which they refuse as
`ARCHIVE_ROLLBACK_DETECTED`. Recovering from that needs the generation walked
past what the Agents already hold.
