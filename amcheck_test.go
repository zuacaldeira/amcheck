package main

import (
	"strings"
	"testing"

	"github.com/prometheus/alertmanager/dispatch"
)

func mustLoad(t *testing.T, path string) *dispatch.Route {
	t.Helper()
	r, err := loadRoute(path)
	if err != nil {
		t.Fatalf("load %s: %v", path, err)
	}
	return r
}

func regressionReceivers(t *testing.T, oldPath, newPath string) map[string]Regression {
	t.Helper()
	regs, _ := diff(mustLoad(t, oldPath), mustLoad(t, newPath))
	got := map[string]Regression{}
	for _, r := range regs {
		got[r.Receiver] = r
	}
	return got
}

func TestDiff_NarrowedMatcher_IsRegression(t *testing.T) {
	got := regressionReceivers(t, "testdata/payments-old.yml", "testdata/payments-narrowed.yml")
	for _, want := range []string{"payments-pager", "payments-slack"} {
		if _, ok := got[want]; !ok {
			t.Errorf("expected regression for receiver %q, got %v", want, got)
		}
	}
	if len(got) != 2 {
		t.Errorf("expected exactly 2 regressions, got %d: %v", len(got), got)
	}
}

func TestDiff_AddedTeam_IsSafe(t *testing.T) {
	if got := regressionReceivers(t, "testdata/payments-old.yml", "testdata/payments-add-team.yml"); len(got) != 0 {
		t.Errorf("adding a sibling route must preserve all old routes, got %v", got)
	}
}

func TestDiff_Identical_IsSafe(t *testing.T) {
	if got := regressionReceivers(t, "testdata/payments-old.yml", "testdata/payments-old.yml"); len(got) != 0 {
		t.Errorf("identical configs must have no regressions, got %v", got)
	}
}

// The regex hardening: an alternation narrowed from "payments|billing" to
// "payments" drops the billing route — invisible to equality-only witnessing.
// The synthesised witness for the "billing" alternative must catch it.
func TestDiff_RegexAlternationNarrowed_IsRegression(t *testing.T) {
	got := regressionReceivers(t, "testdata/regex-old.yml", "testdata/regex-narrowed.yml")
	for _, want := range []string{"finance-slack", "sev-slack"} {
		r, ok := got[want]
		if !ok {
			t.Fatalf("expected regression for receiver %q (dropped alternation branch), got %v", want, got)
		}
		t.Logf("caught %s via witness %s", want, witnessString(r.Witness))
	}
}

// Inhibition awareness: the route is unchanged, but new.yml adds an inhibit
// rule that mutes severity="warning" when a matching critical fires. The
// warning alert still routes to warn-slack — a pure route diff sees nothing —
// but amcheck must flag it as a NEW potential suppression.
func TestNewSuppressions_AddedInhibitRule(t *testing.T) {
	oldCfg, err := loadConfig("testdata/inhibit-old.yml")
	if err != nil {
		t.Fatal(err)
	}
	newCfg, err := loadConfig("testdata/inhibit-new.yml")
	if err != nil {
		t.Fatal(err)
	}
	// Route is unchanged -> no route regression.
	if regs, _ := diff(oldCfg.Route, newCfg.Route); len(regs) != 0 {
		t.Fatalf("route is unchanged; expected 0 route regressions, got %v", regs)
	}
	// But a new inhibition rule can mute the warning alert.
	supps := newSuppressions(oldCfg, newCfg)
	if len(supps) == 0 {
		t.Fatal("expected a potential-suppression finding for the new inhibit rule, got none")
	}
	found := false
	for _, s := range supps {
		if s.Witness["severity"] == "warning" {
			found = true
			if s.Mechanism != "inhibition" {
				t.Errorf("expected mechanism=inhibition, got %q", s.Mechanism)
			}
			t.Logf("caught suppression of %s by %s", witnessString(s.Witness), s.Detail)
		}
	}
	if !found {
		t.Errorf("expected suppression witness with severity=warning, got %v", supps)
	}
	// Adding no inhibition (old vs old) must not flag anything.
	if s := newSuppressions(oldCfg, oldCfg); len(s) != 0 {
		t.Errorf("unchanged inhibition must not be flagged, got %v", s)
	}
}

// Witness enrichment: the route only branches on team, but the new inhibit
// rule targets severity=warning too. amcheck must enrich the route witness
// with the rule's target labels to catch the suppression.
func TestNewSuppressions_EnrichedWithInhibitTargetLabels(t *testing.T) {
	oldCfg, err := loadConfig("testdata/inhibit-enrich-old.yml")
	if err != nil {
		t.Fatal(err)
	}
	newCfg, err := loadConfig("testdata/inhibit-enrich-new.yml")
	if err != nil {
		t.Fatal(err)
	}
	supps := newSuppressions(oldCfg, newCfg)
	if len(supps) != 1 {
		t.Fatalf("expected exactly 1 suppression, got %d: %v", len(supps), supps)
	}
	w := supps[0].Witness
	if w["team"] != "search" || w["severity"] != "warning" {
		t.Errorf("expected enriched witness {team=search, severity=warning}, got %s", witnessString(w))
	}
}

