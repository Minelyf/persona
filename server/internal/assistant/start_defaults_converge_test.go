package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// The converged wire names (#11719): applies_when and candidates, with priority. The old names still decode, reads
// answer both, and a body whose two names disagree is refused — including on the nested start_defaults of PUT
// /api/v1/assistant, which has no DisallowUnknownFields of its own.
func TestStartDefaultsConvergedNamesAndAliases(t *testing.T) {
	s, f := defaultsHub(t)
	ctx := context.Background()
	// New names, with priority: the write works and reaches a real start.
	body := `{"start_defaults":{"rules":[{"applies_when":{"role":"worker"},"then":{
		"candidates":[{"name":"quick","priority":1},{"name":"deep","priority":5}],"effort":"max"}}]}}`
	if code, got := putStartDefaults(t, s, roleFull, "", body); code != http.StatusOK {
		t.Fatalf("converged write: %d %s", code, got)
	}
	// Reads carry both names with the same content, so either generation of client round-trips it.
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/assistant/start-defaults", nil))
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	rule := got["start_defaults"].(map[string]any)["rules"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(rule["when"], rule["applies_when"]) {
		t.Fatalf("when and applies_when differ: %v", rule)
	}
	then := rule["then"].(map[string]any)
	if !reflect.DeepEqual(then["presets"], then["candidates"]) {
		t.Fatalf("presets and candidates differ: %v", then)
	}
	if c := then["candidates"].([]any)[1].(map[string]any); c["priority"] != float64(5) {
		t.Fatalf("priority not carried: %v", c)
	}
	task := defaultsTask()
	if _, _, err := s.startTaskAgent(ctx, &task, false); err != nil {
		t.Fatal(err)
	}
	if st := startsOf(f)[0]; st["model"] != "gw-deep" || st["effort"] != "max" {
		t.Fatalf("the converged policy did not apply: %v", st)
	}
	// The stored policy (Postgres JSON) reads back after carrying both names.
	if p := s.activeStartDefaults(ctx); p == nil || p.Rules[0].When.Role != "worker" || p.Rules[0].Then.Presets[1].Priority != 5 {
		t.Fatalf("the stored policy did not round-trip: %+v", p)
	}
	// Old names still write, on this route and nested in PUT /api/v1/assistant.
	if code, got := putStartDefaults(t, s, roleFull, "", `{"start_defaults":{"rules":[{"when":{},"then":{"presets":[{"name":"quick"}]}}]}}`); code != http.StatusOK {
		t.Fatalf("legacy write: %d %s", code, got)
	}
	nested := httptest.NewRequest(http.MethodPut, "/api/v1/assistant", strings.NewReader(
		`{"start_defaults":{"rules":[{"applies_when":{},"then":{"candidates":[{"name":"deep"}]}}]}}`))
	nested.Header.Set(RoleHeader, roleFull)
	nrec := httptest.NewRecorder()
	s.ServeHTTP(nrec, nested)
	if nrec.Code != http.StatusOK {
		t.Fatalf("nested converged write: %d %s", nrec.Code, nrec.Body.String())
	}
	if p := s.activeStartDefaults(ctx); p == nil || p.Rules[0].Then.Presets[0].Name != "deep" {
		t.Fatalf("the nested write did not store the candidates: %+v", p)
	}
}

// A body that names both generations of a field is accepted when they agree (that is what a read answers) and
// refused when they disagree; a misspelt field is still a 400 everywhere, aliases included.
func TestStartDefaultsAliasConflictsAndUnknownFields(t *testing.T) {
	s, _ := defaultsHub(t)
	for _, c := range []struct {
		name, body string
		code       int
	}{
		{"both matches agree", `{"start_defaults":{"rules":[{"when":{"role":"worker"},"applies_when":{"role":"worker"},"then":{"effort":"max"}}]}}`, http.StatusOK},
		{"both matches disagree", `{"start_defaults":{"rules":[{"when":{"role":"worker"},"applies_when":{"role":"leader"},"then":{"effort":"max"}}]}}`, http.StatusBadRequest},
		{"both lists agree", `{"start_defaults":{"rules":[{"when":{},"then":{"presets":[{"name":"deep"}],"candidates":[{"name":"deep"}]}}]}}`, http.StatusOK},
		{"both lists disagree", `{"start_defaults":{"rules":[{"when":{},"then":{"presets":[{"name":"deep"}],"candidates":[{"name":"quick"}]}}]}}`, http.StatusBadRequest},
		{"misspelt applies_when", `{"start_defaults":{"rules":[{"applies_whenx":{},"then":{"effort":"max"}}]}}`, http.StatusBadRequest},
		{"misspelt candidates", `{"start_defaults":{"rules":[{"when":{},"then":{"candidatez":[{"name":"deep"}]}}]}}`, http.StatusBadRequest},
		{"unknown match field", `{"start_defaults":{"rules":[{"applies_when":{"rol":"worker"},"then":{"effort":"max"}}]}}`, http.StatusBadRequest},
		// An explicit null is present on the wire and must not slip past the conflict check the way a decoded nil
		// pointer would (#11719 review): a null beside a real value is a disagreement.
		{"null match beside a real match", `{"start_defaults":{"rules":[{"when":null,"applies_when":{"role":"leader"},"then":{"effort":"max"}}]}}`, http.StatusBadRequest},
		{"real match beside a null match", `{"start_defaults":{"rules":[{"when":{"role":"worker"},"applies_when":null,"then":{"effort":"max"}}]}}`, http.StatusBadRequest},
		{"null list beside a real list", `{"start_defaults":{"rules":[{"when":{},"then":{"presets":null,"candidates":[{"name":"deep"}]}}]}}`, http.StatusBadRequest},
		{"real list beside a null list", `{"start_defaults":{"rules":[{"when":{},"then":{"presets":[{"name":"deep"}],"candidates":null}}]}}`, http.StatusBadRequest},
		{"both matches null", `{"start_defaults":{"rules":[{"when":null,"applies_when":null,"then":{"effort":"max"}}]}}`, http.StatusOK},
		{"both lists null", `{"start_defaults":{"rules":[{"when":{},"then":{"presets":null,"candidates":null,"effort":"max"}}]}}`, http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			if code, got := putStartDefaults(t, s, roleFull, "", c.body); code != c.code {
				t.Fatalf("%s: %d %s", c.name, code, got)
			}
		})
	}
}

