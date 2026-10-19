---
title: Delegation & vote delays
description: Choose a trusted pillar and set a delay before copying AZ and phase-payout votes.
---

Select a pillar during setup, or choose **Choose pillar to copy** from the main menu. The service copies that pillar's votes on eligible AZ proposals and phase-payout requests.

## The top 30 voters

The picker ranks active pillars by the number of distinct requests they have voted on, excludes your own pillar, and takes the top 30. If fewer are available, it shows them all.

The list is shuffled each time you open it. Ties are randomly ordered before the cutoff, so names near the start of the alphabet do not get priority.

An option may show:

```text
82.0% participation · 82/100 requests
```

Yes, no, and abstain all count as participation. The denominator includes all project proposals and retained phase requests returned by the node, including closed requests. It is not adjusted for when a pillar joined. Replaced or deleted phase revisions cannot be included when the node no longer returns them.

Participation tells you how often a pillar has voted in the available history. It does not tell you whether you agree with its decisions. With no recorded requests, the menu reports that instead of inventing a percentage.

## Copy an address

You can copy any address instead of a pillar. In the picker, choose **Copy an address (proxy votes)** and enter a z1 address. In `config.json`, put the address in `follow`.

Use this to vote from a phone or a wallet you do not trust with your pillar keys. Send the vote from that address to the Accelerator contract, as any wallet can. The contract does not count a vote from an address that runs no pillar, but the block stays on that address's chain. On its next poll the service reads it and casts the same vote with your producer key.

- The address's newest confirmed vote on each request is the one copied. Votes sent by producer address and by pillar name both count; a claimed pillar name is ignored.
- Everything else on this page applies: the copy delay, manual votes and overrides.
- Whoever holds the address's key decides your copied votes, and nothing else. Keep an address for voting only. If its key is lost or exposed, choose another address or a pillar to copy.
- You cannot follow your own producer address; those blocks are the service's own votes.

## What gets copied

The service checks current votes on each poll. It follows open initial project proposals within their voting window and the current voting phase of active projects. If the delegate has not voted, there is no vote to copy yet.

Project proposals and phase payouts have separate request IDs. A vote on a project does not automatically count as a vote on its phase. When an updated phase receives a new votable ID, the app treats it as a fresh voting round.

The app votes on payout requests. It does not submit a project owner's payment request or guarantee an immediate payment; the contract rules and other pillars' votes still determine the result.

## Delay copied votes

Available from **v0.3.0**. Set **Copy vote delay (minutes)** during setup, or select **Copy vote delay: … minutes** in the main menu. Enter a whole number from **0 to 10,080** (7 days). The default is **0**, which preserves copying without an intentional delay. One setting applies to all pillars managed by that configuration.

For example, set **15** to wait at least 15 minutes after the service first observes a vote that needs copying. If it sees the vote at 12:00, the earliest submission is 12:15. The timer starts when the service observes the vote, not when the delegate originally voted on-chain. An old vote first seen after installing the app gets the same delay.

Each proposal and phase-payout request has its own timer:

- Live-mode timers are saved locally and survive service restarts and upgrades. Time spent offline counts toward an existing timer; the service rechecks the current votes before submitting after restart.
- A changed delegate vote or source pillar starts a new timer when observed. If the vote disappears or a round reset is observed, the old timer is discarded. A phase revision with a new request ID starts fresh.
- Manual votes made in the app bypass the copy delay and protect that request. Other votes keep their own timers. External manual votes detected by the service also prevent copying.
- Changing the delay takes effect without restarting the service and recalculates waiting votes from their original observation times. Reducing it, including setting it to 0, can make a waiting vote eligible on the next poll. Already submitted transactions cannot be recalled.
- Dry-run timers are previews held in memory. They are not saved. Switching to Live reloads the durable state, so new observations made only in Dry run start a new live timer.

Open **Pending transactions & manual overrides** to see waiting votes and their earliest submission times in UTC. The `status` command also shows `waiting-delay` and a **COPY AFTER (UTC)** column. Submission happens on a poll after the delay; polling (30 seconds by default), proof of work, retries, or node outages can add time.

A request may close before its timer expires. The app skips closed requests and does not shorten your delay to vote before a deadline. Choose a delay that suits your participation goals.

### What a delay does for privacy

A delay separates your vote from the moment the service observes the delegate's vote. It does **not** hide votes on the public ledger or guarantee that delegation cannot be inferred. Matching choices, repeated timing patterns, and fixed delays can still reveal a relationship. The delay is a timing preference, not an anonymity feature.

## Change who you follow

Choose **Choose pillar to copy**, select a new pillar, and confirm. Your saved [manual overrides](./manual-voting.md) stay in place. Other eligible requests follow the new delegate.

Delegation chains can work, but an empty cycle where two pillars only follow each other produces no initial vote. Choose a pillar that makes voting decisions.
