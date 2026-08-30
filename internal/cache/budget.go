package cache

import (
	"syscall"
)

const (
	// MinBudget / MaxBudget clamp the 5%-of-filesystem term.
	MinBudget int64 = 64 << 20
	MaxBudget int64 = 2 << 30
	SizeFrac        = 0.05
	FreeFrac        = 0.25

	// MaxDownload is the hard cap for a single /download body.
	MaxDownload int64 = 512 << 20
)

// FSInfo is syscall.Statfs of a directory's filesystem.
type FSInfo struct {
	Size uint64
	Free uint64
}

// Statfs reports filesystem size and available bytes for dir
// (Bavail, i.e. space non-root can use).
func Statfs(dir string) (FSInfo, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return FSInfo{}, err
	}
	bsize := uint64(st.Bsize)
	return FSInfo{
		Size: uint64(st.Blocks) * bsize,
		Free: uint64(st.Bavail) * bsize,
	}, nil
}

// ComputeBudget is clamp(5% of fs size, 64MB, 2GB) and never more than
// 25% of currently free space (headroom wins over the 64MB floor).
func ComputeBudget(fsSize, fsFree uint64) int64 {
	if fsSize == 0 {
		return 0
	}
	raw := float64(fsSize) * SizeFrac
	if raw < float64(MinBudget) {
		raw = float64(MinBudget)
	}
	if raw > float64(MaxBudget) {
		raw = float64(MaxBudget)
	}
	headroom := float64(fsFree) * FreeFrac
	if headroom < raw {
		raw = headroom
	}
	if raw < 0 {
		return 0
	}
	return int64(raw)
}

// MaxObjectBytes is the largest single download we will accept:
// min(512MB, 25% of current free space).
func MaxObjectBytes(fsFree uint64) int64 {
	head := int64(float64(fsFree) * FreeFrac)
	if head <= 0 {
		return 0
	}
	if head > MaxDownload {
		return MaxDownload
	}
	return head
}
