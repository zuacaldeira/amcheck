#!/usr/bin/env bash
# Fetch the public real-world Alertmanager config corpus. Public sources only.
set -euo pipefail
cd "$(dirname "$0")"

fetch() { curl -fsSL "$2" -o "$1" && echo "  $1 <- $2"; }

echo "fetching public Alertmanager configs…"
fetch prometheus-simple.yml       https://raw.githubusercontent.com/prometheus/alertmanager/main/doc/examples/simple.yml
fetch alertmanager-conf-good.yml  https://raw.githubusercontent.com/prometheus/alertmanager/main/config/testdata/conf.good.yml
fetch alertmanager-ha.yml         https://raw.githubusercontent.com/prometheus/alertmanager/main/examples/ha/alertmanager.yml

# derived: a realistic risky edit (narrow a regex alternation)
sed 's/foo1|foo2|baz/foo1|foo2/' prometheus-simple.yml > prometheus-simple-v2.yml
echo "  prometheus-simple-v2.yml <- derived (service alternation narrowed)"
echo "done."
