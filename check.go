package main

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
	"time"

	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/dispatch"
	"github.com/prometheus/alertmanager/inhibit"
	"github.com/prometheus/alertmanager/pkg/labels"
	"github.com/prometheus/common/model"
	yaml "gopkg.in/yaml.v2"
)

// Config is the part of an Alertmanager deployment amcheck reasons about: the
// routing tree, the inhibition rules, and any declared silences. Routing and
// inhibition are compiled with Alertmanager's own packages; silences come from
// a separate `amtool silence export` file (they are runtime objects, not part
// of alertmanager.yml).
type Config struct {
	Route    *dispatch.Route
	Inhibit  []*inhibit.InhibitRule
	Silences []Silence
}

// Silence is a compiled declared silence: a set of matchers plus a validity
// window. It masks an alert whose labels match all matchers while active.
type Silence struct {
	ID       string
	Comment  string
	StartsAt time.Time
	EndsAt   time.Time
	Matchers labels.Matchers
}

func (s Silence) active(now time.Time) bool {
	if !s.StartsAt.IsZero() && now.Before(s.StartsAt) {
		return false
	}
	if !s.EndsAt.IsZero() && !now.Before(s.EndsAt) {
		return false
	}
	return true
}

func (s Silence) matches(w Witness) bool { return s.Matchers.Matches(toLabelSet(w)) }

// silence JSON as produced by `amtool silence export`.
type silenceJSON struct {
	ID       string    `json:"id"`
	Comment  string    `json:"comment"`
	StartsAt time.Time `json:"startsAt"`
	EndsAt   time.Time `json:"endsAt"`
	Matchers []struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		IsRegex bool   `json:"isRegex"`
		IsEqual *bool  `json:"isEqual"` // absent ⇒ true, for older exports
	} `json:"matchers"`
}

// loadSilences reads an `amtool silence export` JSON file (a top-level array)
// and compiles each silence's matchers into labels.Matchers.
func loadSilences(path string) ([]Silence, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw []silenceJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("parse silences %s: %w", path, err)
	}
	out := make([]Silence, 0, len(raw))
	for _, s := range raw {
		var ms labels.Matchers
		for _, m := range s.Matchers {
			eq := true
			if m.IsEqual != nil {
				eq = *m.IsEqual
			}
			var t labels.MatchType
			switch {
			case !m.IsRegex && eq:
				t = labels.MatchEqual
			case !m.IsRegex && !eq:
				t = labels.MatchNotEqual
			case m.IsRegex && eq:
				t = labels.MatchRegexp
			default:
				t = labels.MatchNotRegexp
			}
			lm, err := labels.NewMatcher(t, m.Name, m.Value)
			if err != nil {
				return nil, fmt.Errorf("silence %s matcher %s: %w", s.ID, m.Name, err)
			}
			ms = append(ms, lm)
		}
		out = append(out, Silence{ID: s.ID, Comment: s.Comment, StartsAt: s.StartsAt, EndsAt: s.EndsAt, Matchers: ms})
	}
	return out, nil
}

// Witness is a representative alert: a set of label name=value pairs.
type Witness map[string]string

// loadRoute parses an Alertmanager config file and returns the root of its
// routing tree, using Alertmanager's own config + dispatch packages so that
// routing matches production exactly.
func loadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg, err := loadLenient(b)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	c := &Config{Route: dispatch.NewRoute(cfg.Route, nil)}
	for i := range cfg.InhibitRules {
		c.Inhibit = append(c.Inhibit, inhibit.NewInhibitRule(cfg.InhibitRules[i]))
	}
	return c, nil
}

var reUnknownField = regexp.MustCompile(`field (\S+) not found in type config\.plain`)

// Template markers that should never appear in a rendered config's routing
// structure or receiver names (legitimate {{ }} notification templates live in
// notifier fields, which loadLenient strips before this check runs).
var reTemplate = regexp.MustCompile(`\{\{|\{%|\$\{`)

// --- Prometheus Operator AlertmanagerConfig CRD support --------------------
//
// Real Kubernetes teams express Alertmanager config as a monitoring.coreos.com
// AlertmanagerConfig custom resource, not a native alertmanager.yml. Its spec
// mirrors the native config but uses camelCase keys (groupBy, inhibitRules) and
// structured matchers ({name, value, matchType}) instead of strings. We detect
// the CRD and transform its spec into the native shape so the rest of amcheck
// (routing, inhibition, both engines) works unchanged.

