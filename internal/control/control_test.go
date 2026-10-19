package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0x3639/az-vote-delegate/internal/config"
	"github.com/0x3639/az-vote-delegate/internal/delegate"
	"github.com/0x3639/az-vote-delegate/internal/node"
	"github.com/0x3639/znn-sdk-go/api/embedded"
)

var id = strings.Repeat("a", 64)

type fakeNode struct {
	owner   *delegate.Vote
	block   bool
	started chan struct{}
	sends   int
}

func (f *fakeNode) Check(context.Context) error { return nil }
func (f *fakeNode) Close()                      {}
func (f *fakeNode) Pillars(context.Context) (map[string]*embedded.PillarInfo, error) {
	return map[string]*embedded.PillarInfo{"A": {Name: "A"}, "B": {Name: "B"}, "C": {Name: "C"}}, nil
}
func (f *fakeNode) Participation(context.Context) ([]node.Participation, error) {
	return []node.Participation{{Name: "B", Voted: 3, Total: 4}}, nil
}
func (f *fakeNode) ListVotables(context.Context) ([]delegate.Votable, error) {
	return []delegate.Votable{{ID: id, Kind: "phase", Name: "Delivery payment"}}, nil
}
func (f *fakeNode) PillarVotes(_ context.Context, name string, _ []string) (delegate.Votes, error) {
	yes := delegate.Yes
	if name == "B" {
		return delegate.Votes{id: &yes}, nil
	}
	return delegate.Votes{id: f.owner}, nil
}
func (f *fakeNode) SourceVotes(ctx context.Context, follow string, ids []string) (delegate.Votes, error) {
	return f.PillarVotes(ctx, follow, ids)
}
func (f *fakeNode) Executed(context.Context, string) (bool, error) { return true, nil }
func (f *fakeNode) Send(ctx context.Context, _ config.Delegation, _ string, v delegate.Vote, guard func() error, journal func(string) error) error {
	if f.block {
		close(f.started)
		<-ctx.Done()
		return ctx.Err()
	}
	if err := guard(); err != nil {
		return err
	}
	if err := journal(strings.Repeat("b", 64)); err != nil {
		return err
	}
	f.owner = &v
	f.sends++
	return nil
}
func manager(t *testing.T) (*Manager, *fakeNode) {
	t.Helper()
	dir := t.TempDir()
	c := config.Config{Live: true, ChainID: 1, StatePath: filepath.Join(dir, "state.json"), PollIntervalSeconds: 30, PowTimeoutSeconds: 30, Delegations: []config.Delegation{{PillarName: "A", Follow: "B"}}}
	f := &fakeNode{}
	m := &Manager{cfg: c, path: filepath.Join(dir, "config.json"), log: slog.New(slog.NewTextHandler(io.Discard, nil)), wake: make(chan struct{}, 1), fatal: make(chan error, 1)}
	m.factory = func(config.Config) (backend, error) { return f, nil }
	m.engine = &delegate.Engine{State: delegate.NewState(1), PollInterval: 30 * time.Second, PowTimeout: time.Minute, Save: func(s *delegate.State) error { return delegate.SaveState(c.StatePath, s) }}
	m.snapshot(nil, nil)
	return m, f
}
func command(t *testing.T, m *Manager, req Request) Response {
	t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest("POST", "/command", bytes.NewReader(b)))
	var response Response
	if err = json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(rec.Body.String(), err)
	}
	return response
}
func TestUIInterruptsCopyAndPersistsManualIntent(t *testing.T) {
	m, f := manager(t)
	f.block = true
	f.started = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- m.poll(context.Background()) }()
	select {
	case <-f.started:
	case <-time.After(3 * time.Second):
		t.Fatal("copy did not start")
	}
	// The preview remains readable while the signing worker is busy.
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest("GET", "/status", nil))
	if !strings.Contains(rec.Body.String(), "Delivery payment") {
		t.Fatal("UI has no actionable preview during PoW")
	}
	response := command(t, m, Request{Action: "vote", Pillar: "A", ID: id, Vote: "no"})
	if response.Error != "" {
		t.Fatal(response.Error)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	state, err := delegate.LoadState(m.cfg.StatePath, 1)
	if err != nil {
		t.Fatal(err)
	}
	if state.Entries("A")[id].ManualVote == nil || *state.Entries("A")[id].ManualVote != delegate.No {
		t.Fatal("manual choice acknowledged without durable intent")
	}
	f.block = false
	if err = m.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.sends != 1 || *f.owner != delegate.No {
		t.Fatal("copy won over UI manual vote")
	}
}
func TestPauseWorksWhenNodeUnavailable(t *testing.T) {
	m, _ := manager(t)
	m.factory = func(config.Config) (backend, error) { return nil, errors.New("offline") }
	response := command(t, m, Request{Action: "mode", Live: false})
	if response.Error != "" || !m.engine.DryRun || m.cfg.Live {
		t.Fatal("pause failed", response)
	}
}

