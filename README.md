# amcheck

A config-safety linter for Prometheus / Grafana **Alertmanager** routing
configs. It diffs two configs (`old`, `new`) and reports every alert class
that would **silently stop reaching a receiver** it used to reach — the
"route regression" that syntax validation (`amtool check-config`) cannot see.

Built on Alertmanager's own `config` + `dispatch` packages, so routing matches
production exactly. Witness alerts are **derived from the routing tree**, so
you don't author any test cases. It reads both native `alertmanager.yml` and
**Prometheus Operator `AlertmanagerConfig` CRDs** (the Kubernetes-native form).

amcheck grew out of research on session types, but you need none of that theory
to use it: it is a self-contained CLI and CI check.

## Usage

```
amcheck <old-config> <new-config> [flags]

  --mode      operational | exhaustive  (default: operational)
  --explain   show old vs new receivers per regression
  --format    text | json               (default: text)

exit 0 = every old route preserved   exit 1 = regression   exit 2 = usage/config error
```

Example (a matcher narrowed from `team="payments"` to `team=~"payments-.*"`):

```
$ amcheck testdata/payments-old.yml testdata/payments-narrowed.yml --explain

  comparing  testdata/payments-old.yml → testdata/payments-narrowed.yml   engine: operational

  ✗ ROUTE REGRESSION — 2 alert classes lose a receiver

  [1]  payments-pager is no longer reachable
       witness  { severity="critical", team="payments" }
       old  → [payments-pager]
       new  → [default]

  [2]  payments-slack is no longer reachable
       witness  { team="payments" }
       ...
  verdict  FAIL  (route preservation violated)           exit 1
```

## Two engines

- **operational** (default, implemented) — derives witness alerts from the
  matchers in the old tree, replays each through Alertmanager's real
  `dispatch`, and diffs the reached receivers. Ground truth, always decidable.
- **exhaustive** (`--mode=exhaustive`; `subtyping` accepted as an alias) —
  decides route-relation containment by exhaustively enumerating canonical
  alert instances over the label values old distinguishes. Complete over that
  domain: if it reports no regression, no alert old can distinguish loses a
  route. Exponential worst case (hence not the default); reports truncation
  rather than under-checking. Validated to find at least every regression the
  operational engine finds (`TestSubtyping_FindsAtLeastOperational`). A
  compositional form (matcher containment without enumeration) is a possible
  future optimisation; this exhaustive form is exact by construction.

## Witness synthesis

Witnesses are synthesised from the matchers along each route path:

- **equality** (`foo="bar"`) pins the value;
- **regex** (`foo=~"..."`) generates a matching member from the parsed regex
  AST, **expanding alternations** — `severity=~"warning|critical"` yields a
  witness for *both* `warning` and `critical`, which is where narrowing bugs
  usually hide;
- **negation** (`!=`, `!~`) gets avoiding fallback values.

Every synthesised value is verified with Alertmanager's own `Matcher.Matches`
before use, so a witness can never mis-fire: synthesis can only *under-cover*,
never produce a false regression.

## Scope & limitations (MVP)

- **Sound, not complete.** A reported regression is real. Coverage is
  *representative, not exhaustive*: a bounded set of witnesses per route, so a
  regression affecting only some members of an unbounded matcher (e.g. a
  `.*`-tail) can still be missed. Absence of a reported regression is a proof
  of route preservation only when coverage is complete.
- When a path cannot be witnessed at all (unsatisfiable constraints, or a
  regex whose language amcheck cannot synthesise a member of), the tool prints
  a coverage note rather than claiming a clean bill of health.
- **Suppression (inhibition + silences):** amcheck flags a *new* mask that
  could mute an alert which still routes (a "still routes, now silently muted"
  regression). Two mechanisms, unified as a *mask*:
  - **inhibition** — a new inhibit rule; reported as *potential* (only fires
    when a matching source alert is present);
  - **silence** — a new declared silence (pass `amtool silence export` JSON via
    `--old-silences` / `--new-silences`); reported with its expiry, since a
    silence is time-boxed.
  Removing/relaxing a mask (more notifications) is not a safety regression and
  is not flagged.
- Grouping is out of scope for the MVP.

## Hands-on lab

`lab/` is a self-contained Docker stack (Prometheus + Alertmanager + Grafana +
a webhook sink) that fires real alerts, so you can operate routing, inhibition,
and silences live — and see amcheck predict a bad config change, then confirm
it against the running alert stream. See [`lab/README.md`](lab/README.md).

## Build & test

```
go build -o amcheck .
go test ./...
```

Requires Go 1.21+. Depends on `github.com/prometheus/alertmanager`.

## Relationship to existing tools

`amtool config routes test` and the proposed batch-test file
([alertmanager#5167](https://github.com/prometheus/alertmanager/issues/5167))
validate **one** config against test cases **you write**. `amcheck` is
complementary: it **diffs two versions** and **auto-generates** the witness
alerts, catching regressions you never wrote a test for. It does not replace
assertion tests, which still express intent `amcheck` cannot infer.

Not affiliated with or endorsed by Grafana Labs or the Prometheus project.
