## Algorithm for a Single Replication Step

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

**We require that all parallel invocations of this algorithm provide different and unique `jobid`s.**
Violation of `jobid` uniqueness across parallel jobs may result in interference between instances of this algorithm, resulting in potential compromise of resumability.

After a step is *completed*, `jobid` is guaranteed to not be encoded in on-disk state.
Before a step is completed, there is no such guarantee.
Violation of `jobid` invariance before step completion may compromise resumability and may leak the underlying ZFS holds or step bookmarks (i.e. zrepl won't clean them up)
Note the definition of *complete* above.

#### Step Bookmark

A step bookmark is our equivalent of ZFS holds, but for bookmarks.<br/>
A step bookmark is a ZFS bookmark whose name matches the following regex:

```
STEP_BOOKMARK = #zrepl_STEP_bm_G_([0-9a-f]+)_J_(.+)
```

- capture group `1` must be the guid of `zfs get guid ${STEP_BOOKMARK}`, encoded hexadecimal (without leading `0x`) and fixed-length (i.e. padded with leading zeroes)
- capture group `2` must be equal to the `jobid`

### Algorithm

INPUT:

* `jobid`
* `from`: snapshot or bookmark: may be nil for full send
* `to`: snapshot, may never be nil
* `resume_token` (may be nil)
* (`from` and `to` must be on the same filesystem)

#### Prepare-Phase

Send-side: make sure `to` and `from` don't go away

- hold `to` using `idempotent_hold(to, zrepl_STEP_J_${jobid})`
- make sure `from` doesn't go away:
  - if `from` is a snapshot: hold `from` using `idempotent_hold(from, zrepl_STEP_J_${jobid})`
  - else `idempotent_step_bookmark(from)` (`from` is a bookmark)
    - PERMANENT ERROR if this fails (e.g. because `from` is a bookmark whose snapshot no longer exists and we don't have bookmark copying yet (ZFS feature is in development) )
      - Why? we must assume the given bookmark is externally managed (i.e. not in the )
      - this means the bookmark is externally created bookmark and cannot be trusted to persist until the replication step succeeds
      - Maybe provide an override-knob for this behavior

Recv-side: no-op

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

Attempt the replication step:
start `zfs send` and pipe its output into `zfs recv` until an error occurs or `recv` returns without error.

Let us now think about interferences that could occur during this phase, and ensure that none of them compromise the goals of this algorithm, i.e., convergence toward step completion through/and resumability.

**Safety from external pruning**
We are safe from pruning during the replication step because we have guarantees that no external action will destroy send-side `from` and `to`, and recv-side `to` (for both snapshot and bookmark `from`s)<br/>

**Safety In Presence of Network Failures During Replication**
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

If the RPC used for `zfs recv` returned without error, this phase is done.
(Although the *replication step* (this algorithm) is not yet *complete*, see definition of complete).


#### Wind-down TODO NAME

At the end of this phase, all intermediate state we built up to support the resumable replication of the step is destroyed.
However, consumers of this algorithm might want to take advantage of the fact that we currently still have holds / step bookmarks.

**Recv-side: Optionally hold `to` with a caller-defined tag**
Users of the algorithm might want to depend on `to` being available on the receiving side for a longer duration than the lifetime of the current step's algorithm invocation.
For reasons explained in the next paragraph, we cannot guarantee that we have a `hold` simultaneously with `recv`. If that were the case, we could take our time to provide a generalized callback to the user of the algorithm, and have them do whatever they want with `to` while we guarantee that `to` doesn't go away because we hold the lock. But that functionality isn't available, so we only provide a fixed operation right after receive: **take a `hold` with a tag of the algorithm user's choice**. That's what's needed to guarantee that a replication plan, consisting of multiple consecutive steps, can be carried out without a receive-side prune job interfering by destroying `to`. Technically, this step is racy, i.e., it could happen that `to` is destroyed between `recv` and `hold`. But this is a) unlikely and b) not fatal because we can detect that hold fails because the 'dataset does not exist` and re-do the entire transmission since we still hold send-side `from` and `to`, i.e. we just lose a lot of work in rare cases.

So why can't simultaneously `recv` and `hold`?
It seems like [`zfs send --holds`](https://github.com/zfsonlinux/zfs/commit/9c5e88b1ded19cb4b19b9d767d5c71b34c189540) could be used to solve above race condition. But it always sends all holds, and `--holds` is implemented in user-space libzfs, i.e., doesn't happen in the same txg as the recv completion. Thus, the race window is in fact only smaller (unless we oversaw some synchronization in userland zfs).
But if we're willing to entertain the idea a little further, we still hit the problem that `--holds` sends _all_ holds, whereas our goal is to _temporarily_ have >= 1 hold that we own until the callback is done, and then release all of the received holds so that no holds created by us exist after this algorithm completes.
Since there could be concurrent `zfs hold` invocations on the sender and receiver while this algorithm runs, and because `--holds` doesn't provide info about which holds were sent, we cannot correctly destroy _exactly_ those holds that we received.

**Send-side**
We can provide the algorithm user with a generic callback because we have holds / step bookmarks for `to` and `from`, respectively.

Example use case for the sender-side callback:

    Mmove the replication cursor to `to`.
    Side note (this should be moved to the place where we explain how replication planning & execution is done (i.e., multiple steps)):
    - Move the replication cursor 'atomically' by having two of them (fixes [#177](https://github.com/zrepl/zrepl/issues/177))
      - Idempotently cursor-bookmark `to` using `idempotent_bookmark(to, to.guid, #zrepl_replication_cursor_G_${to.guid}_J_${jobid})`
      - Idempotently destroy old cursor-bookmark of `from` `idempotent_destroy(#zrepl_replication_cursor_G_${from.guid}_J_${jobid}, from)`
    - If `from` is a snapshot, release the hold on it using `idempotent_release(from,  zrepl_J_${jobid})`


After the callback is done: cleanup holds & step bookmark:

- `idempotent_release(to, zrepl_STEP_J_${jobid})`
- make sure `from` can go away:
  - if `from` is a snapshot: `idempotent_release(from, zrepl_STEP_J_${jobid})`
  - else `idempotent_step_bookmark_destroy(from)`
    - if `from` fulfills the properties of a step bookmark: destroy it
    - otherwise: it's a bookmark not created by this algorithm that happened to be used for replication: leave it alone
      - that can only happen if user used the override knob

Note that "make sure `from` can go away" is the inverse of "Send-side: make sure `to` and `from` don't go away". Those are relatively complex operations and should be implemented in the same file next to each other to ease maintainability.

---

**NOTES / Pseudo-APIs 

- `idempotent_hold(snapshot s, string tag)` like zfs hold, but doesn't error if hold already exists
- `idempotent_release(snapshot s, string tag)` like zfs hold, but doesn't error if hold already exists
- `idempotent_step_bookmark(snapshot_or_bookmark b)` creates a *step bookmark* of b
  - determine step bookmark name N
  - if `N` already exists, verify it's a correct step bookmark => DONE or ERROR
  - if `b` is a snapshot, issue `zfs bookmark`
  - if `b` is a bookmark:
    - if bookmark cloning supported, use it to duplicate the bookmark
    - else ERROR OUT, with an error that upper layers can identify as such, so that they are able to ignore the fact that we couldn't create a step bookmark 
- `idempotent_destroy(bookmark #b_$GUID, of a snapshot s)` must atomically check that `$GUID == s.guid` before destroying s
- `idempotent_bookmark(snapshot s, $GUID, name #b_$GUID)` must atomically check that `$GUID == s.guid` at the time of bookmark creation
- `idempotent_destroy(snapshot s)` must atomically check that zrepl's `s.guid` matches the current `guid` of the snapshot (i.e. destroy by guid)


## Algorithm for Planning and Executing Replication of a Filesystems

This algorithm describes a technique to plan the replication of a filesystem or zvol.
The algorithm is invoked with a sender and receiver, and a sender-side filesystem path.
It builds a diff between the sender and receiver filesystem bookmarks+snapshots and determines whether a replication conflict exists or whether fast-forward replication using full or incremental sends is possible.
In case of conflict, the algorithm errors out with a conflict description that can be used to manually or automatically resolve the conflict.
Otherwise, the algorithm builds a list of replication steps that are then worked on sequentially by the "Algorithm for a Single Replication Step".

The algorithm ensures that a plan can be executed exactly as planned by aquiring appropriate zfs holds.
The algorithm can be configured to retry a plan when encountering non-permanent errors (e.g. network errors)
However, permanent errors result in the plan being aborted.
Regardless of whether a plan can be executed to completion or is aborted, the algorithm guarantees that leftover artifacts (i.e. holds) of replication between the send-side and receive-side filesystem are cleaed up before a new plan starts executing.
Thus, there might be stale holds on sending or receiving side after a crash or other permanent error, but those are removed before re-planning is done.

The algorithm is fit to be executed in parallel from the same sender to different receivers.

The algorithm reserves the following sub-namespaces:
* zfs hold: `zrepl_FS`

### Definitions

#### ZFS Filesystem Path
We refer to the name of a ZFS filesystem or volume as a *filesystem path*, sometimes just *path*.
The name includes the pool name.

#### Sender and Receiver
Sender and Receiver are passed as RPC stubs to the algorithm.
One of those RPC stubs will typically be local, i.e., call methods on a struct in the same address space.
The other RPC stub may invoke actual calls over the network (unless local replication is used).
The algorithm does not make any assumption about which object is local or an actual stub.
This design decouples the replication logic (described in this algorithm) from the question which side (sender or receiver) initiates the replication.

Apart form a set of common RPCs, Senders implement the `Send` rpc.
Receivers implement the `Receive` rpc.


## Algorithm for Planning and Executing Replication of Multiple Filesystems matched by a Filter
The model of the receiver includes the assumption that it will `Receive` all snapshot streams sent to it into filesystems with paths prefixed with `rootfs`.
This is useful for separating filesystem path namespaces for multiple clients.
The receiver hides this prefixing in its RPC interface, i.e., when responding to `ListFilesystems` rpcs, the prefix is removed before sending the response.
This behavior is useful to achieve symmetry in this algorithm: we do not need to take the prefixing into consideration when computing diffs.
For the receiver, it has the advantage that `Receive` rpc can enforce the namespacing and need not trust this algorithm to apply it correctly.

The receiver is also allowed (and may need to) do implement other filtering / namespace transformations.
For example, when only `foo/a/b` has been received, but not `foo` nor `foo/a`, the receiver must not include the latter two in its `ListFilesystems` response.
The current zrepl receiver endpoint implementation uses the `zrepl:placeholder` property to mark filesystems `foo` and `foo/a` as placeholders, and filters out such filesystems in the `ListFilesystems` response.
Another approach would be to have a flat list of received filesystems per sender, and have a separate table that associates send-side names and receive-side filesystem paths.

Similarly, the sender is allowed to filter its view filesystems and snapshots.

### Algorithm

Determine send-side and recv-side snapshots, and try to build a fast-forward list replication steps STEPS.
STEPs may contain an optional initial full send, and subsequent incremental send steps.
The first step in the list may include a resume token.
If fast-forward is not possible, produce a conflict description and ERROR OUT.
TODOs:
- make it configurable what snapshots are included in the list (i.e. every one we see, only most recent, at least one every X hours, ...)

Ensure that the plan can be carried out by aquiring holds / step bookmarks on each of the steps
- `for to in STEPS.to: idempotent_hold(to, zrepl_FS_)`
- `if STEPS[0].from != nil: idempotent_STEP_bookmark(STEPS[0].from, zrepl_FS_)` TODO fixme different namespace for STEP and FS, generalize the procedure
  
Find the next step that we need to do: (we might be in an second invocation of this algorithm after a network failure and some steps might already be done):
- `res_tok := receiver.ResumeToken(fs)`
- `rmrfsv := receiver.MostRecentFilesystemVersion(fs)`
- if `res_tok !=  nil`: ensure that `res_tok` has a correspondinng step in `STEPS`, otherwise ERROR OUT
- if `rmrfsv != nil`: ensure that `res_tok` has a correspondinng step in `STEPS`, otherwise ERROR OUT
- if `(res_token != nil && rmrfsv != nil)`: ensure that `res_tok` is the subsequent step to the one we found for `rmrfsv`
- if both are nil, ERROR OUT (TODO when can this happen)

WIPfrom here

- `v := if res_tok != nil { res_tok } else rmrfsv`
- `last_completed_step = STEPS.find(|s| s.to == rmrfsv)`
  - if not found
- match up `rfsvs` with the steps we have planned
- 
- Execute the plan by idempotently applying each steps _from the beginning_: `for step in STEPS`
- `resp = receiver.PrepareRecv(step)`
  - if `resp` is `already_received` => this step is done, go to next step
  - if `resp` is `