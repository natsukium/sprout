package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type reclaimDialFunc func(context.Context, string) (net.Conn, error)

func (f reclaimDialFunc) DialContextTCP(ctx context.Context, addr string) (net.Conn, error) {
	return f(ctx, addr)
}

func reclaimTestManifest() *Manifest {
	m := &Manifest{}
	m.Guest.IP, m.Guest.ReclaimPort = "192.0.2.1", 12345
	return m
}

func reclaimTestSysctl(usedFiles, freeVnodes *atomic.Uint32) func(string) (uint32, error) {
	return func(name string) (uint32, error) {
		switch name {
		case "kern.num_files":
			return usedFiles.Load(), nil
		case "kern.maxfiles":
			return 1000, nil
		case "kern.num_vnodes":
			return 750, nil
		case "kern.maxvnodes":
			return 780, nil
		case "kern.free_vnodes":
			if freeVnodes != nil {
				return freeVnodes.Load(), nil
			}
			return 650, nil
		default:
			return 0, fmt.Errorf("unexpected sysctl %s", name)
		}
	}
}

func reclaimTestReply(reply string) net.Conn {
	client, guest := net.Pipe()
	go func() {
		defer guest.Close()
		_, _ = io.WriteString(guest, reply)
	}()
	return client
}

func advanceReclaimClock(d time.Duration) {
	time.Sleep(d)
	synctest.Wait()
}

func TestReclaimWatchThresholdAndCooldown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var used atomic.Uint32
		used.Store(740)
		var calls atomic.Int32
		logs := make(chan string, 4)
		dial := reclaimDialFunc(func(_ context.Context, addr string) (net.Conn, error) {
			if addr != "192.0.2.1:12345" {
				t.Errorf("dialed %q, want the manifest's guest endpoint", addr)
			}
			calls.Add(1)
			return reclaimTestReply("100 90\n"), nil
		})
		startHostTableWatch(ctx, dial, reclaimTestManifest(), reclaimTestSysctl(&used, nil), func(format string, args ...any) {
			logs <- fmt.Sprintf(format, args...)
		})
		synctest.Wait()

		advanceReclaimClock(5 * time.Second)
		if calls.Load() != 0 {
			t.Fatalf("dialed %d times under threshold", calls.Load())
		}
		used.Store(750)
		advanceReclaimClock(5 * time.Second)
		if calls.Load() != 1 || len(logs) != 1 {
			t.Fatalf("at threshold: calls = %d, log lines = %d", calls.Load(), len(logs))
		}
		if line := <-logs; !strings.Contains(line, "host files 750/1000, vnodes 100/780, fuse_inode 100 -> 90") {
			t.Fatalf("missing usage or counts in %q", line)
		}
		used.Store(800)
		advanceReclaimClock(55 * time.Second)
		if calls.Load() != 1 {
			t.Fatalf("dialed %d times before the cooldown expired", calls.Load())
		}
		advanceReclaimClock(5 * time.Second)
		if calls.Load() != 2 {
			t.Fatalf("dialed %d times at the next allowed sample", calls.Load())
		}
		cancel()
		advanceReclaimClock(time.Hour)
		if calls.Load() != 2 {
			t.Fatalf("dialed %d times after shutdown", calls.Load())
		}
	})
}

func TestReclaimWatchBackoffAndReset(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var used atomic.Uint32
		used.Store(800)
		var calls atomic.Int32
		replies := []string{"1000 991\n", "0 0\n", "1000 1200\n", "1000 1000\n", "1000 1000\n", "1000 990\n", "1000 500\n"}
		dial := reclaimDialFunc(func(context.Context, string) (net.Conn, error) {
			return reclaimTestReply(replies[calls.Add(1)-1]), nil
		})
		startHostTableWatch(ctx, dial, reclaimTestManifest(), reclaimTestSysctl(&used, nil), t.Logf)
		synctest.Wait()
		advanceReclaimClock(reclaimSampleInterval)
		for i, wait := range []time.Duration{2, 4, 8, 10, 10, 1} {
			advanceReclaimClock(wait*time.Minute - reclaimSampleInterval)
			if int(calls.Load()) != i+1 {
				t.Fatalf("after reply %d: dialed %d times before %s", i+1, calls.Load(), wait*time.Minute)
			}
			advanceReclaimClock(reclaimSampleInterval)
			if int(calls.Load()) != i+2 {
				t.Fatalf("after reply %d: dialed %d times after %s", i+1, calls.Load(), wait*time.Minute)
			}
		}
	})
}