// Priority orders the usable candidates: the highest priority with anything usable wins, however the list is ordered,
// and a group whose presets cannot run falls through to the next one. All-zero priorities keep the old order/weights.
func TestStartDefaultsPriorityOrdersAndFallsThrough(t *testing.T) {
	ctx := context.Background()
	// Higher priority wins even when it is listed last.
	s, f := defaultsHub(t)
	stubStartDefaults(s, func(StartFacts) (StartDefaults, bool) {
		return StartDefaults{Presets: []PresetChoice{{Name: "quick", Priority: 1}, {Name: "deep", Priority: 9}}}, true
	})
	task := defaultsTask()
	if _, _, err := s.startTaskAgent(ctx, &task, false); err != nil {
		t.Fatal(err)
	}
	if st := startsOf(f)[0]; st["model"] != "gw-deep" {
		t.Fatalf("the higher priority did not win: %v", st)
	}
	// The top group's presets cannot run here: the next group is used, and the skip is named.
	s, f = defaultsHub(t)
	saveTiers(t, s, map[string]ModelTier{
		"elsewhere": {Launcher: "other", Model: "o-1"},
		"quick":     {Launcher: "gateway", Model: "gw-quick"},
		"plain":     {Launcher: "claude", Model: "claude-plain"},
	})
	stubStartDefaults(s, func(StartFacts) (StartDefaults, bool) {
		return StartDefaults{By: "Example", Presets: []PresetChoice{
			{Name: "elsewhere", Priority: 100}, {Name: "gone", Priority: 100},
			{Name: "plain", Priority: 2}, {Name: "quick", Priority: 1},
		}}, true
	})
	task = defaultsTask()
	task.MachinePolicy = machineFlexible
	if _, _, err := s.startTaskAgent(ctx, &task, false); err != nil {
		t.Fatal(err)
	}
	if st := startsOf(f)[0]; st["model"] != "claude-plain" {
		t.Fatalf("the next priority did not take over: %v", st)
	}
	for _, name := range []string{"elsewhere", "gone"} {
		if !strings.Contains(task.Why, name) {
			t.Fatalf("the why does not name the passed-over %s: %q", name, task.Why)
		}
	}
	// Priority bounds are validated like weight's.
	for _, body := range []string{
		`{"start_defaults":{"rules":[{"when":{},"then":{"candidates":[{"name":"deep","priority":-1}]}}]}}`,
		`{"start_defaults":{"rules":[{"when":{},"then":{"candidates":[{"name":"deep","priority":1001}]}}]}}`,
	} {
		if code, got := putStartDefaults(t, s, roleFull, "", body); code != http.StatusBadRequest {
			t.Fatalf("out-of-range priority accepted: %d %s", code, got)
		}
	}
}

