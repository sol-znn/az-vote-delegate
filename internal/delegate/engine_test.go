package delegate

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/0x3639/az-vote-delegate/internal/config"
)

var testID = strings.Repeat("a", 64)
var testHash = strings.Repeat("b", 64)

func ptr(v Vote) *Vote { return &v }
func TestVoteMapping(t *testing.T) {
	for i, name := range []string{"yes", "no", "abstain"} {
		v, err := ParseVote(name)
		if err != nil || v != Vote(i) || v.String() != name {
			t.Fatalf("mapping %s: %v %v", name, v, err)
		}
	}
	if _, err := ParseVote("YES"); err == nil {
		t.Fatal("invalid vote accepted")
	}
	if Vote(3).Valid() || Vote(255).String() != "invalid" {
		t.Fatal("invalid vote mapped")
	}
}
func TestDecide(t *testing.T) {
	cases := []struct {
		name     string
		entry    Entry
		a, b     *Vote
		want     string
		override bool
	}{
		{"nothing", Entry{}, nil, nil, "no-delegate-vote", false},
		{"new yes", Entry{}, nil, ptr(Yes), "mirror", false},
		{"new no", Entry{}, nil, ptr(No), "mirror", false},
		{"abstain", Entry{}, nil, ptr(Abstain), "mirror", false},
		{"in sync", Entry{Cast: ptr(Yes)}, ptr(Yes), ptr(Yes), "in-sync", false},
		{"delegate changed", Entry{Cast: ptr(Yes)}, ptr(Yes), ptr(No), "mirror", false},
		{"manual", Entry{Cast: ptr(Yes)}, ptr(Abstain), ptr(No), "owner-override", true},
		{"unknown matches delegate", Entry{}, ptr(Yes), ptr(Yes), "owner-override", true},
		{"state lost", Entry{}, ptr(No), ptr(Yes), "owner-override", true},
		{"sticky when matches old cast", Entry{Cast: ptr(Yes), OwnerOverride: true}, ptr(Yes), ptr(No), "owner-override", true},
		{"reset", Entry{Cast: ptr(Yes)}, nil, ptr(No), "mirror", false},
		{"override reset", Entry{Cast: ptr(Yes), OwnerOverride: true}, nil, ptr(No), "mirror", false},
		{"reset before delegate votes", Entry{Cast: ptr(Yes), OwnerOverride: true}, nil, nil, "no-delegate-vote", false},
		{"manual without delegate", Entry{}, ptr(Yes), nil, "owner-override", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(&tc.entry, tc.a, tc.b)
			if got != tc.want || tc.entry.OwnerOverride != tc.override {
				t.Fatalf("%s override=%v, want %s override=%v", got, tc.entry.OwnerOverride, tc.want, tc.override)
			}
		})
	}
}

type fakeBackend struct {
	vs                             []Votable
	votes                          map[string]Votes
	sends                          int
	sendErr, errorList, errorVotes error
	during                         func()
	executed                       bool
	journal                        bool
}