func amMap(v interface{}) map[interface{}]interface{} {
	m, _ := v.(map[interface{}]interface{})
	return m
}
func amSlice(v interface{}) []interface{} { s, _ := v.([]interface{}); return s }
func amStr(v interface{}) string          { s, _ := v.(string); return s }

func isAlertmanagerConfigCRD(doc map[string]interface{}) bool {
	return amStr(doc["kind"]) == "AlertmanagerConfig"
}

// crdMatchers converts a CRD matcher list (objects {name,value,matchType} or
// bare strings) into native string matchers like `severity="critical"`.
func crdMatchers(v interface{}) []interface{} {
	var out []interface{}
	for _, item := range amSlice(v) {
		switch t := item.(type) {
		case string:
			out = append(out, t)
		case map[interface{}]interface{}:
			op := "="
			switch amStr(t["matchType"]) {
			case "!=":
				op = "!="
			case "=~":
				op = "=~"
			case "!~":
				op = "!~"
			}
			if b, ok := t["regex"].(bool); ok && b { // legacy boolean form
				op = "=~"
			}
			out = append(out, fmt.Sprintf("%s%s%q", amStr(t["name"]), op, amStr(t["value"])))
		}
	}
	return out
}

func crdRoute(v interface{}) map[interface{}]interface{} {
	r := amMap(v)
	if r == nil {
		return nil
	}
	out := map[interface{}]interface{}{}
	if x := r["receiver"]; x != nil {
		out["receiver"] = x
	}
	if x := r["groupBy"]; x != nil {
		out["group_by"] = x
	}
	if x := r["continue"]; x != nil {
		out["continue"] = x
	}
	if x := r["matchers"]; x != nil {
		out["matchers"] = crdMatchers(x)
	}
	if x := r["match"]; x != nil { // legacy map form
		out["match"] = x
	}
	if x := r["matchRE"]; x != nil {
		out["match_re"] = x
	}
	if kids := amSlice(r["routes"]); kids != nil {
		var conv []interface{}
		for _, k := range kids {
			conv = append(conv, crdRoute(k))
		}
		out["routes"] = conv
	}
	// group_wait / repeat_interval / mute_time_intervals etc. are dropped:
	// irrelevant to route preservation, and time-interval refs would need
	// definitions the CRD carries elsewhere.
	return out
}

func crdInhibitRules(v interface{}) []interface{} {
	var out []interface{}
	for _, item := range amSlice(v) {
		r := amMap(item)
		if r == nil {
			continue
		}
		src := r["sourceMatchers"]
		if src == nil {
			src = r["sourceMatch"]
		}
		tgt := r["targetMatchers"]
		if tgt == nil {
			tgt = r["targetMatch"]
		}
		rule := map[interface{}]interface{}{
			"source_matchers": crdMatchers(src),
			"target_matchers": crdMatchers(tgt),
		}
		if eq := r["equal"]; eq != nil {
			rule["equal"] = eq
		}
		out = append(out, rule)
	}
	return out
}

// crdToNative transforms an AlertmanagerConfig CRD document into a native-config
// map (route / receivers / inhibit_rules).
func crdToNative(doc map[string]interface{}) map[string]interface{} {
	spec := amMap(doc["spec"])
	if spec == nil {
		return nil
	}
	out := map[string]interface{}{}
	if r := spec["route"]; r != nil {
		out["route"] = crdRoute(r)
	}
	if recs := spec["receivers"]; recs != nil {
		out["receivers"] = recs // notifier configs are stripped to names later
	}
	if inh := spec["inhibitRules"]; inh != nil {
		out["inhibit_rules"] = crdInhibitRules(inh)
	}
	return out
}

