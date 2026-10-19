package delegate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/0x3639/az-vote-delegate/internal/config"
)

type Vote uint8

const (
	Yes Vote = iota
	No
	Abstain
)

func (v Vote) Valid() bool { return v <= Abstain }
func (v Vote) String() string {
	switch v {
	case Yes:
		return "yes"
	case No:
		return "no"
	case Abstain:
		return "abstain"
	}
	return "invalid"
}
func ParseVote(s string) (Vote, error) {
	switch s {
	case "yes":
		return Yes, nil
	case "no":
		return No, nil
	case "abstain":
		return Abstain, nil
	}
	return 0, fmt.Errorf("invalid vote %q", s)
}
func Equal(a, b *Vote) bool { return a == nil && b == nil || a != nil && b != nil && *a == *b }
func VoteText(v *Vote) string {
	if v == nil {
		return "-"
	}
	return v.String()
}

type Votable struct {
	ID          string
	Kind        string
	Name        string
	ProjectID   string
	ProjectName string
	Description string
	URL         string
	ZNN         string
	QSR         string
	Expires     time.Time
}
type Votes map[string]*Vote

// Send must call guard after preparation and journal immediately before publish.
// journal persists the signed hash before any network submission can occur.
type Backend interface {
	ListVotables(context.Context) ([]Votable, error)
	PillarVotes(context.Context, string, []string) (Votes, error)
	// SourceVotes reads what a delegation copies: a pillar's votes, or the
	// latest vote blocks sent by an address (see config.FollowAddress).
	SourceVotes(context.Context, string, []string) (Votes, error)
	Send(context.Context, config.Delegation, string, Vote, func() error, func(string) error) error
	Executed(context.Context, string) (bool, error)
}

var ErrRejected = errors.New("node rejected publish")
var ErrChanged = errors.New("chain state changed during preparation; reconsider next poll")
var ErrPersistence = errors.New("cannot persist state; stopping")

// CopyObservation records when this source vote first needed copying. Its due
// time is computed from the current setting, so changing the delay applies to
// votes already waiting without losing their original observation time.
type CopyObservation struct {
	Follow     string    `json:"follow"`
	Vote       Vote      `json:"vote"`
	ObservedAt time.Time `json:"observedAt"`
}

type Entry struct {
	CopyObservation *CopyObservation `json:"copyObservation,omitempty"`
	Cast            *Vote            `json:"cast,omitempty"`
	BlockHash       string           `json:"blockHash,omitempty"`
	At              time.Time        `json:"at,omitempty"`
	OwnerOverride   bool             `json:"ownerOverride,omitempty"`
	ManualVote      *Vote            `json:"manualVote,omitempty"`
	ManualConfirmed bool             `json:"manualConfirmed,omitempty"`
	Pending         *Pending         `json:"pending,omitempty"`
	Failures        int              `json:"failures,omitempty"`
	RetryAt         time.Time        `json:"retryAt,omitempty"`
}
type Pending struct {
	Vote      Vote      `json:"vote"`
	BlockHash string    `json:"blockHash"`
	At        time.Time `json:"at"`
}
type State struct {
	Version int                          `json:"version"`
	ChainID uint64                       `json:"chainId"`
	Pillars map[string]map[string]*Entry `json:"pillars"`
}

func NewState(chainID uint64) *State {
	return &State{Version: 1, ChainID: chainID, Pillars: map[string]map[string]*Entry{}}
}
func (s *State) Entries(name string) map[string]*Entry {
	if s.Pillars[name] == nil {
		s.Pillars[name] = map[string]*Entry{}
	}
	return s.Pillars[name]
}

// Decide only changes observed override/reset state. Unknown existing votes are
// protected even if they happen to agree with the delegate.
func Decide(e *Entry, a, b *Vote) string {
	if a == nil {
		e.OwnerOverride = false
		e.Cast = nil
	} else if e.Cast == nil || !Equal(a, e.Cast) {
		e.OwnerOverride = true
	}
	if e.OwnerOverride {
		return "owner-override"
	}
	if b == nil {
		return "no-delegate-vote"
	}
	if Equal(a, b) {
		return "in-sync"
	}
	return "mirror"
}
