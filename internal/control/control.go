// Package control owns the only signer/state writer. The terminal UI talks to
// its owner-only Unix socket; it never opens a second signing account chain.
package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/0x3639/az-vote-delegate/internal/config"
	"github.com/0x3639/az-vote-delegate/internal/delegate"
	"github.com/0x3639/az-vote-delegate/internal/node"
	"github.com/0x3639/znn-sdk-go/api/embedded"
)

type Delegation struct {
	Pillar string
	Follow string
}
type Snapshot struct {
	CopyDelayMinutes int
	Live             bool
	ChainID          uint64
	Delegations      []Delegation
	Rows             []delegate.Row
	State            *delegate.State
	Updated          time.Time
	Error            string
}
type Request struct {
	CopyDelayMinutes int
	Action           string
	Pillar           string
	ID               string
	Vote             string
	Follow           string
	Live             bool
	OnlyUnvoted      bool
}
type Response struct {
	Snapshot   *Snapshot            `json:"snapshot,omitempty"`
	Candidates []node.Participation `json:"candidates,omitempty"`
	Message    string               `json:"message,omitempty"`
	Error      string               `json:"error,omitempty"`
}
type backend interface {
	delegate.Backend
	Check(context.Context) error
	Pillars(context.Context) (map[string]*embedded.PillarInfo, error)
	Participation(context.Context) ([]node.Participation, error)
	Close()
}

type Manager struct {
	readConfig config.Config
	lastRows   []delegate.Row
	factory    func(config.Config) (backend, error)
	mu         sync.Mutex
	cancelMu   sync.Mutex
	active     context.CancelFunc
	cacheMu    sync.RWMutex
	cache      []byte
	cfg        config.Config
	path       string
	keys       node.Keys
	engine     *delegate.Engine
	log        *slog.Logger
	wake       chan struct{}
	fatal      chan error
}

func SocketPath(c config.Config) string {
	return filepath.Join(filepath.Dir(c.StatePath), "control.sock")
}

