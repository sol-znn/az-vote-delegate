package tui

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"

	"github.com/0x3639/az-vote-delegate/internal/config"
	"github.com/0x3639/az-vote-delegate/internal/control"
	"github.com/0x3639/az-vote-delegate/internal/node"
	"github.com/charmbracelet/huh"
)

func topCandidates(all []node.Participation, self string, shuffle func(int, func(int, int))) []node.Participation {
	candidates := make([]node.Participation, 0, len(all))
	for _, p := range all {
		if p.Name != self {
			candidates = append(candidates, p)
		}
	}
	swap := func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] }
	// Shuffle first to break equal-vote ties fairly, including the 30th slot.
	shuffle(len(candidates), swap)
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Voted > candidates[j].Voted })
	candidates = candidates[:min(30, len(candidates))]
	shuffle(len(candidates), swap)
	return candidates
}

func chooseDelegate(ctx context.Context, all []node.Participation, self, backLabel string) (string, error) {
	options := []huh.Option[string]{}
	for _, p := range topCandidates(all, self, rand.Shuffle) {
		label := Clean(p.Name) + " · no recorded requests"
		if p.Total > 0 {
			label = fmt.Sprintf("%s · %.1f%% participation · %d/%d requests", Clean(p.Name), 100*float64(p.Voted)/float64(p.Total), p.Voted, p.Total)
		}
		options = append(options, huh.NewOption(label, p.Name))
	}
	if len(options) == 0 {
		return "", errors.New("no other active pillars found; if you just upgraded, restart the voting service")
	}
	chosen := options[0].Value
	options = append(options, huh.NewOption("Copy an address (proxy votes)", byAddress), huh.NewOption(backLabel, ""))
	err := form(ctx, huh.NewSelect[string]().Title("Choose a pillar to copy").Description("Your pillar: "+Clean(self)+". Top 30 by recorded votes, shuffled; type to search.\nParticipation = votes / all retained project and phase requests, including closed requests. Not adjusted for when a pillar joined.\nApplies to projects and phase payouts; existing manual overrides stay in place.").Height(18).Filtering(true).Options(options...).Value(&chosen))
	if err == nil && chosen == "" {
		err = errBack
	}
	if err == nil && chosen == byAddress {
		chosen = ""
		err = form(ctx, huh.NewInput().Title("Address to copy").Description("Any z1 address. Its latest vote on each request is copied, such as one sent from a phone or a wallet you do not trust with your pillar keys. The contract does not count such votes for an address that runs no pillar.").Value(&chosen).Validate(func(s string) error {
			if _, ok := config.FollowAddress(s); !ok {
				return errors.New("enter a z1 address")
			}
			return nil
		}))
	}
	return chosen, err
}

// byAddress cannot collide with a pillar name, which never contains a NUL.
const byAddress = "\x00address"

func unvotedRows(s *control.Snapshot, phases bool) []int {
	indices := []int{}
	for i, r := range s.Rows {
		if (r.Votable.Kind == "phase") != phases || r.Owner != nil {
			continue
		}
		// A queued manual choice belongs in Activity, not the to-do list. A
		// pending copy remains selectable so the operator can still override it.
		if s.State != nil {
			e := s.State.Pillars[r.Pillar][r.Votable.ID]
			if e != nil && e.ManualVote != nil && !e.ManualConfirmed {
				continue
			}
		}
		indices = append(indices, i)
	}
	return indices
}
