package zfs

import (
	"fmt"

	"github.com/pkg/errors"
)

const ReplicationCursorBookmarkName = "zrepl_replication_cursor"

// may return nil for both values, indicating there is no cursor
func ZFSGetReplicationCursor(fs *DatasetPath) (*FilesystemVersion, error) {
	versions, err := ZFSListFilesystemVersions(fs, nil)
	if err != nil {
		return nil, err
	}
	for _, v := range versions {
		if v.Type == Bookmark && v.Name == ReplicationCursorBookmarkName {
			return &v, nil
		}
	}
	return nil, nil
}

// expGuid is the expected guid of snapname
// if fs@snapname has a different guid, the replication cursor won't be set
func ZFSSetReplicationCursor(fs *DatasetPath, snapname string, expGuid uint64) (err error) {
	if fs.Length() == 0 {
		return errors.New("filesystem name must not be empty")
	}
	if len(snapname) == 0 {
		return errors.New("snapname must not be empty")
	}
	// must not check expGuid == 0, that might be legitimate
	snapPath := fmt.Sprintf("%s@%s", fs.ToString(), snapname)

	debug("replication cursor: snap path %q", snapPath)
	snapProps, err := ZFSGetCreateTXGAndGuid(snapPath)
	if err != nil {
		return errors.Wrap(err, "cannot get snapshot createtxg and guid")
	}
	if expGuid != snapProps.Guid {
		return fmt.Errorf("expected guid %v != actual guid %v for snap name %q", expGuid, snapProps.Guid, snapPath)
	}

	bookmarkPath := fmt.Sprintf("%s#%s", fs.ToString(), ReplicationCursorBookmarkName)
	bookmarkProps, err := ZFSGetCreateTXGAndGuid(bookmarkPath)
	_, bookmarkNotExistErr := err.(*DatasetDoesNotExist)
	if err != nil && !bookmarkNotExistErr {
		return errors.Wrap(err, "cannot get bookmark txg")
	}
	if err == nil {
		// bookmark does exist

		if snapProps.CreateTXG < bookmarkProps.CreateTXG {
			return errors.New("cannot can only be advanced, not set back")
		}

		if bookmarkProps.Guid == snapProps.Guid {
			return nil // no action required
		}

		// FIXME make safer by using new temporary bookmark, then rename, possible with channel programs
		// https://github.com/zfsonlinux/zfs/pull/7902/files might support this but is too new
		if err := ZFSDestroy(bookmarkPath); err != nil {
			return errors.Wrap(err, "cannot destroy current cursor to move it to new")
		}
		// fallthrough
	}

	if err := ZFSBookmark(fs, snapname, ReplicationCursorBookmarkName); err != nil {
		return errors.Wrapf(err, "cannot create bookmark")
	}

	return nil
}