func Run(ctx context.Context, path string, c config.Config, keys node.Keys, dry bool, log *slog.Logger, reset bool) error {
	// Dry-run also owns the control socket and may switch into live mode.
	unlock, err := delegate.LockState(c.StatePath)
	if err != nil {
		return err
	}
	defer unlock()
	state := delegate.NewState(c.ChainID)
	if !reset {
		state, err = delegate.LoadState(c.StatePath, c.ChainID)
		if err != nil {
			return err
		}
	}
	n, err := node.Open(c, keys, log)
	if err != nil {
		return err
	}
	err = n.Check(ctx)
	n.Close()
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return err
	}
	c.Live = !dry
	m := &Manager{readConfig: config.Config{NodeURL: c.NodeURL, ChainID: c.ChainID}, cfg: c, path: path, keys: keys, log: log, wake: make(chan struct{}, 1), fatal: make(chan error, 1)}
	m.engine = &delegate.Engine{State: state, Save: func(s *delegate.State) error { return delegate.SaveState(c.StatePath, s) }, Log: log, CopyDelay: c.CopyDelay(), PollInterval: c.PollInterval(), PowTimeout: c.PowTimeout(), DryRun: dry}
	if !dry {
		if err = m.engine.Save(state); err != nil {
			return err
		}
	}
	m.snapshot(nil, nil)
	socket := SocketPath(c)
	if info, err := os.Lstat(filepath.Dir(socket)); err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("the state directory must have mode 0700 for the private control socket; choose a dedicated directory")
	}
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return errors.New("control socket path is occupied by a non-socket file")
		}
		conn, dialErr := net.DialTimeout("unix", socket, time.Second)
		if dialErr == nil {
			_ = conn.Close()
			return errors.New("a control service is already running in this state directory")
		}
		if err = os.Remove(socket); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close(); _ = os.Remove(socket) }()
	if err = os.Chmod(socket, 0600); err != nil {
		return err
	}
	server := &http.Server{Handler: m, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 2 * time.Minute, IdleTimeout: 15 * time.Second}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- server.Serve(listener) }()
	defer func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		_ = server.Close()
	}()
	log.Info("control service ready", "socket", socket, "dryRun", dry)
	for ctx.Err() == nil {
		if err = m.poll(ctx); errors.Is(err, delegate.ErrPersistence) {
			return err
		}
		timer := time.NewTimer(c.PollInterval())
		select {
		case <-ctx.Done():
		case err = <-m.fatal:
			timer.Stop()
			return err
		case err = <-serveErrors:
			timer.Stop()
			return err
		case <-m.wake:
		case <-timer.C:
		}
		timer.Stop()
	}
	m.stopWork()
	// Wait for any accepted UI mutation, then flush before releasing the lock.
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.engine.DryRun {
		return m.engine.Save(m.engine.State)
	}
	return nil
}
func (m *Manager) stopWork() {
	m.cancelMu.Lock()
	defer m.cancelMu.Unlock()
	if m.active != nil {
		m.active()
	}
}
func (m *Manager) signal() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}
func (m *Manager) snapshot(rows []delegate.Row, err error) {
	// Called with mu held (or before serving). Marshal now to detach all mutable
	// state from concurrent readers and never expose keys or password paths.
	if rows != nil {
		m.lastRows = rows
	}
	s := Snapshot{CopyDelayMinutes: m.cfg.CopyDelayMinutes, Live: !m.engine.DryRun, ChainID: m.cfg.ChainID, Rows: m.lastRows, State: m.engine.State, Updated: time.Now().UTC()}
	for _, d := range m.cfg.Delegations {
		s.Delegations = append(s.Delegations, Delegation{Pillar: d.PillarName, Follow: d.Follow})
	}
	if err != nil {
		s.Error = err.Error()
	}
	b, marshalErr := json.Marshal(Response{Snapshot: &s})
	if marshalErr != nil {
		b = []byte(`{"error":"cannot encode status"}`)
	}
	m.cacheMu.Lock()
	m.cache = b
	m.cacheMu.Unlock()
}
func (m *Manager) poll(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	m.cancelMu.Lock()
	m.active = cancel
	m.cancelMu.Unlock()
	defer func() { m.cancelMu.Lock(); m.active = nil; m.cancelMu.Unlock() }()
	n, err := m.open(m.cfg)
	var rows []delegate.Row
	if err == nil {
		defer n.Close()
		err = n.Check(work)
		if err == nil {
			m.engine.Backend = n
			if !m.engine.DryRun {
				// Publish a read-only preview before any long PoW so the UI
				// can select a request and interrupt an in-flight copy.
				raw, cloneErr := json.Marshal(m.engine.State)
				var cloned delegate.State
				if cloneErr == nil {
					cloneErr = json.Unmarshal(raw, &cloned)
				}
				if cloneErr != nil {
					return cloneErr
				}
				preview := *m.engine
				preview.State = &cloned
				preview.DryRun = true
				preview.Log = nil
				previewRows, previewErr := preview.Reconcile(work, m.cfg.Delegations)
				if previewErr != nil {
					m.snapshot(nil, previewErr)
					return previewErr
				}
				if previewRows == nil {
					previewRows = []delegate.Row{}
				}
				m.snapshot(previewRows, nil)
			}
			rows, err = m.engine.Reconcile(work, m.cfg.Delegations)
		}
	}
	if errors.Is(err, delegate.ErrPersistence) {
		return err
	}
	if work.Err() != nil && ctx.Err() == nil {
		return nil
	} // interrupted for a UI command
	if rows == nil && err == nil {
		rows = []delegate.Row{}
	}
	m.snapshot(rows, err)
	if err != nil && ctx.Err() == nil {
		m.log.Error("poll failed", "error", err)
	}
	return err
}
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodGet && r.URL.Path == "/status" {
		m.cacheMu.RLock()
		b := append([]byte(nil), m.cache...)
		m.cacheMu.RUnlock()
		_, _ = w.Write(b)
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/command" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var req Request
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		http.Error(w, "invalid request", 400)
		return
	}
	if req.Action == "pillars" || req.Action == "requests" {
		response, err := m.read(r.Context(), req.Action)
		if err != nil {
			response.Error = err.Error()
		}
		_ = json.NewEncoder(w).Encode(response)
		return
	}
	// Cancel PoW before waiting for the state lock. A vote already published
	// remains journaled, and the queued manual vote waits for its receipt.
	if !m.lockCommand(r.Context()) {
		http.Error(w, "request canceled", http.StatusRequestTimeout)
		return
	}
	response, err := m.command(r.Context(), req)
	if err != nil {
		response.Error = err.Error()
	}
	m.mu.Unlock()
	if errors.Is(err, delegate.ErrPersistence) {
		select {
		case m.fatal <- err:
		default:
		}
	}
	if err == nil && req.Action != "pillars" {
		m.signal()
	}
	if err := json.NewEncoder(w).Encode(response); err != nil {
		m.log.Debug("control response disconnected")
	}
}
func (m *Manager) command(ctx context.Context, req Request) (Response, error) {
	if req.Action == "delay" {
		if err := config.ValidateCopyDelay(req.CopyDelayMinutes); err != nil {
			return Response{}, err
		}
		next := m.cfg
		next.CopyDelayMinutes = req.CopyDelayMinutes
		if err := config.Save(m.path, next); err != nil {
			return Response{}, err
		}
		m.cfg = next
		m.engine.CopyDelay = next.CopyDelay()
		m.snapshot(nil, nil)
		return Response{Message: "Copy delay saved. Applies to waiting and future copied votes; manual votes are not delayed."}, nil
	}
	if req.Action == "mode" && !req.Live {
		next := m.cfg
		next.Live = false
		if err := config.Save(m.path, next); err != nil {
			return Response{}, err
		}
		m.cfg = next
		m.engine.DryRun = true
		m.snapshot(nil, nil)
		return Response{Message: "New votes paused. Already submitted transactions may still finish."}, nil
	}
	n, err := m.open(m.cfg)
	if err != nil {
		return Response{}, err
	}
	defer n.Close()
	m.engine.Backend = n
	if req.Action == "mode" {
		next := m.cfg
		next.Live = req.Live
		if req.Live {
			if err = n.Check(ctx); err != nil {
				return Response{}, err
			}
			if m.engine.DryRun {
				state, err := delegate.LoadState(next.StatePath, next.ChainID)
				if err != nil {
					return Response{}, err
				}
				m.engine.State = state
			}
		}
		if err = config.Save(m.path, next); err != nil {
			return Response{}, err
		}
		m.cfg = next
		m.engine.DryRun = !req.Live
		m.snapshot(nil, nil)
		return Response{Message: "Voting mode saved."}, nil
	}
	index := -1
	for i, d := range m.cfg.Delegations {
		if d.PillarName == req.Pillar {
			index = i
			break
		}
	}
	if index < 0 {
		return Response{}, errors.New("unknown managed pillar")
	}
	d := m.cfg.Delegations[index]
	switch req.Action {
	case "follow":
		if req.Follow == d.PillarName {
			return Response{}, errors.New("a pillar cannot follow itself")
		}
		next := m.cfg
		next.Delegations = append([]config.Delegation(nil), m.cfg.Delegations...)
		next.Delegations[index].Follow = req.Follow
		nextNode, openErr := m.open(next)
		if openErr != nil {
			return Response{}, openErr
		}
		defer nextNode.Close()
		if err = nextNode.Check(ctx); err != nil {
			return Response{}, err
		}
		if err = config.Save(m.path, next); err != nil {
			return Response{}, err
		}
		m.cfg = next
	case "vote":
		v, err := delegate.ParseVote(req.Vote)
		if err != nil {
			return Response{}, err
		}
		if err = n.Check(ctx); err != nil {
			return Response{}, err
		}
		if req.OnlyUnvoted {
			votes, err := n.PillarVotes(ctx, d.PillarName, []string{req.ID})
			if err != nil {
				return Response{}, err
			}
			if votes[req.ID] != nil {
				return Response{}, errors.New("your pillar has already voted on this request; refresh the list")
			}
		}
		if err = m.engine.QueueManual(ctx, d, req.ID, v); err != nil {
			return Response{}, err
		}
	case "resume":
		if err = n.Check(ctx); err != nil {
			return Response{}, err
		}
		if m.engine.DryRun {
			return Response{}, errors.New("enable live voting before resuming copy voting")
		}
		if err = m.engine.Resume(ctx, d, req.ID); err != nil {
			return Response{}, err
		}
	default:
		return Response{}, fmt.Errorf("unknown action %q", req.Action)
	}
	m.snapshot(nil, nil)
	if req.Action == "vote" {
		return Response{Message: "Manual choice saved. Copy voting is blocked for this request. Any earlier pending vote must finish first; your vote can then be applied if the request is still open."}, nil
	}
	return Response{Message: "Saved."}, nil
}