// Weights share starts only within the winning priority group: the roll's bound is that group's weight sum, never
// the whole list's, and a lower-priority candidate cannot be rolled into the top group's start.
func TestStartDefaultsWeightsWithinTheTopPriority(t *testing.T) {
	ctx := context.Background()
	old := presetIntN
	t.Cleanup(func() { presetIntN = old })
	var bounds []int
	presetIntN = func(n int) int { bounds = append(bounds, n); return n - 1 } // always the last of the rolled group
	s, f := defaultsHub(t)
	stubStartDefaults(s, func(StartFacts) (StartDefaults, bool) {
		return StartDefaults{Presets: []PresetChoice{
			{Name: "deep", Priority: 5, Weight: 1}, {Name: "quick", Priority: 5, Weight: 3},
			{Name: "plain", Priority: 1, Weight: 40}, // lower priority: never in the roll, whatever its weight
		}}, true
	})
	task := defaultsTask()
	if _, _, err := s.startTaskAgent(ctx, &task, false); err != nil {
		t.Fatal(err)
	}
	if st := startsOf(f)[0]; st["model"] != "gw-quick" {
		t.Fatalf("the roll did not stay in the top group: %v", st)
	}
	if !slices.Equal(bounds, []int{4}) {
		t.Fatalf("roll bounds = %v, want the top group's weight sum 4", bounds)
	}
	// The production roll is the crypto-grade source and stays in range; the var stays injectable.
	presetIntN = old
	for range 64 {
		if got := presetIntN(7); got < 0 || got >= 7 {
			t.Fatalf("presetIntN out of range: %d", got)
		}
	}
}

// A retry re-evaluates the rule and re-rolls, keeping nothing of the earlier try's priority choice: the caller's own
// values are restored first, and a rule that no longer fits leaves no preset, effort or ultracode behind.
func TestStartDefaultsRetryRerollsAndKeepsNothingOfTheOldRule(t *testing.T) {
	ctx := context.Background()
	old := presetIntN
	t.Cleanup(func() { presetIntN = old })
	rolls := 0
	presetIntN = func(n int) int { rolls++; return n - 1 } // deep(1), quick(3): the last roll picks quick
	s, f := defaultsHub(t)
	first := true
	stubStartDefaults(s, func(StartFacts) (StartDefaults, bool) {
		if first {
			return StartDefaults{Ultracode: boolp(true), Effort: "max", Presets: []PresetChoice{
				{Name: "deep", Priority: 1}, {Name: "quick", Priority: 5, Weight: 2}, {Name: "plain", Priority: 5, Weight: 1},
			}}, true
		}
		return StartDefaults{}, false // the policy no longer applies at all
	})
	task := defaultsTask()
	if _, _, err := s.startTaskAgent(ctx, &task, false); err != nil {
		t.Fatal(err)
	}
	if st := startsOf(f)[0]; st["model"] != "claude-plain" || st["ultracode"] != true || st["effort"] != "max" {
		t.Fatalf("the first rule did not apply: %v", st)
	}
	if rolls != 1 {
		t.Fatalf("the first try did not roll once: %d", rolls)
	}
	first = false
	if _, _, err := s.startTaskAgent(ctx, &task, true); err != nil {
		t.Fatal(err)
	}
	st := startsOf(f)[1]
	if st["model"] != "gw-model" || st["launcher"] != "gateway" || st["ultracode"] != nil || st["effort"] == "max" {
		t.Fatalf("the retry kept the old rule's values: %v", st)
	}
	if rolls != 1 {
		t.Fatalf("the retry rolled although no rule fits: %d", rolls)
	}
	if task.Ultracode != nil || task.Effort == "max" || task.Tier != "" {
		t.Fatalf("the task kept policy values: %+v", task)
	}
}

