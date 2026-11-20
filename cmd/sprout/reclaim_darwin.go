package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	reclaimSampleInterval = 5 * time.Second
	// Leave a quarter of the file table for growth between samples and while
	// the guest reclaims.
	reclaimTriggerPercent = 75
	reclaimCooldown       = time.Minute
	reclaimMaxCooldown    = 10 * time.Minute
	reclaimMinGainPercent = 1
	reclaimGrowthPercent  = 5
	reclaimFailureLimit   = 3
	reclaimRetryDelay     = 15 * time.Second
	reclaimTimeout        = 4 * time.Second
	reclaimReplyBytes     = 2*20 + 2 // Two uint64s, a space, and a newline.
)

func startReclaimWatch(ctx context.Context, vn dialer, m *Manifest) {
	startFileTableWatch(ctx, vn, m, unix.SysctlUint32, log.Printf)
}

func startFileTableWatch(ctx context.Context, vn dialer, m *Manifest, sysctl func(string) (uint32, error), logf func(string, ...any)) {
	if m.Guest.ReclaimPort == 0 {
		return
	}
	addr := net.JoinHostPort(m.Guest.IP, strconv.Itoa(int(m.Guest.ReclaimPort)))
	go func() {
		ticker := time.NewTicker(reclaimSampleInterval)
		defer ticker.Stop()

		cooldown := reclaimCooldown
		var wait time.Duration
		var lastRequest time.Time
		var usedAtRequest uint32
		var failures int
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			used, limit, err := readFileTable(sysctl)
			if err != nil {
				logf("virtiofs reclaim: cannot sample host file table: %v", err)
				return
			}
			if uint64(used)*100 < uint64(limit)*reclaimTriggerPercent {
				continue
			}
			if time.Since(lastRequest) < wait {
				if wait <= reclaimCooldown || used <= usedAtRequest ||
					uint64(used-usedAtRequest)*100 < uint64(limit)*reclaimGrowthPercent {
					continue
				}
				cooldown = reclaimCooldown
			}

			// The dial through vn bypasses the session tracker, so reclaim never
			// counts as activity for idle auto-stop.
			usedAtRequest = used
			before, after, err := requestReclaim(ctx, vn, addr)
			if ctx.Err() != nil {
				return
			}
			lastRequest = time.Now()
			if err != nil {
				failures = min(failures+1, reclaimFailureLimit)
			} else {
				failures = 0
			}

			switch {
			case failures > 0 && failures < reclaimFailureLimit:
				wait = reclaimRetryDelay
			case err != nil || after > before || before-after < uint64(limit)*reclaimMinGainPercent/100:
				cooldown = min(2*cooldown, reclaimMaxCooldown)
				wait = cooldown
			default:
				cooldown = reclaimCooldown
				wait = cooldown
			}
			if err != nil {
				logf("virtiofs reclaim: host files %d/%d: %v, retry delay %s", used, limit, err, wait)
			} else {
				logf("virtiofs reclaim: host files %d/%d, fuse_inode %d -> %d, cooldown %s", used, limit, before, after, wait)
			}
		}
	}()
}

// Only the file table tracks the files a guest caches. On a vfkit host, 21,000
// more cached virtiofs inodes raised kern.num_files by 21,000 but in-use
// vnodes by only 1,500, and a reclaim lowered kern.num_files alone.
func readFileTable(sysctl func(string) (uint32, error)) (used, limit uint32, err error) {
	used, err = sysctl("kern.num_files")
	if err != nil {
		return 0, 0, fmt.Errorf("kern.num_files: %w", err)
	}
	limit, err = sysctl("kern.maxfiles")
	if err != nil {
		return 0, 0, fmt.Errorf("kern.maxfiles: %w", err)
	}
	return used, limit, nil
}

func requestReclaim(ctx context.Context, vn dialer, addr string) (before, after uint64, err error) {
	ctx, cancel := context.WithTimeout(ctx, reclaimTimeout)
	defer cancel()
	conn, err := vn.DialContextTCP(ctx, addr)
	if err != nil {
		return 0, 0, fmt.Errorf("dial guest: %w", err)
	}
	defer conn.Close()
	// DialContextTCP only observes cancellation until the connection is made.
	stopClose := context.AfterFunc(ctx, func() { conn.Close() })
	defer stopClose()
	raw, err := io.ReadAll(io.LimitReader(conn, reclaimReplyBytes+1))
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		return 0, 0, fmt.Errorf("read guest reply: %w", err)
	}

	line, newline := strings.CutSuffix(string(raw), "\n")
	first, second, _ := strings.Cut(line, " ")
	before, errBefore := strconv.ParseUint(first, 10, 64)
	after, errAfter := strconv.ParseUint(second, 10, 64)
	if len(raw) > reclaimReplyBytes || !newline || errBefore != nil || errAfter != nil {
		return 0, 0, errors.New("invalid guest reclaim reply: expected two unsigned counts and a newline")
	}
	return before, after, nil
}