func TestExplicitModesPersistAndDryRunDoesNotSubmit(t *testing.T) {
	m, f := manager(t)
	m.cfg.NodeURL = "http://127.0.0.1:1" // The injected backend performs all reads.
	m.cfg.Delegations[0].ProducerKeyFile = filepath.Join(t.TempDir(), "unused-key")
	m.cfg.Delegations[0].PasswordEnv = "UNUSED_TEST_PASSWORD"
	for _, live := range []bool{false, true, false} {
		before := f.sends
		response := command(t, m, Request{Action: "mode", Live: live})
		if response.Error != "" {
			t.Fatal(response.Error)
		}
		saved, err := config.Load(m.path)
		if err != nil || saved.Live != live || m.engine.DryRun == live {
			t.Fatalf("mode was not saved/applied: live=%v, config=%+v, error=%v", live, saved, err)
		}
		if err := m.poll(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !live && f.sends != before {
			t.Fatal("dry run submitted a vote")
		}
		if live && f.sends != before+1 {
			t.Fatal("live mode did not submit the delegate's vote")
		}
	}
}
func TestFailedManualSaveIsFatal(t *testing.T) {
	m, f := manager(t)
	m.engine.Save = func(*delegate.State) error { return errors.New("disk full") }
	response := command(t, m, Request{Action: "vote", Pillar: "A", ID: id, Vote: "abstain"})
	if response.Error == "" || f.sends != 0 {
		t.Fatal("manual failure acknowledged as success")
	}
	select {
	case err := <-m.fatal:
		if !errors.Is(err, delegate.ErrPersistence) {
			t.Fatal(err)
		}
	default:
		t.Fatal("state persistence error was not fatal")
	}
}
func TestFollowChangesPreserveManualOverride(t *testing.T) {
	m, _ := manager(t)
	vote := delegate.Yes
	m.engine.State.Entries("A")[id] = &delegate.Entry{ManualVote: &vote, OwnerOverride: true}
	response := command(t, m, Request{Action: "follow", Pillar: "A", Follow: "C"})
	if response.Error != "" || m.cfg.Delegations[0].Follow != "C" || m.engine.State.Entries("A")[id].ManualVote == nil {
		t.Fatal(response)
	}
}

func TestFreshRequestsAndParticipationDoNotAcquireSigningLock(t *testing.T) {
	m, f := manager(t)
	// The cached status predates this vote. The menu must read fresh votes.
	no := delegate.No
	f.owner = &no
	m.mu.Lock()
	defer m.mu.Unlock()
	done := make(chan Response, 1)
	go func() { done <- command(t, m, Request{Action: "requests"}) }()
	select {
	case res := <-done:
		if res.Error != "" || res.Snapshot == nil || len(res.Snapshot.Rows) != 1 || !delegate.Equal(res.Snapshot.Rows[0].Owner, &no) {
			t.Fatalf("fresh request read: %+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request list blocked on signing lock")
	}
	res := command(t, m, Request{Action: "pillars"})
	if res.Error != "" || len(res.Candidates) != 1 || res.Candidates[0].Voted != 3 || res.Candidates[0].Total != 4 {
		t.Fatalf("participation lost: %+v", res)
	}
	if len(m.engine.State.Pillars) != 0 || f.sends != 0 {
		t.Fatal("read-only menu mutated voting state")
	}
}

func TestUnansweredMenuRejectsVoteCompletedSinceListing(t *testing.T) {
	m, f := manager(t)
	for _, vote := range []delegate.Vote{delegate.Yes, delegate.No, delegate.Abstain} {
		f.owner = &vote
		res := command(t, m, Request{Action: "vote", Pillar: "A", ID: id, Vote: "yes", OnlyUnvoted: true})
		if !strings.Contains(res.Error, "already voted") || len(m.engine.State.Pillars) != 0 {
			t.Fatalf("overwrote an answered request: %+v", res)
		}
	}
	f.owner = nil
	res := command(t, m, Request{Action: "vote", Pillar: "A", ID: id, Vote: "no", OnlyUnvoted: true})
	if res.Error != "" || m.engine.State.Pillars["A"][id].ManualVote == nil {
		t.Fatalf("unanswered phase could not be voted on: %+v", res)
	}
}

func TestCopyDelaySavedAndAppliedWithoutRestart(t *testing.T) {
	m, f := manager(t)
	m.cfg.NodeURL = "http://127.0.0.1:1"
	m.cfg.Delegations[0].ProducerKeyFile = filepath.Join(t.TempDir(), "unused-key")
	m.cfg.Delegations[0].PasswordEnv = "UNUSED_TEST_PASSWORD"
	response := command(t, m, Request{Action: "delay", CopyDelayMinutes: 15})
	if response.Error != "" {
		t.Fatal(response.Error)
	}
	saved, err := config.Load(m.path)
	if err != nil || saved.CopyDelayMinutes != 15 || m.engine.CopyDelay != 15*time.Minute {
		t.Fatal(saved, err)
	}
	if err = m.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.sends != 0 || m.lastRows[0].Action != "waiting-delay" {
		t.Fatal("delay was not applied")
	}
	response = command(t, m, Request{Action: "delay", CopyDelayMinutes: 0})
	if response.Error != "" {
		t.Fatal(response.Error)
	}
	if err = m.poll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.sends != 1 {
		t.Fatal("clearing delay did not release waiting vote")
	}
}

func TestCopyDelayValidationAndSaveFailureLeaveSettingUnchanged(t *testing.T) {
	m, _ := manager(t)
	m.factory = func(config.Config) (backend, error) { return nil, errors.New("offline") }
	// Changing a local timing setting must work without a node connection.
	if response := command(t, m, Request{Action: "delay", CopyDelayMinutes: 30}); response.Error != "" {
		t.Fatal(response.Error)
	}
	for _, minutes := range []int{-1, 10081} {
		if response := command(t, m, Request{Action: "delay", CopyDelayMinutes: minutes}); response.Error == "" {
			t.Fatal("invalid delay accepted")
		}
	}
	m.path = filepath.Dir(m.path) // Cannot replace a directory with a config file.
	if response := command(t, m, Request{Action: "delay", CopyDelayMinutes: 5}); response.Error == "" {
		t.Fatal("failed save acknowledged")
	}
	if m.cfg.CopyDelayMinutes != 30 || m.engine.CopyDelay != 30*time.Minute {
		t.Fatal("failed command changed running setting")
	}
}