// The preview answers what the policy would do and writes nothing: no stored policy, no start, no task, no admission
// claim, no note. A draft in the body is validated but never stored; without one, the policy in force is read.
func TestStartDefaultsPreviewIsReadOnly(t *testing.T) {
	s, f := defaultsHub(t)
	ctx := context.Background()
	// A stored policy exists, so a draft preview must not replace it and a draft-less preview must use it.
	setSettings(t, s, func(v *Settings) {
		v.StartDefaults = &StartDefaultsPolicy{By: "owner", Rules: []StartDefaultsRule{
			{When: StartDefaultsMatch{Role: "leader"}, Then: StartDefaultsThen{Effort: "low"}},
		}}
	})
	before, err := json.Marshal(s.settings(ctx).StartDefaults)
	if err != nil {
		t.Fatal(err)
	}
	draft := `{"start_defaults":{"rules":[{"applies_when":{"role":"worker"},"then":{
		"candidates":[{"name":"gone","priority":9},{"name":"deep","priority":2,"weight":2},{"name":"quick","priority":2,"weight":1}],
		"effort":"max","ultracode":true,"goal":"done_when","plan_first":true}}]}}`
	post := func(body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/assistant/start-defaults/preview", strings.NewReader(body))
		req.Header.Set(RoleHeader, roleFull)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	code, out := post(`{"task":{"machine":"box","role":"worker","supervisor":"leader","done_when":2},` + draft[1:])
	if code != http.StatusOK {
		t.Fatalf("preview: %d %v", code, out)
	}
	if out["matched"] != true || out["rule_index"] != float64(0) {
		t.Fatalf("the draft rule did not match: %v", out)
	}
	if out["effort"] != "max" || out["ultracode"] != true || out["goal"] != "done_when" || out["plan_first"] != true {
		t.Fatalf("the draft's extras are missing: %v", out)
	}
	usable, _ := out["usable"].([]any)
	if len(usable) != 2 { // only the top usable priority, with "gone" passed over
		t.Fatalf("usable = %v", out["usable"])
	}
	for _, row := range usable {
		if row.(map[string]any)["priority"] != float64(2) {
			t.Fatalf("a lower-priority row leaked into usable: %v", usable)
		}
	}
	passed, _ := out["passed_over"].([]any)
	if len(passed) != 1 || passed[0].(map[string]any)["name"] != "gone" {
		t.Fatalf("passed_over = %v", out["passed_over"])
	}
	if _, ok := out["sample"]; !ok {
		t.Fatalf("no sample pick: %v", out)
	}
	// Nothing changed: same stored policy, no Agent started, no plan or task written.
	after, _ := json.Marshal(s.settings(ctx).StartDefaults)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the preview changed the stored policy:\nbefore %s\nafter  %s", before, after)
	}
	if len(startsOf(f)) != 0 {
		t.Fatalf("the preview started an Agent: %v", startsOf(f))
	}
	if s.taskStarting.Load() != 0 {
		t.Fatal("the preview claimed a start")
	}
	// A draft-less preview reads the stored policy (role leader), and an unmatched one says so without side effects.
	code, out = post(`{"task":{"machine":"box","role":"leader"}}`)
	if code != http.StatusOK || out["matched"] != true || out["effort"] != "low" {
		t.Fatalf("the stored policy was not previewed: %d %v", code, out)
	}
	code, out = post(`{"task":{"machine":"box","role":"worker"}}`)
	if code != http.StatusOK || out["matched"] != false {
		t.Fatalf("an unmatched task should report matched=false: %d %v", code, out)
	}
	if len(startsOf(f)) != 0 {
		t.Fatal("a preview started an Agent")
	}
	// Validation: machine required, unknown machine 404, unknown field 400, bad draft 400.
	if code, _ := post(`{"task":{"role":"worker"}}`); code != http.StatusBadRequest {
		t.Fatalf("a preview without a machine: %d", code)
	}
	if code, _ := post(`{"task":{"machine":"nope"}}`); code != http.StatusNotFound {
		t.Fatalf("an unknown machine: %d", code)
	}
	if code, _ := post(`{"task":{"machine":"box"},"wat":1}`); code != http.StatusBadRequest {
		t.Fatalf("an unknown preview field was accepted")
	}
	if code, _ := post(`{"task":{"machine":"box"},"start_defaults":{"rules":[{"when":{},"then":{"effort":"banana"}}]}}`); code != http.StatusBadRequest {
		t.Fatalf("a bad draft was accepted")
	}
}

