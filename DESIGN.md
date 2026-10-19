# Accelerator-Z vote delegation daemon — design

Date: 2026-10-18. Status: initial design; implementation and operator UI amendments are documented at https://www.0x3639.com/az-vote-delegate/.
Source of chain facts: go-zenon `dev` at b5f93c2 (`vm/embedded/implementation/common.go`,
`vm/embedded/implementation/accelerator.go`, `vm/embedded/definition/common.go`,
`rpc/api/embedded/accelerator.go`). SDK facts: `github.com/0x3639/znn-sdk-go` v0.5.0
(local checkout `~/Github/znn-sdk-go`).

## 1. Intent

A pillar owner who does not want to keep up with every Accelerator-Z (AZ) proposal names
another pillar they trust. From then on, whatever that pillar votes on an AZ project or
phase, the owner's pillar votes the same way, automatically, until the owner votes by hand
or turns the daemon off.

Why off-chain: the AZ embedded contract has no delegation concept. A vote is an account
block to the accelerator contract and is accepted only from the pillar's stake (owner)
address (`VoteByName`) or its block-producing address (`VoteByProdAddress`). Adding
delegation on-chain means a new ABI method, storage change and a spork (Phase 2 under the
Phase 1 scope rule). A daemon that mirrors votes using the producer key needs no protocol
change and looks on-chain exactly like the pillar voting normally.

## 2. Goals, non-goals, anti-goals

Goals
- One small Go daemon, runs on (or next to) the pillar host, no protocol changes.
- Mirrors the delegate pillar's votes on every open AZ votable (project vote, phase vote).
- Re-mirrors automatically after a phase update wipes votes, and after a restart, with no
  dependence on having seen the delegate's vote block.
- The owner's own manual vote on a votable always wins over the mirror.
- Safe by default: dry-run mode, idempotent, never casts a vote the delegate has not cast.

Non-goals
- On-chain or enforceable delegation; nothing visible on-chain says "A follows B".
- Using the owner (stake) key. Only the producer key is used.
- GUI, Telegram/Discord notifications, auto-fusing plasma, multi-network in one process.
- Delegating anything other than AZ votes (no consensus, spork or pillar-delegation
  semantics).

Anti-goals (failure even if it "works")
- Casting a vote the delegate did not cast, or casting the wrong direction.
- Overwriting a vote the owner placed by hand.
- Exposing the producer key password (command line, logs, config in git).
- Spamming: more than one send per votable per change, or retry storms on RPC errors.
- Voting on the wrong network because the node URL pointed at a testnet.

## 3. Chain facts the builder must rely on

Vote entry point (use this one): `VoteByProdAddress(id hash, vote uint8)` sent from the
pillar's block-producing address to the accelerator contract, amount 0. The contract looks
the sender up in the active pillar list by `BlockProducingAddress` and stores the vote under
that pillar's name. Plasma cost: `EmbeddedSimple` (2.5 × 21000 = 52,500 plasma); fee 0.

Vote values: `0 = yes`, `1 = no`, `2 = abstain`; anything ≥ 3 is rejected.

Storage: one vote per `(votable id, pillar name)`. A new vote from the same pillar
overwrites the old one. Reading: `embedded.accelerator.getPillarVotes(name, [ids])` returns
one entry per id, `null` where the pillar has not voted.

What is votable, and when:
- A project whose `status == 0` (Voting). Its votable id is the project id. The window is
  14 days from `creationTimestamp`; after that the contract closes the project. It can also
  flip to Active earlier once votes pass the threshold (yes > no and total > 33 % of
  pillars), after which the project id is no longer votable.
- The current phase (last entry of `phaseIds`) of a project whose `status == 1` (Active),
  when that phase's `status == 0` (Voting). Its votable id is the phase id. There is no
  time limit; it stays votable until it passes.
- `embedded.accelerator.getAll(pageIndex, pageSize)` returns projects with `phases` and
  per-votable `votes` breakdowns embedded; page size is capped by the node (`RpcMaxPageSize`,
  builder to check the value and page through).
- When a project owner updates a phase, the contract deletes all pillar votes for that
  phase. The mirror must therefore re-check every poll; it must not assume a cast vote
  persists.

