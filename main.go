// Command amcheck is a config-safety linter for Prometheus / Grafana
// Alertmanager routing configs. It diffs two configs (old, new) and reports
// every alert class that would silently stop reaching a receiver it used to
// reach — the "route regression" that syntax validation cannot see.
//
// It is built on Alertmanager's own config + dispatch packages, so routing
// matches production exactly. Witness alerts are derived from the routing
// tree, so no test cases need to be authored.
//
// Two engines: a fast "operational" mode that samples one witness per route
// (plus inhibition), and an "exhaustive" mode that decides route preservation
// completely over the label values the configs distinguish.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	fs := flag.NewFlagSet("amcheck", flag.ContinueOnError)
	fs.Usage = usage
	var (
		mode    = fs.String("mode", "operational", "check engine: operational | exhaustive")
		format  = fs.String("format", "text", "output format: text | json")
		explain = fs.Bool("explain", false, "show old vs new receivers for each regression")
		oldSil  = fs.String("old-silences", "", "amtool silence export JSON for the OLD side")
		newSil  = fs.String("new-silences", "", "amtool silence export JSON for the NEW side")
	)

	// Allow flags anywhere on the line (before/after/between positionals),
	// unlike the stdlib default which stops at the first positional.
	flagTokens, positionals := splitArgs(args)
	if err := fs.Parse(flagTokens); err != nil {
		return 2
	}

	if len(positionals) != 2 {
		usage()
		return 2
	}
	oldPath, newPath := positionals[0], positionals[1]

	// "subtyping" is the historical name for the exhaustive engine, kept as a
	// hidden backward-compatible alias.
	engineMode := *mode
	if engineMode == "subtyping" {
		engineMode = "exhaustive"
	}
	if engineMode != "operational" && engineMode != "exhaustive" {
		fmt.Fprintf(os.Stderr, "amcheck: unknown mode %q (want operational | exhaustive)\n", *mode)
		return 2
	}

	oldCfg, err := loadConfigWithSilences(oldPath, *oldSil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "amcheck: %v\n", err)
		return 2
	}
	newCfg, err := loadConfigWithSilences(newPath, *newSil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "amcheck: %v\n", err)
		return 2
	}

	var (
		engine string
		regs   []Regression
		supps  []Suppression
		note   string
	)
	switch engineMode {
	case "operational":
		engine = "operational (sampled)"
		var partial bool
		regs, partial = diff(oldCfg.Route, newCfg.Route)
		supps = newSuppressions(oldCfg, newCfg)
		if partial {
			note = "some route paths could not be witnessed (unsatisfiable constraints or a\n" +
				"         matcher amcheck cannot synthesise a member of) — absence of a regression\n" +
				"         there is not yet a proof of route preservation."
		}
	case "exhaustive":
		engine = "exhaustive (complete over distinguishable label values)"
		var truncated bool
		regs, truncated = subtypingDecision(oldCfg.Route, newCfg.Route)
		// Route-relation containment only; inhibition analysis lives in
		// operational mode. Complete over the distinguishable domain.
		if truncated {
			note = "search hit the instance cap; result is a lower bound on regressions,\n" +
				"         not a complete decision. Simplify the configs or split the check."
		}
	}

	if *format == "json" {
		return reportJSON(oldPath, newPath, engine, regs, supps, note != "")
	}
	return reportText(oldPath, newPath, engine, regs, supps, note, *explain)
}

