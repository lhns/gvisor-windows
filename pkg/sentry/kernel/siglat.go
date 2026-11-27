// gvisor-windows: signal delivery latency trace (GVISOR_SIGLAT=1, or a threshold
// in ms), docs/results/3e-signal-latency.md.
//
// A timer signal (ITIMER_REAL, POSIX timers) is followed from the expiry to the
// thread's first trap after the handler started, in stages:
//
//	late    the timer goroutine ran after the expiry (Sentry clock)
//	fire    NotifyTimer: the signal is sent
//	sent    SendSignal returned in the timer goroutine (the task was told)
//	out     the task left its block (woken by the interrupt) or Platform.Switch
//	deliver the handler frame is set up
//	resume  Platform.Switch is entered to run the handler
//	return  Platform.Switch returned (the handler's rt_sigreturn, usually)
//
// A trace whose total exceeds the threshold is printed to stderr with the stages
// and the platform's account of the last switch; DumpSigLat prints a histogram.
// One trace at a time: this is for single-threaded latency tests.

package kernel

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/sentry/ktime"
)

// SigLat enables the trace; set from the environment at start.
var SigLat, sigLatMin = func() (bool, int64) {
	v := os.Getenv("GVISOR_SIGLAT")
	if v == "" || v == "0" {
		return false, 0
	}
	ms, err := strconv.Atoi(v)
	if err != nil || ms <= 1 {
		ms = 20
	}
	return true, int64(ms) * 1e6
}()

func init() {
	ktime.TraceLate = SigLat
	if SigLat {
		ktime.LateHook = sigLatTick
	}
}

// sigLatTimedOut reports a sigtimedwait that timed out although a signal it
// waited for was pending: the signal did not wake the task.
func sigLatTimedOut(signo int32) {
	fmt.Fprintf(os.Stderr, "SIGLAT sigtimedwait timed out with signal %d pending (qpc %.3f ms, %s UTC)\n",
		signo, float64(profNow())/1e6, time.Now().UTC().Format("15:04:05.000"))
}

// sigLatTick reports a Sentry timer that ran late by the threshold, with the
// timer clock's reading beside the host's QPC (a gap between the two that
// changes means the Sentry clock drifted).
func sigLatTick(late, now int64) {
	if late >= sigLatMin {
		fmt.Fprintf(os.Stderr, "SIGLAT ktime tick %.2f ms after its expiry (clock %.3f ms, qpc %.3f ms, %s UTC)\n",
			float64(late)/1e6, float64(now)/1e6, float64(profNow())/1e6, time.Now().UTC().Format("15:04:05.000"))
	}
}

type sigLatTrace struct {
	t                                *Task
	signo                            int32
	late                             int64 // ns of the Sentry clock
	blocked                          int64 // when t blocked, if it was woken from a block
	fire, sent, out, deliver, resume int64
	how                              string // "block" or "switch": how the task was reached
	cpuFire, cpuSent, cpuOut         int32  // processors at fire, sent and out
	outInfo                          string // the platform's account of the interrupted Switch
}

// sigLatBuckets are the upper bounds (ms) of the histogram of total latencies.
var sigLatBuckets = [...]float64{0.5, 1, 2, 5, 10, 20, 50, 100, 1e18}

var sigLat struct {
	mu      sync.Mutex
	cur     sigLatTrace
	blockT  *Task
	blockAt int64
	n, over int64
	dropped int64
	dog     *time.Timer // dumps the goroutines if the send takes sigLatDogAfter
	dumps   int
	max     float64
	hist    [len(sigLatBuckets)]int64
}

// SigLatPlatformInfo is implemented by platform contexts that can describe their
// last Switch for the trace.
type SigLatPlatformInfo interface {
	SigLatInfo() string
}

func sigLatFire(signo int32) {
	now := profNow()
	late := ktime.LastLate()
	sigLat.mu.Lock()
	if sigLat.cur.fire != 0 {
		sigLat.dropped++
	}
	sigLat.cur = sigLatTrace{signo: signo, late: late, fire: now, cpuFire: profCPU(), cpuSent: -1, cpuOut: -1}
	if sigLat.dog != nil {
		sigLat.dog.Stop()
	}
	if sigLat.dumps < sigLatMaxDumps {
		sigLat.dog = time.AfterFunc(sigLatDogAfter, func() { sigLatDump(now) })
	}
	sigLat.mu.Unlock()
}

// A send of a timer signal that takes longer than sigLatDogAfter dumps every
// goroutine's stack (at most sigLatMaxDumps times), to show what it waits for.
const (
	sigLatDogAfter = 50 * time.Millisecond
	sigLatMaxDumps = 3
)

func sigLatDump(fire int64) {
	sigLat.mu.Lock()
	stuck := sigLat.cur.fire == fire && sigLat.cur.sent == 0
	if stuck {
		sigLat.dumps++
	}
	sigLat.mu.Unlock()
	if !stuck {
		return
	}
	buf := make([]byte, 4<<20)
	buf = buf[:runtime.Stack(buf, true)]
	fmt.Fprintf(os.Stderr, "SIGLAT send of a timer signal not done after %v (fire qpc %.3f ms, qpc %.3f ms); goroutines:\n%s\nSIGLAT end of goroutines\n",
		sigLatDogAfter, float64(fire)/1e6, float64(profNow())/1e6, buf)
}

