package firecracker

import (
	"bufio"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
)

// idRange gives each VM its own uid and gid: the start of the range plus
// the index of the TAP lease. Thus the Firecracker process of one VM cannot
// signal, trace, or open the files of another VM. The ranges come from
// /etc/subuid and /etc/subgid, where install.sh reserves them for the
// jailer user.
type idRange struct {
	uid, gid int // the IDs of lease index 0
	count    int
}

// of returns the uid and gid of the VM with this lease index.
func (r idRange) of(index int) (uid, gid int, err error) {
	if index < 0 || index >= r.count {
		return 0, 0, fmt.Errorf("firecracker: the jailer ID range has room for %d VMs", r.count)
	}
	return r.uid + index, r.gid + index, nil
}

// readIDRange reads the ranges of the user name from subuid and subgid.
func readIDRange(name, subuid, subgid string) (idRange, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return idRange{}, fmt.Errorf("find the jailer user: %w", err)
	}
	uid, uidCount, err := readSubIDs(subuid, u.Username, u.Uid)
	if err != nil {
		return idRange{}, err
	}
	gid, gidCount, err := readSubIDs(subgid, u.Username, u.Uid)
	if err != nil {
		return idRange{}, err
	}
	return idRange{uid: uid, gid: gid, count: min(uidCount, gidCount)}, nil
}

// readSubIDs returns the first range of a user in a subuid or subgid file.
// A line is "<name or uid>:<start>:<count>". A range that starts at 0 would
// give a VM the root ID, so it is an error.
func readSubIDs(path, name, uid string) (start, count int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, fmt.Errorf("read the jailer ID range: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Split(strings.TrimSpace(sc.Text()), ":")
		if len(fields) != 3 || (fields[0] != name && fields[0] != uid) {
			continue
		}
		start, err1 := strconv.Atoi(fields[1])
		count, err2 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || start <= 0 || count <= 0 {
			return 0, 0, fmt.Errorf("%s: the range %q of %s is not valid", path, sc.Text(), name)
		}
		return start, count, nil
	}
	if err := sc.Err(); err != nil {
		return 0, 0, err
	}
	return 0, 0, fmt.Errorf("%s has no ID range for %s; run the install script again", path, name)
}
