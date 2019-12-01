## Algorithm for a single replication step

The algorithm described below describes how a single replication step must be implemented.
A *replication step* is a full send of a snapshot `to` for initial replication, or an incremental send (`from => to`) for incremental replication.

The algorithm **ensures resumability** of the replication step in presence of
* uncoordinated or unsynchronized destruction of the filesystem, snapshots, or bookmarks involved in the replication step
* network failures at any time
* other instances of this algorithm executing the same step in parallel (e.g. concurrent replication to different destinations)
To accomplish this goal, the algorithm **assumes ownersip of parts of the ZFS hold tag namespace and the bookmark namespace**:
* holds with prefix `zrepl_STEP` on any snapshot are reserved for zrepl
* bookmarks with prefix `zrepl_STEP` are reserved for zrepl
Manipulation of these reserved sub-namespaces through software other than zrepl may result in undefined behavior at runtime.

Note that the algorithm **does not ensure** that a replication *plan*, which describes a *set* of replication steps, can be carried out successfully.
If that is desirable, additional measures outside of this algorithm must be taken.
Note that unless stated otherwise, these measures must not rely on the 

---

### Definitions:

#### Step Completion & Invariants

The replication step (full `to` send or `from => to` send) is complete iff the algorithm ran to completion without errors or a permanent non-network error is reported by sender or receiver.

Specifically, the algorithm may be invoked with the same `from` and `to` arguments, and potentially a `resume_token`, after a temporary (like network-related) failure:
**Unless permanent errors occur, repeated invocations of the algorithm with updated resume token will converge monotonically (but not strictly monotonically) toward completion.**

Note that the mere existence of `to` on the receiving side does not constitue completion, since there may still be post-recv actions to be performed on sender and receiver.

#### Job and Job ID
This algorithm supports *multiple* instances of itself to be run in parallel on the *same* step (full `to` / `from => to` pair).
An example for this is concurrent replication jobs that replicate the same dataset to different receivers.

We require that all parallel invocations provide different and unique `jobid`s.
Violation of `jobid` uniqueness across parallel jobs may result in interference between instances of this algorithm, resulting in potential compromise of resumability.

After a step is completed, `jobid` is guaranteed to not be encoded in on-disk state.
Before a step is commpleted, there is no such guarantee.
The correctness of the algorithm relies on `jobid` not changing for the time the step is not complete.
Violation of `jobid` invariance during a replication step may compromise resumability and may leak the underlying ZFS holds or step bookmarks.
Note the definition of *complete* above.

#### Step Bookmark

A step bookmark is the equivalent of a ZFS hold, but for bookmarks.
A step bookmark is a ZFS bookmark whose name matches the following regex:

```
STEP_BOOKMARK = #zrepl_STEP_bm_G_([0-9a-f]+)_J_(.+)
```

- capture group `1` must be the guid of `zfs get guid ${STEP_BOOKMARK}`, encoded hexadecimal (without leading `0x`) and fixed-length (i.e. padded with leading zeroes)
- capture group `2` must be equal to the `jobid`

### Algorithm

INPUT: `jobid`, `from` (may be nil to indicate full send), `to`, `resume_token` (may be nil)
`from` may be a snapshot or a bookmark, `to` must be a snapshot.

#### Prepare-Phase

**Send-side**

- hold `to` using `idempotent_hold(to, zrepl_STEP_J_${jobid})`
- make sure `from` doesn't go away:
  - if `from` is a snapshot: hold `from` using `idempotent_hold(from, zrepl_STEP_J_${jobid})`
  - else `idempotent_step_bookmark(from)` (`from` is a bookmark)
    - PERMANENT ERROR if this fails (e.g. because `from` is a bookmark whose snapshot no longer exists and we don't have bookmark copying yet (ZFS feature is in development) )
      - Why? we must assume the given bookmark is externally managed (i.e. not in the )
      - this means the bookmark is externally created bookmark and cannot be trusted to persist until the replication step succeeds
      - Maybe provide an override-knob for this behavior

**Recv-side**

- make sure `from` doesn't go away once we received enough data for the step to be resumable
  - need not do anyhthing (in particular no holds on `from`) because `zfs recv -s` does that itself
    ```text
    # do a partial incremental recv @a=>@b (i.e. recv with -s, and abort the incremental recv in the middle)
    # try destroying the incremental source on the receiving side
    zfs destroy p1/recv@a
    cannot destroy 'p1/recv@a': snapshot has dependent clones
    use '-R' to destroy the following datasets:
    p1/recv/%recv
    # => doesn't work, because zfs recv is implemented as a `clone` internally, that's exactly what we want
    ```
- `to` cannot be destroyed while being received, because it isn't visible as a snapshot yet (it isn't yet one after all)

