package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/0x3639/az-vote-delegate/internal/config"
	"github.com/0x3639/az-vote-delegate/internal/delegate"
	"github.com/0x3639/znn-sdk-go/api/embedded"
	"github.com/0x3639/znn-sdk-go/rpc_client"
	"github.com/0x3639/znn-sdk-go/wallet"
	"github.com/0x3639/znn-sdk-go/zenon"
	"github.com/zenon-network/go-zenon/chain/nom"
	"github.com/zenon-network/go-zenon/common/types"
	chainapi "github.com/zenon-network/go-zenon/rpc/api"
	"github.com/zenon-network/go-zenon/vm/embedded/definition"
)

const pageSize uint32 = 1024
const votingWindow = 14 * 24 * time.Hour

type Keys map[string]*wallet.KeyPair

func (keys Keys) Destroy() {
	for _, key := range keys {
		key.Destroy()
	}
}
func LoadKeys(ds []config.Delegation) (Keys, error) {
	keys := Keys{}
	addresses := map[string]bool{}
	for _, d := range ds {
		key, err := loadKey(d)
		if err != nil {
			keys.Destroy()
			return nil, fmt.Errorf("%s: %w", d.PillarName, err)
		}
		address, err := key.GetAddress()
		if err != nil {
			key.Destroy()
			keys.Destroy()
			return nil, err
		}
		if addresses[address.String()] {
			key.Destroy()
			keys.Destroy()
			return nil, errors.New("multiple delegations use the same producer address")
		}
		addresses[address.String()] = true
		keys[d.PillarName] = key
	}
	return keys, nil
}
func loadKey(d config.Delegation) (*wallet.KeyPair, error) {
	password, err := config.Password(d)
	if err != nil {
		return nil, err
	}
	return KeyFromFile(d, password)
}

func KeyFromFile(d config.Delegation, password string) (*wallet.KeyPair, error) {
	// Avoid creating a wallet directory during a read-only key check.
	if info, err := os.Stat(filepath.Dir(d.ProducerKeyFile)); err != nil || !info.IsDir() {
		return nil, errors.New("producer wallet directory does not exist")
	}
	manager, err := wallet.NewKeyStoreManager(filepath.Dir(d.ProducerKeyFile))
	if err != nil {
		return nil, err
	}
	store, err := manager.ReadKeyStore(password, filepath.Base(d.ProducerKeyFile))
	if err != nil {
		return nil, errors.New("cannot open producer key file (check format, path and password)")
	}
	// GetKeyPair creates its own derived private key. Remove the seed material
	// retained in the temporary store as soon as derivation completes.
	defer func() { clear(store.Entropy); clear(store.Seed); store.Mnemonic = "" }()
	return store.GetKeyPair(d.ProducerKeyIndex)
}

type Node struct {
	Client *rpc_client.RpcClient
	Config config.Config
	Keys   Keys
	Log    *slog.Logger
}

func Open(c config.Config, keys Keys, log *slog.Logger) (*Node, error) {
	options := rpc_client.DefaultClientOptions()
	options.AutoReconnect = false // A fresh connection is opened for each poll.
	options.HealthCheckInterval = 0
	options.HTTPTimeout = 30 * time.Second
	client, err := rpc_client.NewRpcClientWithOptions(c.NodeURL, options)
	if err != nil {
		return nil, errors.New("cannot connect to configured node")
	}
	return &Node{Client: client, Config: c, Keys: keys, Log: log}, nil
}
func (n *Node) Close() { n.Client.Stop() }