// sigLatSent is called when the timer goroutine has sent the signal.
func sigLatSent() {
	now := profNow()
	cpu := profCPU()
	sigLat.mu.Lock()
	if c := &sigLat.cur; c.fire != 0 && c.sent == 0 {
		c.sent, c.cpuSent = now, cpu
		if sigLat.dog != nil {
			sigLat.dog.Stop()
		}
	}
	sigLat.mu.Unlock()
}

// sigLatBlock is called when t blocks.
func sigLatBlock(t *Task) {
	now := profNow()
	sigLat.mu.Lock()
	sigLat.blockT, sigLat.blockAt = t, now
	sigLat.mu.Unlock()
}

// sigLatOut is called when t was interrupted out of a block or out of Switch.
func sigLatOut(t *Task, how string) {
	now := profNow()
	sigLat.mu.Lock()
	c := &sigLat.cur
	if c.fire != 0 && c.out == 0 {
		c.out, c.t, c.how, c.cpuOut = now, t, how, profCPU()
		if how == "block" && sigLat.blockT == t {
			c.blocked = sigLat.blockAt
		}
	}
	sigLat.mu.Unlock()
}

func sigLatDeliver(t *Task, signo int32) {
	now := profNow()
	sigLat.mu.Lock()
	c := &sigLat.cur
	if c.fire != 0 && c.signo == signo && c.deliver == 0 {
		if c.out == 0 {
			c.out, c.how, c.cpuOut = now, "running", profCPU()
		}
		c.deliver, c.t = now, t
	}
	sigLat.mu.Unlock()
}

// sigLatSwitch is called before (enter) and after Platform.Switch.
func sigLatSwitch(t *Task, enter bool, err error) {
	now := profNow()
	sigLat.mu.Lock()
	defer sigLat.mu.Unlock()
	c := &sigLat.cur
	if c.fire == 0 || (c.t != nil && c.t != t) {
		return
	}
	if enter {
		if c.deliver != 0 && c.resume == 0 {
			c.resume = now
		}
		return
	}
	if c.resume == 0 {
		if c.out == 0 && err != nil {
			c.out, c.t, c.how, c.cpuOut = now, t, "switch", profCPU()
			if p, ok := t.p.(SigLatPlatformInfo); ok {
				c.outInfo = p.SigLatInfo()
			}
		}
		return
	}
	ms := func(a, b int64) float64 { return float64(b-a) / 1e6 }
	total := ms(c.fire, now) + max(float64(c.late)/1e6, 0)
	sigLat.n++
	for i, b := range sigLatBuckets {
		if total < b {
			sigLat.hist[i]++
			break
		}
	}
	sigLat.max = max(sigLat.max, total)
	if total*1e6 >= float64(sigLatMin) {
		sigLat.over++
		info := c.outInfo
		if info == "" {
			info = "none (not reached in Switch)"
		}
		sent := c.sent
		if sent == 0 {
			sent = c.fire
		}
		blocked := ""
		if c.blocked != 0 {
			blocked = fmt.Sprintf(", blocked %.2f ms before the signal", ms(c.blocked, c.fire))
		}
		fmt.Fprintf(os.Stderr, "SIGLAT signal %d total %.2f ms: timer late %.2f, fire->sent %.2f, sent->out(%s) %.2f, out->deliver %.2f, deliver->resume %.2f, resume->return %.2f (%v)%s; cpu fire/sent/out %d/%d/%d; platform at out: %s; fire qpc %.3f ms, qpc %.3f ms, %s UTC\n",
			c.signo, total, float64(c.late)/1e6, ms(c.fire, sent), c.how, ms(sent, c.out), ms(c.out, c.deliver), ms(c.deliver, c.resume), ms(c.resume, now), err,
			blocked, c.cpuFire, c.cpuSent, c.cpuOut, info, float64(c.fire)/1e6, float64(now)/1e6, time.Now().UTC().Format("15:04:05.000"))
	}
	sigLat.cur = sigLatTrace{}
}

// DumpSigLat prints the histogram of traced latencies.
func DumpSigLat(w io.Writer) {
	sigLat.mu.Lock()
	defer sigLat.mu.Unlock()
	fmt.Fprintf(w, "SIGLAT summary: %d timer signals traced, %d over %d ms, max %.2f ms, %d dropped; ms:", sigLat.n, sigLat.over, sigLatMin/1e6, sigLat.max, sigLat.dropped)
	for i, b := range sigLatBuckets {
		if b > 1e17 {
			fmt.Fprintf(w, " >=%g:%d", sigLatBuckets[i-1], sigLat.hist[i])
		} else {
			fmt.Fprintf(w, " <%g:%d", b, sigLat.hist[i])
		}
	}
	fmt.Fprintln(w)
}