// Agreement / soundness: the exhaustive subtyping engine must find at least
// every route regression the sampled operational engine finds, across all
// route testdata pairs. Equivalently: if subtyping reports SAFE, operational
// reports SAFE.
func TestSubtyping_FindsAtLeastOperational(t *testing.T) {
	pairs := [][2]string{
		{"testdata/payments-old.yml", "testdata/payments-narrowed.yml"},
		{"testdata/payments-old.yml", "testdata/payments-add-team.yml"},
		{"testdata/payments-old.yml", "testdata/payments-old.yml"},
		{"testdata/regex-old.yml", "testdata/regex-narrowed.yml"},
		{"lab/alertmanager/alertmanager.yml", "lab/alertmanager/alertmanager-v2.yml"},
	}
	for _, p := range pairs {
		t.Run(p[0]+"→"+p[1], func(t *testing.T) {
			old := mustLoad(t, p[0])
			new := mustLoad(t, p[1])
			opRegs, _ := diff(old, new)
			subRegs, truncated := subtypingDecision(old, new)
			if truncated {
				t.Fatalf("subtyping search truncated for %v — raise cap or shrink fixture", p)
			}
			sub := map[string]bool{}
			for _, r := range subRegs {
				sub[r.Receiver] = true
			}
			for _, r := range opRegs {
				if !sub[r.Receiver] {
					t.Errorf("operational found regression on %q that exhaustive subtyping missed — subtyping is unsound",
						r.Receiver)
				}
			}
		})
	}
}

// Reading A: a declared silence (added on the new side) that mutes an alert
// old still delivers must be flagged, with mechanism "silence".
func TestNewSuppressions_DeclaredSilence(t *testing.T) {
	// Same routing on both sides; new attaches a silence for team=search.
	oldCfg, err := loadConfigWithSilences("testdata/inhibit-enrich-old.yml", "")
	if err != nil {
		t.Fatal(err)
	}
	newCfg, err := loadConfigWithSilences("testdata/inhibit-enrich-old.yml", "testdata/silence-search.json")
	if err != nil {
		t.Fatal(err)
	}
	// Route unchanged → no route regression.
	if regs, _ := diff(oldCfg.Route, newCfg.Route); len(regs) != 0 {
		t.Fatalf("route unchanged; expected 0 route regressions, got %v", regs)
	}
	supps := newSuppressions(oldCfg, newCfg)
	if len(supps) != 1 {
		t.Fatalf("expected exactly 1 silence suppression, got %d: %v", len(supps), supps)
	}
	if supps[0].Mechanism != "silence" {
		t.Errorf("expected mechanism=silence, got %q", supps[0].Mechanism)
	}
	if supps[0].Witness["team"] != "search" {
		t.Errorf("expected witness team=search, got %s", witnessString(supps[0].Witness))
	}
	// The same silence present on BOTH sides is not a new suppression.
	both, err := loadConfigWithSilences("testdata/inhibit-enrich-old.yml", "testdata/silence-search.json")
	if err != nil {
		t.Fatal(err)
	}
	if s := newSuppressions(both, both); len(s) != 0 {
		t.Errorf("unchanged silence must not be flagged, got %v", s)
	}
}

// Prometheus Operator AlertmanagerConfig CRDs (camelCase fields, structured
// matchers) are transformed to the native shape; a narrowing edit expressed in
// CRD form must be caught just like a native one.
func TestDiff_AlertmanagerConfigCRD(t *testing.T) {
	old := mustLoad(t, "testdata/crd-old.yml")
	new := mustLoad(t, "testdata/crd-new.yml")
	regs, _ := diff(old, new)
	got := map[string]bool{}
	for _, r := range regs {
		got[r.Receiver] = true
	}
	for _, want := range []string{"payments", "payments-pager"} {
		if !got[want] {
			t.Errorf("expected CRD route regression on %q, got %v", want, regs)
		}
	}
}

func TestLoadConfig_RejectsUnrenderedTemplate(t *testing.T) {
	_, err := loadConfig("testdata/unrendered-template.yml")
	if err == nil || !strings.Contains(err.Error(), "template") {
		t.Fatalf("expected an unrendered-template rejection, got: %v", err)
	}
}

func TestLoadSilences_ParsesExportFormat(t *testing.T) {
	sils, err := loadSilences("testdata/silence-search.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(sils) != 1 || len(sils[0].Matchers) != 1 {
		t.Fatalf("expected 1 silence with 1 matcher, got %v", sils)
	}
	if !sils[0].matches(Witness{"team": "search"}) {
		t.Error("silence should match team=search")
	}
	if sils[0].matches(Witness{"team": "payments"}) {
		t.Error("silence should not match team=payments")
	}
}

func TestSynthValues_ExpandsAlternation(t *testing.T) {
	vals, ok := synthValues("warning|critical")
	if !ok {
		t.Fatal("failed to synthesise values for alternation")
	}
	set := map[string]bool{}
	for _, v := range vals {
		set[v] = true
	}
	if !set["warning"] || !set["critical"] {
		t.Errorf("expected both alternation branches, got %v", vals)
	}
}

func TestRun_ExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"regression", []string{"testdata/payments-old.yml", "testdata/payments-narrowed.yml"}, 1},
		{"safe", []string{"testdata/payments-old.yml", "testdata/payments-add-team.yml"}, 0},
		{"flags-after-positionals", []string{"testdata/payments-old.yml", "testdata/payments-narrowed.yml", "--explain"}, 1},
		{"exhaustive-safe", []string{"--mode", "exhaustive", "testdata/payments-old.yml", "testdata/payments-add-team.yml"}, 0},
		{"exhaustive-regression", []string{"--mode", "exhaustive", "testdata/payments-old.yml", "testdata/payments-narrowed.yml"}, 1},
		{"subtyping-alias-still-works", []string{"--mode", "subtyping", "testdata/payments-old.yml", "testdata/payments-narrowed.yml"}, 1},
		{"bad-usage", []string{"only-one-arg.yml"}, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := run(c.args); got != c.want {
				t.Errorf("run(%v) = %d, want %d", c.args, got, c.want)
			}
		})
	}
}
