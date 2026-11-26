# Cloud sheds

A server with the [remote backend](../reference/remote-backend.md) gets its machines from the account service: each shed is a cloud VM with a **lease**, and the account service deletes the machine when the lease ends. The Sheds pane shows these sheds in their own way and lets you extend the lease, with no change to how you create, open or stop them. A server with no cloud sheds looks as before.

Every word on a cloud row is decided once in the shared Rust core (`shed-app`'s `cloud.rs`) and drawn by the app; the Linux and macOS apps show the same text.

## A cloud row

| Part | What it shows |
|---|---|
| Tag | `Cloud`, in its own colour, where a local shed shows its runtime (`vz`, `fc`). |
| Meta line | The size, the status and the time left: `2 vCPU · 8 GB · running · deleted in 5h 10m`. A size with no numbers shows its name. |
| Buttons | The usual terminal, start and stop buttons, and Delete on a stopped shed. **Restart is never offered**: a cloud shed has none. |
| `details` | Opens the [cloud detail](#the-details-panel). |

The time left reads `deleted in 1d 2h`, `deleted in 1h 20m` or `deleted in 20m`. Beside it a row can say:

| Words | Meaning | Drawn |
|---|---|---|
| `deleted in 5h 10m` | More than the warning time is left. | Muted |
| `deleted in 1h 20m` | Within the warning time of the end. The warning time is the lease's own (`warn_s`), 2 hours when the server says none. | Amber |
| `lease ended` | The lease is at or past its end; the machine is being deleted. Start and Stop are not offered. | Red |
| `lease unknown` | The account service has not answered for the machine yet, or its time cannot be read. No extend is offered. | Muted |

## The details panel

`details` opens a **Cloud machine** panel under the row, with no egress section (a cloud shed has no egress profile).

| Fact | Value |
|---|---|
| Image | The image the machine was made from. |
| Created | When the machine was made, in UTC. |
| Run time | `running for 3h 2m` for a running shed; a dash otherwise. |
| Lease end | The time and what is left: `15:40 UTC (deleted in 5h 10m)`. |
| Cap | The latest the lease can ever reach. |

A time on today's date reads `14:30 UTC`; on another day it carries the date, `2026-12-01 10:30 UTC`.

## Extending a lease

A running or stopped shed whose lease is known and has not ended shows an **Extend** line in its details panel when the server supports extending and the lease can be extended. It holds:

- Presets of **+6 h, +12 h, +24 h and +48 h**, as the account service offers them. Each is clipped to the most the cap allows, so near the cap you see fewer and smaller presets (duplicates are dropped).
- A **custom hours** field and an **Extend** button, taking whole hours from 1 to the most the cap allows (the field's placeholder reads `1–114`, for example). Enter in the field extends too. Text that is not a whole number in that range leaves the button disabled.

An extend moves the lease **later by that many hours** from its present end, not from now. A success says `Extended by 6 h; deleted at 15:40 UTC`, and the row's time left updates. The buttons are disabled while an extend runs. The extend belongs to the shed, not to its open panel: closing and reopening the details panel (or leaving the pane) shows it still running and then its outcome, and offers no second extend until it ends. No Extend line shows at the cap, on a lease that has ended, or on a service client's shed.

### When an extend is refused

A refusal from the account service shows fixed words, never the server's own text. The API codes are on the [API reference](../reference/api.md); the same words come from `shed extend` in the CLI.

| Code | Words shown |
|---|---|
| `EXTEND_NOT_ALLOWED` | This cloud shed's lease cannot be extended (a service client's shed, or the account service's policy). |
| `LEASE_AT_CAP` | The extend would take the lease past its cap; extend by fewer hours. |
| `LEASE_ENDED` | This cloud shed's lease has ended; its machine is being deleted. |
| `PERSON_INACTIVE` | The shed's person is no longer active at the account service. |
| `COMPUTE_DELETING` | The account service is deleting this cloud shed's machine (its lease ended, or it was deleted at the service). |
| `COMPUTE_UNAVAILABLE` | The account service could not be reached, or this server has not finished reading its machines; try again later. |

When no server answers at all, the message is the app's own error text. A lost reply is safe to retry: the app sends one request id per extend, so the lease moves once.

## The usage line

Under the heading of each server that has cloud sheds, a line compares your machines with the account service's limits:

```text
2 of 4 machines (1 running, 1 stopped) · organization 7 of 10
```

The first part counts the machines you hold against your limit; the second counts the whole organization. A caller who is not a person of the service (a local admin) sees only `organization 7 of 10`. A server whose usage cannot be read shows no line. The line is read again at once when the server's cloud rows change (a create, a delete, a start or a stop), and otherwise at most every 30 seconds.

## Deleting a cloud shed

**Delete** on a cloud row asks first, with **Delete *name*?** and the words *Deletes the machine and its disk.* Choose **Delete** to go on or **Cancel** (or Esc) to keep it. Delete is offered on a shed that is not running or changing state; stop a running shed first. The question stands only while the shed's current row still offers Delete: if the shed is started elsewhere, or goes, the question closes and nothing is deleted. A local shed deletes at once, as before.

## See also

- [Remote backend](../reference/remote-backend.md): statuses, leases, the account service's part and error codes.
- [API reference](../reference/api.md): the extend call and its `request_id`.
- [IPC](ipc.md): `shed.extend`, `cloud.usage`, the `cloud_row` on `sheds.list` and the test doors for these controls.
