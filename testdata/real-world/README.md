# Real-world config corpus

Public, legitimately-sourced Alertmanager configs for stress-testing amcheck on
genuine complexity (nested routes, regex alternations, inhibition, features the
toy fixtures lack). **All from public repositories** — no client or production
data. Regenerate with `./fetch.sh`.

| File | Source |
|------|--------|
| `prometheus-simple.yml` | prometheus/alertmanager `doc/examples/simple.yml` |
| `alertmanager-conf-good.yml` | prometheus/alertmanager `config/testdata/conf.good.yml` |
| `alertmanager-ha.yml` | prometheus/alertmanager `examples/ha/alertmanager.yml` |
| `prometheus-simple-v2.yml` | derived: `simple.yml` with a realistic risky edit (see below) |

## What this corpus found

**Version skew is real.** The first run failed:

```
parse prometheus-simple.yml: yaml: unmarshal errors:
  line 124: field tracing not found in type config.plain
```

amcheck vendors Alertmanager's parser at a pinned version; real configs target
newer versions with newer top-level fields (`tracing`, …). Fix: a lenient
loader (`loadLenient` in `check.go`) that drops unknown *top-level* fields — never
the ones amcheck reasons about (`route`, `receivers`, `inhibit_rules`) — and
reports each drop on stderr. This decouples amcheck from the Alertmanager
release treadmill.

## A realistic regression caught

`prometheus-simple-v2.yml` narrows the service alternation
`service=~"foo1|foo2|baz"` to `service=~"foo1|foo2"` (someone "cleans up" a
matcher). amcheck catches that `{service="baz", severity="critical"}` alerts
silently stop paging `team-X-pager`:

```
amcheck prometheus-simple.yml prometheus-simple-v2.yml --explain
  ✗ ROUTE REGRESSION — 1 alert class loses a receiver
  [1]  team-X-pager is no longer reachable
       witness  { service="baz", severity="critical" }
       old → [team-X-pager]   new → [team-X-mails]
```

The witness `service="baz"` was auto-synthesised from the regex alternation —
no test case authored. Both engines (`operational`, `--mode exhaustive`) agree.

## Try it

```
cd tools/amcheck
for f in testdata/real-world/*.yml; do ./amcheck "$f" "$f"; done   # parse-robustness
./amcheck testdata/real-world/prometheus-simple.yml testdata/real-world/prometheus-simple-v2.yml --explain
```

## Mining real git history

`../../scripts/mine-history.sh <owner/repo> <path>` clones a public repo and
runs amcheck between every consecutive revision of a config file — pointing the
tool at *real changes people actually committed*. It reports regression / safe /
skipped pair counts (skipped = one side did not parse).

Two robustness findings came straight out of this:

1. **Version skew** — the vendored parser rejects newer top-level fields
   (`tracing`). Fixed by `loadLenient` dropping unknown top-level fields.
2. **Notifier noise** — real configs carry `${ENV}` placeholders and secrets in
   notifier configs (`slack_api_url: '${SLACK_WEBHOOK_URL}'`) that fail URL
   validation, even though amcheck only needs the route tree + receiver *names*.
   Fixed by stripping global/templates/notifier content before parsing.

With those fixes amcheck flagged a real change in `elcruzo/LLMetrics`: a commit
renamed `critical-receiver` → `critical-alerts` (a route regression by
definition — the old receiver name is no longer reachable) and added an
inhibition rule. Whether intentional or not, that is precisely the change a
reviewer should confirm.

## Provenance & ethics

Every file here is fetched from a public GitHub repository under its own open
licence. amcheck is never pointed at real clients' or third parties'
production data. To test on *real* configs you control, use your own
Alertmanager git history or your own Grafana Cloud tenant's exported config.
