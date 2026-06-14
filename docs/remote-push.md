# Remote push: getting the fleet to your phone

zdev's daemon already knows when an agent is waiting, stuck, or dead. The
*away-from-desk* hook turns that into a push notification over a channel
**you** own — no network code in the daemon.

## How it hangs off the daemon

`zdevd` resolves a notification backend once at startup. When
`ZDEV_NOTIFY_CMD` is set, that command becomes the backend: on every fleet
tier-escalation or death, the daemon runs

```
sh -c "$ZDEV_NOTIFY_CMD"
```

under a **~1.5s deadline** (SIGKILL on expiry), with the payload in the
environment:

| env var               | meaning                                                        |
|-----------------------|----------------------------------------------------------------|
| `ZDEV_NOTIFY_PROJECT` | the digest leader's project name                               |
| `ZDEV_NOTIFY_MSG`     | the human message (e.g. `still waiting (5m) · 2 more waiting`)  |
| `ZDEV_NOTIFY_SOUND`   | macOS sound name (ignored by the push adapters)                |
| `ZDEV_NOTIFY_KIND`    | wait cost-class: `""`, `permission`, `decision`, or `dead`     |
| `ZDEV_NOTIFY_AGE`     | the leader's wait age in seconds at fire time                  |

The daemon never learns what the command does — that's the deliberate
non-feature that keeps the 5-dep / zero-network ethos intact. The adapters
in `bin/` are the operator's one-liner, made reusable.

Because the daemon already collapses a burst of crossings into **one**
notification per pass, an adapter wired directly to `ZDEV_NOTIFY_CMD`
already behaves well. The digest spooler (below) extends that coalescing
across a *time window*, which is the real off-desk win.

> **Budget rule for any custom adapter:** finish well under 1.5s and always
> exit 0. A hung or failing notifier must never stall the hub loop. The
> bundled adapters bound `curl` to ~1.2s and swallow every error.

## Backends

Config samples live in `config/notify-remote.env.example`. Source the block
you want from your shell rc, a private env file, or a systemd
`EnvironmentFile=` (keep it `0600` — these hold tokens).

### ntfy

```sh
export ZDEV_NTFY_URL="https://ntfy.example.com/zdev-fleet"
export ZDEV_NTFY_TOKEN="tk_xxx"
export ZDEV_NOTIFY_CMD="zdev-notify-ntfy"
```

> ### ⚠️ Never use a public ntfy topic
> An ntfy topic name is the *only* access control on `ntfy.sh`. The push
> body carries your **project names**, and the daemon's messages can carry
> branch context. A topic on the public `ntfy.sh` server is
> **world-readable AND world-writable** to anyone who guesses the name —
> that leaks your fleet's project/branch names to the world and lets
> strangers spam your phone. **Always** point `ZDEV_NTFY_URL` at a
> self-hosted server with auth, or at minimum an access-token-protected
> *reserved* topic. The adapter **fails closed**: with no `ZDEV_NTFY_URL`
> set it sends nothing (rather than fall back to a guessable default), so a
> misconfiguration can't leak.

Optional `ZDEV_NTFY_USERPW="user:pass"` for basic-auth servers;
`ZDEV_NTFY_TIMEOUT` (default `1.2`) tunes the curl budget.

### Pushover

```sh
export ZDEV_PUSHOVER_TOKEN="<application token>"
export ZDEV_PUSHOVER_USER="<user or group key>"
export ZDEV_NOTIFY_CMD="zdev-notify-pushover"
```

Pushover is inherently authenticated — delivery needs **both** the
application token and your user/group key, over HTTPS to
`api.pushover.net`. There's no anonymous mode to leak through; just keep
the credentials out of shared dotfiles. Optional `ZDEV_PUSHOVER_DEVICE`
targets a single device.

## KIND → priority, AGE → urgency

Both adapters map the wait's cost-class to the backend's priority and the
wait *age* to an urgency escalation. The 15m STUCK tier has no distinct
`KIND` — the daemon signals it purely through `ZDEV_NOTIFY_AGE >= 900`, so
the adapters escalate on age.

| `ZDEV_NOTIFY_KIND` | ntfy priority / tags          | Pushover priority / sound |
|--------------------|-------------------------------|---------------------------|
| `dead`             | 5 (max) · skull,rotating_light| 1 (high) · siren          |
| `permission`       | 4 (high) · warning,lock       | 1 (high) · pushover       |
| `decision`         | 4 (high) · speech_balloon     | 0 (normal) · pushover     |
| `""` (unknown)     | 3 (default) · bell            | 0 (normal) · pushover     |
| `AGE >= 900s`      | → 5 (max), +hourglass         | → 1 (high)                |

Pushover **emergency** priority (2) is deliberately *not* used: it requires
`retry`/`expire` params and forces an explicit ack on the phone — push
fatigue waiting to happen for a fleet babysitter. If you want it for deaths,
wrap the adapter and pass `priority=2` yourself.

## The fleet digest (the durable wedge)

Per-session push parity is conceded to Remote Control on purpose. What zdev
does well off-desk is the **fleet digest**: instead of one push per tier
crossing, coalesce a window of crossings into a single push —

```
3 agents waiting, oldest 12m (proj-a, proj-b, proj-c)
```

`zdev-notify-digest` is a stateful wrapper that sits **in front of** a real
backend:

```sh
export ZDEV_DIGEST_BACKEND="zdev-notify-ntfy"   # or zdev-notify-pushover, or a full command
export ZDEV_DIGEST_WINDOW=300                   # cadence seconds (default 5m)
export ZDEV_NOTIFY_CMD="zdev-notify-digest"
```

It appends each event to a spool file, and emits at most one push per
`ZDEV_DIGEST_WINDOW`, carrying the count of distinct waiting projects, the
oldest wait age, and the project names — then clears the spool. It computes
"oldest" from each wait's *start* time (`now − age`), so the age stays
honest as the window fills. The very first event arms the window silently
so the first push you get is already a digest, not a singleton.

**Two classes pierce the window and push immediately:**

- `KIND == dead` — an agent died; a 3am death can't sit in a 5m buffer.
- `AGE >= 900s` — the 15m STUCK tier (tune via `ZDEV_DIGEST_PIERCE_AGE`).

Everything is script-side: a spool file plus a last-emit timestamp under
`${TMPDIR}/zdev-digest` (override with `ZDEV_DIGEST_DIR`). No daemon change.

### Digest vs. per-event — which to wire

| | per-event (adapter direct) | digest (spooler in front) |
|-|----------------------------|---------------------------|
| Latency | immediate on every crossing | up to one window | 
| Volume  | one push per crossing burst | ≤ one push per window |
| Best for| small fleets, at-desk backup| many agents, off-desk |
| Urgency | always immediate           | death + 15m STUCK still immediate |

Rule of thumb: wire the **digest** when you're babysitting more than a
couple of agents or genuinely away — it's the difference between a buzzing
pocket and one glance that says "3 waiting, oldest 12m". Wire an adapter
**directly** when you want every crossing and the volume is low.

## Testing

`scripts/test-notify-adapters.sh` drives each adapter with synthetic
`ZDEV_NOTIFY_*` env, stubs `curl` so nothing leaves the machine, and asserts
the outbound URL/payload/priority shape, the fail-closed security behavior,
and the digest's coalesce-vs-pierce semantics. It runs in CI alongside the
opencode/codex adapter contracts. Run it locally with:

```sh
bash scripts/test-notify-adapters.sh
```