// The preview honours the caller's explicit values exactly as a start does: a named launcher, model or tier leaves no
// preset to choose, and an explicit effort or ultracode (on or off) is named as winning over the policy's.
func TestStartDefaultsPreviewCallerValuesWin(t *testing.T) {
	s, _ := defaultsHub(t)
	draft := `,"start_defaults":{"rules":[{"applies_when":{},"then":{"candidates":[{"name":"deep"}],"effort":"max","ultracode":true}}]}`
	post := func(caller string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/assistant/start-defaults/preview",
			strings.NewReader(`{"task":{"machine":"box"},`+strings.TrimPrefix(caller, ",")+draft+`}`))
		req.Header.Set(RoleHeader, roleFull)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	for _, caller := range []string{`"caller":{"launcher":"claude"}`, `"caller":{"model":"gw-own"}`, `"caller":{"tier":"quick"}`} {
		out := post(caller)
		if out["preset_blocked_by_caller"] != true {
			t.Fatalf("%s did not block the preset: %v", caller, out)
		}
		if _, ok := out["usable"]; ok {
			t.Fatalf("%s still listed usable presets: %v", caller, out)
		}
	}
	out := post(`"caller":{"effort":"low","ultracode":false}`)
	wins, _ := out["caller_wins"].([]any)
	if len(wins) != 2 {
		t.Fatalf("caller_wins = %v", out["caller_wins"])
	}
	// A caller tier that itself names an effort also keeps the policy's effort out.
	out = post(`"caller":{"tier":"deep"}`)
	wins, _ = out["caller_wins"].([]any)
	if !slices.Contains(wins, "effort") {
		t.Fatalf("the tier's effort did not win: %v", out)
	}
}

// boolp is a pointer to b, for the retry test's ultracode.
func boolp(b bool) *bool { return &b }