func Call(ctx context.Context, c config.Config, request *Request) (Response, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", SocketPath(c))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 2 * time.Minute}
	method, path := http.MethodGet, "/status"
	var body io.Reader
	if request != nil {
		b, err := json.Marshal(request)
		if err != nil {
			return Response{}, err
		}
		body = bytes.NewReader(b)
		method = http.MethodPost
		path = "/command"
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, body)
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return Response{}, fmt.Errorf("cannot reach voting service: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != 200 {
		return Response{}, fmt.Errorf("control service returned HTTP %d", res.StatusCode)
	}
	var response Response
	if err = json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(&response); err != nil {
		return response, err
	}
	if response.Error != "" {
		return response, errors.New(response.Error)
	}
	return response, nil
}

func (m *Manager) open(c config.Config) (backend, error) {
	if m.factory != nil {
		return m.factory(c)
	}
	return node.Open(c, m.keys, m.log)
}

func (m *Manager) read(ctx context.Context, action string) (Response, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	// Read-only connections never wait for or cancel the signing worker.
	var n backend
	var err error
	if m.factory != nil {
		n, err = m.factory(m.readConfig)
	} else {
		n, err = node.Open(m.readConfig, nil, m.log)
	}
	if err != nil {
		return Response{}, err
	}
	defer n.Close()
	if action == "pillars" {
		candidates, err := n.Participation(ctx)
		return Response{Candidates: candidates}, err
	}
	m.cacheMu.RLock()
	raw := append([]byte(nil), m.cache...)
	m.cacheMu.RUnlock()
	var response Response
	if err = json.Unmarshal(raw, &response); err != nil || response.Snapshot == nil {
		return Response{}, errors.New("service snapshot unavailable")
	}
	s := response.Snapshot
	requests, err := n.ListVotables(ctx)
	if err != nil {
		return Response{}, err
	}
	ids := make([]string, 0, len(requests))
	for _, v := range requests {
		ids = append(ids, v.ID)
	}
	s.Rows = []delegate.Row{}
	for _, d := range s.Delegations {
		owner, err := n.PillarVotes(ctx, d.Pillar, ids)
		if err != nil {
			return Response{}, err
		}
		follow, err := n.SourceVotes(ctx, d.Follow, ids)
		if err != nil {
			return Response{}, err
		}
		for _, v := range requests {
			s.Rows = append(s.Rows, delegate.Row{Pillar: d.Pillar, Follow: d.Follow, Votable: v, Owner: owner[v.ID], Delegate: follow[v.ID], Action: "open"})
		}
	}
	s.Updated, s.Error = time.Now().UTC(), ""
	return response, nil
}

func (m *Manager) lockCommand(ctx context.Context) bool {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		m.stopWork()
		if m.mu.TryLock() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-tick.C:
		}
	}
}
