// Package endpoint implements replication endpoints for use with package replication.
package endpoint

import (
	"context"
	"fmt"
	"os"
	"path"

	"github.com/pkg/errors"

	"github.com/zrepl/zrepl/replication/logic/pdu"
	"github.com/zrepl/zrepl/util/chainlock"
	"github.com/zrepl/zrepl/util/envconst"
	"github.com/zrepl/zrepl/util/semaphore"
	"github.com/zrepl/zrepl/zfs"
)

type SenderConfig struct {
	FSF     zfs.DatasetFilter
	Encrypt bool
}

// Sender implements replication.ReplicationEndpoint for a sending side
type Sender struct {
	FSFilter zfs.DatasetFilter
	encrypt  bool
}

func NewSender(conf SenderConfig) *Sender {
	return &Sender{FSFilter: conf.FSF, encrypt: conf.Encrypt}
}

func (s *Sender) filterCheckFS(fs string) (*zfs.DatasetPath, error) {
	dp, err := zfs.NewDatasetPath(fs)
	if err != nil {
		return nil, err
	}
	if dp.Length() == 0 {
		return nil, errors.New("empty filesystem not allowed")
	}
	pass, err := s.FSFilter.Filter(dp)
	if err != nil {
		return nil, err
	}
	if !pass {
		return nil, fmt.Errorf("endpoint does not allow access to filesystem %s", fs)
	}
	return dp, nil
}

func (s *Sender) ListFilesystems(ctx context.Context, r *pdu.ListFilesystemReq) (*pdu.ListFilesystemRes, error) {
	fss, err := zfs.ZFSListMapping(ctx, s.FSFilter)
	if err != nil {
		return nil, err
	}
	rfss := make([]*pdu.Filesystem, len(fss))
	for i := range fss {
		encEnabled, err := zfs.ZFSGetEncryptionEnabled(ctx, fss[i].ToString())
		if err != nil {
			return nil, errors.Wrap(err, "cannot get filesystem encryption status")
		}
		rfss[i] = &pdu.Filesystem{
			Path: fss[i].ToString(),
			// ResumeToken does not make sense from Sender
			IsPlaceholder: false, // sender FSs are never placeholders
			IsEncrypted:   encEnabled,
		}
	}
	res := &pdu.ListFilesystemRes{Filesystems: rfss}
	return res, nil
}

func (s *Sender) ListFilesystemVersions(ctx context.Context, r *pdu.ListFilesystemVersionsReq) (*pdu.ListFilesystemVersionsRes, error) {
	lp, err := s.filterCheckFS(r.GetFilesystem())
	if err != nil {
		return nil, err
	}
	fsvs, err := zfs.ZFSListFilesystemVersions(lp, nil)
	if err != nil {
		return nil, err
	}
	rfsvs := make([]*pdu.FilesystemVersion, len(fsvs))
	for i := range fsvs {
		rfsvs[i] = pdu.FilesystemVersionFromZFS(&fsvs[i])
	}
	res := &pdu.ListFilesystemVersionsRes{Versions: rfsvs}
	return res, nil

}

var maxConcurrentZFSSendSemaphore = semaphore.New(envconst.Int64("ZREPL_ENDPOINT_MAX_CONCURRENT_SEND", 10))

