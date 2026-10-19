# AZ Vote Delegate

Too busy to vote on AZs? Copy a pillar you trust while keeping control of your own votes. This terminal app uses your existing producer key to vote on AZ proposals and phase payouts; it never needs your wallet seed or stake key.

## Quick start

On your Linux pillar host, run as the user who owns the producer key:

```bash
curl -fsSL https://raw.githubusercontent.com/0x3639/az-vote-delegate/main/install.sh | sudo sh
```

Omit `sudo` if your node runs as a regular user.

1. Follow the setup menu and install the background service.
2. Choose a pillar or an address to copy, and a **copy vote delay** in minutes (`0` means no delay).
3. Preview in **Dry run**, or select **Live** to start voting.

Copying an address lets you vote from a wallet you do not trust with your pillar keys: send the vote from that address, and the service casts the same vote with your producer key. Any address can send a vote block to the Accelerator contract; the contract does not count it, but it stays on that address's chain, where the service can read it. Whoever holds that address's key decides your copied votes, so use one you keep for voting.

Reopen the menu with `sudo az-vote-delegate` (or `~/.local/bin/az-vote-delegate` for a regular-user installation). Change delegates, adjust the delay, cast manual votes, or upgrade from there. Manual votes protect that request while other votes keep copying.

**[Full documentation & quick start](https://www.0x3639.com/az-vote-delegate/)**

- [Installation & setup](https://www.0x3639.com/az-vote-delegate/getting-started/)
- [Delegation & vote delays](https://www.0x3639.com/az-vote-delegate/delegation/)
- [Manual voting](https://www.0x3639.com/az-vote-delegate/manual-voting/)
- [Updates](https://www.0x3639.com/az-vote-delegate/updates/) · [Troubleshooting](https://www.0x3639.com/az-vote-delegate/troubleshooting/)
- [Commands & configuration](https://www.0x3639.com/az-vote-delegate/commands/) · [Development](https://www.0x3639.com/az-vote-delegate/development/)

**Compatibility:** Linux amd64/arm64; legacy fused plasma / proof of work only. Dynamic Plasma is not supported. Votes remain public; a delay does not guarantee privacy.

[GPL-3.0](LICENSE). Bundled dependency notices are included in release archives.