func fixture() (*Engine, *fakeBackend, []config.Delegation) {
	f := &fakeBackend{vs: []Votable{{ID: testID}}, votes: map[string]Votes{"A": {}, "B": {testID: ptr(Yes)}}, executed: true}
	e := &Engine{Backend: f, State: NewState(1), Save: func(*State) error { return nil }, PollInterval: 30 * time.Second, PowTimeout: time.Second, Now: func() time.Time { return time.Unix(10000, 0) }}
	return e, f, []config.Delegation{{PillarName: "A", Follow: "B"}}
}
func (f *fakeBackend) ListVotables(context.Context) ([]Votable, error) { return f.vs, f.errorList }
func (f *fakeBackend) PillarVotes(_ context.Context, name string, ids []string) (Votes, error) {
	v := Votes{}
	for _, id := range ids {
		v[id] = f.votes[name][id]
	}
	return v, f.errorVotes
}
func (f *fakeBackend) SourceVotes(ctx context.Context, follow string, ids []string) (Votes, error) {
	return f.PillarVotes(ctx, follow, ids)
}
func (f *fakeBackend) Executed(context.Context, string) (bool, error) { return f.executed, nil }
func (f *fakeBackend) Send(_ context.Context, d config.Delegation, id string, v Vote, guard func() error, journal func(string) error) error {
	f.sends++
	if f.during != nil {
		f.during()
	}
	if err := guard(); err != nil {
		return err
	}
	if f.sendErr != nil && !f.journal {
		return f.sendErr
	}
	if err := journal(testHash); err != nil {
		return err
	}
	if f.sendErr != nil {
		return f.sendErr
	}
	f.votes[d.PillarName][id] = ptr(v)
	return nil
}
func reconcile(t *testing.T, e *Engine, ds []config.Delegation) []Row {
	t.Helper()
	rows, err := e.Reconcile(context.Background(), ds)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
func TestLifecycle(t *testing.T) {
	e, f, ds := fixture()
	reconcile(t, e, ds)
	if f.sends != 1 {
		t.Fatal("initial mirror missing")
	}
	reconcile(t, e, ds)
	reconcile(t, e, ds)
	if f.sends != 1 {
		t.Fatal("duplicate send")
	}
	f.votes["B"][testID] = ptr(No)
	reconcile(t, e, ds)
	reconcile(t, e, ds)
	if f.sends != 2 || *f.votes["A"][testID] != No {
		t.Fatal("delegate change not mirrored")
	}
	f.votes["A"][testID] = ptr(Abstain)
	reconcile(t, e, ds)
	f.votes["A"][testID] = ptr(No)
	reconcile(t, e, ds)
	if f.sends != 2 || !e.State.Entries("A")[testID].OwnerOverride {
		t.Fatal("owner override was not sticky")
	}
	f.votes["A"][testID] = nil
	f.votes["B"][testID] = nil
	reconcile(t, e, ds)
	if e.State.Entries("A")[testID].OwnerOverride {
		t.Fatal("reset did not clear override")
	}
	f.votes["B"][testID] = ptr(Abstain)
	reconcile(t, e, ds)
	if f.sends != 3 || *f.votes["A"][testID] != Abstain {
		t.Fatal("reset not mirrored")
	}
}
func TestPendingAndRestartDoNotDuplicate(t *testing.T) {
	e, f, ds := fixture()
	f.executed = false
	reconcile(t, e, ds)
	f.votes["A"][testID] = nil
	path := t.TempDir() + "/state.json"
	if err := SaveState(path, e.State); err != nil {
		t.Fatal(err)
	}
	s, err := LoadState(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	e.State = s
	for range 3 {
		rows := reconcile(t, e, ds)
		if rows[0].Action != "pending" {
			t.Fatal(rows)
		}
	}
	if f.sends != 1 {
		t.Fatal("duplicated pending vote")
	}
}
func TestUnknownPublishOutcomeIsHeld(t *testing.T) {
	e, f, ds := fixture()
	f.journal = true
	f.executed = false
	f.sendErr = errors.New("response lost")
	if _, err := e.Reconcile(context.Background(), ds); err == nil {
		t.Fatal("error missing")
	}
	if e.State.Entries("A")[testID].Pending == nil {
		t.Fatal("lost journal")
	}
	reconcile(t, e, ds)
	if f.sends != 1 {
		t.Fatal("retried ambiguous publish")
	}
}
func TestRetryBackoffAndRejection(t *testing.T) {
	for _, journal := range []bool{false, true} {
		e, f, ds := fixture()
		f.journal = journal
		f.sendErr = ErrRejected
		now := time.Unix(10000, 0)
		e.Now = func() time.Time { return now }
		for i, wait := range []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second} {
			if _, err := e.Reconcile(context.Background(), ds); err == nil {
				t.Fatal("expected send error")
			}
			entry := e.State.Entries("A")[testID]
			if entry.Pending != nil || entry.RetryAt.Sub(now) != wait {
				t.Fatalf("retry %d: %+v", i, entry)
			}
			reconcile(t, e, ds)
			if f.sends != i+1 {
				t.Fatal("retry storm")
			}
			now = now.Add(wait)
		}
		entry := e.State.Entries("A")[testID]
		for range 50 {
			e.fail(entry)
		}
		if entry.RetryAt.Sub(now) != 30*time.Minute {
			t.Fatal("backoff not capped")
		}
	}
}
func TestGuardStopsChangedVote(t *testing.T) {
	for _, name := range []string{"A", "B"} {
		t.Run(name, func(t *testing.T) {
			e, f, ds := fixture()
			f.during = func() { f.votes[name][testID] = ptr(Abstain) }
			rows := reconcile(t, e, ds)
			if rows[0].Action != "changed" || e.State.Entries("A")[testID].Pending != nil {
				t.Fatal("published stale decision")
			}
		})
	}
}
func TestPersistenceFailurePreventsPublish(t *testing.T) {
	e, f, ds := fixture()
	count := 0
	e.Save = func(*State) error {
		count++
		if count == 2 {
			return errors.New("disk full")
		}
		return nil
	}
	_, err := e.Reconcile(context.Background(), ds)
	if !errors.Is(err, ErrPersistence) || f.votes["A"][testID] != nil {
		t.Fatal("published without journal", err)
	}
}
func TestDryRunDoesNotWriteOrSend(t *testing.T) {
	e, f, ds := fixture()
	e.DryRun = true
	e.Save = func(*State) error { t.Fatal("dry-run wrote state"); return nil }
	rows := reconcile(t, e, ds)
	if rows[0].Action != "would-mirror" || f.sends != 0 {
		t.Fatal(rows)
	}
}
func TestSnapshotFailureAndPruning(t *testing.T) {
	e, f, ds := fixture()
	e.State.Entries("A")[testID] = &Entry{OwnerOverride: true}
	f.vs = nil
	f.errorList = errors.New("RPC unavailable")
	if _, err := e.Reconcile(context.Background(), ds); err == nil {
		t.Fatal("expected list error")
	}
	if e.State.Entries("A")[testID] == nil {
		t.Fatal("state pruned on partial snapshot")
	}
	f.errorList = nil
	reconcile(t, e, ds)
	if len(e.State.Entries("A")) != 0 {
		t.Fatal("closed state retained")
	}
}
func TestVoteReadFailureAndCancellationNeverSend(t *testing.T) {
	e, f, ds := fixture()
	f.errorVotes = errors.New("RPC unavailable")
	if _, err := e.Reconcile(context.Background(), ds); err == nil || f.sends != 0 {
		t.Fatal("sent after failed reads")
	}
	f.errorVotes = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Reconcile(ctx, ds); !errors.Is(err, context.Canceled) || f.sends != 0 {
		t.Fatal("sent after cancellation")
	}
}

func TestObservedManualVoteDuringPreparationStaysSticky(t *testing.T) {
	e, f, ds := fixture()
	e.State.Entries("A")[testID] = &Entry{Cast: ptr(Yes)}
	f.votes["A"][testID] = ptr(Yes)
	f.votes["B"][testID] = ptr(No)
	f.during = func() { f.votes["A"][testID] = ptr(Abstain) }
	reconcile(t, e, ds)
	if !e.State.Entries("A")[testID].OwnerOverride {
		t.Fatal("observed manual vote not recorded")
	}
	f.votes["A"][testID] = ptr(Yes)
	f.during = nil
	rows := reconcile(t, e, ds)
	if rows[0].Action != "owner-override" || f.sends != 1 {
		t.Fatal("observed override lost after vote returned to previous value")
	}
}

func TestAppManualSameValueSurvivesDelegateChangeAndRestart(t *testing.T) {
	e, f, ds := fixture()
	reconcile(t, e, ds)
	reconcile(t, e, ds)
	path := t.TempDir() + "/state.json"
	e.Save = func(s *State) error { return SaveState(path, s) }
	if err := e.QueueManual(context.Background(), ds[0], testID, Yes); err != nil {
		t.Fatal(err)
	}
	reconcile(t, e, ds)
	state, err := LoadState(path, 1)
	if err != nil {
		t.Fatal(err)
	}
	e.State = state
	f.votes["B"][testID] = ptr(No)
	rows := reconcile(t, e, ds)
	if rows[0].Action != "manual-override" || f.sends != 1 || !e.State.Entries("A")[testID].ManualConfirmed {
		t.Fatal("same-value manual vote lost its override")
	}
	if err = e.Resume(context.Background(), ds[0], testID); err != nil {
		t.Fatal(err)
	}
	reconcile(t, e, ds)
	if f.sends != 2 || *f.votes["A"][testID] != No {
		t.Fatal("resume failed")
	}
}
func TestAppManualWaitsForInFlightCopy(t *testing.T) {
	e, f, ds := fixture()
	f.executed = false
	reconcile(t, e, ds)
	if err := e.QueueManual(context.Background(), ds[0], testID, No); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		reconcile(t, e, ds)
	}
	if f.sends != 1 {
		t.Fatal("manual raced with unconfirmed copy")
	}
	f.executed = true
	reconcile(t, e, ds)
	reconcile(t, e, ds)
	if f.sends != 2 || *f.votes["A"][testID] != No || !e.State.Entries("A")[testID].ManualConfirmed {
		t.Fatal("manual choice did not win after copy executed")
	}
	f.votes["B"][testID] = ptr(Abstain)
	reconcile(t, e, ds)
	if f.sends != 2 {
		t.Fatal("copy overwrote app manual choice")
	}
}
func TestPhasePayoutReplacementAndManualOverride(t *testing.T) {
	e, f, ds := fixture()
	f.vs[0].Kind = "phase"
	reconcile(t, e, ds)
	reconcile(t, e, ds)
	if err := e.QueueManual(context.Background(), ds[0], testID, Abstain); err != nil {
		t.Fatal(err)
	}
	reconcile(t, e, ds)
	reconcile(t, e, ds)
	if *f.votes["A"][testID] != Abstain {
		t.Fatal("manual phase payout failed")
	}
	replacement := strings.Repeat("c", 64)
	f.vs = []Votable{{ID: replacement, Kind: "phase"}}
	f.votes["B"][replacement] = ptr(No)
	reconcile(t, e, ds)
	if e.State.Entries("A")[testID] != nil || f.votes["A"][replacement] == nil || *f.votes["A"][replacement] != No {
		t.Fatal("updated payment request was not copied independently")
	}
}
func TestAppManualResetAndFailures(t *testing.T) {
	e, f, ds := fixture()
	if err := e.QueueManual(context.Background(), ds[0], testID, No); err != nil {
		t.Fatal(err)
	}
	f.sendErr = ErrRejected
	if _, err := e.Reconcile(context.Background(), ds); err == nil {
		t.Fatal("expected rejection")
	}
	if e.State.Entries("A")[testID].ManualVote == nil {
		t.Fatal("failed manual intent was lost")
	}
	f.sendErr = nil
	e.Now = func() time.Time { return time.Unix(20000, 0) }
	reconcile(t, e, ds)
	reconcile(t, e, ds)
	f.votes["A"][testID] = nil
	reconcile(t, e, ds)
	if e.State.Entries("A")[testID].ManualVote != nil || *f.votes["A"][testID] != Yes {
		t.Fatal("reset did not resume copying")
	}
	e.DryRun = true
	if err := e.QueueManual(context.Background(), ds[0], testID, No); err == nil {
		t.Fatal("dry-run accepted a manual vote")
	}
}