func TestReclaimWatchVnodeThresholdAndGain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var used, free atomic.Uint32
		used.Store(740)
		free.Store(166)
		var calls atomic.Int32
		dial := reclaimDialFunc(func(context.Context, string) (net.Conn, error) {
			if calls.Add(1) == 1 {
				return reclaimTestReply("100 93\n"), nil
			}
			return reclaimTestReply("100 92\n"), nil
		})
		startHostTableWatch(t.Context(), dial, reclaimTestManifest(), reclaimTestSysctl(&used, &free), t.Logf)
		synctest.Wait()
		advanceReclaimClock(reclaimSampleInterval)
		if calls.Load() != 0 {
			t.Fatal("dialed below the vnode threshold")
		}
		free.Store(165)
		advanceReclaimClock(reclaimSampleInterval)
		if calls.Load() != 1 {
			t.Fatalf("at 75%% vnode usage: got %d requests, want 1", calls.Load())
		}
		advanceReclaimClock(time.Minute)
		if calls.Load() != 1 {
			t.Fatalf("one minute after freeing 7 of 780 vnodes: got %d requests, want 1", calls.Load())
		}
		advanceReclaimClock(time.Minute)
		if calls.Load() != 2 {
			t.Fatalf("two minutes after freeing 7 of 780 vnodes: got %d requests, want 2", calls.Load())
		}
		advanceReclaimClock(time.Minute)
		if calls.Load() != 3 {
			t.Fatalf("one minute after freeing 8 of 780 vnodes: got %d requests, want 3", calls.Load())
		}
	})
}

func TestReadHostTablesClampsVnodeUsage(t *testing.T) {
	var used, free atomic.Uint32
	free.Store(751)
	_, vnodes, err := readHostTables(reclaimTestSysctl(&used, &free))
	if err != nil || vnodes.used != 0 {
		t.Fatalf("vnode usage = %d, err = %v, want 0 and nil", vnodes.used, err)
	}
}

func TestReadHostTablesRejectsZeroLimit(t *testing.T) {
	var used atomic.Uint32
	read := reclaimTestSysctl(&used, nil)
	for _, limit := range []string{"kern.maxfiles", "kern.maxvnodes"} {
		t.Run(limit, func(t *testing.T) {
			_, _, err := readHostTables(func(name string) (uint32, error) {
				if name == limit {
					return 0, nil
				}
				return read(name)
			})
			if err == nil || !strings.Contains(err.Error(), limit) {
				t.Fatalf("got error %v, want it to name %s", err, limit)
			}
		})
	}
}

func TestReclaimWatchGrowthBreaksBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var used, free atomic.Uint32
		used.Store(800)
		free.Store(650)
		var calls atomic.Int32
		dial := reclaimDialFunc(func(context.Context, string) (net.Conn, error) {
			calls.Add(1)
			return reclaimTestReply("1000 1000\n"), nil
		})
		startHostTableWatch(ctx, dial, reclaimTestManifest(), reclaimTestSysctl(&used, &free), t.Logf)
		synctest.Wait()
		advanceReclaimClock(reclaimSampleInterval)
		used.Store(790)
		for _, available := range []uint32{650, 88} {
			free.Store(available)
			advanceReclaimClock(reclaimSampleInterval)
			if calls.Load() != 1 {
				t.Fatalf("bypassed back-off with %d free vnodes and %d calls", available, calls.Load())
			}
		}
		free.Store(87)
		advanceReclaimClock(reclaimSampleInterval)
		if calls.Load() != 2 {
			t.Fatalf("at 5%% growth: got %d requests, want 2", calls.Load())
		}
		used.Store(899)
		advanceReclaimClock(2*time.Minute - reclaimSampleInterval)
		if calls.Load() != 2 {
			t.Fatalf("bypassed back-off after less than 5%% growth since the second request")
		}
		advanceReclaimClock(reclaimSampleInterval)
		if calls.Load() != 3 {
			t.Fatalf("back-off did not reset: got %d requests after 2 minutes, want 3", calls.Load())
		}
	})
}

func TestReclaimWatchRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var used atomic.Uint32
		used.Store(800)
		var calls atomic.Int32
		logs := make(chan string, 4)
		dial := reclaimDialFunc(func(context.Context, string) (net.Conn, error) {
			switch calls.Add(1) {
			case 2:
				return reclaimTestReply("invalid\n"), nil
			case 5, 7:
				return reclaimTestReply("100 10\n"), nil
			default:
				return nil, errors.New("connection refused")
			}
		})
		startHostTableWatch(ctx, dial, reclaimTestManifest(), reclaimTestSysctl(&used, nil), func(format string, args ...any) {
			logs <- fmt.Sprintf(format, args...)
		})
		synctest.Wait()
		advanceReclaimClock(reclaimSampleInterval)
		for i, step := range []struct {
			wait    time.Duration
			message string
		}{
			{reclaimRetryDelay, "dial guest: connection refused"},
			{reclaimRetryDelay, "invalid guest reclaim reply"},
			{2 * time.Minute, "dial guest: connection refused"},
			{4 * time.Minute, "dial guest: connection refused"},
			{time.Minute, "fuse_inode 100 -> 10"},
			{reclaimRetryDelay, "dial guest: connection refused"},
		} {
			advanceReclaimClock(step.wait - reclaimSampleInterval)
			if int(calls.Load()) != i+1 || len(logs) != 1 {
				t.Fatalf("before retry %d: calls = %d, log lines = %d", i+1, calls.Load(), len(logs))
			}
			if line := <-logs; !strings.Contains(line, step.message) {
				t.Fatalf("retry %d: log = %q, want %q", i+1, line, step.message)
			}
			advanceReclaimClock(reclaimSampleInterval)
			if int(calls.Load()) != i+2 {
				t.Fatalf("retry %d: calls = %d", i+1, calls.Load())
			}
		}
	})
}

func TestReclaimWatchSkippedKeepsCooldownAndClearsFailures(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var used atomic.Uint32
		used.Store(800)
		var calls atomic.Int32
		logs := make(chan string, 4)
		dial := reclaimDialFunc(func(context.Context, string) (net.Conn, error) {
			switch calls.Add(1) {
			case 2, 3, 5:
				return nil, errors.New("connection refused")
			case 4:
				return reclaimTestReply("skipped\n"), nil
			default:
				return reclaimTestReply("100 100\n"), nil
			}
		})
		startHostTableWatch(t.Context(), dial, reclaimTestManifest(), reclaimTestSysctl(&used, nil), func(format string, args ...any) {
			logs <- fmt.Sprintf(format, args...)
		})
		synctest.Wait()
		advanceReclaimClock(reclaimSampleInterval)
		for i, wait := range []time.Duration{2 * time.Minute, reclaimRetryDelay, reclaimRetryDelay, 2 * time.Minute, reclaimRetryDelay, 4 * time.Minute} {
			advanceReclaimClock(wait - reclaimSampleInterval)
			if int(calls.Load()) != i+1 || len(logs) != 1 {
				t.Fatalf("after request %d, waited less than %s: calls = %d, logs = %d", i+1, wait, calls.Load(), len(logs))
			}
			if line := <-logs; i == 3 && !strings.Contains(line, "host files 800/1000, vnodes 100/780, guest skipped because it reclaimed recently, cooldown 2m0s") {
				t.Fatalf("unexpected skip log: %q", line)
			}
			advanceReclaimClock(reclaimSampleInterval)
			if int(calls.Load()) != i+2 {
				t.Fatalf("after request %d, waited %s: got %d requests, want %d", i+1, wait, calls.Load(), i+2)
			}
		}
	})
}