#### Replication Phase

**Attempt the replication step (i.e. start `zfs send` and `zfs recv`) until an error occurs or `recv` returns without error.

We now discus whether we are still safe in situations that *come to mind* (TODO):

**Safety from external pruning**
We are safe from pruning during the replication step because we have guarantees that no external action will destroy send-side `from` and `to`, and recv-side `to` (for both snapshot and bookmark `from`s)<br/>

**Safety In Presence of Network Failures During Replication**<br/>
We are safe from any network failures during the replication step:
- Network failure before the replication starts:
  - The next attempt will find send-side `from` and `to`, and recv-side `from` still present due to holds (see **Safety from external pruning** above)
  - It will retry the step from the start
  - If the step planning algorithm does not include the step, for example because a snapshot filter configuration was changed by the user inbetween which hides `from` or `to` from the second planning attempt: **tough luck, we're leaking all holds**
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
  - Variant B: Success from the sender's perspective, failure from the receiver's perspective
    - No idea how this would happen except for bugs in error reporting in the replication protocol
    - Misclassification by the sender, most likely broken error handling in the sender or replication protocol
    - => the sender will release holds and move the replication cursor while the receiver won't => tough luck

The replication is done (although the *replication step* (this algorithm) is not yet).
We know this because **receiver has explicitly confirmed that the receive has been completed successfully**.

#### Wind-down TODO NAME

**Recv-side**
TODO TOD TODO (`zfs send -h`, then clean up all holds except for zrepl_STEP_J_${jobid} on the receiving side)

- Idempotently hold `to` using `idempotent_hold(to, zrepl_STEP_J_${jobid})` TODO WHY?
  - Ideally, this should happen at the time of `zfs recv`.
    - We can use the `zfs send -h` feature to send the sender's hold to the receiver (=> `jobid` !?)
  - Otherwise, there's a brief window where the receive-side external pruning might destroy `to`.
  - However, we're still holding send-side `from` and `to`, and recv-side `from`.
  - So, if recv-side `to` were pruned, we could still re-try this replication step.
- Release `from` using `idempotent_release(from, zrepl_J_${jobid})` TODO only necessary if we hold it in pre-actions, which I'm not convinced is actually necessary

**Send-side**

TODO invoke callback for replication plan executor to move its replication cursor.
That callback does the following:

- Move the replication cursor 'atomically' by having two of them (fixes [#177](https://github.com/zrepl/zrepl/issues/177))
  - Idempotently cursor-bookmark `to` using `idempotent_bookmark(to, to.guid, #zrepl_replication_cursor_G_${to.guid}_J_${jobid})`
  - Idempotently destroy old cursor-bookmark of `from` `idempotent_destroy(#zrepl_replication_cursor_G_${from.guid}_J_${jobid}, from)`
- If `from` is a snapshot, release the hold on it using `idempotent_release(from,  zrepl_J_${jobid})`

---

**NOTES**

- `idempotent_hold(snapshot s, string tag)` like zfs hold, but doesn't error if hold already exists
- `idempotent_release(snapshot s, string tag)` like zfs hold, but doesn't error if hold already exists
- `idempotent_destroy(bookmark #b_$GUID, of a snapshot s)` must atomically check that `$GUID == s.guid` before destroying s
- `idempotent_bookmark(snapshot s, $GUID, name #b_$GUID)` must atomically check that `$GUID == s.guid` at the time of bookmark creation
- `idempotent_destroy(snapshot s)` must atomically check that zrepl's `s.guid` matches the current `guid` of the snapshot (i.e. destroy by guid)