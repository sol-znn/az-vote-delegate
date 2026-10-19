---
title: Installation & setup
description: Install az-vote-delegate on a Linux pillar host and choose how it votes.
---

Install on the Linux host that runs your Zenon pillar. You need an amd64 or arm64 machine, access to a trusted node, and your existing **producer key**. The app uses the producer key, never the owner's stake key.

This release supports legacy fused plasma and proof of work. **Dynamic Plasma is not supported.**

## Install

For a node operated as root:

```bash
curl -fsSL https://raw.githubusercontent.com/0x3639/az-vote-delegate/main/install.sh | sudo sh
```

If your node runs as an unprivileged user, sign in as that user and omit `sudo`:

```bash
curl -fsSL https://raw.githubusercontent.com/0x3639/az-vote-delegate/main/install.sh | sh
```

The installer downloads the latest stable release, verifies its SHA-256 checksum, and opens setup in your terminal. No compiler or Docker is required. Root installations use `/usr/local/bin`; user installations use `~/.local/bin`.

If setup does not open automatically, run `sudo az-vote-delegate` for a root installation or `~/.local/bin/az-vote-delegate` for a user installation.

## Complete setup

1. **Connect to your node.** Confirm the node URL and expected chain ID. The defaults are `ws://127.0.0.1:35998` and mainnet chain ID `1`.
2. **Install the service.** Choose **Yes** when asked to install and start a background service. It keeps working after you close the terminal menu.
3. **Load your producer key.** Choose **Use existing znnd configuration**, or select a producer key file and enter its derivation index and password. No new key is generated and your znnd configuration is unchanged.
4. **Choose a pillar to copy.** The menu shows the top 30 active voters in random order, with participation figures. [How participation is calculated](./delegation.md). To vote from another wallet instead, choose **Copy an address (proxy votes)**. [Copy an address](./delegation.md#copy-an-address).
5. **Set a copy vote delay.** Enter the number of minutes to wait before copying each vote. `0` means no delay; manual votes bypass this setting. [Delay behavior](./delegation.md#delay-copied-votes).
6. **Choose your voting mode.** **Dry run** previews decisions without submitting votes; **Live** submits votes on eligible AZ proposals and phase-payout requests. Dry run is selected by default.
7. **Save and start.** Review the mode and confirm. The service starts in the mode you selected.

The wizard saves a private password file so the service can unlock the key after a reboot. The file is readable and writable only by its owner (mode `0600`). Keep it and your application configuration private.

For a regular user's service to keep running after logout and start at boot, enable lingering. Replace `YOUR_NODE_USER` with the account that runs the node:

```bash
sudo loginctl enable-linger YOUR_NODE_USER
```

## Check and run

The main menu shows your mode, your pillar, and the pillar it follows. Start in **Dry run** to review the decisions on your node. When ready, select **Voting mode: Dry run / Live**, choose **Live**, and confirm.

Check the [service status and logs](./service.md) to confirm the background worker is running. Closing the menu does not stop it. Switching to Dry run does not undo transactions already submitted.

## Reopen the menu

For a root installation:

```bash
sudo az-vote-delegate
```

For a user installation:

```bash
~/.local/bin/az-vote-delegate
```

Use the same user and configuration each time. **Esc** goes back one screen; at the main menu it exits. **Ctrl+C** exits the menu from any screen. During setup, Esc cancels unsaved setup and **Shift+Tab** returns to the previous field within a step.

Next, learn about [manual votes and phase payouts](./manual-voting.md) or [upgrading an existing installation](./updates.md).
