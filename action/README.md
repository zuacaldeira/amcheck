# amcheck GitHub Action

Fail a pull request that silently drops an alert route (or adds a new
suppression) to your Alertmanager config. Shift-left for alert routing: catch
the mistake in review, not during an incident.

## Usage

```yaml
- uses: actions/checkout@v4
  with: { fetch-depth: 0 }        # history is needed to diff the base version
- uses: bica-tools/amcheck/action@v1
  with:
    config: alertmanager.yml
```

See [`example-workflow.yml`](example-workflow.yml) for a complete workflow.

## Inputs

| Input | Default | Description |
|-------|---------|-------------|
| `config` | (required) | Path to the Alertmanager config file. |
| `mode` | `operational` | `operational` (fast, sampled) or `exhaustive` (complete). |
| `base-ref` | PR base / `HEAD^` | Git ref to diff against. |
| `old-silences` / `new-silences` | — | `amtool silence export` JSON per side. |
| `comment` | `true` | Post the result as a PR comment (needs `pull-requests: write`). |

## What it does

On a PR that touches the config, it diffs the base version against the head
version with amcheck and:

- writes the findings to the **job summary**;
- adds an `::error::` **annotation** and **fails the check** on a regression
  (exit 1), so it can be a required status;
- optionally posts a **PR comment** with the report.

Exit status mirrors amcheck: `0` safe, `1` regression / new suppression,
non-zero otherwise.

## How it runs

Composite action: sets up Go, builds amcheck from source, and runs it. No Docker
image or external download. (A future release could ship a prebuilt binary to
skip the build.)

Not affiliated with or endorsed by Grafana Labs or the Prometheus project.
