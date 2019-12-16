package zfs

import "fmt"

// An instance of this type returned by MakeJobID guarantees
// that that instance's JobID.String() can be used in a ZFS dataset name and hold tag.
type JobID struct {
	jid string
}

func MakeJobID(s string) (JobID, error) {
	if len(s) == 0 {
		return JobID{}, fmt.Errorf("must not be empty string")
	}

	_, err := NewDatasetPath(s)
	if err != nil {
		return JobID{}, fmt.Errorf("must be usable in a ZFS dataset path: %s", err)
	}

	return JobID{s}, nil
}

func MustMakeJobID(s string) JobID {
	jid, err := MakeJobID(s)
	if err != nil {
		panic(err)
	}
	return jid
}

func (j JobID) String() string {
	if j.jid == "" {
		panic("use of uninitialized JobID")
	}
	return j.jid
}

func (j JobID) MustValidate() { _ = j.String() }