func (s *Sender) Send(ctx context.Context, r *pdu.SendReq) (*pdu.SendRes, zfs.StreamCopier, error) {

	_, err := s.filterCheckFS(r.Filesystem)
	if err != nil {
		return nil, nil, err
	}
	switch r.Encrypted {
	case pdu.Tri_DontCare:
		// ok, fallthrough outer FIXME
	case pdu.Tri_False:
		if s.encrypt {
			return nil, nil, errors.New("only encrytped sends allowed (send -w + encryption!= off), but unencrytped send requested")
		}
		// fallthrough outer
	case pdu.Tri_True:
		if !s.encrypt {
			return nil, nil, errors.New("only unencrypted sends allowed, but encrypted send requested")
		}
		// fallthrough outer
	default:
		return nil, nil, fmt.Errorf("unknown pdu.Tri variant %q", r.Encrypted)
	}

	sendArgsVersionFromPDU := func(fsv *pdu.FilesystemVersion) *zfs.ZFSSendArgVersion {
		if fsv == nil {
			return nil
		}
		return &zfs.ZFSSendArgVersion{RelName: fsv.RelName(), GUID: fsv.Guid}
	}
	sendArgs := zfs.ZFSSendArgs{
		FS:          r.Filesystem,
		From:        sendArgsVersionFromPDU(r.From),
		To:          sendArgsVersionFromPDU(r.To),
		Encrypted:   &zfs.NilBool{B: s.encrypt},
		ResumeToken: r.ResumeToken, // nil or not nil, depending on decoding success
	}

	getLogger(ctx).Debug("acquire concurrent send semaphore")
	// TODO use try-acquire and fail with resource-exhaustion rpc status
	// => would require handling on the client-side
	// => this is a dataconn endpoint, doesn't have the status code semantics of gRPC
	guard, err := maxConcurrentZFSSendSemaphore.Acquire(ctx)
	if err != nil {
		return nil, nil, err
	}
	defer guard.Release()

	si, err := zfs.ZFSSendDry(ctx, sendArgs)
	if err != nil {
		return nil, nil, errors.Wrap(err, "zfs send dry failed")
	}

	// From now on, assume that sendArgs has been validated by ZFSSendDry
	// (because validation invovles shelling out, it's actually a little expensive)
	// FIXME: track validation status in sendArgs, make it immutable?
	sendArgsFSDP, _ := zfs.NewDatasetPath(sendArgs.FS)

	var expSize int64 = 0      // protocol says 0 means no estimate
	if si.SizeEstimate != -1 { // but si returns -1 for no size estimate
		expSize = si.SizeEstimate
	}
	res := &pdu.SendRes{
		ExpectedSize:    expSize,
		UsedResumeToken: r.ResumeToken != "",
	}

	if r.DryRun {
		return res, nil, nil
	}

	// make sure incremental replication is resumable even if `From` is destroyed
	// (if `From` is a snapshot), by bookmarking it
	if sendArgs.From != nil && r.GetFrom().GetType() == pdu.FilesystemVersion_Snapshot {
		// in theory, this should always be a no-op (idempotent!)
		err := zfs.ZFSSetReplicationCursor(sendArgsFSDP, sendArgs.From.RelName, sendArgs.From.GUID)
		if err != nil {
			return nil, nil, errors.Wrap(err, "cannot set replication cursor to `from` version before starting send")
		}
	}
	// make sure replication is resumable by creating a hold on `To`
	err = zfs.ZFSHold(ctx, sendArgs.FS, sendArgs.To.RelName, s.holdTag())
	if err != nil {
		return nil, nil, errors.Wrapf(err, "cannot hold `to` version %q before starting send", sendArgs.To.RelName)
	}
	// bookmark and release of hold are done in p.moveCursorAndReleaseSendHold

	streamCopier, err := zfs.ZFSSend(ctx, sendArgs)
	if err != nil {
		return nil, nil, errors.Wrap(err, "zfs send failed")
	}
	return res, streamCopier, nil
}

func (p *Sender) DestroySnapshots(ctx context.Context, req *pdu.DestroySnapshotsReq) (*pdu.DestroySnapshotsRes, error) {
	dp, err := p.filterCheckFS(req.Filesystem)
	if err != nil {
		return nil, err
	}
	return doDestroySnapshots(ctx, dp, req.Snapshots)
}

func (p *Sender) Ping(ctx context.Context, req *pdu.PingReq) (*pdu.PingRes, error) {
	res := pdu.PingRes{
		Echo: req.GetMessage(),
	}
	return &res, nil
}

func (p *Sender) PingDataconn(ctx context.Context, req *pdu.PingReq) (*pdu.PingRes, error) {
	return p.Ping(ctx, req)
}

func (p *Sender) WaitForConnectivity(ctx context.Context) error {
	return nil
}

func (p *Sender) ReplicationCursor(ctx context.Context, req *pdu.ReplicationCursorReq) (*pdu.ReplicationCursorRes, error) {
	dp, err := p.filterCheckFS(req.Filesystem)
	if err != nil {
		return nil, err
	}

	cursor, err := zfs.ZFSGetReplicationCursor(dp)
	if err != nil {
		return nil, err
	}
	if cursor == nil {
		return &pdu.ReplicationCursorRes{Result: &pdu.ReplicationCursorRes_Notexist{Notexist: true}}, nil
	}
	return &pdu.ReplicationCursorRes{Result: &pdu.ReplicationCursorRes_Guid{Guid: cursor.Guid}}, nil
}