// loadLenient parses an Alertmanager config, dropping unknown TOP-LEVEL fields
// (e.g. `tracing` from a newer Alertmanager) so configs targeting newer versions
// still load; the fields amcheck reasons about (route, receivers, inhibit_rules)
// are never dropped. It first transforms a Prometheus Operator AlertmanagerConfig
// CRD into the native shape.
func loadLenient(b []byte) (*config.Config, error) {
	var doc map[string]interface{}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if isAlertmanagerConfigCRD(doc) {
		native := crdToNative(doc)
		if native == nil {
			return nil, fmt.Errorf("AlertmanagerConfig CRD has no usable .spec")
		}
		doc = native
	}

	// amcheck reasons only about routing: the route tree, inhibit rules,
	// silences, time intervals, and receiver NAMES. Strip notifier-level
	// content — global defaults, templates, and each receiver's notifier
	// configs — which is irrelevant to route preservation and routinely fails
	// validation in real configs (unresolved ${ENV} URLs, secrets, invalid
	// smarthosts, unknown notifier types). Receiver names are preserved so
	// route references still resolve.
	delete(doc, "global")
	delete(doc, "templates")
	if recs, ok := doc["receivers"].([]interface{}); ok {
		bare := make([]interface{}, 0, len(recs))
		for _, r := range recs {
			if rm, ok := r.(map[interface{}]interface{}); ok {
				if name, ok := rm["name"]; ok {
					bare = append(bare, map[interface{}]interface{}{"name": name})
				}
			}
		}
		doc["receivers"] = bare
	}

	// After stripping notifier content, any remaining template markers mean the
	// file is an unrendered template (receiver names or matchers like
	// {{ pillar... }} or ${ENV}), not a real config amcheck can reason about.
	if stripped, err := yaml.Marshal(doc); err == nil && reTemplate.Match(stripped) {
		return nil, fmt.Errorf("config appears to be an unrendered template (contains {{ }}, {%% %%} or ${ }); render it before checking")
	}

	for i := 0; i < 32; i++ {
		out, err := yaml.Marshal(doc)
		if err != nil {
			return nil, err
		}
		cfg, lerr := config.Load(string(out))
		if lerr == nil {
			return cfg, nil
		}
		m := reUnknownField.FindStringSubmatch(lerr.Error())
		if m == nil {
			return nil, lerr // a real error, not an unknown field
		}
		field := m[1]
		if _, ok := doc[field]; !ok {
			return nil, lerr // unknown field is nested, not top-level — can't drop safely
		}
		delete(doc, field)
		fmt.Fprintf(os.Stderr, "amcheck: note: ignoring unknown top-level config field %q (newer Alertmanager?)\n", field)
	}
	return nil, fmt.Errorf("too many unknown fields to drop")
}

// loadConfigWithSilences loads a config and, if silencesPath is non-empty,
// attaches the declared silences from that `amtool silence export` file.
func loadConfigWithSilences(configPath, silencesPath string) (*Config, error) {
	c, err := loadConfig(configPath)
	if err != nil {
		return nil, err
	}
	if silencesPath != "" {
		sils, err := loadSilences(silencesPath)
		if err != nil {
			return nil, err
		}
		c.Silences = sils
	}
	return c, nil
}

// loadRoute is a convenience for callers that only need the routing tree.
func loadRoute(path string) (*dispatch.Route, error) {
	c, err := loadConfig(path)
	if err != nil {
		return nil, err
	}
	return c.Route, nil
}

func toLabelSet(w Witness) model.LabelSet {
	lset := model.LabelSet{}
	for k, v := range w {
		lset[model.LabelName(k)] = model.LabelValue(v)
	}
	return lset
}

// buildWitnesses derives representative alerts from the routing tree by
// synthesising, for every root->node path, label values that satisfy the
// matchers along it. This is the "auto-witness" step: the user authors no
// test cases.
//
// The second return value is true iff some path could not be witnessed
// (unsatisfiable constraints, or a matcher whose language amcheck cannot
// synthesise a member of) — i.e. coverage is partial there.
func buildWitnesses(r *dispatch.Route) ([]Witness, bool) {
	var out []Witness
	unsat := false
	for _, p := range routePaths(r) {
		if ws, ok := synthWitnesses(p); ok {
			out = append(out, ws...)
		} else {
			unsat = true
		}
	}
	return dedupeWitnesses(out), unsat
}