func reportText(oldPath, newPath, engine string, regs []Regression, supps []Suppression, note string, explain bool) int {
	fmt.Printf("\n  comparing  %s → %s\n  engine     %s\n\n", oldPath, newPath, engine)
	if len(regs) == 0 && len(supps) == 0 {
		fmt.Printf("  ✓ OK — every route in %s is preserved by %s\n", oldPath, newPath)
		printNote(note)
		fmt.Println("\n  verdict  PASS                                          exit 0")
		return 0
	}

	if len(regs) > 0 {
		noun, verb := "alert class", "loses"
		if len(regs) != 1 {
			noun, verb = "alert classes", "lose"
		}
		fmt.Printf("  ✗ ROUTE REGRESSION — %d %s %s a receiver\n\n", len(regs), noun, verb)
		for i, r := range regs {
			fmt.Printf("  [%d]  %s is no longer reachable\n", i+1, r.Receiver)
			fmt.Printf("       witness  %s\n", witnessString(r.Witness))
			if explain {
				fmt.Printf("       old  → %v\n", r.OldRecv)
				fmt.Printf("       new  → %v\n", r.NewRecv)
			}
			fmt.Println()
		}
	}

	if len(supps) > 0 {
		noun := "alert class"
		if len(supps) != 1 {
			noun = "alert classes"
		}
		fmt.Printf("  ⚠ NEW SUPPRESSION — %d %s still routes but can be newly muted\n\n", len(supps), noun)
		for i, s := range supps {
			caveat := ""
			switch s.Mechanism {
			case "inhibition":
				caveat = "  (only when a matching source alert is also firing)"
			case "silence":
				caveat = "  (active now; time-boxed — delivered again after the silence expires)"
			}
			fmt.Printf("  [%d]  %s still reaches %v but a NEW %s can mute it%s\n", i+1, witnessString(s.Witness), s.Receivers, s.Mechanism, caveat)
			fmt.Printf("       %s\n\n", s.Detail)
		}
	}

	printNote(note)
	if len(regs) > 0 {
		fmt.Println("  verdict  FAIL  (route preservation violated)           exit 1")
	} else {
		fmt.Println("  verdict  FAIL  (new suppression)                       exit 1")
	}
	return 1
}

func printNote(note string) {
	if note != "" {
		fmt.Printf("  note   %s\n", note)
	}
}

type jsonReport struct {
	Old             string        `json:"old"`
	New             string        `json:"new"`
	Engine          string        `json:"engine"`
	Verdict         string        `json:"verdict"`
	PartialCoverage bool          `json:"partial_coverage"`
	Regressions     []Regression  `json:"regressions"`
	Suppressions    []Suppression `json:"potential_suppressions"`
}

func reportJSON(oldPath, newPath, engine string, regs []Regression, supps []Suppression, partial bool) int {
	verdict := "PASS"
	code := 0
	if len(regs) > 0 || len(supps) > 0 {
		verdict = "FAIL"
		code = 1
	}
	rep := jsonReport{
		Old: oldPath, New: newPath, Engine: engine, Verdict: verdict,
		PartialCoverage: partial, Regressions: regs, Suppressions: supps,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rep)
	return code
}

// splitArgs partitions the command line into flag tokens and positional
// arguments, so flags may appear anywhere. Value-taking flags in separate-
// token form (e.g. "--mode operational") consume the following token.
func splitArgs(args []string) (flagTokens, positionals []string) {
	valueFlags := map[string]bool{
		"-mode": true, "--mode": true, "-format": true, "--format": true,
		"-old-silences": true, "--old-silences": true,
		"-new-silences": true, "--new-silences": true,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" { // everything after -- is positional
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") && a != "-" {
			flagTokens = append(flagTokens, a)
			if valueFlags[a] && !strings.Contains(a, "=") && i+1 < len(args) {
				i++
				flagTokens = append(flagTokens, args[i])
			}
			continue
		}
		positionals = append(positionals, a)
	}
	return flagTokens, positionals
}

func usage() {
	fmt.Fprint(os.Stderr, `amcheck — Alertmanager config-safety linter

  amcheck <old-config> <new-config> [flags]

Reports every alert class that reaches a receiver under <old-config> but not
under <new-config> (a silently dropped route), with a witness alert.

Flags:
  --mode      operational | exhaustive  (default operational)
              operational: fast, samples one witness per route (+ inhibition)
              exhaustive:  complete decision over distinguishable label values
                           ("subtyping" accepted as an alias)
  --explain   show old vs new receivers per regression
  --format    text | json               (default text)
  --old-silences FILE   amtool silence export JSON for the OLD side
  --new-silences FILE   amtool silence export JSON for the NEW side

Exit codes:  0 = all routes preserved   1 = regression   2 = usage/config error
`)
}