func (p *Sender) HintMostRecentCommonAncestor(ctx context.Context, r *pdu.HintMostRecentCommonAncestorReq) (*pdu.HintMostRecentCommonAncestorRes, error) {
	if r.GetFilesystem() == "" {
		return nil, fmt.Errorf("`Filesystem` must not be empty")
	}
	if r.GetVersion().GetType() != pdu.FilesystemVersion_Snapshot {
		return nil, fmt.Errorf("`Version.Type` must be a snapshot")
	}
	if r.GetVersion().GetName() == "" {
		return nil, fmt.Errorf("`Version.Name` must not be empty")
	}

	// TODO clear all of the holds aquired before Send() for versions older than r.GetVersion

	err := p.moveCursorAndReleaseSendHold(ctx, r.GetFilesystem(), r.GetVersion())
	if err != nil {
	}
	panic("not implemented")

	// return &pdu.ReplicationCursorRes{Result: &pdu.ReplicationCursorRes_Guid{Guid: guid}}, nil

}

func (p *Sender) SendCompleted(ctx context.Context, r *pdu.SendCompletedReq) (*pdu.SendCompletedRes, error) {
	orig := r.GetOriginalReq() // may be nil, always use proto getters
	err := p.moveCursorAndReleaseSendHold(ctx, orig.GetFilesystem(), orig.GetTo())
	if err != nil {
		return nil, err
	}
	return &pdu.SendCompletedRes{}, nil
}

func (p *Sender) holdTag() string {
	panic("not implemented")
}

// mrcv = most recent common version
func (p *Sender) moveCursorAndReleaseSendHold(ctx context.Context, fs string, mrcv *pdu.FilesystemVersion) error {

	dp, err := zfs.NewDatasetPath(fs)
	if err != nil {
		return errors.Wrap(err, "`Filesystem` is not a valid dataset path")
	} else if dp.Length() == 0 {
		return fmt.Errorf("`Filesystem` must not be empty")
	}
	if mrcv.GetName() == "" {
		return fmt.Errorf("`To.Name` must not be empty")
	}
	if mrcv.GetType() != pdu.FilesystemVersion_Snapshot {
		return fmt.Errorf("`To.Type` must be a snapshot")
	}

	log := getLogger(ctx).WithField("guid", mrcv.GetGuid()).
		WithField("fs", dp.ToString()).
		WithField("mrcv", mrcv.String())

	log.Debug("move replication cursor to most recent common version")
	err = zfs.ZFSSetReplicationCursor(dp, mrcv.GetName(), mrcv.GetGuid())
	if err != nil {
		// it is correct to not release the hold if we can't move the cursor!
		return errors.Wrap(err, "cannot set replication cursor")
	}
	log.Info("successfully moved replication cursor")

	log = log.WithField("hold_tag", p.holdTag())
	log.Debug("release holds earlier than most recent common version")
	err = zfs.ZFSReleaseAllOlderAndIncludingGUID(ctx, dp.ToString(), mrcv.GetGuid(), p.holdTag())
	if err != nil {
		return errors.Wrap(err, "cannot release holds up to most recent common version")
	}

	return nil
}

func (p *Sender) Receive(ctx context.Context, r *pdu.ReceiveReq, receive zfs.StreamCopier) (*pdu.ReceiveRes, error) {
	return nil, fmt.Errorf("sender does not implement Receive()")
}

type FSFilter interface { // FIXME unused
	Filter(path *zfs.DatasetPath) (pass bool, err error)
}

// FIXME: can we get away without error types here?
type FSMap interface { // FIXME unused
	FSFilter
	Map(path *zfs.DatasetPath) (*zfs.DatasetPath, error)
	Invert() (FSMap, error)
	AsFilter() FSFilter
}

// Receiver implements replication.ReplicationEndpoint for a receiving side
type Receiver struct {
	rootWithoutClientComponent *zfs.DatasetPath
	appendClientIdentity       bool

	recvParentCreationMtx *chainlock.L
}

func NewReceiver(rootDataset *zfs.DatasetPath, appendClientIdentity bool) *Receiver {
	if rootDataset.Length() <= 0 {
		panic(fmt.Sprintf("root dataset must not be an empty path: %v", rootDataset))
	}
	return &Receiver{
		rootWithoutClientComponent: rootDataset.Copy(),
		appendClientIdentity:       appendClientIdentity,
		recvParentCreationMtx:      chainlock.New(),
	}
}