// routePaths returns, for every node of the routing tree, the conjunction of
// matchers from the root down to that node — one matcher list per route.
func routePaths(r *dispatch.Route) [][]*labels.Matcher {
	var out [][]*labels.Matcher
	var walk func(node *dispatch.Route, acc []*labels.Matcher)
	walk = func(node *dispatch.Route, acc []*labels.Matcher) {
		cur := make([]*labels.Matcher, len(acc), len(acc)+len(node.Matchers))
		copy(cur, acc)
		cur = append(cur, node.Matchers...)
		out = append(out, cur)
		for _, c := range node.Routes {
			walk(c, cur)
		}
	}
	walk(r, nil)
	return out
}

// synthWitnesses produces the representative alerts satisfying a conjunction
// of matchers. Matchers are grouped by label name; each name yields one or
// more candidate values (e.g. one per alternation branch); witnesses are the
// (capped) cross-product. Returns ok=false if any label admits no value.
func synthWitnesses(ms []*labels.Matcher) ([]Witness, bool) {
	const witnessCap = 32
	byName := map[string][]*labels.Matcher{}
	var order []string
	for _, m := range ms {
		if _, seen := byName[m.Name]; !seen {
			order = append(order, m.Name)
		}
		byName[m.Name] = append(byName[m.Name], m)
	}

	witnesses := []Witness{{}}
	for _, name := range order {
		vals := resolveValues(byName[name])
		if len(vals) == 0 {
			return nil, false // this label admits no satisfying value
		}
		var next []Witness
		for _, w := range witnesses {
			for _, v := range vals {
				nw := Witness{}
				for k, vv := range w {
					nw[k] = vv
				}
				nw[name] = v
				next = append(next, nw)
				if len(next) >= witnessCap {
					break
				}
			}
			if len(next) >= witnessCap {
				break
			}
		}
		witnesses = next
	}
	return witnesses, true
}

// resolveValues returns label values that satisfy EVERY matcher on one label
// name. Equality matchers pin the value; regex matchers are synthesised
// (expanding alternations); negative-only labels get avoiding fallbacks.
// Every candidate is verified with Alertmanager's own Matcher.Matches, so a
// value that does not truly satisfy the constraints is never emitted.
func resolveValues(ms []*labels.Matcher) []string {
	const perNameCap = 8
	var raw []string
	hasEq := false
	for _, m := range ms {
		if m.Type == labels.MatchEqual {
			hasEq = true
			raw = append(raw, m.Value)
		}
	}
	if !hasEq {
		for _, m := range ms {
			if m.Type == labels.MatchRegexp {
				if vs, ok := synthValues(m.Value); ok {
					raw = append(raw, vs...)
				}
			}
		}
		// Fallbacks for negative-only labels (!= / !~) or failed synthesis.
		raw = append(raw, "amcheck-synth", "amcheck-1", "x", "z", "0", "none")
	}

	seen := map[string]struct{}{}
	var out []string
	for _, v := range raw {
		if _, dup := seen[v]; dup {
			continue
		}
		if satisfiesAll(v, ms) {
			seen[v] = struct{}{}
			out = append(out, v)
			if len(out) >= perNameCap {
				break
			}
		}
	}
	return out
}

func satisfiesAll(v string, ms []*labels.Matcher) bool {
	for _, m := range ms {
		if !m.Matches(v) {
			return false
		}
	}
	return true
}

// synthValues returns representative strings that fully match the (anchored)
// Alertmanager regex, expanding alternations so that a|b|c yields all of
// a, b, c. Returns ok=false if the pattern cannot be parsed or synthesised.
func synthValues(pattern string) ([]string, bool) {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil, false
	}
	vals := emitAll(re.Simplify(), 0)
	if len(vals) == 0 {
		return nil, false
	}
	return vals, true
}

