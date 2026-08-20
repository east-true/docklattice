# Power-cut gate

Status: PASS

Every other gate that removes something removes a process. `SIGKILL` leaves the
page cache intact and the kernel flushes it afterwards, so a harness that kills
an Agent proves the Agent can die - not that anything reached the platter. This
gate removes the power.

A hypervisor is the only thing on hand that can do that. The gate drives a
disposable libvirt guest through four steps: set up a Server, an Agent and a
fixture project inside it; leave a writer running; `virsh destroy`, which cuts
power and performs no shutdown of any kind; boot the guest again and check every
promise the product made before the lights went out.

## Why the writer matters

Cutting power to a guest that finished all its writes a minute earlier mostly
proves the kernel flushed them, which was never in doubt. The interesting moment
is power disappearing *during* a write the API is in the middle of
acknowledging.

So setup leaves a writer running and returns. The writer rewrites one file in a
loop through `PUT /api/v1/projects/{uid}/files`, and after each acknowledgement
appends one line to a journal and `sync`s it by hand. The journal is
deliberately more durable than the thing it measures: if an acknowledgement
survives the cut and the content it acknowledged does not, that is a durability
failure and the gate says so.

Every attempt's content is fully determined by its number, so verification can
reconstruct any attempt from the journal alone rather than trusting a record
written by a process that was killed mid-sentence.

The host waits a jittered 2-10 seconds after setup before cutting, so the cut
does not always land at the same point of the loop.

## What is asserted after the guest boots

| Assertion | What a failure would mean |
| --- | --- |
| both containers restarted themselves | the product cannot come back without an operator, under a restart policy that says it should |
| the Server answers | the database did not survive a power cut |
| the Agent is ACTIVE under its original id | identity state did not survive |
| the project uid is unchanged | the project the operator was working on became a different project |
| the file on disk is the last journaled attempt, or the one after it | an acknowledged write did not survive - the durability promise is false |
| exactly one marker line in the file | a write was torn across the cut |
| acknowledged operations still report success | the Server forgot work it had confirmed |
| the incarnation advanced | the Agent did not notice it had died |
| `AUDIT_CONTINUITY_UNCERTAIN` recorded | the audit stream resumed as if nothing happened |
| the audit page did not shrink | append-only history was rewritten |
| the project secret is absent from recovery evidence | a recovery path leaked `.env` content |

The file assertion accepts the attempt *after* the last journaled one because
the acknowledgement can be in flight when the power goes: that write was
committed and durable, and the process that would have journaled it was killed
before it could. What it does not accept is anything older, which would mean an
acknowledgement outlived its data.

## Inputs and safety boundary

```sh
./scripts/run-power-cut-e2e.sh \
  /absolute/new/evidence-directory \
  dp-vm-clean \
  sha256:<server> sha256:<agent> sha256:<fixture>
```

The VM name must begin with `dp-vm-`; nothing else is acted on. The physical
host's Docker is never touched - every container, network and file this gate
creates lives inside the guest, and the only host-side action is libvirt's.
Guests are made by [`../../scripts/vm-lab-provision.sh`](../../scripts/vm-lab-provision.sh)
and are disposable: destroy and recreate rather than repair.

## Recorded execution

    started_at        2026-08-20T10:39:37Z
    power_cut_at      2026-08-20T10:40:05Z
    booted_at         2026-08-20T10:40:28Z
    finished_at       2026-08-20T10:40:30Z
    cut_delay         9s of jitter after setup
    guest             dp-vm-clean, Ubuntu 24.04, Docker 29.1.3
    release_revision  c6366b83dc31c712b58ace47fe384bffb15a2a32
    server_image_id   sha256:0c05818885eb56673b95608de83bb2b0ea7401ad8ed23c9018809ad87c4de6ee
    agent_image_id    sha256:0d221f24ed5cb744e9b3b785bdbdf738cb3b950827951b4856e09acb9fda99f2

The cut was real, and the guest's own journal is the proof: the boot that was
running ends at `10:39:50` with kernel messages about a veth device and no
shutdown sequence at all, and the next boot begins at `10:40:11`. The fifteen
seconds between that last flushed entry and the recorded cut are journald's
own unflushed tail - lost, as everything unflushed was.

| Assertion | Result |
| --- | --- |
| `containers_restarted_themselves` | PASS |
| `server_answers_after_power_cut` | PASS |
| `agent_returned_active_same_identity` | PASS |
| `project_uid_stable` | PASS |
| `writer_acknowledged_writes` | 7 |
| `durable_write_survived` | attempt 8, acknowledged after the last journal flush |
| `file_not_torn` | PASS |
| `acknowledged_operations_survived` | PASS |
| `incarnation_advanced` | 1 -> 2 |
| `audit_continuity_uncertain_recorded` | PASS |
| `audit_did_not_shrink` | 2 -> 22 |
| `secret_not_leaked` | PASS |

The journal's last acknowledgement was attempt 7 and the file on disk held
attempt 8, whole. That is the case the gate was built to catch: power vanished
between the write being committed and the writer learning about it, and what
the product had already done - staged file, `fsync`, `RENAME_EXCHANGE`,
directory `fsync` - held. An earlier run at the previous revision landed the
same way (journal 6, disk 7).

## What this does not measure

- **One filesystem.** ext4 on virtio, with the guest's default mount options and
  QEMU's default cache mode. A different filesystem, or a host that lies about
  `fsync`, is a different experiment.
- **One write path.** The project file write. The Server database, the Agent
  identity state and the Audit WAL are checked for *consistency* after the cut,
  not for the same in-flight durability property.
- **No repetition.** Two runs, two cuts. This says the durability path holds
  where it was cut, not that it holds at every instruction.