func TestReclaimWatchStopsOnSysctlError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var used atomic.Uint32
		var samples atomic.Int32
		logs := make(chan string, 2)
		baseSysctl := reclaimTestSysctl(&used, nil)
		sysctl := func(name string) (uint32, error) {
			if name == "kern.free_vnodes" {
				samples.Add(1)
				return 0, errors.New("unknown sysctl")
			}
			return baseSysctl(name)
		}
		startHostTableWatch(t.Context(), nil, reclaimTestManifest(), sysctl, func(format string, args ...any) {
			logs <- fmt.Sprintf(format, args...)
		})
		synctest.Wait()
		advanceReclaimClock(time.Hour)
		if samples.Load() != 1 || len(logs) != 1 {
			t.Fatalf("samples = %d, log lines = %d, want one of each", samples.Load(), len(logs))
		}
		if line := <-logs; !strings.Contains(line, "kern.free_vnodes: unknown sysctl") {
			t.Fatalf("log = %q, want the sysctl and its error", line)
		}
	})
}

func TestReclaimWatchKeepsOneRequestInFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var used atomic.Uint32
		used.Store(800)
		var calls atomic.Int32
		release := make(chan struct{})
		defer close(release)
		dial := reclaimDialFunc(func(ctx context.Context, _ string) (net.Conn, error) {
			calls.Add(1)
			<-ctx.Done()
			// Keep the first request unwinding past several sample ticks.
			<-release
			return nil, ctx.Err()
		})
		startHostTableWatch(ctx, dial, reclaimTestManifest(), reclaimTestSysctl(&used, nil), t.Logf)
		synctest.Wait()
		advanceReclaimClock(reclaimTimeout + 3*reclaimSampleInterval)
		if calls.Load() != 1 {
			t.Fatalf("dialed %d times while the first request was still in flight", calls.Load())
		}
	})
}

func TestReclaimWatchOffWithoutPort(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		startHostTableWatch(t.Context(), nil, &Manifest{}, nil, nil)
		advanceReclaimClock(time.Minute)
	})
}

func TestRequestReclaimRejectsMalformedReplies(t *testing.T) {
	for _, reply := range []string{
		"", "3 2", "3  2\n", "-3 2\n", "3 2\n1 0\n",
		"18446744073709551616 0\n", strings.Repeat("9", reclaimReplyBytes+1), "skipped", "skipped 1\n",
	} {
		t.Run(reply, func(t *testing.T) {
			dial := reclaimDialFunc(func(context.Context, string) (net.Conn, error) {
				return reclaimTestReply(reply), nil
			})
			if _, err := requestReclaim(t.Context(), dial, ""); err == nil {
				t.Fatalf("accepted %q", reply)
			}
		})
	}
}

func TestRequestReclaimParsesSkipped(t *testing.T) {
	dial := reclaimDialFunc(func(context.Context, string) (net.Conn, error) {
		return reclaimTestReply("skipped\n"), nil
	})
	if reply, err := requestReclaim(t.Context(), dial, ""); err != nil || !reply.skipped {
		t.Fatalf("reply = %+v, err = %v, want skipped and nil", reply, err)
	}
}

func TestRequestReclaimStopsReading(t *testing.T) {
	for _, want := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(want.Error(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				client, guest := net.Pipe()
				defer guest.Close()
				dial := reclaimDialFunc(func(context.Context, string) (net.Conn, error) { return client, nil })
				done := make(chan error, 1)
				go func() {
					_, err := requestReclaim(ctx, dial, "")
					done <- err
				}()
				advanceReclaimClock(time.Second)
				if len(done) != 0 {
					t.Fatal("stopped reading before the guest's one-second wait")
				}
				if want == context.Canceled {
					cancel()
					synctest.Wait()
				} else {
					advanceReclaimClock(reclaimTimeout - time.Second)
				}
				select {
				case err := <-done:
					if !errors.Is(err, want) {
						t.Fatalf("request returned %v, want %v", err, want)
					}
				default:
					t.Fatal("request is still waiting for the guest")
				}
			})
		})
	}
}