// The candidate source seam: an injected source replaces the profile's data without touching matching or retry, and a
// preview and a real start read the same rows. A source that cannot be read leaves no preset, as the profile's error
// always has.
func TestStartDefaultsCandidateSourceSeam(t *testing.T) {
	ctx := context.Background()
	s, f := defaultsHub(t)
	s.startCandidates = stubCandidateSource{rows: map[string]startCandidate{
		"deep":  {Name: "deep", Launcher: "gateway", Available: false, Reason: "gateway is not available yet"},
		"quick": {Name: "quick", Launcher: "gateway", Available: true},
	}}
	stubStartDefaults(s, func(StartFacts) (StartDefaults, bool) {
		return StartDefaults{Presets: []PresetChoice{{Name: "deep", Priority: 5}, {Name: "quick", Priority: 1}}}, true
	})
	task := defaultsTask()
	if _, _, err := s.startTaskAgent(ctx, &task, false); err != nil {
		t.Fatal(err)
	}
	if st := startsOf(f)[0]; st["model"] != "gw-quick" {
		t.Fatalf("the injected source was not used: %v", st)
	}
	// An unreadable source keeps the old fail-closed behavior: no preset, the ordinary start, no error.
	s2, f2 := defaultsHub(t)
	s2.startCandidates = stubCandidateSource{err: context.DeadlineExceeded}
	stubStartDefaults(s2, func(StartFacts) (StartDefaults, bool) {
		return StartDefaults{Presets: []PresetChoice{{Name: "deep"}}}, true
	})
	task = defaultsTask()
	if _, _, err := s2.startTaskAgent(ctx, &task, false); err != nil {
		t.Fatal(err)
	}
	if st := startsOf(f2)[0]; st["model"] != "gw-model" {
		t.Fatalf("an unreadable source did not fall back to the ordinary start: %v", st)
	}
}

// stubCandidateSource is a fixed candidate source for the seam test; err makes it unreadable.
type stubCandidateSource struct {
	rows map[string]startCandidate
	err  error
}

func (s stubCandidateSource) candidates(context.Context, string) (map[string]startCandidate, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.rows, nil
}

// The owner's own write still may omit the expiry, and an extension's still may not: the converged names did not
// loosen that guard, and a policy past its expiry matches nothing.
func TestStartDefaultsConvergedExpiryUnchanged(t *testing.T) {
	s, _ := defaultsHub(t)
	rule := `{"applies_when":{},"then":{"candidates":[{"name":"deep"}]}}`
	if code, got := putStartDefaults(t, s, roleExtension, "kit", `{"start_defaults":{"rules":[`+rule+`]}}`); code != http.StatusBadRequest {
		t.Fatalf("an extension's policy without expiry was accepted: %d %s", code, got)
	}
	soon := time.Now().Add(20 * time.Minute).UTC().Format(time.RFC3339)
	if code, got := putStartDefaults(t, s, roleExtension, "kit", `{"start_defaults":{"expires_at":"`+soon+`","rules":[`+rule+`]}}`); code != http.StatusOK {
		t.Fatalf("an expiring converged policy was refused: %d %s", code, got)
	}
	past := time.Now().Add(-time.Minute)
	setSettings(t, s, func(v *Settings) { v.StartDefaults.ExpiresAt = &past })
	if p := s.activeStartDefaults(context.Background()); p != nil {
		t.Fatalf("an expired policy is still in force: %+v", p)
	}
}