func TestClientIdentity(rootFS *zfs.DatasetPath, clientIdentity string) error {
	_, err := clientRoot(rootFS, clientIdentity)
	return err
}

func clientRoot(rootFS *zfs.DatasetPath, clientIdentity string) (*zfs.DatasetPath, error) {
	rootFSLen := rootFS.Length()
	clientRootStr := path.Join(rootFS.ToString(), clientIdentity)
	clientRoot, err := zfs.NewDatasetPath(clientRootStr)
	if err != nil {
		return nil, err
	}
	if rootFSLen+1 != clientRoot.Length() {
		return nil, fmt.Errorf("client identity must be a single ZFS filesystem path component")
	}
	return clientRoot, nil
}

func (s *Receiver) clientRootFromCtx(ctx context.Context) *zfs.DatasetPath {
	if !s.appendClientIdentity {
		return s.rootWithoutClientComponent.Copy()
	}

	clientIdentity, ok := ctx.Value(ClientIdentityKey).(string)
	if !ok {
		panic(fmt.Sprintf("ClientIdentityKey context value must be set"))
	}

	clientRoot, err := clientRoot(s.rootWithoutClientComponent, clientIdentity)
	if err != nil {
		panic(fmt.Sprintf("ClientIdentityContextKey must have been validated before invoking Receiver: %s", err))
	}
	return clientRoot
}

type subroot struct {
	localRoot *zfs.DatasetPath
}

var _ zfs.DatasetFilter = subroot{}

// Filters local p
func (f subroot) Filter(p *zfs.DatasetPath) (pass bool, err error) {
	return p.HasPrefix(f.localRoot) && !p.Equal(f.localRoot), nil
}

func (f subroot) MapToLocal(fs string) (*zfs.DatasetPath, error) {
	p, err := zfs.NewDatasetPath(fs)
	if err != nil {
		return nil, err
	}
	if p.Length() == 0 {
		return nil, errors.Errorf("cannot map empty filesystem")
	}
	c := f.localRoot.Copy()
	c.Extend(p)
	return c, nil
}

func (s *Receiver) ListFilesystems(ctx context.Context, req *pdu.ListFilesystemReq) (*pdu.ListFilesystemRes, error) {
	root := s.clientRootFromCtx(ctx)
	filtered, err := zfs.ZFSListMapping(ctx, subroot{root})
	if err != nil {
		return nil, err
	}
	// present filesystem without the root_fs prefix
	fss := make([]*pdu.Filesystem, 0, len(filtered))
	for _, a := range filtered {
		l := getLogger(ctx).WithField("fs", a)
		ph, err := zfs.ZFSGetFilesystemPlaceholderState(a)
		if err != nil {
			l.WithError(err).Error("error getting placeholder state")
			return nil, errors.Wrapf(err, "cannot get placeholder state for fs %q", a)
		}
		l.WithField("placeholder_state", fmt.Sprintf("%#v", ph)).Debug("placeholder state")
		if !ph.FSExists {
			l.Error("inconsistent placeholder state: filesystem must exists")
			err := errors.Errorf("inconsistent placeholder state: filesystem %q must exist in this context", a.ToString())
			return nil, err
		}
		token, err := zfs.ZFSGetReceiveResumeTokenOrEmptyStringIfNotSupported(ctx, a)
		if err != nil {
			l.WithError(err).Error("cannot get receive resume token")
			return nil, err
		}
		encEnabled, err := zfs.ZFSGetEncryptionEnabled(ctx, a.ToString())
		if err != nil {
			l.WithError(err).Error("cannot get encryption enabled status")
			return nil, err
		}
		l.WithField("receive_resume_token", token).Debug("receive resume token")
		fmt.Fprintf(os.Stderr, "FIXME LOGGING NOT WORKING HERE: receive_resume_token = %q\n", token)

		a.TrimPrefix(root)

		fs := &pdu.Filesystem{
			Path:          a.ToString(),
			IsPlaceholder: ph.IsPlaceholder,
			ResumeToken:   token,
			IsEncrypted:   encEnabled,
		}
		fss = append(fss, fs)
	}
	if len(fss) == 0 {
		getLogger(ctx).Debug("no filesystems found")
		return &pdu.ListFilesystemRes{}, nil
	}
	return &pdu.ListFilesystemRes{Filesystems: fss}, nil
}

