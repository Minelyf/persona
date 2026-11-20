package assistant

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/nwverse/persona/server/internal/apierr"
	"github.com/nwverse/persona/server/internal/proto"
)

// startDefaultsPreviewRoutes serves POST /api/v1/assistant/start-defaults/preview: what a policy would do for a
// task, read-only. It is the same matching and candidate selection a real start uses (matchStartDefaults,
// chooseStartPreset), so a preview and a start cannot disagree. Nothing is written: no settings change, no task or
// plan is touched, no Agent starts, no admission claim, no note on any task. A draft policy in the body is only
// decoded and validated, never stored, so it needs no expires_at. Registered from startDefaultsRoutes
// (start_defaults_routes.go), same assistant.start_defaults scope and read-allowed role as the GET.
func (s *Service) startDefaultsPreviewRoutes() {
	s.handle("POST /api/v1/assistant/start-defaults/preview", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Task struct {
				Machine    string `json:"machine"`
				Role       string `json:"role,omitempty"`
				Readonly   bool   `json:"readonly,omitempty"`
				Supervisor string `json:"supervisor,omitempty"`
				DoneWhen   int    `json:"done_when,omitempty"`
			} `json:"task"`
			Caller struct {
				Launcher  string `json:"launcher,omitempty"`
				Model     string `json:"model,omitempty"`
				Tier      string `json:"tier,omitempty"`
				Effort    string `json:"effort,omitempty"`
				Ultracode *bool  `json:"ultracode,omitempty"`
			} `json:"caller"`
			StartDefaults *StartDefaultsPolicy `json:"start_defaults,omitempty"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, startDefaultsBodyLimit))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			apierr.Write(w, http.StatusBadRequest, err)
			return
		}
		ctx := r.Context()
		if strings.TrimSpace(in.Task.Machine) == "" {
			apierr.Write(w, http.StatusBadRequest, errors.New("task.machine is required"))
			return
		}
		m, ok := s.machineByName(ctx, in.Task.Machine)
		if !ok {
			apierr.Write(w, http.StatusNotFound, errUnknownMachine)
			return
		}
		if in.Task.Role != "" && !slices.Contains(startDefaultsRoles, in.Task.Role) {
			apierr.Write(w, http.StatusBadRequest, errors.New("task.role must be one of "+strings.Join(startDefaultsRoles, ", ")))
			return
		}
		if in.Task.Supervisor != "" && !slices.Contains(dispatchClasses, in.Task.Supervisor) {
			apierr.Write(w, http.StatusBadRequest, errors.New("task.supervisor must be a dispatch conversation class"))
			return
		}
		if in.Task.DoneWhen < 0 {
			apierr.Write(w, http.StatusBadRequest, errors.New("task.done_when must be 0 or more"))
			return
		}
		if in.Caller.Effort != "" && !slices.Contains(proto.EffortLevels, in.Caller.Effort) {
			apierr.Write(w, http.StatusBadRequest, errors.New("caller.effort must be one of "+strings.Join(proto.EffortLevels, ", ")))
			return
		}
		policy := in.StartDefaults
		if policy != nil {
			// A draft is never stored: validate it as the owner's write would, but with no expiry requirement.
			if err := policy.validate(true, time.Now().UTC()); err != nil {
				apierr.Write(w, http.StatusBadRequest, err)
				return
			}
		} else {
			policy = s.activeStartDefaults(ctx)
		}
		facts := StartFacts{
			Machine: m.ID, Role: in.Task.Role, Readonly: in.Task.Readonly,
			Supervisor: in.Task.Supervisor, DoneWhen: in.Task.DoneWhen,
		}
		apierr.WriteJSON(w, s.previewStartDefaults(ctx, policy, facts, in.Caller.Launcher, in.Caller.Model, in.Caller.Tier, in.Caller.Effort, in.Caller.Ultracode))
	})
}

// previewStartDefaults answers the preview body. policy nil or with no fitting rule reports matched=false and the
// caller's own start, which is exactly what happens without the feature. It performs only reads.
func (s *Service) previewStartDefaults(ctx context.Context, policy *StartDefaultsPolicy, facts StartFacts, launcher, model, tier, effort string, ultracode *bool) map[string]any {
	out := map[string]any{"matched": false}
	if policy != nil && policy.By != "" {
		out["by"] = policy.By
	}
	if policy == nil {
		return out
	}
	ruleIndex := -1
	for i, r := range policy.Rules {
		if startDefaultsMatch(r.When, facts) {
			ruleIndex = i
			break
		}
	}
	d, matched := matchStartDefaults(policy, facts)
	if !matched {
		return out
	}
	out["matched"] = true
	out["rule_index"] = ruleIndex
	out["applies_when"] = policy.Rules[ruleIndex].When
	if d.Goal != "" {
		out["goal"] = d.Goal
	}
	if d.PlanFirst {
		out["plan_first"] = true
	}
	// Caller values win, exactly as applyStartPreset and applyStartExtras decide. A caller launcher, model or tier
	// leaves no room for a preset at all; its effort, or its tier's effort, keeps the policy's effort out; an
	// explicit ultracode (on or off) keeps the policy's switch out.
	callerWins := []string{}
	callerNamedLaunch := strings.TrimSpace(launcher) != "" || strings.TrimSpace(model) != "" || strings.TrimSpace(tier) != ""
	tierEffort := false
	if strings.TrimSpace(tier) != "" {
		if tm, err := s.tierModel(ctx, tier); err == nil && tm.Effort != "" {
			tierEffort = true
		}
	}
	if strings.TrimSpace(effort) != "" || tierEffort {
		callerWins = append(callerWins, "effort")
	}
	if ultracode != nil {
		callerWins = append(callerWins, "ultracode")
	}
	if callerNamedLaunch {
		out["preset_blocked_by_caller"] = true
	} else if len(d.Presets) > 0 {
		// The very selection a start would run: same source, same priority/weight rule, same passed-over reasons.
		source, err := s.candidateSource().candidates(ctx, facts.Machine)
		if err != nil {
			out["candidates_error"] = err.Error()
		} else {
			usable, skipped := usablePresets(d.Presets, source)
			rows := make([]map[string]any, 0, len(usable))
			for _, c := range topPriorityGroup(usable) {
				rows = append(rows, map[string]any{"name": c.Name, "priority": c.Priority, "weight": c.Weight, "launcher": source[c.Name].Launcher})
			}
			out["usable"] = rows
			if len(skipped) > 0 {
				passed := make([]map[string]any, 0, len(skipped))
				for _, why := range skipped {
					name, reason, _ := strings.Cut(why, ": ")
					passed = append(passed, map[string]any{"name": name, "reason": reason})
				}
				out["passed_over"] = passed
			}
			// One sampled pick, labelled as such: it is illustrative, not a promise of what a later start rolls.
			if len(usable) > 0 {
				out["sample"] = weightedPreset(presetIntN, usable).Name
			}
		}
	}
	if d.Effort != "" {
		out["effort"] = d.Effort
	}
	if d.Ultracode != nil {
		out["ultracode"] = *d.Ultracode
	}
	if len(callerWins) > 0 {
		out["caller_wins"] = callerWins
	}
	return out
}
