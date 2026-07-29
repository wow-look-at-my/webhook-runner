# Durable batch backlogs

`internal/backlog` + the state port's `/backlog` routes: a named, per-hook list
of opaque item ids that one run fills and later runs drain a slice at a time.

## Not internal/queue — the two halves

`internal/queue` answers **WHEN to run**: enqueue a key, and its dispatcher
starts a run of that hook when the entry comes due (deduped by key, schedulable
with a delay, anti-spammed by MinInterval). That is the right primitive for
"this subject needs another look soon".

`internal/backlog` answers **WHAT IS LEFT** for a run that already exists and
can only afford part of the work. The two are not interchangeable: pr-minder's
hourly reconcile has ~250 open PRs to re-check, and one container per PR is not
a reconcile, it is a stampede — that walk wants one run draining a slice, with
the remainder surviving until the next tick.

## Why it exists here

A hook run is a container that lives for one delivery. "Work I did not get to"
therefore has nowhere to live inside a hook — so every hook that walks a large
fleet builds the same thing on top of the KV store: a cursor string, a rotation
rule, a resume rule, and the off-by-one bugs that come with them. It is also
built badly, unavoidably: **a cursor is a position in a list the next run
re-derives**, so it silently means something different the moment that list
changes.

pr-minder shipped exactly that. Its reconcile tick sliced the first 50
candidates off a stable list and re-checked those 50 every hour, and the log
line said `169 candidate PRs exceed the per-tick cap (50) — reconciling the
first 50; the next tick continues`. The next tick re-derived the same order and
re-checked the same 50. The other 119 — each holding an owed conflict re-check,
a lease, or an arm the tick exists to heal — were never visited at all.

Operator ruling: *"Queue should exist in webhook-runner. This is completely out
of scope for a webhook impl."* So the backlog is a runner primitive, beside the
KV store, the locks, the waits and the spawn verb.

## The contract

| Verb | Route (state socket) | Body | Answers |
|---|---|---|---|
| Push | `POST /backlog/{name}/push` | `{"items":["o/r#1", ...]}` | `{queued, duplicates, dropped, depth}` |
| Take | `POST /backlog/{name}/take` | `{"count":1..1000}` (default 1) | `{items, depth}` |
| Stat | `GET /backlog/{name}` | — | `{name, depth}` |
| List | `GET /backlogs` | — | `[{name, depth}, ...]` |

- **Push is a SET UNION that preserves order.** An item already queued is
  reported as a duplicate and keeps its ORIGINAL position. That is what makes
  the intended usage safe: re-push the entire candidate set every tick — the
  stateless way to say "this is the work that exists" — without the queue
  growing without bound, and without an item at the back starving because
  re-pushes keep moving it to the front.
- **Take REMOVES**, at-most-once. No leases, no acks, no visibility timeouts.
  Consumers of a backlog like this are idempotent reconcilers whose next tick
  re-derives the same work, so a run that dies mid-item loses nothing the next
  push does not restore — and in exchange there is no in-flight state to leak
  and no lease clock to tune.
- **Depth is observable** (per queue and per namespace), so "the backlog is not
  draining" is a number a hook logs and an operator reads, not an inference.
- **The namespace is the hook**, derived from the bearer token exactly like the
  KV routes — never from the URL.
- **Disk-backed**, one `<data-dir>/backlogs/<namespace>.json` per namespace,
  written temp+rename on every mutation (the KV store's persistence shape). A
  queue outliving its process is the entire point: a restart mid-fleet-walk
  must not restart the walk. Unlike KV entries there are no TTLs — take is what
  removes an item, and a queue nobody drains is a visible depth, not silent rot.

## Bounds

Depth 10000 items/queue, 512 bytes/item, 64 queues/namespace, 256 namespaces
(all `backlog.Config` defaults). Overflow is **counted and reported** as
`dropped`, never silently swallowed, and logged server-side — a full backlog
means the consumer is not keeping up, which is exactly the thing the caller
must be able to see. A rejected push (bad name, oversized item) mutates
nothing, not even the namespace it would have created.

## Using it from a hook

Push what exists, take what you can afford this run, log the depth:

```ts
await fetch(`${HOOK_KV_URL}/backlog/reconcile/push`, {
  method: 'POST',
  headers: { authorization: `Bearer ${HOOK_KV_TOKEN}`, 'content-type': 'application/json' },
  body: JSON.stringify({ items: candidates }),
});
const r = await fetch(`${HOOK_KV_URL}/backlog/reconcile/take`, { /* ... */ body: JSON.stringify({ count: 25 }) });
const { items, depth } = await r.json();
```

Deploy-first, like every state-API addition: a hook that calls `/backlog` needs a
runner with these routes. An older runner 404s them, and a hook should treat
404/405 as "primitive unavailable" and degrade loudly rather than wedge.