Account chain: the producer address has its own account chain. The node signs momentums
with the producer key but never sends account blocks from it, so the daemon is normally the
only writer. If the owner also votes from the producer address by hand (znn-cli), two
writers race on height; the SDK refetches the frontier at send time, and the daemon retries
once on a publish error.

Plasma: a producer address usually has no fused QSR, so the SDK will generate PoW for every
vote (difficulty = plasma × 1500, so ~79 M for 52,500 plasma; seconds to a minute or two on
a CPU). Fusing a small amount of QSR to the producer address removes PoW entirely; the
daemon should log which path it used. This release supports the legacy fused-plasma /
PoW flow only; Dynamic Plasma is explicitly out of scope. The SDK queries
`embedded.plasma.getRequiredPoWForAccountBlock`, but that alone does not establish
Dynamic Plasma compatibility. Supporting it requires a separate implementation and
validation effort.

## 4. How it works

### 4.1 Reconcile loop (the whole algorithm)

Every `pollInterval` (default 30 s), for each configured delegation `A follows B`:

1. Fetch all projects (page through `getAll`). Build the set `V` of currently votable ids
   per §3.
2. `bVotes = getPillarVotes(B, V)`, `aVotes = getPillarVotes(A, V)`.
3. For each id in `V`:
   - `b = bVotes[id]`, `a = aVotes[id]`, `cast = state[A][id]` (what the daemon last cast,
     or none).
   - If `b == nil`: nothing to mirror. Do not touch A's vote. Continue.
   - If `a != nil && a != cast`: A's on-chain vote was not placed by the daemon (owner
     voted by hand, or a vote from before the daemon started). Record
     `state[A][id].ownerOverride = true`, log once, continue. The override is sticky for
     that id until A's vote disappears (phase reset) or the id leaves `V`.
   - If `ownerOverride` is set and `a == nil`: the votes were wiped; clear the override and
     fall through.
   - If `a == b`: in sync. Continue.
   - Otherwise send `VoteByProdAddress(id, b.vote)` signed with A's producer key, then set
     `state[A][id] = {cast: b.vote, hash: sentBlockHash, at: now}`.
4. Drop state entries whose id is no longer in `V`.

Properties worth stating in the README: stateless with respect to *seeing* B's vote block
(it only reads current chain state), idempotent (re-running changes nothing when in sync),
and chain-of-delegation friendly (if B itself mirrors C, A sees B's on-chain vote).
A→B and B→A at the same time simply never vote; document, do not detect.

### 4.2 Send path

`zenon.NewZenon(client)` + `AcceleratorApi.VoteByProducerAddress(id, vote)` template +
`Zenon.SendWithContext(ctx, tpl, keyPair)` with a PoW timeout (default 5 min). One send
in flight per delegation at a time. On error: log, retry once after the next poll, then
back off exponentially per id (cap 30 min) so a permanently failing id cannot spam.

Before the first send after startup, verify preconditions and refuse to run if any fails:
- `embedded.pillar.getAll` lists a pillar whose `producerAddress == keyPair.Address`;
  record its name as A and check it matches `pillarName` in config (guards against a wrong
  key file).
- The delegate pillar B exists in the same list.
- The node's chain identifier matches `chainId` in config (guards against voting on the
  wrong network).

### 4.3 State

Small JSON file (`statePath`, default next to the config): per pillar, per votable id:
`{cast, blockHash, at, ownerOverride}`. Written atomically (temp + rename). Losing it is
safe: the daemon then treats every existing A vote as an owner override until the next
phase reset, which is the conservative direction. `--reset-state` clears it.

### 4.4 Config