// emitAll walks a parsed regex AST and returns representative full-match
// strings, expanding alternations (bounded). Constructs it cannot synthesise
// a member of yield an empty slice, which propagates to "unsatisfiable".
func emitAll(re *syntax.Regexp, depth int) []string {
	const branchCap = 8
	if depth > 60 {
		return nil
	}
	switch re.Op {
	case syntax.OpEmptyMatch,
		syntax.OpBeginLine, syntax.OpEndLine,
		syntax.OpBeginText, syntax.OpEndText,
		syntax.OpWordBoundary, syntax.OpNoWordBoundary:
		return []string{""}
	case syntax.OpLiteral:
		return []string{string(re.Rune)}
	case syntax.OpCharClass:
		if r, ok := pickRune(re.Rune); ok {
			return []string{string(r)}
		}
		return nil
	case syntax.OpAnyChar, syntax.OpAnyCharNotNL:
		return []string{"x"}
	case syntax.OpCapture:
		return emitAll(re.Sub[0], depth+1)
	case syntax.OpStar, syntax.OpQuest:
		return []string{""} // zero occurrences: minimal match
	case syntax.OpPlus:
		return emitAll(re.Sub[0], depth+1)
	case syntax.OpRepeat:
		cur := []string{""}
		for i := 0; i < re.Min; i++ {
			cur = cross(cur, emitAll(re.Sub[0], depth+1), branchCap)
		}
		return cur
	case syntax.OpConcat:
		cur := []string{""}
		for _, s := range re.Sub {
			cur = cross(cur, emitAll(s, depth+1), branchCap)
		}
		return cur
	case syntax.OpAlternate:
		var out []string
		for _, s := range re.Sub {
			out = append(out, emitAll(s, depth+1)...)
			if len(out) >= branchCap {
				return out[:branchCap]
			}
		}
		return out
	default:
		return nil
	}
}

// cross returns the (capped) cartesian concatenation of two string sets.
// If either side is empty, the result is empty (propagates unsatisfiability).
func cross(a, b []string, cap int) []string {
	if len(a) == 0 || len(b) == 0 {
		return nil
	}
	var out []string
	for _, x := range a {
		for _, y := range b {
			out = append(out, x+y)
			if len(out) >= cap {
				return out
			}
		}
	}
	return out
}

// pickRune chooses a representative rune from a character class's rune ranges,
// preferring a friendly [a-zA-Z0-9] character for readable witnesses.
func pickRune(rr []rune) (rune, bool) {
	friendly := []rune{'a', 'b', 'x', 'z', '0', '1', 'A'}
	for i := 0; i+1 < len(rr); i += 2 {
		lo, hi := rr[i], rr[i+1]
		for _, c := range friendly {
			if c >= lo && c <= hi {
				return c, true
			}
		}
		if lo >= 0x20 && lo != 0x7f {
			return lo, true
		}
	}
	if len(rr) >= 2 {
		return rr[0], true
	}
	return 0, false
}

