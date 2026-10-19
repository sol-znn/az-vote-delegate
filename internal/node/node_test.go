package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/0x3639/az-vote-delegate/internal/config"
	"github.com/0x3639/az-vote-delegate/internal/delegate"
	"github.com/0x3639/znn-sdk-go/api/embedded"
	"github.com/0x3639/znn-sdk-go/utils"
	"github.com/0x3639/znn-sdk-go/wallet"
	"github.com/0x3639/znn-sdk-go/zenon"
	"github.com/zenon-network/go-zenon/chain/nom"
	"github.com/zenon-network/go-zenon/common/types"
	chainpow "github.com/zenon-network/go-zenon/pow"
	chainapi "github.com/zenon-network/go-zenon/rpc/api"
	"github.com/zenon-network/go-zenon/vm/embedded/definition"
	nativewallet "github.com/zenon-network/go-zenon/wallet"
)

func hash(c string) types.Hash { return types.HexToHashPanic(strings.Repeat(c, 64)) }
func keyPair(t *testing.T) *wallet.KeyPair {
	t.Helper()
	s, err := wallet.NewKeyStoreFromEntropy(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	kp, err := s.GetKeyPair(0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(kp.Destroy)
	return kp
}
func TestNativeZnndWalletCompatibility(t *testing.T) {
	// Generate an encrypted file with the wallet code used by znnd, then load
	// it through the exact production ReadKeyStore path. No operator key used.
	entropy := bytes.Repeat([]byte{0x42}, 32)
	sdkStore, err := wallet.NewKeyStoreFromEntropy(entropy)
	if err != nil {
		t.Fatal(err)
	}
	native := &nativewallet.KeyStore{Entropy: entropy, Seed: sdkStore.Seed, Mnemonic: sdkStore.Mnemonic}
	_, base, err := native.DeriveForIndexPath(0)
	if err != nil {
		t.Fatal(err)
	}
	native.BaseAddress = base.Address
	encrypted, err := native.Encrypt("test-password")
	if err != nil {
		t.Fatal(err)
	}
	encrypted.Path = filepath.Join(t.TempDir(), "producer")
	if err = encrypted.Write(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AZ_WALLET_TEST_PASSWORD", "test-password")
	for _, index := range []int{0, 3} {
		keys, err := LoadKeys([]config.Delegation{{PillarName: "A", ProducerKeyFile: encrypted.Path, ProducerKeyIndex: index, PasswordEnv: "AZ_WALLET_TEST_PASSWORD"}})
		if err != nil {
			t.Fatal(err)
		}
		address, err := keys["A"].GetAddress()
		if err != nil {
			t.Fatal(err)
		}
		_, expected, err := native.DeriveForIndexPath(uint32(index))
		if err != nil {
			t.Fatal(err)
		}
		if *address != expected.Address {
			t.Fatal("SDK derives a different producer address")
		}
		keys.Destroy()
	}
	t.Setenv("AZ_WALLET_TEST_PASSWORD", "wrong-password")
	if _, err = LoadKeys([]config.Delegation{{PillarName: "A", ProducerKeyFile: encrypted.Path, PasswordEnv: "AZ_WALLET_TEST_PASSWORD"}}); err == nil || strings.Contains(err.Error(), "wrong-password") {
		t.Fatal("password handling", err)
	}
}

func TestVotableSelection(t *testing.T) {
	now := time.Unix(2000000, 0)
	id, phase, old := hash("a"), hash("b"), hash("c")
	tests := []struct {
		name string
		p    *embedded.Project
		want string
		bad  bool
	}{
		{"project", &embedded.Project{Id: id, Status: 0, CreationTimestamp: now.Unix() - 1}, id.String(), false},
		{"expired", &embedded.Project{Id: id, Status: 0, CreationTimestamp: now.Add(-votingWindow).Unix()}, "", false},
		{"future", &embedded.Project{Id: id, Status: 0, CreationTimestamp: now.Unix() + 1}, "", false},
		{"active no phase", &embedded.Project{Id: id, Status: 1}, "", false},
		{"only current phase", &embedded.Project{Id: id, Status: 1, PhaseIds: []types.Hash{old, phase}, Phases: []*embedded.Phase{{Phase: &embedded.PhaseInfo{Id: old, ProjectID: id, Status: 0}}, {Phase: &embedded.PhaseInfo{Id: phase, ProjectID: id, Status: 0}}}}, phase.String(), false},
		{"paid phase", &embedded.Project{Id: id, Status: 1, PhaseIds: []types.Hash{phase}, Phases: []*embedded.Phase{{Phase: &embedded.PhaseInfo{Id: phase, ProjectID: id, Status: 1}}}}, "", false},
		{"missing phase", &embedded.Project{Id: id, Status: 1, PhaseIds: []types.Hash{phase}}, "", true},
		{"closed", &embedded.Project{Id: id, Status: 2}, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vs, err := selectVotables([]*embedded.Project{tc.p}, now)
			if (err != nil) != tc.bad {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(vs) != 0 {
					t.Fatal(vs)
				}
			} else if len(vs) != 1 || vs[0].ID != tc.want {
				t.Fatal(vs)
			}
		})
	}
}

type rpcFixture struct {
	lookup         *chainapi.AccountBlock
	mu             sync.Mutex
	chain          uint64
	producer       types.Address
	difficulty     uint64
	frontier       *chainapi.AccountBlock
	published      *nom.AccountBlock
	journaled      bool
	requireJournal bool
	reject         bool
	pillarVotes    map[string]delegate.Votes
	pillarList     []map[string]any
	votes          []*definition.PillarVote
	projects       []*embedded.Project
	projectPages   []uint32
	voteSizes      []int
}

func newNode(t *testing.T) (*Node, *rpcFixture) {
	t.Helper()
	kp := keyPair(t)
	address, err := kp.GetAddress()
	if err != nil {
		t.Fatal(err)
	}
	f := &rpcFixture{chain: 7, producer: *address}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		var req struct {
			ID     json.RawMessage   `json:"id"`
			Method string            `json:"method"`
			Params []json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		var result any
		var rpcErr any
		switch req.Method {
		case "ledger.getFrontierMomentum":
			now := time.Unix(2000000, 0)
			result = &chainapi.Momentum{Momentum: &nom.Momentum{Version: 1, ChainIdentifier: f.chain, Height: 10, Hash: hash("c"), TimestampUnix: uint64(now.Unix())}}
		case "ledger.getFrontierAccountBlock":
			result = f.frontier
		case "ledger.getAccountBlockByHash":
			result = f.lookup
		case "embedded.pillar.getAll":
			result = map[string]any{"count": 2, "list": []map[string]any{{"name": "A", "producerAddress": f.producer, "weight": "0", "currentStats": map[string]any{}}, {"name": "B", "weight": "0", "currentStats": map[string]any{}}}}
			if f.pillarList != nil {
				result = map[string]any{"count": len(f.pillarList), "list": f.pillarList}
			}
		case "embedded.accelerator.getAll":
			var page, size uint32
			if err := json.Unmarshal(req.Params[0], &page); err != nil {
				t.Error(err)
			}
			if err := json.Unmarshal(req.Params[1], &size); err != nil {
				t.Error(err)
			}
			f.projectPages = append(f.projectPages, page)
			start := min(int(page*size), len(f.projects))
			end := min(start+int(size), len(f.projects))
			list := make([]map[string]any, 0, end-start)
			for _, p := range f.projects[start:end] {
				phases := []map[string]any{}
				for _, phase := range p.Phases {
					info := phase.Phase
					phases = append(phases, map[string]any{"phase": map[string]any{"id": info.Id, "projectID": info.ProjectID, "status": info.Status, "znnFundsNeeded": "0", "qsrFundsNeeded": "0"}})
				}
				list = append(list, map[string]any{"id": p.Id, "status": p.Status, "creationTimestamp": p.CreationTimestamp, "votes": p.Votes, "znnFundsNeeded": "0", "qsrFundsNeeded": "0", "phaseIds": p.PhaseIds, "phases": phases})
			}
			result = map[string]any{"count": len(f.projects), "list": list}
		case "embedded.accelerator.getPillarVotes":
			var ids []types.Hash
			if err := json.Unmarshal(req.Params[1], &ids); err != nil {
				t.Error(err)
			}
			f.voteSizes = append(f.voteSizes, len(ids))
			if f.pillarVotes != nil {
				var name string
				if err := json.Unmarshal(req.Params[0], &name); err != nil {
					t.Error(err)
				}
				batch := make([]*definition.PillarVote, len(ids))
				for i, id := range ids {
					if v := f.pillarVotes[name][id.String()]; v != nil {
						batch[i] = &definition.PillarVote{Id: id, Name: name, Vote: uint8(*v)}
					}
				}
				result = batch
			} else if f.votes != nil {
				result = f.votes
			} else {
				result = make([]*definition.PillarVote, len(ids))
			}
		case "embedded.plasma.getRequiredPoWForAccountBlock":
			result = &embedded.GetRequiredResult{AvailablePlasma: 0, BasePlasma: 52500, RequiredDifficulty: f.difficulty}
		case "ledger.publishRawTransaction":
			if f.requireJournal && !f.journaled {
				t.Error("published before journal")
			}
			f.published = new(nom.AccountBlock)
			if err := json.Unmarshal(req.Params[0], f.published); err != nil {
				t.Error(err)
			}
			if f.reject {
				rpcErr = map[string]any{"code": -32000, "message": "invalid height"}
			}
		default:
			t.Errorf("unexpected RPC: %s", req.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		response := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
		if rpcErr != nil {
			delete(response, "result")
			response["error"] = rpcErr
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	}))
	t.Cleanup(server.Close)
	c := config.Config{NodeURL: server.URL, ChainID: 7, Delegations: []config.Delegation{{PillarName: "A", Follow: "B"}}}
	n, err := Open(c, Keys{"A": kp}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	return n, f
}
func TestSDKSendWithContextEmbeddedFlow(t *testing.T) {
	for _, difficulty := range []uint64{0, 1000} {
		t.Run(delegate.Vote(difficulty%3).String(), func(t *testing.T) {
			n, f := newNode(t)
			f.difficulty = difficulty
			if difficulty == 0 {
				f.frontier = &chainapi.AccountBlock{AccountBlock: nom.AccountBlock{Height: 5, Hash: hash("d"), Amount: big.NewInt(0)}}
			}
			tpl := n.Client.AcceleratorApi.VoteByProducerAddress(hash("a"), 2)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			block, err := zenon.NewZenon(n.Client).SendWithContext(ctx, tpl, n.Keys["A"])
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.published == nil || block.ChainIdentifier != 7 || block.ToAddress != types.AcceleratorContract || block.Amount.Sign() != 0 || block.Difficulty != difficulty {
				t.Fatal("incorrect embedded send")
			}
			if f.frontier == nil {
				if block.Height != 1 || block.PreviousHash != types.ZeroHash {
					t.Fatal("new account frontier")
				}
			} else if block.Height != 6 || block.PreviousHash != hash("d") {
				t.Fatal("existing frontier")
			}
			expected, err := utils.GetTransactionHash(block)
			if err != nil || expected != block.Hash {
				t.Fatal("hash mismatch", err)
			}
			valid, err := n.Keys["A"].Verify(block.Signature, block.Hash.Bytes())
			if err != nil || !valid {
				t.Fatal("invalid signature", err)
			}
			if difficulty > 0 && !chainpow.CheckPoWNonce(block) {
				t.Fatal("invalid node PoW")
			}
			var params struct {
				Id   types.Hash
				Vote uint8
			}
			if err = definition.ABIAccelerator.UnpackMethod(&params, definition.VoteByProdAddressMethodName, block.Data); err != nil {
				t.Fatal(err)
			}
			if params.Id != hash("a") || params.Vote != 2 {
				t.Fatal("incorrect vote data")
			}
		})
	}
}
func TestSendGuardJournalAndRejection(t *testing.T) {
	n, f := newNode(t)
	f.requireJournal = true
	guardErr := errors.New("manual vote appeared")
	err := n.Send(context.Background(), n.Config.Delegations[0], hash("a").String(), delegate.Yes, func() error { return guardErr }, func(string) error { t.Fatal("journal after rejected guard"); return nil })
	if !errors.Is(err, guardErr) {
		t.Fatal(err)
	}
	f.mu.Lock()
	sent := f.published != nil
	f.mu.Unlock()
	if sent {
		t.Fatal("published after changed vote")
	}
	f.reject = true
	err = n.Send(context.Background(), n.Config.Delegations[0], hash("a").String(), delegate.Yes, func() error { return nil }, func(h string) error {
		if h == "" {
			t.Fatal("empty journal hash")
		}
		f.mu.Lock()
		f.journaled = true
		f.mu.Unlock()
		return nil
	})
	if !errors.Is(err, delegate.ErrRejected) {
		t.Fatalf("wanted definite rejection: %v", err)
	}
}
func TestCheckRejectsWrongChainAndProducer(t *testing.T) {
	n, f := newNode(t)
	if err := n.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.chain = 1
	if err := n.Check(context.Background()); err == nil {
		t.Fatal("wrong chain accepted")
	}
	f.chain = 7
	f.producer = types.ZeroAddress
	if err := n.Check(context.Background()); err == nil {
		t.Fatal("wrong producer accepted")
	}
	address, err := n.Keys["A"].GetAddress()
	if err != nil {
		t.Fatal(err)
	}
	f.producer = *address
	n.Config.Delegations[0].Follow = "missing"
	if err := n.Check(context.Background()); err == nil {
		t.Fatal("missing delegate accepted")
	}
	n.Config.Delegations[0].Follow = "z1qrw2vecdddmft89xh6yqencmjergsshhfmks0l"
	if err := n.Check(context.Background()); err != nil {
		t.Fatalf("address delegate refused: %v", err)
	}
	n.Config.Delegations[0].Follow = address.String()
	if err := n.Check(context.Background()); err == nil {
		t.Fatal("own producer address accepted as delegate")
	}
}
func TestVoteResponseValidationAndBatching(t *testing.T) {
	n, f := newNode(t)
	id := hash("a")
	for _, vote := range []*definition.PillarVote{{Id: id, Name: "A", Vote: 3}, {Id: hash("b"), Name: "A", Vote: 0}, {Id: id, Name: "B", Vote: 0}} {
		f.votes = []*definition.PillarVote{vote}
		if _, err := n.PillarVotes(context.Background(), "A", []string{id.String()}); err == nil {
			t.Fatal("invalid response accepted")
		}
	}
	f.votes = []*definition.PillarVote{}
	if _, err := n.PillarVotes(context.Background(), "A", []string{id.String()}); err == nil {
		t.Fatal("short response accepted")
	}
	f.votes = nil
	f.voteSizes = nil
	ids := make([]string, 1025)
	for i := range ids {
		ids[i] = id.String()
	}
	if _, err := n.PillarVotes(context.Background(), "A", ids); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.voteSizes) != 2 || f.voteSizes[0] != 1024 || f.voteSizes[1] != 1 {
		t.Fatal(f.voteSizes)
	}
}
func TestProjectPagination(t *testing.T) {
	n, f := newNode(t)
	for i := range 1025 {
		id := types.Hash{}
		id[0] = byte(i)
		id[1] = byte(i >> 8)
		f.projects = append(f.projects, &embedded.Project{Id: id, Status: 0, CreationTimestamp: 1999999, ZnnFundsNeeded: big.NewInt(0), QsrFundsNeeded: big.NewInt(0), Votes: &embedded.VoteBreakdown{}})
	}
	vs, err := n.ListVotables(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(vs) != 1025 || len(f.projectPages) != 2 || f.projectPages[1] != 1 {
		t.Fatal("pagination", len(vs), f.projectPages)
	}
}

func TestExecutionRequiresMatchingConfirmedReceipt(t *testing.T) {
	n, f := newNode(t)
	id := hash("a")
	check := func(want bool, bad bool) {
		t.Helper()
		done, err := n.Executed(context.Background(), id.String())
		if done != want || (err != nil) != bad {
			t.Fatalf("executed=%v err=%v", done, err)
		}
	}
	check(false, false)
	f.lookup = &chainapi.AccountBlock{AccountBlock: nom.AccountBlock{Hash: id, Height: 1, Amount: big.NewInt(0)}}
	check(false, false)
	f.lookup.PairedAccountBlock = &chainapi.AccountBlock{AccountBlock: nom.AccountBlock{Hash: hash("b"), FromBlockHash: id, Height: 1, Amount: big.NewInt(0)}}
	check(false, false)
	f.lookup.PairedAccountBlock.ConfirmationDetail = &chainapi.AccountBlockConfirmationDetail{NumConfirmations: 1}
	check(true, false)
	f.lookup.PairedAccountBlock.FromBlockHash = hash("c")
	check(false, true)
}

func TestCanceledSendDoesNotPublish(t *testing.T) {
	n, f := newNode(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := n.Send(ctx, n.Config.Delegations[0], hash("a").String(), delegate.Yes, func() error { return nil }, func(string) error { t.Fatal("journaled canceled send"); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.published != nil {
		t.Fatal("published canceled send")
	}
}
func TestAddressVotesTakeLatestConfirmedVoteBlock(t *testing.T) {
	proxy := types.ParseAddressPanic("z1qrw2vecdddmft89xh6yqencmjergsshhfmks0l")
	a, b, c := hash("a"), hash("b"), hash("c")
	confirmed := &chainapi.AccountBlockConfirmationDetail{NumConfirmations: 1}
	block := func(height uint64, data []byte, to types.Address, detail *chainapi.AccountBlockConfirmationDetail) *chainapi.AccountBlock {
		return &chainapi.AccountBlock{AccountBlock: nom.AccountBlock{Address: proxy, Height: height, BlockType: nom.BlockTypeUserSend, ToAddress: to, Data: data}, ConfirmationDetail: detail}
	}
	byProd := func(id types.Hash, v uint8) []byte {
		return definition.ABICommon.PackMethodPanic(definition.VoteByProdAddressMethodName, id, v)
	}
	byName := func(id types.Hash, name string, v uint8) []byte {
		return definition.ABICommon.PackMethodPanic(definition.VoteByNameMethodName, id, name, v)
	}
	blocks := []*chainapi.AccountBlock{
		block(1, byProd(a, 0), types.AcceleratorContract, confirmed),
		block(2, byName(a, "Other", 1), types.AcceleratorContract, confirmed), // newest on a, name ignored
		block(3, byProd(b, 2), types.AcceleratorContract, confirmed),
		block(4, byProd(b, 0), types.AcceleratorContract, nil),               // unconfirmed
		block(5, byProd(c, 0), types.PillarContract, confirmed),              // wrong contract
		block(6, byProd(hash("d"), 0), types.AcceleratorContract, confirmed), // not asked for
	}
	votes, err := addressVotes(proxy, blocks, []string{a.String(), b.String(), c.String()})
	if err != nil {
		t.Fatal(err)
	}
	if delegate.VoteText(votes[a.String()]) != "no" || delegate.VoteText(votes[b.String()]) != "abstain" || votes[c.String()] != nil || len(votes) != 3 {
		t.Fatal(votes)
	}
	blocks[0].Address = types.ZeroAddress
	if _, err := addressVotes(proxy, blocks, []string{a.String()}); err == nil {
		t.Fatal("block from another address accepted")
	}
}
