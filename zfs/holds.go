package zfs

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/pkg/errors"
)

// no need for feature tests, holds have been around forever

// func ListAllHolds()

func ZFSHold(ctx context.Context, fs, snap, tag string) error {
	panic("not implemented")
}

// if the hold doesn't exist, this is not an error
func zfsRelease(ctx context.Context, fs, snap, tag string) error {
	panic("not implemented")
}

func ZFSHolds(ctx context.Context, fs, snap string) ([]string, error) {
	if err := validateZFSFilesystem(fs); err != nil {
		return nil, errors.Wrap(err, "`fs` is not a valid filesystem path")
	}
	if snap == "" {
		return nil, fmt.Errorf("`snap` must not be empty")
	}
	dp := fmt.Sprintf("%s@%s", fs, snap)
	output, err := exec.CommandContext(ctx, "zfs", "holds", "-H", dp).CombinedOutput()
	if err != nil {
		return nil, &ZFSError{output, errors.Wrap(err, "zfs holds failed")}
	}
	scan := bufio.NewScanner(bytes.NewReader(output))
	var tags []string
	for scan.Scan() {
		// NAME              TAG  TIMESTAMP
		comps := strings.SplitN(scan.Text(), "\t", 3)
		if len(comps) != 3 {
			return nil, fmt.Errorf("zfs holds: unexpected output\n%s", output)
		}
		if comps[0] != dp {
			return nil, fmt.Errorf("zfs holds: unexpected output: expecting %q as first component, got %q\n%s", dp, comps[0], output)
		}
		tags = append(tags, comps[1])
	}
	return tags, nil
}

// if the hold doesn't exist, this is not an error
func ZFSReleaseAllOlderAndIncludingGUID(ctx context.Context, fs string, snapGuid uint64, tag string) error {
	// TODO channel program support still unreleased but
	// might be a huge performance improvement
	// https://github.com/zfsonlinux/zfs/pull/7902/files

	if err := validateZFSFilesystem(fs); err != nil {
		return errors.Wrap(err, "`fs` is not a valid filesystem path")
	}
	if tag == "" {
		return fmt.Errorf("`tag` must not be empty`")
	}

	output, err := exec.CommandContext(ctx,
		"zfs", "list", "-o", "name,guid,userrefs",
		"-s", "createtxg",
		"-H", "-t", "snapshot", "-r", "-d", "1", fs).CombinedOutput()
	if err != nil {
		return &ZFSError{output, errors.Wrap(err, "cannot list snapshots and their userrefs")}
	}
	scan := bufio.NewScanner(bytes.NewReader(output))
	var releaseSnaps []string
	foundGuid := false
	for scan.Scan() {
		comps := strings.SplitN(scan.Text(), "\t", 3)
		if len(comps) != 3 {
			return fmt.Errorf("unexpected ZFS output: not 3 columns\n%s", output)
		}
		snap := comps[0]
		guid, err := strconv.ParseUint(comps[1], 10, 64)
		if err != nil {
			return fmt.Errorf("unexpected ZFS output: cannot parse guid %q: %s\n%s", comps[1], err, output)
		}
		userrefs, err := strconv.ParseUint(comps[2], 10, 64)
		if err != nil {
			return fmt.Errorf("unexpected ZFS output: cannot parse userrefs %q: %s\n%s", comps[2], err, output)
		}
		if userrefs > 0 {
			releaseSnaps = append(releaseSnaps, snap)
		}
		if guid == snapGuid {
			foundGuid = true
			break
		}
	}
	if !foundGuid {
		return fmt.Errorf("canont find snapshot with guid %v", snapGuid)
	}

	if len(releaseSnaps) == 0 {
		return nil
	}

	cumLens := make([]int, len(releaseSnaps))
	for i := 1; i < len(releaseSnaps); i++ {
		cumLens[i] = cumLens[i-1] + len(releaseSnaps[i])
	}
	maxInvocationLen := 12 * os.Getpagesize()
	var noSuchTagLines, otherLines []string
	for i := 0; i < len(releaseSnaps); {
		var j = i
		for ; j < len(releaseSnaps); j++ {
			if cumLens[j]-cumLens[i] > maxInvocationLen {
				break
			}
		}
		args := []string{"release", tag}
		args = append(args, releaseSnaps[i:j]...)
		output, err := exec.CommandContext(ctx, "zfs", args...).CombinedOutput()
		if pe, ok := err.(*os.PathError); err != nil && ok && pe.Err == syscall.E2BIG {
			maxInvocationLen = maxInvocationLen / 2
			continue
		}
		maxInvocationLen = maxInvocationLen + os.Getpagesize()
		i = j

		// even if release fails for datasets where there's no hold with the tag
		// the hold is still released on datasets which have a hold with the tag
		// FIXME verify this in a platformtest
		// => screen-scrape
		scan := bufio.NewScanner(bytes.NewReader(output))
		for scan.Scan() {
			line := scan.Text()
			if strings.Contains(line, "no such tag on this dataset") {
				noSuchTagLines = append(noSuchTagLines, line)
			} else {
				otherLines = append(otherLines, line)
			}
		}

	}
	if len(otherLines) > 0 {
		return fmt.Errorf("unknown zfs error while releasing hold with tag %q: unidentified stderr lines\n%s", strings.Join(otherLines, "\n"))
	}
	return nil

}
