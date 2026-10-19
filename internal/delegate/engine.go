package delegate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/0x3639/az-vote-delegate/internal/config"
)

type Row struct {
	Pillar, Follow  string
	Votable         Votable
	Owner, Delegate *Vote
	Action          string
	CopyAfter       time.Time
}
type Engine struct {
	Backend      Backend
	State        *State
	Save         func(*State) error
	Log          *slog.Logger
	CopyDelay    time.Duration
	PollInterval time.Duration
	PowTimeout   time.Duration
	DryRun       bool
	Now          func() time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}
func (e *Engine) persist() error {
	if e.DryRun {
		return nil
	}
	if err := e.Save(e.State); err != nil {
		return fmt.Errorf("%w: %v", ErrPersistence, err)
	}
	return nil
}
func (e *Engine) fail(entry *Entry) {
	if entry.Failures < 32 {
		entry.Failures++
	}
	delay := e.PollInterval
	for i := 1; i < entry.Failures && delay < 30*time.Minute; i++ {
		delay *= 2
	}
	if delay > 30*time.Minute {
		delay = 30 * time.Minute
	}
	entry.RetryAt = e.now().Add(delay)
}

func (e *Engine) Reconcile(ctx context.Context, ds []config.Delegation) ([]Row, error) {
	votables, err := e.Backend.ListVotables(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(votables))
	active := map[string]bool{}
	for _, v := range votables {
		if active[v.ID] {
			return nil, errors.New("duplicate votable in snapshot")
		}
		active[v.ID] = true
		ids = append(ids, v.ID)
	}
	var rows []Row
	var failures []error
	for _, d := range ds {
		if err = ctx.Err(); err != nil {
			return rows, err
		}
		entries := e.State.Entries(d.PillarName)
		// Only prune after obtaining a complete, successful project snapshot.
		for id := range entries {
			if !active[id] {
				delete(entries, id)
			}
		}
		if len(ids) == 0 {
			continue
		}
		bVotes, err := e.Backend.SourceVotes(ctx, d.Follow, ids)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s delegate votes: %w", d.PillarName, err))
			continue
		}
		aVotes, err := e.Backend.PillarVotes(ctx, d.PillarName, ids)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s owner votes: %w", d.PillarName, err))
			continue
		}
		for _, v := range votables {
			if err = ctx.Err(); err != nil {
				return rows, err
			}
			a, b := aVotes[v.ID], bVotes[v.ID]
			if (a != nil && !a.Valid()) || (b != nil && !b.Valid()) {
				return rows, errors.New("invalid vote in snapshot")
			}
			entry := entries[v.ID]
			if entry == nil {
				entry = &Entry{}
				entries[v.ID] = entry
			}
			row := Row{Pillar: d.PillarName, Follow: d.Follow, Votable: v, Owner: a, Delegate: b}
			if entry.Pending != nil {
				entry.CopyObservation = nil
				// Wait for the contract's receive block before treating an absent or old
				// vote as a reset. Publishing a send block is not contract execution.
				done, err := e.Backend.Executed(ctx, entry.Pending.BlockHash)
				if err != nil {
					failures = append(failures, err)
					row.Action = "pending"
					rows = append(rows, row)
					continue
				}
				if !done {
					row.Action = "pending"
					rows = append(rows, row)
					e.log(row, false)
					continue
				}
				// Votes were fetched before the execution check; refresh to avoid using
				// a snapshot that predates the just-confirmed contract call.
				fresh, err := e.Backend.PillarVotes(ctx, d.PillarName, []string{v.ID})
				if err != nil {
					failures = append(failures, err)
					row.Action = "pending"
					rows = append(rows, row)
					continue
				}
				a = fresh[v.ID]
				row.Owner = a
				p := entry.Pending
				entry.Pending = nil
				if a != nil && *a == p.Vote {
					vote := p.Vote
					entry.Cast = &vote
					entry.BlockHash = p.BlockHash
					entry.At = p.At
					entry.Failures = 0
					entry.RetryAt = time.Time{}
				} else {
					e.fail(entry)
				}
				if err = e.persist(); err != nil {
					return rows, err
				}
			}
			// A disappeared daemon vote indicates a reset round, even if the
			// delegate has already voted the same way again.
			if entry.Cast != nil && a == nil {
				entry.CopyObservation = nil
			}
			previousOverride := entry.OwnerOverride
			target := b
			manual := entry.ManualVote != nil
			if manual && entry.ManualConfirmed && a == nil {
				// A confirmed manual vote disappeared: the round was reset.
				entry.ManualVote = nil
				entry.ManualConfirmed = false
				entry.OwnerOverride = false
				manual = false
			}
			if manual {
				target = entry.ManualVote
				entry.OwnerOverride = true
				if Equal(a, target) {
					entry.ManualConfirmed = true
				}
				row.Action = "manual-override"
				if !entry.ManualConfirmed {
					row.Action = "mirror"
				}
			} else {
				row.Action = Decide(entry, a, b)
			}
			if manual || row.Action != "mirror" || e.CopyDelay == 0 {
				entry.CopyObservation = nil
			} else {
				o := entry.CopyObservation
				if o == nil || o.Follow != d.Follow || o.Vote != *b {
					o = &CopyObservation{Follow: d.Follow, Vote: *b, ObservedAt: e.now()}
					entry.CopyObservation = o
				}
				row.CopyAfter = o.ObservedAt.Add(e.CopyDelay)
				if e.now().Before(row.CopyAfter) {
					row.Action = "waiting-delay"
				}
			}
			if row.Action == "mirror" && e.now().Before(entry.RetryAt) {
				row.Action = "backoff"
			}
			if row.Action == "mirror" {
				if e.DryRun {
					row.Action = "would-mirror"
					if manual {
						row.Action = "would-vote-manually"
					}
				} else {
					// Persist override/reset observations before attempting more side effects.
					if err = e.persist(); err != nil {
						return rows, err
					}
					sendCtx, cancel := context.WithTimeout(ctx, e.PowTimeout)
					guard := func() error { return e.guard(sendCtx, d, v.ID, entry, a, b, manual) }
					journal := func(hash string) error {
						entry.Pending = &Pending{Vote: *target, BlockHash: hash, At: e.now()}
						entry.CopyObservation = nil
						return e.persist()
					}
					err = e.Backend.Send(sendCtx, d, v.ID, *target, guard, journal)
					cancel()
					if errors.Is(err, ErrPersistence) {
						return rows, err
					}
					if err != nil {
						if errors.Is(err, ErrRejected) {
							entry.Pending = nil
						}
						if errors.Is(err, ErrChanged) {
							row.Action = "changed"
						} else {
							e.fail(entry)
							row.Action = "send-error"
							failures = append(failures, fmt.Errorf("%s %s send: %w", d.PillarName, v.ID, err))
						}
					}
					if err == nil && manual {
						row.Action = "manual-submitted"
					}
					// A successful publish remains pending until execution is observed.
					if err = e.persist(); err != nil {
						return rows, err
					}
				}
			}
			rows = append(rows, row)
			e.log(row, previousOverride)
		}
	}
	if err = e.persist(); err != nil {
		return rows, err
	}
	return rows, errors.Join(failures...)
}

