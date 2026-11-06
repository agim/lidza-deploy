package agent

import (
	"errors"
	"fmt"
)

// Disk-full prevention. Every large operation the agent starts on its own
// disk (move exports, uploads and restores, database backups, file archives
// and builds) keeps a reserve of free space, so a full disk never stops the
// applications and databases already running on the server. An operation is
// refused before it starts when it would take the disk below the reserve,
// and a streaming write stops once the disk is down to it.

const (
	// diskReserveMinimum is the smallest reserve: small disks keep 2 GiB.
	diskReserveMinimum = 2 << 30
	// diskWarnPercent is the used share at which server health warns.
	diskWarnPercent = 85
	// buildDiskHeadroom is what a build is assumed to need above the
	// reserve: image layers or a release tree and its dependencies.
	buildDiskHeadroom = 1 << 30
)

// diskStat measures the filesystem holding a path; tests replace it.
var diskStat = diskHealth

// errDiskReserve stops a streaming write before it fills the disk.
var errDiskReserve = errors.New("stopped to keep free disk space on this server; free space or add disk, then retry")

// diskReserve is the free space an operation never goes below: 5% of the
// disk, at least 2 GiB.
func diskReserve(disk DiskHealth) uint64 {
	return max(disk.Total/20, diskReserveMinimum)
}

// diskLow reports a disk at or above the warning threshold, or already
// within its reserve.
func diskLow(disk DiskHealth) bool {
	return disk.Total > 0 && (disk.UsedPercent >= diskWarnPercent || disk.Available <= diskReserve(disk))
}

// keepDiskReserve fails once the disk holding dir is down to its reserve.
func keepDiskReserve(dir string) error {
	disk, err := diskStat(dir)
	if err != nil {
		return err
	}
	if disk.Available <= diskReserve(disk) {
		return errDiskReserve
	}
	return nil
}

// diskFits refuses an operation that needs `need` bytes in dir when that
// would take the disk below its reserve. what names the operation in the
// message, for example "the database backup".
func diskFits(dir string, need uint64, what string) error {
	disk, err := diskStat(dir)
	if err != nil {
		return fmt.Errorf("free disk space for %s could not be measured: %w", what, err)
	}
	reserve := diskReserve(disk)
	if disk.Available <= need+reserve {
		if need == 0 {
			return fmt.Errorf("not enough free disk space for %s: %s is free and the server keeps a %s reserve; free space or add disk, then retry", what, moveHumanBytes(disk.Available), moveHumanBytes(reserve))
		}
		return fmt.Errorf("not enough free disk space for %s: it needs about %s plus a %s reserve, and %s is free; free space or add disk, then retry", what, moveHumanBytes(need), moveHumanBytes(reserve), moveHumanBytes(disk.Available))
	}
	return nil
}

// reserveWriter fails a write once the disk is down to its reserve; it checks
// every 64 MiB.
type reserveWriter struct {
	dir     string
	written int64
}

func (r *reserveWriter) Write(p []byte) (int, error) {
	if r.written/(64<<20) != (r.written+int64(len(p)))/(64<<20) {
		if err := keepDiskReserve(r.dir); err != nil {
			return 0, err
		}
	}
	r.written += int64(len(p))
	return len(p), nil
}