// SDK v0.5.0 only applies SendWithContext to PoW, not RPC. Closing the
// per-poll connection interrupts blocked WS calls. HTTP calls retain their
// configured 30-second transport timeout.
func call[T any](ctx context.Context, n *Node, fn func() (T, error)) (T, error) {
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(bounded, n.Close)
	defer stop()
	var zero T
	if err := bounded.Err(); err != nil {
		return zero, err
	}
	result, err := fn()
	if bounded.Err() != nil {
		return zero, bounded.Err()
	}
	return result, err
}
func (n *Node) chainTime(ctx context.Context) (time.Time, error) {
	m, err := call(ctx, n, n.Client.LedgerApi.GetFrontierMomentum)
	if err != nil {
		return time.Time{}, err
	}
	if m == nil || m.Momentum == nil || m.TimestampUnix == 0 {
		return time.Time{}, errors.New("node returned no frontier momentum")
	}
	if m.ChainIdentifier != n.Config.ChainID {
		return time.Time{}, fmt.Errorf("chainId mismatch: expected %d, got %d", n.Config.ChainID, m.ChainIdentifier)
	}
	return time.Unix(int64(m.TimestampUnix), 0), nil
}
func (n *Node) Check(ctx context.Context) error {
	if _, err := n.chainTime(ctx); err != nil {
		return err
	}
	pillars, err := n.Pillars(ctx)
	if err != nil {
		return err
	}
	for _, d := range n.Config.Delegations {
		key := n.Keys[d.PillarName]
		if key == nil {
			return errors.New("missing producer key")
		}
		address, err := key.GetAddress()
		if err != nil {
			return err
		}
		owner, follow := pillars[d.PillarName], pillars[d.Follow]
		if owner == nil || owner.ProducerAddress != *address || owner.RevokeTimestamp != 0 {
			return fmt.Errorf("%s: key does not match an active pillar producer address", d.PillarName)
		}
		if source, ok := config.FollowAddress(d.Follow); ok {
			// The producer's own blocks are the daemon's copies; following them copies nothing.
			if source == *address {
				return fmt.Errorf("%s cannot follow its own producer address", d.PillarName)
			}
			continue
		}
		if follow == nil || follow.RevokeTimestamp != 0 {
			return fmt.Errorf("delegate %s is not an active pillar", d.Follow)
		}
	}
	return nil
}

func (n *Node) ListVotables(ctx context.Context) ([]delegate.Votable, error) {
	now, err := n.chainTime(ctx)
	if err != nil {
		return nil, err
	}
	projects, err := n.projects(ctx)
	if err != nil {
		return nil, err
	}
	return selectVotables(projects, now)
}