func (s *Receiver) ListFilesystemVersions(ctx context.Context, req *pdu.ListFilesystemVersionsReq) (*pdu.ListFilesystemVersionsRes, error) {
	root := s.clientRootFromCtx(ctx)
	lp, err := subroot{root}.MapToLocal(req.GetFilesystem())
	if err != nil {
		return nil, err
	}

	fsvs, err := zfs.ZFSListFilesystemVersions(lp, nil)
	if err != nil {
		return nil, err
	}

	rfsvs := make([]*pdu.FilesystemVersion, len(fsvs))
	for i := range fsvs {
		rfsvs[i] = pdu.FilesystemVersionFromZFS(&fsvs[i])
	}

	return &pdu.ListFilesystemVersionsRes{Versions: rfsvs}, nil
}

func (s *Receiver) Ping(ctx context.Context, req *pdu.PingReq) (*pdu.PingRes, error) {
	res := pdu.PingRes{
		Echo: req.GetMessage(),
	}
	return &res, nil
}

func (s *Receiver) PingDataconn(ctx context.Context, req *pdu.PingReq) (*pdu.PingRes, error) {
	return s.Ping(ctx, req)
}

func (s *Receiver) WaitForConnectivity(ctx context.Context) error {
	return nil
}

func (s *Receiver) ReplicationCursor(context.Context, *pdu.ReplicationCursorReq) (*pdu.ReplicationCursorRes, error) {
	return nil, fmt.Errorf("ReplicationCursor not implemented for Receiver")
}

func (s *Receiver) Send(ctx context.Context, req *pdu.SendReq) (*pdu.SendRes, zfs.StreamCopier, error) {
	return nil, nil, fmt.Errorf("receiver does not implement Send()")
}

var maxConcurrentZFSRecvSemaphore = semaphore.New(envconst.Int64("ZREPL_ENDPOINT_MAX_CONCURRENT_RECV", 10))