One JSON file (same style as znnd's `config.json`). Example:

```json
{
  "nodeUrl": "ws://127.0.0.1:35998",
  "chainId": 1,
  "pollIntervalSeconds": 30,
  "powTimeoutSeconds": 300,
  "statePath": "/var/lib/az-vote-delegate/state.json",
  "delegations": [
    {
      "pillarName": "MyPillar",
      "follow": "TrustedPillar",
      "producerKeyFile": "/home/znn/.znn/wallet/producer",
      "producerKeyIndex": 0,
      "passwordEnv": "AZ_DELEGATE_PASSWORD_MYPILLAR"
    }
  ]
}
```

- Password comes from the named environment variable, or from `passwordFile` (0600) as an
  alternative; never from the command line or the config file itself.
- `producerKeyFile` is the same key file the pillar's znnd `Producer` config points at
  (znnd wallet key files: argon2id + AES-256-GCM JSON with `baseAddress`). The SDK's
  `wallet.KeyStoreManager.ReadKeyStore` reads that format (it has a key-file conformance
  test); the builder verifies against a real znnd-created key file before shipping.
- Several delegations per process are allowed (operators with more than one pillar). Each
  has its own key and state.

### 4.5 CLI

```
az-vote-delegate run     --config path [--dry-run]     # daemon; dry-run logs the sends it would make
az-vote-delegate status  --config path                  # one-shot table: votable, B's vote, A's vote, action
az-vote-delegate check   --config path                  # preconditions only (key opens, pillars found, chain id)
az-vote-delegate version
```

`run` handles SIGINT/SIGTERM cleanly (finish the in-flight send or cancel its PoW, write
state, exit 0). Logs are structured key=value lines on stderr, one line per decision:
`decision=mirror pillar=A follow=B id=... vote=yes` / `decision=owner-override ...` /
`decision=in-sync ...` (in-sync at debug level only).

### 4.6 Operations

Ship a `systemd` unit example (like ZenonSpamBotv2's) running as the same unprivileged
user that owns the key file, `EnvironmentFile=` for the password, `Restart=on-failure`.
No Docker in v1. README states plainly: the daemon holds the producer key in memory while
running, which is the same exposure the pillar node already has; run it on the pillar host
or a host with equivalent protection, and point `nodeUrl` at the pillar's own node or at a
node the operator trusts (reads and the publish both go through it).

## 5. Decisions already made

- Off-chain mirror, not an on-chain feature (user, 2026-10-18).
- Go, module `github.com/0x3639/znn-sdk-go` v0.5.0, Go 1.26 (user: "use the 0x3639 go
  sdk"). No direct dependency on go-zenon beyond what the SDK pulls in.
- Producer key only; `VoteByProdAddress` only (my call: the owner key stays cold; the
  producer key is already hot on the node).
- Owner's manual vote wins, sticky per votable until a reset (my guess at the user's intent,
  stated in chat and not objected to).
- Polling, not subscriptions (my call: a 30 s poll is cheap, survives websocket drops, and
  the reconcile is stateless anyway).
- JSON config and JSON state (my call: matches znnd's config style).
- Multiple delegations per process (my call).
- Repo and binary name: `az-vote-delegate` (user, 2026-10-18), under the 0x3639 GitHub account.

## 6. Left to the builder

- Package layout, logging library, CLI library (stdlib `flag` is fine).
- Exact backoff numbers, log levels, state-file schema version field.
- Whether `status` prints the time left in a project's 14-day window (nice to have).
- Unit tests: the reconcile function takes an interface (`listVotables`, `pillarVotes`,
  `send`) so it can be table-tested with a fake; cover every branch in §4.1, including
  phase reset, owner override, override cleared by reset, delegate changing their vote,
  delegate abstaining, and state loss. Test the vote-value mapping both ways.
- Integration: one smoke run against a devnet or testnet node with a real key file; record
  the command in the README. go-zenon's `docs/testnet/` describes a local testnet.
- Verify two SDK assumptions early and report if either fails: (1) `ReadKeyStore` opens a
  znnd-created producer key file; (2) `SendWithContext` sets chain id, frontier, PoW and
  signature correctly for an embedded call from an address with zero fused plasma.

## 7. Acceptance

- `check` passes against a mainnet node with a real producer key file (dry).
- On a testnet: B votes yes on a project → within one poll A's vote reads yes; B changes to
  no → A reads no; owner votes abstain from A's producer address by hand → daemon logs
  owner-override and does not touch it; phase updated by project owner → A's vote is gone,
  daemon re-mirrors B's new vote.
- Unit tests for §4.1 pass; `go vet` and `golangci-lint` clean.

## 8. Operator UI and release amendment (2026-10-18)

The implementation now includes a NOMCTL-style terminal menu, a first-run wizard,
a checksum-verifying curl installer, static Linux release builds, and a private
Unix control socket. The background service owns every signing operation. Manual
votes from the UI are persisted as explicit per-votable choices, including votes
that equal the previous daemon vote. A manual request cancels copy preparation
and waits for any already submitted vote to execute before being applied, if the
request remains open. External-tool votes retain the limitations described in
the README. The initial assumption that every manual vote can be inferred from
current vote values alone is not a guarantee.

Project proposals and phase-payout requests have the same copy/manual paths.
The go-zenon UpdatePhase implementation replaces the current phase ID with the
update block hash, deletes the old votes and removes the old votable hash. The
new ID therefore starts a fresh round; an old manual override is not inherited.
A same-ID observed vote wipe also clears a confirmed override.

New state fields manualVote and manualConfirmed persist UI ownership. The live
config field saves the operator's chosen voting mode. Setup presents explicit
Dry run and Live choices, defaulting to Dry run, followed by a separate save
confirmation. The installed service starts in the confirmed mode. See README.md for
current setup, operation and release instructions.

## 9. Installation and delegate-picker amendment (2026-10-18)

The curl installer launches the terminal menu through /dev/tty. Setup explicitly
asks whether to install and start the service. Noninteractive installations and
AZ_NO_UI=1 skip the menu. Later runs of az-vote-delegate reopen it.

Both setup and delegate changes show the top 30 active pillars other than the
operator's own pillar, ranked by the number of retained AZ project and phase
requests with a recorded vote. Equal counts are shuffled before the cutoff, then
the selected 30 are shuffled for display. Each shows voted/total and a percentage.
The denominator includes all retained requests, even closed ones. It is not
adjusted for historical pillar eligibility or deleted phase revisions.

Manual project and phase lists read fresh chain votes and show only unanswered
requests. All three vote values count as answered. Queued manual choices move
to Activity, which also offers resuming copy voting for existing overrides. The
service rechecks that a selected request remains unanswered before queuing a
manual choice. Explicit override persistence and pending-transaction handling
remain unchanged underneath this menu filter.

## Copy an address amendment (2026-10-19)

`follow` takes either a pillar name or a z1 address. A pillar's votes are read
from contract state as before. An address's votes are read from its own
account chain: for each votable id, the newest confirmed user-send block to the
Accelerator contract whose data is `VoteByProdAddress(id, vote)` or
`VoteByName(id, name, vote)`, with any claimed name ignored.

This is a proxy vote. The contract accepts a vote block
from any address and refuses it on receive unless the sender is a pillar's owner
or producer, so the vote is never counted, but the send block is confirmed and
stays on the sender's chain. Copying it lets an operator vote from a phone or a
wallet they do not trust with the pillar's keys: the key on that device can
direct the pillar's AZ votes and nothing else, and can be replaced by pointing
`follow` somewhere else.

The address's votes go through the same reconcile as a pillar's: copy delay,
owner override, manual votes, the pre-publish guard. Following the pillar's own
producer address is refused at `check`, since those blocks are the daemon's own
copies. The address chain is read from height 1 on each poll, which is cheap for
an address kept for voting and slow for a busy one. A pillar named exactly like
a valid address would be read as an address; none is known.

## Configurable copy delay amendment

`copyDelayMinutes` defaults to zero and accepts whole minutes from 0 through 10080.
Each request persists a source pillar, vote, and first-observed timestamp while it
needs copying. Reconciliation waits until that timestamp plus the configured delay,
without blocking other requests. The existing active-request and owner/delegate
checks still run before publishing. A changed source vote, disappeared vote, observed
round reset, or changed source pillar starts a new timer. Manual intent clears the
timer and bypasses the delay. Live timers survive restarts; dry-run observations do
not persist. Changing the setting applies to waiting and future copies. Timing is
approximate because polling, PoW, retries, and outages can add time; no request is
submitted early to beat a closing window. Public vote correlation remains possible.