// A policy at the very top of every bound must still round-trip through the dual-name GET within the write body
// limit: 16 rules of 16 candidates each, every name 64 runes, every priority and weight at its maximum. The write
// accepts one name, the read answers both, and that read is itself a valid write body.
func TestStartDefaultsRoundTripStaysWithinBodyLimit(t *testing.T) {
	s, _ := defaultsHub(t)
	rules := make([]StartDefaultsRule, 0, startDefaultsMaxRules)
	for i := 0; i < startDefaultsMaxRules; i++ {
		cands := make([]StartDefaultsPreset, 0, startDefaultsMaxPresets)
		for j := 0; j < startDefaultsMaxPresets; j++ {
			cands = append(cands, StartDefaultsPreset{Name: strings.Repeat("é", 64), Priority: startDefaultsMaxPriority, Weight: startDefaultsMaxWeight})
		}
		rules = append(rules, StartDefaultsRule{When: StartDefaultsMatch{Role: "worker"}, Then: StartDefaultsThen{Presets: cands, Effort: "max"}})
	}
	body, err := json.Marshal(map[string]any{"start_defaults": StartDefaultsPolicy{Rules: rules}})
	if err != nil {
		t.Fatal(err)
	}
	if code, got := putStartDefaults(t, s, roleFull, "", string(body)); code != http.StatusOK {
		t.Fatalf("the maximum single-name policy was refused: %d %s", code, got[:200])
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/assistant/start-defaults", nil))
	got := rec.Body.Bytes()
	if len(got) >= startDefaultsBodyLimit {
		t.Fatalf("the dual-name read is %d bytes, at or over the %d-byte write limit; a GET-to-PUT round-trip would break",
			len(got), startDefaultsBodyLimit)
	}
	// The read carries both names with equal values, so it is itself accepted as a write (this is what an older or
	// newer client does when it saves the policy back untouched).
	if code, out := putStartDefaults(t, s, roleFull, "", string(got)); code != http.StatusOK {
		t.Fatalf("the dual-name read could not be written back: %d %s", code, out[:200])
	}
}

// The preview answers what a real start would do, not just what the policy literally says: an omitted role is a
// worker (roleOf), the goal follows startGoalSource (nothing without criteria, Codex's default when no rule names
// one, off turning even that off), and effort/ultracode reach Claude Code only. Candidate rows come from the same
// injected source a start would read.
func TestStartDefaultsPreviewMatchesRealStartSemantics(t *testing.T) {
	s, _ := defaultsHub(t)
	post := func(body string) map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/assistant/start-defaults/preview", strings.NewReader(body))
		req.Header.Set(RoleHeader, roleFull)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	// An omitted role is a worker: a worker-scoped rule matches a preview that never names a role.
	out := post(`{"task":{"machine":"box"},"start_defaults":{"rules":[{"applies_when":{"role":"worker"},"then":{"effort":"max"}}]}}`)
	if out["matched"] != true || out["effort"] != "max" {
		t.Fatalf("an omitted role did not match the worker rule: %v", out)
	}
	// goal done_when with no criteria: a real start sets no goal, so the preview must not echo it.
	out = post(`{"task":{"machine":"box","done_when":0},"start_defaults":{"rules":[{"applies_when":{},"then":{"goal":"done_when"}}]}}`)
	if _, ok := out["goal"]; ok {
		t.Fatalf("a goal was previewed with no acceptance criteria: %v", out)
	}
	// The same rule with criteria does set it.
	out = post(`{"task":{"machine":"box","done_when":2},"start_defaults":{"rules":[{"applies_when":{},"then":{"goal":"done_when"}}]}}`)
	if out["goal"] != "done_when" {
		t.Fatalf("the goal was not previewed with criteria: %v", out)
	}
	// No fitting rule: a new Codex start takes the default goal, Claude Code takes none.
	out = post(`{"agent":"codex","task":{"machine":"box","done_when":2}}`)
	if out["goal_by_default"] != true {
		t.Fatalf("the Codex default goal was not previewed: %v", out)
	}
	out = post(`{"task":{"machine":"box","done_when":2}}`)
	if _, ok := out["goal_by_default"]; ok {
		t.Fatalf("Claude Code should take no default goal: %v", out)
	}
	// goal off turns the Codex default off too.
	out = post(`{"agent":"codex","task":{"machine":"box","done_when":2},"start_defaults":{"rules":[{"applies_when":{},"then":{"goal":"off"}}]}}`)
	if _, ok := out["goal"]; ok {
		t.Fatalf("goal off still previewed a goal: %v", out)
	}
	if _, ok := out["goal_by_default"]; ok {
		t.Fatalf("goal off left the Codex default on: %v", out)
	}
	// Effort and ultracode reach Claude Code only: a Codex preview reports neither, though the policy names both.
	out = post(`{"agent":"codex","task":{"machine":"box"},"start_defaults":{"rules":[{"applies_when":{},"then":{"effort":"max","ultracode":true}}]}}`)
	if _, ok := out["effort"]; ok {
		t.Fatalf("a Codex preview advertised effort: %v", out)
	}
	if _, ok := out["ultracode"]; ok {
		t.Fatalf("a Codex preview advertised ultracode: %v", out)
	}
	// The preview reads the injected candidate source, the same one a start uses.
	s.startCandidates = stubCandidateSource{rows: map[string]startCandidate{
		"deep":  {Name: "deep", Launcher: "gateway", Available: false, Reason: "gateway is not available yet"},
		"quick": {Name: "quick", Launcher: "gateway", Available: true},
	}}
	out = post(`{"task":{"machine":"box"},"start_defaults":{"rules":[{"applies_when":{},"then":{"candidates":[{"name":"deep"},{"name":"quick"}]}}]}}`)
	usable, _ := out["usable"].([]any)
	if len(usable) != 1 || usable[0].(map[string]any)["name"] != "quick" {
		t.Fatalf("the preview did not use the injected candidate source: %v", out["usable"])
	}
	passed, _ := out["passed_over"].([]any)
	if len(passed) != 1 || passed[0].(map[string]any)["name"] != "deep" {
		t.Fatalf("the preview did not report the injected source's unavailable row: %v", out["passed_over"])
	}
}

// The preview writes nothing anywhere in the assistant's settings, not just the policy field, and starts no Agent.
func TestStartDefaultsPreviewLeavesAllSettingsUntouched(t *testing.T) {
	s, f := defaultsHub(t)
	ctx := context.Background()
	setSettings(t, s, func(v *Settings) {
		v.StartDefaults = &StartDefaultsPolicy{By: "owner", Rules: []StartDefaultsRule{{Then: StartDefaultsThen{Effort: "max"}}}}
	})
	before, err := json.Marshal(s.settings(ctx))
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"task":{"machine":"box"}}`,
		`{"task":{"machine":"box"},"start_defaults":{"rules":[{"applies_when":{},"then":{"candidates":[{"name":"deep"}],"effort":"low","ultracode":true}}]}}`,
	} {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/assistant/start-defaults/preview", strings.NewReader(body))
		req.Header.Set(RoleHeader, roleFull)
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("preview: %d %s", rec.Code, rec.Body.String())
		}
	}
	after, _ := json.Marshal(s.settings(ctx))
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the preview changed settings:\nbefore %s\nafter  %s", before, after)
	}
	if len(startsOf(f)) != 0 || s.taskStarting.Load() != 0 {
		t.Fatalf("the preview started or claimed work: starts %v", startsOf(f))
	}
}

// Accepting both aliases with equal values keeps their content: the stored rule still scopes to the role it named,
// so a regression that dropped the match on a both-names write would be caught.
func TestStartDefaultsAliasAgreementKeepsTheMatch(t *testing.T) {
	s, _ := defaultsHub(t)
	if code, got := putStartDefaults(t, s, roleFull, "",
		`{"start_defaults":{"rules":[{"when":{"role":"worker"},"applies_when":{"role":"worker"},"then":{"effort":"max"}}]}}`); code != http.StatusOK {
		t.Fatalf("both-names write: %d %s", code, got)
	}
	p := s.activeStartDefaults(context.Background())
	if p == nil || len(p.Rules) != 1 || p.Rules[0].When.Role != "worker" {
		t.Fatalf("the both-names write lost the match: %+v", p)
	}
	if startDefaultsMatch(p.Rules[0].When, StartFacts{Role: "leader"}) {
		t.Fatal("the stored rule now matches leaders too")
	}
	if !startDefaultsMatch(p.Rules[0].When, StartFacts{Role: "worker"}) {
		t.Fatal("the stored rule no longer matches workers")
	}
}

// The production roll really draws from the trusted entropy source: point it at a failing reader and the roll must
// surface that failure (the package's panic precedent), never fall back to a predictable value.
func TestPresetIntNReadsTheTrustedEntropySource(t *testing.T) {
	oldReader, oldRoll := presetEntropy, presetIntN
	t.Cleanup(func() { presetEntropy, presetIntN = oldReader, oldRoll })
	presetIntN = oldRoll // the production implementation, not another test's stub
	presetEntropy = failingEntropy{}
	defer func() {
		if recover() == nil {
			t.Fatal("a failing entropy source did not fail the roll: the value came from somewhere else")
		}
	}()
	presetIntN(4)
}

// failingEntropy always fails, to prove the roll reads the entropy source.
type failingEntropy struct{}

func (failingEntropy) Read([]byte) (int, error) { return 0, errors.New("entropy unavailable") }