func (e *Engine) guard(ctx context.Context, d config.Delegation, id string, entry *Entry, a, b *Vote, manual bool) error {
	vs, err := e.Backend.ListVotables(ctx)
	if err != nil {
		return err
	}
	active := false
	for _, v := range vs {
		if v.ID == id {
			active = true
			break
		}
	}
	if !active {
		return ErrChanged
	}
	bv, err := e.Backend.SourceVotes(ctx, d.Follow, []string{id})
	if err != nil {
		return err
	}
	av, err := e.Backend.PillarVotes(ctx, d.PillarName, []string{id})
	if err != nil {
		return err
	}
	if !Equal(av[id], a) {
		previousOverride := entry.OwnerOverride
		action := "manual-override"
		if !manual {
			action = Decide(entry, av[id], bv[id])
		}
		if err := e.persist(); err != nil {
			return err
		}
		e.log(Row{Pillar: d.PillarName, Follow: d.Follow, Votable: Votable{ID: id}, Owner: av[id], Delegate: bv[id], Action: action}, previousOverride)
		return ErrChanged
	}
	if !manual && !Equal(bv[id], b) {
		entry.CopyObservation = nil
		if err := e.persist(); err != nil {
			return err
		}
		return ErrChanged
	}
	return ctx.Err()
}
func (e *Engine) log(row Row, previousOverride bool) {
	if e.Log == nil {
		return
	}
	level := slog.LevelInfo
	if row.Action == "in-sync" || row.Action == "no-delegate-vote" || row.Action == "pending" || row.Action == "backoff" || row.Action == "waiting-delay" || ((row.Action == "owner-override" || row.Action == "manual-override") && previousOverride) {
		level = slog.LevelDebug
	}
	vote := row.Delegate
	if entry := e.State.Entries(row.Pillar)[row.Votable.ID]; entry != nil && entry.ManualVote != nil {
		vote = entry.ManualVote
	}
	e.Log.Log(context.Background(), level, "decision", "decision", row.Action, "pillar", row.Pillar, "follow", row.Follow, "id", row.Votable.ID, "vote", VoteText(vote))
}
