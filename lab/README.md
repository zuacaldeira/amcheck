# Alerting Lab — hands-on Prometheus + Alertmanager + Grafana

A self-contained local stack that fires **real alerts** so you can operate the
alerting ecosystem end to end: routing, inhibition, silences, notifications —
and close the loop with `amcheck` to catch a bad config change *before* you
ship it.

This is a learning environment. Break it, edit configs, re-load, watch what
changes. That muscle memory is the point.

## Prerequisites

- Docker + `docker compose`
- (for the amcheck steps) Go 1.21+ and a built `amcheck` binary:
  ```
  cd ..            # tools/amcheck
  go build -o amcheck .
  cd lab
  ```

## Start it

```
docker compose up -d
docker compose ps
```

| Service | URL | What it's for |
|---|---|---|
| Prometheus | http://localhost:9090/alerts | the alerts firing at the source |
| Alertmanager | http://localhost:9093 | routing, grouping, inhibition, silences |
| Grafana | http://localhost:3000 (anon admin) | the same data, Grafana-side |
| Sink logs | `docker compose logs -f sink` | the notifications **actually delivered** |

The `sink` is a webhook logger: every notification Alertmanager sends shows up
in its logs, tagged by receiver path (`/payments`, `/search`, …). This is your
ground truth for "what actually got delivered."

---

## Walkthrough

### 1. See real alerts firing
Open Prometheus → **Alerts**. Four demo alerts fire immediately (they use
`expr: vector(1)`): two `PaymentsHealth` (warning + critical),
`PaymentsCheckoutErrors` (critical), `SearchLatency` (warning). No broken
service needed — a steady, real alert stream.

### 2. Watch routing
Open Alertmanager (:9093). Alerts are grouped by `[alertname, team]`. Inspect
the routing tree from inside the container with **amtool** (bundled in the
image):
```
docker compose exec alertmanager amtool config routes show --config.file=/etc/alertmanager/alertmanager.yml
docker compose exec alertmanager amtool config routes test --config.file=/etc/alertmanager/alertmanager.yml team=payments severity=critical
```
The second command prints the receiver a given alert routes to — this is the
single-config, single-label test that `amcheck` generalises to a diff.

Confirm delivery in the sink:
```
docker compose logs sink | grep -o '"path":"/[a-z-]*"' | sort | uniq -c
```
You should see `/payments-critical`, `/search`, `/default` — but **not** the
payments *warning* (it's inhibited; next step).

### 3. Watch inhibition mute an alert
The baseline has an inhibit rule: a firing **payments critical** mutes the
**payments warning** of the same `alertname` + `team`. In the Alertmanager UI
the `PaymentsHealth` warning shows as **inhibited/suppressed**, and it never
reaches the sink. Prove it: stop the critical and the warning appears.

*(Edit `prometheus/rules.yml`, comment out the critical `PaymentsHealth`, then
`curl -XPOST http://localhost:9090/-/reload`. Within seconds the warning is no
longer inhibited and the sink receives `/payments`. Put it back afterwards.)*

### 4. Create a silence
Silences are runtime mutes (unlike inhibition, which is config). Add one:
```
docker compose exec alertmanager amtool silence add team=search --duration=1h --comment="lab" --alertmanager.url=http://localhost:9093
docker compose exec alertmanager amtool silence query --alertmanager.url=http://localhost:9093
```
`/search` stops arriving at the sink. Expire it:
`amtool silence expire <id>`.

### 5. The amcheck loop — catch a regression *before* applying it
`alertmanager-v2.yml` is a proposed change that looks harmless:
`team="payments"` narrowed to `team=~"payments-prod"`, plus a new inhibit rule
for `team=search`. **Predict its effect before touching production:**
```
../amcheck alertmanager/alertmanager.yml alertmanager/alertmanager-v2.yml --explain
```
amcheck reports two **route regressions** (`payments`, `payments-critical` now
fall through to `default`) and one **potential suppression** (`team=search`
warnings can now be muted). Exit code 1 — this is your CI gate.

Now *confirm it live*. Apply v2 and reload, then watch the sink:
```
cp alertmanager/alertmanager.yml /tmp/am-backup.yml
cp alertmanager/alertmanager-v2.yml alertmanager/alertmanager.yml
docker compose kill -s SIGHUP alertmanager     # hot-reload
docker compose logs --since=30s sink | grep -o '"path":"/[a-z-]*"' | sort | uniq -c
```
`/payments` and `/payments-critical` stop arriving — exactly what amcheck
predicted. Restore:
```
cp /tmp/am-backup.yml alertmanager/alertmanager.yml
docker compose kill -s SIGHUP alertmanager
```

You just used the tool the way it's meant to be used: **predict statically,
verify against a real alert stream.**

## Stop it

```
docker compose down
```

---

## Where to go deeper (the ecosystem, in order)

1. **Routing** — matchers, `continue`, `group_by`, the tree walk. Read the
   [routing docs](https://prometheus.io/docs/alerting/latest/configuration/#route)
   and the `dispatch` package in `prometheus/alertmanager` (that's the code
   `amcheck` reuses).
2. **Inhibition & silences** — the two suppression mechanisms; know exactly how
   they differ from routing.
3. **Grouping & notification pipeline** — `group_wait` / `group_interval` /
   `repeat_interval`; the dedup → group → inhibit → silence → notify pipeline.
4. **Grafana Alerting** — how Grafana-managed alert rules and notification
   policies map onto these same concepts.
5. **HA & at-scale** — Alertmanager gossip/clustering, Mimir's ruler; this is
   the "distributed systems" the job cares about.

Each concept has a direct `amcheck` analogue (routing = the branch type,
inhibition = suppression check). Learning one reinforces the other.

*Not affiliated with or endorsed by Grafana Labs or the Prometheus project.*
