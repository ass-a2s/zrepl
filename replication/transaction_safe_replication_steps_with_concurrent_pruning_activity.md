## Algorithm for transaction-safe replication steps with uncontrolled concurrent pruning activity

Goal: ensure forward progress in performing a single replication step in the presence of intermediary failures during replication and uncontrolled external snapshot pruning activity on either side
= guaranteeing that a full retry or resumable send & recv retry won't have to abort because the network failed sometime during replication or because an external tool pruned away the step's `from` or `to`

---

Fully sequential procedure for replication_step(`from`, `to`, `jobid`).

Prepare Send-side:

- hold `to` using `idempotent_hold(to, zrepl_${jobid})`
- make sure `from` doesn't go away:
  - if `from` is a snapshot: hold `from` using `idempotent_hold(from, zrepl_${jobid})`
  - else check if `from` is a valid replication cursor bookmark, otherwise go to next if branch
    - `from` must have the form `#zrepl_replication_cursor_G_([0-9]+)_J_(.+)`
    - capture group `1` must be the guid of `zfs get guid ${from}`
    - capture group `2` must be qual to `jobid`
  - else if `from` is another bookmark: ERROR OUT
    - Why? we must assume the given bookmark is externally managed (i.e. not in the )
    - this means the bookmark is externally created bookmark and cannot be trusted to persist until the replication step succeeds
    - => ERROR OUT (or clone the bookmark to become a `#zrepl_replication_cursor_${guid}_${jobid}`-form bookmark)

Prepare Recv-side:

- hold `from` using `if from != nil: idempotent_hold(from, zrepl_J_${jobid})`
- `to` cannot be destroyed while being received (ASSUMPTION)

Attempt the replication step.<br>
**Safety from external pruning**
We are safe from pruning during the replication step because we have guarantees that no external action will destroy send-side `from` and `to`, and recv-side `to` (for both snapshot and bookmark `from`s)<br>
**Safety In Presence of Network Failures During Replication**
Further, we are safe from any network failures during the replication step:
- Network failure before the replication starts:
  - The next attempt will find send-side `from` and `to`, and recv-side `from` still present due to locks
  - It will retry the step from the start
  - If the step planning algorithm does not include the step, for example because a snapshot filter configuration was changed by the user inbetween which hides `from` or `to` from the second planning attempt: tough luck, we're leaking all holds
- Network failure during the replication
  - The next attempt will find send-side `from` and `to`, and recv-side `from` still present due to locks
  - If resume state is present on the receiver, the resume token will also continue to work because `from`s and `to` are still present
- Network failure at the end of the replication step stream transmission
  - Variant A: Failure from the sender's perspective, success from the receiver's perspective
    - (This situation is the reason why we are developing this algorithm, it actually happened!)
    - receive-side `to` doesn't have a hold and could be affectecd by pruning policy
    - receive-side `from` is still locked, so the next attempt will
        - determine that `to` is still on the receive-side and continue below
        - determine that receive-side `to` has been pruned and re-transmit `from` to `to`, which is guaranteed to work because all locks for this are still held
  - Variant B: Success form the sender's perspective, failure from the receiver's perspective
    - No idea how this would happen except for bugs in error reporting in the replication protocol
    - Misclassification by the sender, most likely broken error handling in the sender or replication protocol
    - => the sender will release holds and move the replication cursor while the receiver won't => tough luck

INVARIANT: the receiver has explicitly confirmed that the receive has been completed successfully.

Post-actions Recv-side:

- Idempotently hold `to` using `idempotent_hold(to, zrepl_${jobid})`
  - Ideally, this should happen at the time of `zfs recv`.
    - We can use the `zfs send -h` feature to send the sender's hold to the receiver (=> `jobid` !?)
  - Otherwise, there's a brief window where the receive-side external pruning might destroy `to`.
  - However, we're still holding send-side `from` and `to`, and recv-side `from`.
  - So, if recv-side `to` were pruned, we could still re-try this replication step.
- Release `from` using `idempotent_release(from, zrepl_${jobid})`

Post-actions Send-Side:

- Move the replication cursor 'atomically' by having two of them (fixes [#177](https://github.com/zrepl/zrepl/issues/177))
  - Idempotently cursor-bookmark `to` using `idempotent_bookmark(to, to.guid, #zrepl_replication_cursor_${to.guid}_J_${jobid})`
  - Idempotently destroy old cursor-bookmark of `from` `idempotent_destroy(#zrepl_replication_cursor_${from.guid}, from)`
- If `from` is a snapshot, release the hold on it using `idempotent_release(from,  zrepl_J_${jobid})`

---

**NOTES**

- `idempotent_hold(snapshot s, string tag)` like zfs hold, but doesn't error if hold already exists
- `idempotent_release(snapshot s, string tag)` like zfs hold, but doesn't error if hold already exists
- `idempotent_destroy(bookmark #b_$GUID, of a snapshot s)` must atomically check that `$GUID == s.guid` before destroying s
- `idempotent_bookmark(snapshot s, $GUID, name #b_$GUID)` must atomically check that `$GUID == s.guid` at the time of bookmark creation
- `idempotent_destroy(snapshot s)` must atomically check that zrepl's `s.guid` matches the current `guid` of the snapshot (i.e. destroy by guid)