// Manual control is scoped to pillar + request ID, not to the whole service or
// to every phase of the same project. Exercise disk reloads and elapsed time.
func TestManualOverridesPersistWhileOtherRequestsKeepCopying(t *testing.T) {
	e, f, ds := fixture()
	project := strings.Repeat("1", 64)
	manualPhase := strings.Repeat("2", 64)
	copiedPhase := strings.Repeat("3", 64)
	otherProject := strings.Repeat("4", 64)
	f.vs = []Votable{
		{ID: project, Kind: "project"},
		{ID: manualPhase, Kind: "phase", ProjectID: project},
		{ID: copiedPhase, Kind: "phase", ProjectID: project},
		{ID: otherProject, Kind: "project"},
	}
	for _, v := range f.vs {
		f.votes["B"][v.ID] = ptr(Yes)
	}
	path := filepath.Join(t.TempDir(), "state.json")
	save := func(s *State) error { return SaveState(path, s) }
	e.Save = save
	for _, id := range []string{project, manualPhase} {
		if err := e.QueueManual(context.Background(), ds[0], id, No); err != nil {
			t.Fatal(err)
		}
	}
	restart := func() {
		t.Helper()
		state, err := LoadState(path, 1)
		if err != nil {
			t.Fatal(err)
		}
		// Create a new worker, rather than retaining any in-memory override state.
		e = &Engine{Backend: f, State: state, Save: save, PollInterval: 30 * time.Second, PowTimeout: time.Second}
	}
	restart() // Queued intent must survive a restart before the vote is sent.
	reconcile(t, e, ds)
	reconcile(t, e, ds)
	if f.sends != 4 {
		t.Fatalf("got %d sends, want two manual and two copied votes", f.sends)
	}
	restart() // Confirmed overrides must also survive upgrades/restarts.
	e.Now = func() time.Time { return time.Now().Add(90 * 24 * time.Hour) }
	for _, v := range f.vs {
		f.votes["B"][v.ID] = ptr(Abstain)
	}
	reconcile(t, e, ds)
	reconcile(t, e, ds)
	if f.sends != 6 {
		t.Fatalf("got %d sends, want only the two copied requests to change", f.sends)
	}
	for _, id := range []string{project, manualPhase} {
		entry := e.State.Entries("A")[id]
		if !Equal(f.votes["A"][id], ptr(No)) || entry.ManualVote == nil || !entry.ManualConfirmed {
			t.Fatalf("manual override lost for %s", id)
		}
	}
	for _, id := range []string{copiedPhase, otherProject} {
		if !Equal(f.votes["A"][id], ptr(Abstain)) || e.State.Entries("A")[id].ManualVote != nil {
			t.Fatalf("unrelated request %s stopped copying", id)
		}
	}
	// Returning one request to copying leaves the other manual override intact.
	if err := e.Resume(context.Background(), ds[0], manualPhase); err != nil {
		t.Fatal(err)
	}
	reconcile(t, e, ds)
	if f.sends != 7 || !Equal(f.votes["A"][manualPhase], ptr(Abstain)) || !Equal(f.votes["A"][project], ptr(No)) {
		t.Fatal("resuming one request affected another request's override")
	}
}