func (n *Node) projects(ctx context.Context) ([]*embedded.Project, error) {
	var projects []*embedded.Project
	seen := map[types.Hash]bool{}
	count := -1
	for page := uint32(0); ; page++ {
		list, err := call(ctx, n, func() (*embedded.ProjectList, error) { return n.Client.AcceleratorApi.GetAll(page, pageSize) })
		if err != nil {
			return nil, err
		}
		if list == nil || list.Count < 0 {
			return nil, errors.New("invalid project list")
		}
		if count < 0 {
			count = list.Count
		}
		if count != list.Count {
			return nil, errors.New("project list changed while paging; retry")
		}
		for _, p := range list.List {
			if p == nil || seen[p.Id] {
				return nil, errors.New("invalid or duplicated project")
			}
			seen[p.Id] = true
			projects = append(projects, p)
		}
		if len(projects) >= count {
			break
		}
		if len(list.List) == 0 {
			return nil, errors.New("incomplete project list")
		}
	}
	return projects, nil
}
func selectVotables(projects []*embedded.Project, now time.Time) ([]delegate.Votable, error) {
	var result []delegate.Votable
	for _, p := range projects {
		if p == nil {
			return nil, errors.New("nil project")
		}
		if p.Status == 0 {
			expires := time.Unix(p.CreationTimestamp, 0).Add(votingWindow)
			if now.Before(expires) && !now.Before(time.Unix(p.CreationTimestamp, 0)) {
				result = append(result, delegate.Votable{ID: p.Id.String(), Kind: "project", Name: p.Name, ProjectID: p.Id.String(), ProjectName: p.Name, Description: p.Description, URL: p.Url, ZNN: amount(p.ZnnFundsNeeded), QSR: amount(p.QsrFundsNeeded), Expires: expires})
			}
		} else if p.Status == 1 && len(p.PhaseIds) > 0 {
			last := p.PhaseIds[len(p.PhaseIds)-1]
			found := false
			for _, phase := range p.Phases {
				if phase == nil || phase.Phase == nil {
					return nil, errors.New("nil phase")
				}
				info := phase.Phase
				if info.Id == last {
					if found || info.ProjectID != p.Id {
						return nil, errors.New("invalid current phase")
					}
					found = true
					if info.Status == 0 {
						result = append(result, delegate.Votable{ID: info.Id.String(), Kind: "phase", Name: info.Name, ProjectID: p.Id.String(), ProjectName: p.Name, Description: info.Description, URL: info.Url, ZNN: amount(info.ZnnFundsNeeded), QSR: amount(info.QsrFundsNeeded)})
					}
				}
			}
			if !found {
				return nil, errors.New("missing current phase")
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}
func (n *Node) PillarVotes(ctx context.Context, name string, ids []string) (delegate.Votes, error) {
	result := delegate.Votes{}
	for start := 0; start < len(ids); start += int(pageSize) {
		end := min(start+int(pageSize), len(ids))
		hashes := make([]types.Hash, end-start)
		for i, id := range ids[start:end] {
			h, err := types.HexToHash(id)
			if err != nil {
				return nil, err
			}
			hashes[i] = h
		}
		batch, err := n.votes(ctx, name, hashes)
		if err != nil {
			return nil, err
		}
		for id, v := range batch {
			result[id] = v
		}
	}
	return result, nil
}
func (n *Node) votes(ctx context.Context, name string, hashes []types.Hash) (delegate.Votes, error) {
	votes, err := call(ctx, n, func() ([]*definition.PillarVote, error) { return n.Client.AcceleratorApi.GetPillarVotes(name, hashes) })
	if err != nil {
		return nil, err
	}
	if len(votes) != len(hashes) {
		return nil, errors.New("incomplete pillar vote response")
	}
	result := delegate.Votes{}
	for i, v := range votes {
		id := hashes[i].String()
		result[id] = nil
		if v != nil {
			if v.Id != hashes[i] || v.Name != name || v.Vote > 2 {
				return nil, errors.New("mismatched or invalid pillar vote response")
			}
			vote := delegate.Vote(v.Vote)
			result[id] = &vote
		}
	}
	return result, nil
}

// SourceVotes reads the votes to copy: a pillar's from contract state, or an
// address's from its own account chain.
func (n *Node) SourceVotes(ctx context.Context, follow string, ids []string) (delegate.Votes, error) {
	address, ok := config.FollowAddress(follow)
	if !ok {
		return n.PillarVotes(ctx, follow, ids)
	}
	var blocks []*chainapi.AccountBlock
	for height := uint64(1); ; height += uint64(pageSize) {
		list, err := call(ctx, n, func() (*chainapi.AccountBlockList, error) {
			return n.Client.LedgerApi.GetAccountBlocksByHeight(address, height, uint64(pageSize))
		})
		if err != nil {
			return nil, err
		}
		if list == nil {
			return nil, errors.New("invalid account block list")
		}
		blocks = append(blocks, list.List...)
		if len(list.List) < int(pageSize) {
			break
		}
	}
	return addressVotes(address, blocks, ids)
}

// addressVotes takes each id's vote from the newest confirmed vote block the
// address sent to the Accelerator contract. The contract refuses such a vote
// from an address that is not a pillar's owner or producer, but the send block
// is still on the address's chain (a proxy vote), and
// this is where it is read back. Both vote methods count and a claimed pillar
// name is ignored, so the address's latest word on an id is the one copied.
func addressVotes(address types.Address, blocks []*chainapi.AccountBlock, ids []string) (delegate.Votes, error) {
	want := map[types.Hash]bool{}
	result := delegate.Votes{}
	for _, id := range ids {
		h, err := types.HexToHash(id)
		if err != nil {
			return nil, err
		}
		want[h] = true
		result[id] = nil
	}
	heights := map[types.Hash]uint64{}
	for _, b := range blocks {
		if b == nil || b.Address != address {
			return nil, errors.New("mismatched account block response")
		}
		if b.BlockType != nom.BlockTypeUserSend || b.ToAddress != types.AcceleratorContract || b.ConfirmationDetail == nil || len(b.Data) < 4 {
			continue
		}
		method, err := definition.ABICommon.MethodById(b.Data[:4])
		if err != nil || (method.Name != definition.VoteByNameMethodName && method.Name != definition.VoteByProdAddressMethodName) {
			continue
		}
		param := new(definition.PillarVote)
		if definition.ABICommon.UnpackMethod(param, method.Name, b.Data) != nil || param.Vote > uint8(delegate.Abstain) || !want[param.Id] {
			continue
		}
		if b.Height > heights[param.Id] {
			heights[param.Id] = b.Height
			vote := delegate.Vote(param.Vote)
			result[param.Id.String()] = &vote
		}
	}
	return result, nil
}

func (n *Node) Executed(ctx context.Context, hash string) (bool, error) {
	h, err := types.HexToHash(hash)
	if err != nil {
		return false, err
	}
	block, err := call(ctx, n, func() (*chainapi.AccountBlock, error) { return n.Client.LedgerApi.GetAccountBlockByHash(h) })
	if err != nil {
		return false, err
	}
	if absentBlock(block) {
		return false, nil
	}
	if block.Hash != h {
		return false, errors.New("mismatched account block response")
	}
	if block.PairedAccountBlock == nil {
		return false, nil
	}
	if block.PairedAccountBlock.FromBlockHash != h {
		return false, errors.New("mismatched contract receipt")
	}
	return block.PairedAccountBlock.ConfirmationDetail != nil && block.PairedAccountBlock.ConfirmationDetail.NumConfirmations > 0, nil
}
func (n *Node) Send(ctx context.Context, d config.Delegation, id string, vote delegate.Vote, guard func() error, journal func(string) error) error {
	if !vote.Valid() {
		return errors.New("invalid vote")
	}
	if err := n.Check(ctx); err != nil {
		return err
	}
	h, err := types.HexToHash(id)
	if err != nil {
		return err
	}
	tpl := n.Client.AcceleratorApi.VoteByProducerAddress(h, uint8(vote))
	// Pin the configured chain even if the endpoint changes during preparation.
	tpl.ChainIdentifier = n.Config.ChainID
	z := zenon.NewZenon(n.Client)
	stop := context.AfterFunc(ctx, n.Close)
	defer stop()
	if err = ctx.Err(); err != nil {
		return err
	}
	// The SDK's prepare+publish flow is equivalent to SendWithContext, but
	// provides a guard and durable journal between signing and submission.
	block, err := z.PrepareBlockWithContext(ctx, tpl, n.Keys[d.PillarName])
	if err != nil {
		return err
	}
	if err = n.Check(ctx); err != nil {
		return err
	}
	// A second writer may have advanced the producer chain during PoW.
	frontier, err := call(ctx, n, func() (*chainapi.AccountBlock, error) {
		return n.Client.LedgerApi.GetFrontierAccountBlock(block.Address)
	})
	if err != nil {
		return err
	}
	if (absentBlock(frontier) && block.Height != 1) || (!absentBlock(frontier) && (frontier.Height+1 != block.Height || frontier.Hash != block.PreviousHash)) {
		return delegate.ErrChanged
	}
	if err = guard(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = journal(block.Hash.String()); err != nil {
		return err
	}
	if n.Log != nil {
		path := "fused-plasma"
		if block.Difficulty > 0 {
			path = "pow"
		}
		n.Log.Info("prepared vote", "pillar", d.PillarName, "id", id, "plasma", path, "difficulty", block.Difficulty, "blockHash", block.Hash.String())
	}
	attempted := false
	_, err = call(ctx, n, func() (bool, error) { attempted = true; return true, n.Client.LedgerApi.PublishRawTransaction(block) })
	if err != nil && !attempted {
		return fmt.Errorf("%w: publication canceled before submission", delegate.ErrRejected)
	}
	if err != nil {
		// A structured rejection plus an absent block is safe to retry. A lost
		// response has an unknown outcome: retain the journal and wait, never
		// generate a second vote blindly.
		var coded interface{ ErrorCode() int }
		if errors.As(err, &coded) {
			existing, lookupErr := call(ctx, n, func() (*chainapi.AccountBlock, error) { return n.Client.LedgerApi.GetAccountBlockByHash(block.Hash) })
			if lookupErr == nil && absentBlock(existing) {
				return fmt.Errorf("%w: %v", delegate.ErrRejected, err)
			}
		}
	}
	return err
}

// SDK v0.5.0 decodes a null ledger response into an allocated zero block.
func absentBlock(block *chainapi.AccountBlock) bool {
	return block == nil || (block.Height == 0 && block.Hash == types.ZeroHash)
}

func (n *Node) Pillars(ctx context.Context) (map[string]*embedded.PillarInfo, error) {
	pillars := map[string]*embedded.PillarInfo{}
	count := -1
	for page := uint32(0); ; page++ {
		list, err := call(ctx, n, func() (*embedded.PillarInfoList, error) { return n.Client.PillarApi.GetAll(page, pageSize) })
		if err != nil {
			return nil, err
		}
		if list == nil || list.Count < 0 {
			return nil, errors.New("invalid pillar list")
		}
		if count < 0 {
			count = list.Count
		}
		if count != list.Count {
			return nil, errors.New("pillar list changed while paging; retry")
		}
		for _, p := range list.List {
			if p == nil || pillars[p.Name] != nil {
				return nil, errors.New("invalid or duplicated pillar")
			}
			pillars[p.Name] = p
		}
		if len(pillars) >= count {
			break
		}
		if len(list.List) == 0 {
			return nil, errors.New("incomplete pillar list")
		}
	}

	return pillars, nil
}

func amount(v *big.Int) string {
	if v == nil {
		return "0"
	}
	return new(big.Rat).SetFrac(v, big.NewInt(100000000)).FloatString(8)
}