// receivers returns the set of receivers an alert with the given labels would
// reach under the routing tree, computed by Alertmanager's real matcher.
func receivers(r *dispatch.Route, w Witness) []string {
	set := map[string]struct{}{}
	for _, mr := range r.Match(toLabelSet(w)) {
		set[mr.RouteOpts.Receiver] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Regression records a receiver that some alert no longer reaches.
type Regression struct {
	Receiver string   `json:"receiver"`
	Witness  Witness  `json:"witness"`
	OldRecv  []string `json:"old_receivers"`
	NewRecv  []string `json:"new_receivers"`
}

// diff computes route-preservation regressions: receivers reachable under old
// but not under new, for the auto-derived witnesses. The bool reports partial
// witness coverage. This is the operational engine — ground truth for the
// witnesses it checks, always decidable.
func diff(old, new *dispatch.Route) ([]Regression, bool) {
	witnesses, partial := buildWitnesses(old)
	// Report at most one (smallest) witness per dropped receiver.
	best := map[string]Regression{}
	for _, w := range witnesses {
		oldR := receivers(old, w)
		newR := receivers(new, w)
		newSet := map[string]struct{}{}
		for _, r := range newR {
			newSet[r] = struct{}{}
		}
		for _, r := range oldR {
			if _, ok := newSet[r]; ok {
				continue
			}
			cand := Regression{Receiver: r, Witness: w, OldRecv: oldR, NewRecv: newR}
			if prev, seen := best[r]; !seen || len(w) < len(prev.Witness) {
				best[r] = cand
			}
		}
	}
	out := make([]Regression, 0, len(best))
	for _, reg := range best {
		out = append(out, reg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Receiver < out[j].Receiver })
	return out, partial
}

// --- exhaustive engine -----------------------------------------------------
//
// The exhaustive engine decides route preservation by enumerating canonical
// alert instances over the label VALUES the two configs can distinguish (the
// literals + regex representatives appearing in either tree, plus an "other"
// value and an "absent" case per label). Over that finite domain it is a
// COMPLETE decision: if it reports no regression, no alert the two configs can
// tell apart loses a route. This is the complete counterpart to the
// operational engine's per-route sampling. Exponential worst case, so it is
// not the default; a hard cap bounds the search and reports truncation rather
// than silently under-checking.
//
// A compositional form (deciding containment by matcher containment, without
// enumeration) is a possible future optimisation; this exhaustive form is
// exact by construction.

const (
	valAbsent = "\x00absent"          // sentinel: label not set on the alert
	valOther  = "amcheck-other-value" // a value distinct from the listed literals
)

func matchersByName(r *dispatch.Route, acc map[string][]*labels.Matcher) {
	for _, m := range r.Matchers {
		acc[m.Name] = append(acc[m.Name], m)
	}
	for _, c := range r.Routes {
		matchersByName(c, acc)
	}
}

// canonicalValues returns the distinct values worth trying for one label:
// every literal / regex representative that any matcher on it references
// (both the matching and the negated side), plus an "other" value and the
// "absent" case.
func canonicalValues(ms []*labels.Matcher) []string {
	seen := map[string]bool{}
	var vals []string
	add := func(v string) {
		if !seen[v] {
			seen[v] = true
			vals = append(vals, v)
		}
	}
	for _, m := range ms {
		switch m.Type {
		case labels.MatchEqual, labels.MatchNotEqual:
			add(m.Value)
		case labels.MatchRegexp, labels.MatchNotRegexp:
			if vs, ok := synthValues(m.Value); ok {
				for _, v := range vs {
					add(v)
				}
			}
		}
	}
	add(valOther)
	add(valAbsent)
	return vals
}

// subtypingDecision decides route preservation exhaustively over the canonical
// instances. Returns the regressions found and whether the search was
// truncated by the cap.
func subtypingDecision(old, new *dispatch.Route) ([]Regression, bool) {
	const cap = 5000
	// Enumerate over the values OLD distinguishes: we check that alerts old
	// routed to a receiver still reach it. Values that appear only in new are
	// new routing intent (e.g. a freshly added team route), not a regression
	// of an existing route — enumerating them would flag benign catch-all
	// reroutes.
	byName := map[string][]*labels.Matcher{}
	matchersByName(old, byName)

	var names []string
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)

	instances := []Witness{{}}
	truncated := false
	for _, n := range names {
		var next []Witness
		for _, w := range instances {
			for _, v := range canonicalValues(byName[n]) {
				nw := Witness{}
				for k, vv := range w {
					nw[k] = vv
				}
				if v != valAbsent {
					nw[n] = v
				}
				next = append(next, nw)
				if len(next) >= cap {
					truncated = true
					break
				}
			}
			if truncated {
				break
			}
		}
		instances = next
		if truncated {
			break
		}
	}

	best := map[string]Regression{}
	for _, w := range instances {
		oldR := receivers(old, w)
		newR := receivers(new, w)
		ns := map[string]struct{}{}
		for _, r := range newR {
			ns[r] = struct{}{}
		}
		for _, r := range oldR {
			if _, ok := ns[r]; ok {
				continue
			}
			cand := Regression{Receiver: r, Witness: w, OldRecv: oldR, NewRecv: newR}
			if prev, ok := best[r]; !ok || len(w) < len(prev.Witness) {
				best[r] = cand
			}
		}
	}
	out := make([]Regression, 0, len(best))
	for _, reg := range best {
		out = append(out, reg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Receiver < out[j].Receiver })
	return out, truncated
}

// witnessString renders a witness as { a="x", b="y" } with stable ordering.
func witnessString(w Witness) string {
	if len(w) == 0 {
		return "{ }"
	}
	keys := make([]string, 0, len(w))
	for k := range w {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	s := "{ "
	for i, k := range keys {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("%s=%q", k, w[k])
	}
	return s + " }"
}

// Suppression records an alert that still routes to a receiver but could be
// newly muted by a mask (inhibition rule OR declared silence) present in `new`
// and absent in `old`. Mechanism is "inhibition" (potential: needs a firing
// source) or "silence" (active but time-boxed).
type Suppression struct {
	Witness   Witness  `json:"witness"`
	Receivers []string `json:"receivers"`
	Mechanism string   `json:"mechanism"`
	Detail    string   `json:"detail"`
}

// canBeInhibited reports whether an alert with labels w matches the target of
// some inhibition rule — i.e. a suitable source alert could mute it. This
// over-approximates real suppression (it does not require the source to be
// firing), which is why findings are reported as *potential*.
func canBeInhibited(rules []*inhibit.InhibitRule, w Witness) (bool, *inhibit.InhibitRule) {
	lset := toLabelSet(w)
	for _, r := range rules {
		if r.TargetMatchers.Matches(lset) {
			return true, r
		}
	}
	return false, nil
}

// maskedBy reports whether a config masks an alert w (at time now), and by
// which mechanism. Inhibition is checked first, then active silences.
func maskedBy(cfg *Config, w Witness, now time.Time) (mechanism, detail string) {
	if ok, r := canBeInhibited(cfg.Inhibit, w); ok {
		return "inhibition", describeRule(r)
	}
	for _, s := range cfg.Silences {
		if s.active(now) && s.matches(w) {
			return "silence", describeSilence(s)
		}
	}
	return "", ""
}

// newSuppressions finds alerts that old delivers and that a NEW mask — an
// inhibition rule or a declared silence present in new but not effective in
// old — could mute: the "still routes, now silently muted" regression that
// pure route diffing misses.
func newSuppressions(old, new *Config) []Suppression {
	now := time.Now()
	best := map[string]Suppression{}
	consider := func(w Witness) {
		oldR := receivers(old.Route, w)
		if len(oldR) == 0 {
			return
		}
		if m, _ := maskedBy(old, w, now); m != "" {
			return // already masked under old
		}
		mech, detail := maskedBy(new, w, now)
		if mech == "" {
			return
		}
		key := mech + "|" + detail
		cand := Suppression{Witness: w, Receivers: oldR, Mechanism: mech, Detail: detail}
		if prev, ok := best[key]; !ok || len(w) < len(prev.Witness) {
			best[key] = cand
		}
	}

	// For each route in old: the bare route witnesses, plus witnesses enriched
	// with each new mask's matcher labels — so we catch masks that target
	// labels the route itself does not branch on.
	var enrich [][]*labels.Matcher
	for _, nr := range new.Inhibit {
		enrich = append(enrich, nr.TargetMatchers)
	}
	for _, s := range new.Silences {
		enrich = append(enrich, s.Matchers)
	}
	for _, p := range routePaths(old.Route) {
		if ws, ok := synthWitnesses(p); ok {
			for _, w := range ws {
				consider(w)
			}
		}
		for _, extra := range enrich {
			combined := make([]*labels.Matcher, len(p), len(p)+len(extra))
			copy(combined, p)
			combined = append(combined, extra...)
			if ws, ok := synthWitnesses(combined); ok {
				for _, w := range ws {
					consider(w)
				}
			}
		}
	}

	out := make([]Suppression, 0, len(best))
	for _, s := range best {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Mechanism != out[j].Mechanism {
			return out[i].Mechanism < out[j].Mechanism
		}
		return out[i].Detail < out[j].Detail
	})
	return out
}

func describeSilence(s Silence) string {
	id := s.ID
	if len(id) > 8 {
		id = id[:8]
	}
	end := "no expiry"
	if !s.EndsAt.IsZero() {
		end = "until " + s.EndsAt.Format(time.RFC3339)
	}
	return fmt.Sprintf("silence %s %s %s (%q)", id, matchersString(s.Matchers), end, s.Comment)
}

func describeRule(r *inhibit.InhibitRule) string {
	var eq []string
	for k := range r.Equal {
		eq = append(eq, string(k))
	}
	sort.Strings(eq)
	return fmt.Sprintf("source%s target%s equal[%s]",
		matchersString(r.SourceMatchers), matchersString(r.TargetMatchers), strings.Join(eq, ","))
}

func matchersString(ms labels.Matchers) string {
	parts := make([]string, 0, len(ms))
	for _, m := range ms {
		parts = append(parts, m.String())
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func dedupeWitnesses(in []Witness) []Witness {
	seen := map[string]struct{}{}
	var out []Witness
	for _, w := range in {
		key := witnessString(w)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, w)
	}
	return out
}
