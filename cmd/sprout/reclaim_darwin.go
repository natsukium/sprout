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
	// Leave a quarter of either table for growth between samples and while the
	// guest reclaims.
	reclaimTriggerPermille = 750
	reclaimCooldown        = time.Minute
	reclaimMaxCooldown     = 10 * time.Minute
	reclaimMinGainPercent  = 1
	reclaimGrowthPermille  = 50
	reclaimFailureLimit    = 3
	reclaimRetryDelay      = 15 * time.Second
	reclaimTimeout         = 30 * time.Second
	reclaimReplyBytes      = 2*20 + 2 // Two uint64s, a space, and a newline.
)

type reclaimTable struct {
	used, limit uint64
}

func (table reclaimTable) usagePermille() uint64 {
	return table.used * 1000 / table.limit
}

type reclaimReply struct {
	before, after uint64
	skipped       bool
}

func startReclaimWatch(ctx context.Context, vn dialer, m *Manifest) {
	startHostTableWatch(ctx, vn, m, unix.SysctlUint32, log.Printf)
}

func startHostTableWatch(ctx context.Context, vn dialer, m *Manifest, sysctl func(string) (uint32, error), logf func(string, ...any)) {
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
		var usageAtRequest uint64
		var failures int
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}

			files, vnodes, err := readHostTables(sysctl)
			if err != nil {
				logf("virtiofs reclaim: cannot sample host tables: %v", err)
				return
			}
			fuller, usage := files, files.usagePermille()
			if vnodeUsage := vnodes.usagePermille(); vnodeUsage > usage {
				fuller, usage = vnodes, vnodeUsage
			}
			if usage < reclaimTriggerPermille {
				continue
			}
			early := time.Since(lastRequest) < wait
			if early && (wait <= reclaimCooldown || usage < usageAtRequest+reclaimGrowthPermille) {
				continue
			}

			// The dial through vn bypasses the session tracker, so reclaim never
			// counts as activity for idle auto-stop.
			usageAtRequest = usage
			reply, err := requestReclaim(ctx, vn, addr)
			if ctx.Err() != nil {
				return
			}
			lastRequest = time.Now()
			if err != nil {
				failures = min(failures+1, reclaimFailureLimit)
			} else {
				failures = 0
			}
			host := fmt.Sprintf("host files %d/%d, vnodes %d/%d", files.used, files.limit, vnodes.used, vnodes.limit)
			if reply.skipped {
				wait = cooldown
				logf("virtiofs reclaim: %s, guest skipped because it reclaimed recently, cooldown %s", host, wait)
				continue
			}
			if early {
				cooldown = reclaimCooldown
			}

			minGain := (fuller.limit*reclaimMinGainPercent + 99) / 100
			switch {
			case failures > 0 && failures < reclaimFailureLimit:
				wait = reclaimRetryDelay
			case err != nil || reply.after > reply.before || reply.before-reply.after < minGain:
				cooldown = min(2*cooldown, reclaimMaxCooldown)
				wait = cooldown
			default:
				cooldown = reclaimCooldown
				wait = cooldown
			}
			if err != nil {
				logf("virtiofs reclaim: %s: %v, retry delay %s", host, err, wait)
			} else {
				logf("virtiofs reclaim: %s, fuse_inode %d -> %d, cooldown %s", host, reply.before, reply.after, wait)
			}
		}
	}()
}

// Each distinct cached guest file costs a host vnode. On a 128 GiB Apple
// silicon Mac, kern.maxvnodes is about half kern.maxfiles (263,168 vs 491,520).
// A full vnode table makes new_vnode return ENFILE, even when a unixgram
// sendto looks up the peer socket path without opening a file. Instances
// caching the same file share one vnode but hold one file each, so either
// table can fill first.
//
// XNU can count open vnodes as free until their 25th reference or until
// new_vnode walks past them. So the in-use count can read low. But new_vnode
// drops each open vnode it finds on the free list and fails only when no
// reusable vnode is left, so vnodes in use reach kern.maxvnodes before
// new_vnode returns ENFILE.
func readHostTables(sysctl func(string) (uint32, error)) (files, vnodes reclaimTable, err error) {
	var freeVnodes uint64
	for _, field := range []struct {
		name    string
		value   *uint64
		nonzero bool
	}{
		{"kern.num_files", &files.used, false},
		{"kern.maxfiles", &files.limit, true},
		{"kern.num_vnodes", &vnodes.used, false},
		{"kern.free_vnodes", &freeVnodes, false},
		{"kern.maxvnodes", &vnodes.limit, true},
	} {
		value, err := sysctl(field.name)
		if err != nil {
			return reclaimTable{}, reclaimTable{}, fmt.Errorf("%s: %w", field.name, err)
		}
		if field.nonzero && value == 0 {
			return reclaimTable{}, reclaimTable{}, fmt.Errorf("%s: zero limit", field.name)
		}
		*field.value = uint64(value)
	}
	vnodes.used -= min(vnodes.used, freeVnodes)
	return files, vnodes, nil
}

func requestReclaim(ctx context.Context, vn dialer, addr string) (reclaimReply, error) {
	ctx, cancel := context.WithTimeout(ctx, reclaimTimeout)
	defer cancel()
	conn, err := vn.DialContextTCP(ctx, addr)
	if err != nil {
		return reclaimReply{}, fmt.Errorf("dial guest: %w", err)
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
		return reclaimReply{}, fmt.Errorf("read guest reply: %w", err)
	}

	reply := string(raw)
	if reply == "skipped\n" {
		return reclaimReply{skipped: true}, nil
	}
	line, newline := strings.CutSuffix(reply, "\n")
	first, second, _ := strings.Cut(line, " ")
	before, errBefore := strconv.ParseUint(first, 10, 64)
	after, errAfter := strconv.ParseUint(second, 10, 64)
	if len(raw) > reclaimReplyBytes || !newline || errBefore != nil || errAfter != nil {
		return reclaimReply{}, errors.New("invalid guest reclaim reply: expected skipped or two unsigned counts, followed by a newline")
	}
	return reclaimReply{before: before, after: after}, nil
}
