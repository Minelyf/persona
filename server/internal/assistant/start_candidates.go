package assistant

import (
	"context"
	"slices"

	"github.com/nwverse/persona/server/internal/proto"
)

// startCandidate is one preset a start-defaults rule may choose on a machine: whether it can start there now, and
// why not. Availability mirrors the start path exactly (a hidden launcher whose health check passes still counts,
// an absent health record leaves a preset offered), so a preview and a real start never disagree.
type startCandidate struct {
	Name      string
	Launcher  string
	Available bool
	Reason    string // why it cannot start now; "" when Available
}

// startCandidateSource lists the presets a rule can pick from on a machine. The built-in source reads the active
// model profile with the machine's launcher list and health, the same data GET /api/v1/assistant/worker-presets
// serves. A dynamic candidates source implements this one interface and is set on the Service at construction:
// rule matching, priority/weight selection and the per-try retry reset in start_defaults.go stay unchanged.
type startCandidateSource interface {
	candidates(ctx context.Context, machine string) (map[string]startCandidate, error)
}

// candidateSource answers with the Service's source, or the model-profile one when none was injected.
func (s *Service) candidateSource() startCandidateSource {
	if s.startCandidates != nil {
		return s.startCandidates
	}
	return profileCandidateSource{s: s}
}

// profileCandidateSource is the built-in startCandidateSource: the active model profile's tiers, judged on the
// machine's own launcher list and health (launchersHere, hiddenUsable, launcherUnusable), never pickerTiers'
// filtered list, which would hide the reason a preset was passed over.
type profileCandidateSource struct{ s *Service }

func (p profileCandidateSource) candidates(ctx context.Context, machine string) (map[string]startCandidate, error) {
	prof, _, err := p.s.activeModelProfile(ctx)
	if prof == nil || err != nil {
		return nil, err
	}
	// Read the machine's launcher list once per call; hidden launchers still start (#4661), and hiddenUsable says
	// whether a hidden one runs, so no preset needs another launcher-list RPC.
	list, listKnown := p.s.launchersHere(ctx, machine)
	hidden := p.s.hiddenLaunchers(ctx)
	out := make(map[string]startCandidate, len(prof.Tiers))
	for name, tier := range prof.Tiers {
		c := startCandidate{Name: name, Launcher: tier.Launcher, Available: true}
		// A hidden launcher is startable when its health check passes; a plain one must be in the machine's list.
		has := !listKnown || slices.ContainsFunc(list, func(l proto.Launcher) bool { return l.Name == tier.Launcher })
		if !has && slices.Contains(hidden, tier.Launcher) && p.s.hiddenUsable(ctx, machine, tier.Launcher) {
			has = true
		}
		switch {
		case listKnown && !has:
			c.Available, c.Reason = false, "this computer has no launcher "+tier.Launcher
		default:
			if _, bad := p.s.launcherUnusable(ctx, machine, tier.Launcher); bad {
				c.Available, c.Reason = false, tier.Launcher+" is not available yet"
			}
		}
		out[name] = c
	}
	return out, nil
}