func (s *Receiver) Receive(ctx context.Context, req *pdu.ReceiveReq, receive zfs.StreamCopier) (*pdu.ReceiveRes, error) {
	getLogger(ctx).Debug("incoming Receive")
	defer receive.Close()

	root := s.clientRootFromCtx(ctx)
	lp, err := subroot{root}.MapToLocal(req.Filesystem)
	if err != nil {
		return nil, err
	}

	// create placeholder parent filesystems as appropriate
	//
	// Manipulating the ZFS dataset hierarchy must happen exclusively.
	// TODO: Use fine-grained locking to allow separate clients / requests to pass
	// 		 through the following section concurrently when operating on disjoint
	//       ZFS dataset hierarchy subtrees.
	var visitErr error
	func() {
		getLogger(ctx).Debug("begin aquire recvParentCreationMtx")
		defer s.recvParentCreationMtx.Lock().Unlock()
		getLogger(ctx).Debug("end aquire recvParentCreationMtx")
		defer getLogger(ctx).Debug("release recvParentCreationMtx")

		f := zfs.NewDatasetPathForest()
		f.Add(lp)
		getLogger(ctx).Debug("begin tree-walk")
		f.WalkTopDown(func(v zfs.DatasetPathVisit) (visitChildTree bool) {
			if v.Path.Equal(lp) {
				return false
			}
			ph, err := zfs.ZFSGetFilesystemPlaceholderState(v.Path)
			getLogger(ctx).
				WithField("fs", v.Path.ToString()).
				WithField("placeholder_state", fmt.Sprintf("%#v", ph)).
				WithField("err", fmt.Sprintf("%s", err)).
				WithField("errType", fmt.Sprintf("%T", err)).
				Debug("placeholder state for filesystem")
			if err != nil {
				visitErr = err
				return false
			}

			if !ph.FSExists {
				if s.rootWithoutClientComponent.HasPrefix(v.Path) {
					if v.Path.Length() == 1 {
						visitErr = fmt.Errorf("pool %q not imported", v.Path.ToString())
					} else {
						visitErr = fmt.Errorf("root_fs %q does not exist", s.rootWithoutClientComponent.ToString())
					}
					getLogger(ctx).WithError(visitErr).Error("placeholders are only created automatically below root_fs")
					return false
				}
				l := getLogger(ctx).WithField("placeholder_fs", v.Path)
				l.Debug("create placeholder filesystem")
				err := zfs.ZFSCreatePlaceholderFilesystem(v.Path)
				if err != nil {
					l.WithError(err).Error("cannot create placeholder filesystem")
					visitErr = err
					return false
				}
				return true
			}
			getLogger(ctx).WithField("filesystem", v.Path.ToString()).Debug("exists")
			return true // leave this fs as is
		})
	}()
	getLogger(ctx).WithField("visitErr", visitErr).Debug("complete tree-walk")
	if visitErr != nil {
		return nil, visitErr
	}

	// determine whether we need to rollback the filesystem / change its placeholder state
	var clearPlaceholderProperty bool
	var recvOpts zfs.RecvOptions
	ph, err := zfs.ZFSGetFilesystemPlaceholderState(lp)
	if err == nil && ph.FSExists && ph.IsPlaceholder {
		recvOpts.RollbackAndForceRecv = true
		clearPlaceholderProperty = true
	}
	if clearPlaceholderProperty {
		if err := zfs.ZFSSetPlaceholder(lp, false); err != nil {
			return nil, fmt.Errorf("cannot clear placeholder property for forced receive: %s", err)
		}
	}

	if req.ClearResumeToken && ph.FSExists {
		if err := zfs.ZFSRecvClearResumeToken(lp.ToString()); err != nil {
			return nil, errors.Wrap(err, "cannot clear resume token")
		}
	}

	recvOpts.SavePartialRecvState, err = zfs.ResumeRecvSupported(ctx, lp)
	if err != nil {
		return nil, errors.Wrap(err, "cannot determine whether we can use resumable send & recv")
	}

	getLogger(ctx).Debug("acquire concurrent recv semaphore")
	// TODO use try-acquire and fail with resource-exhaustion rpc status
	// => would require handling on the client-side
	// => this is a dataconn endpoint, doesn't have the status code semantics of gRPC
	guard, err := maxConcurrentZFSRecvSemaphore.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer guard.Release()

	getLogger(ctx).WithField("opts", fmt.Sprintf("%#v", recvOpts)).Debug("start receive command")

	if err := zfs.ZFSRecv(ctx, lp.ToString(), receive, recvOpts); err != nil {
		getLogger(ctx).
			WithError(err).
			WithField("opts", recvOpts).
			Error("zfs receive failed")
		return nil, err
	}
	return &pdu.ReceiveRes{}, nil
}

func (s *Receiver) DestroySnapshots(ctx context.Context, req *pdu.DestroySnapshotsReq) (*pdu.DestroySnapshotsRes, error) {
	root := s.clientRootFromCtx(ctx)
	lp, err := subroot{root}.MapToLocal(req.Filesystem)
	if err != nil {
		return nil, err
	}
	return doDestroySnapshots(ctx, lp, req.Snapshots)
}

func doDestroySnapshots(ctx context.Context, lp *zfs.DatasetPath, snaps []*pdu.FilesystemVersion) (*pdu.DestroySnapshotsRes, error) {
	reqs := make([]*zfs.DestroySnapOp, len(snaps))
	ress := make([]*pdu.DestroySnapshotRes, len(snaps))
	errs := make([]error, len(snaps))
	for i, fsv := range snaps {
		if fsv.Type != pdu.FilesystemVersion_Snapshot {
			return nil, fmt.Errorf("version %q is not a snapshot", fsv.Name)
		}
		ress[i] = &pdu.DestroySnapshotRes{
			Snapshot: fsv,
			// Error set after batch operation
		}
		reqs[i] = &zfs.DestroySnapOp{
			Filesystem: lp.ToString(),
			Name:       fsv.Name,
			ErrOut:     &errs[i],
		}
	}
	zfs.ZFSDestroyFilesystemVersions(reqs)
	for i := range reqs {
		if errs[i] != nil {
			if de, ok := errs[i].(*zfs.DestroySnapshotsError); ok && len(de.Reason) == 1 {
				ress[i].Error = de.Reason[0]
			} else {
				ress[i].Error = errs[i].Error()
			}
		}
	}
	return &pdu.DestroySnapshotsRes{
		Results: ress,
	}, nil
}
