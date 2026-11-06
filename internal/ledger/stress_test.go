package ledger_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/vivekyarra/dibs/internal/engine"
	"github.com/vivekyarra/dibs/internal/ledger"
)

// incr is one read-modify-write: load the ticket, bump Attempts, save, log.
func incr(l *ledger.Ledger, n int, actor string) error {
	return l.Tx(context.Background(), func(tx *ledger.Tx) error {
		t, err := tx.Ticket(n)
		if err != nil {
			return err
		}
		t.Attempts++
		if err := tx.SaveTicket(t, time.Now()); err != nil {
			return err
		}
		return tx.Event(n, actor, "attempt", time.Now(), nil)
	})
}

func prepareCounter(t *testing.T, path string) {
	t.Helper()
	l, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.Tx(context.Background(), func(tx *ledger.Tx) error {
		return tx.SaveTicket(ledger.Ticket{Number: 1, Title: "counter", State: engine.InProgress}, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
}

func readCounter(t *testing.T, path string) (attempts, events int) {
	t.Helper()
	l, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if err := l.Tx(context.Background(), func(tx *ledger.Tx) error {
		tk, err := tx.Ticket(1)
		if err != nil {
			return err
		}
		evs, err := tx.Events(1, "attempt")
		attempts, events = tk.Attempts, len(evs)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return
}

// 32 goroutines × 200 read-modify-write transactions on one row: no lost updates.
func TestStressGoroutines(t *testing.T) {
	if testing.Short() {
		t.Skip("stress")
	}
	const workers, per = 32, 200
	path := filepath.Join(t.TempDir(), "stress.sqlite")
	prepareCounter(t, path)
	l, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Serial baseline for the throughput report (uses ticket 2 so the count on #1 stays exact).
	if err := l.Tx(context.Background(), func(tx *ledger.Tx) error {
		return tx.SaveTicket(ledger.Ticket{Number: 2, Title: "baseline", State: engine.InProgress}, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	bs := time.Now()
	for i := 0; i < per; i++ {
		if err := incr(l, 2, "serial"); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("serial: %d tx in %v = %.0f tx/s", per, time.Since(bs).Round(time.Millisecond), float64(per)/time.Since(bs).Seconds())
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				if err := incr(l, 1, fmt.Sprintf("g%d", w)); err != nil {
					errs <- fmt.Errorf("worker %d op %d: %w", w, i, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	el := time.Since(start)
	l.Close()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	got, evs := readCounter(t, path)
	if got != workers*per || evs != workers*per {
		t.Fatalf("lost updates: attempts=%d events=%d want %d", got, evs, workers*per)
	}
	t.Logf("goroutines: %d tx in %v = %.0f tx/s", workers*per, el.Round(time.Millisecond), float64(workers*per)/el.Seconds())
}

const childEnv = "DIBS_LEDGER_STRESS_CHILD"

// TestStressChildProcess is the body of one child process in TestStressProcesses.
func TestStressChildProcess(t *testing.T) {
	path := os.Getenv(childEnv)
	if path == "" {
		t.Skip("only runs as a child of TestStressProcesses")
	}
	n, _ := strconv.Atoi(os.Getenv(childEnv + "_N"))
	id := os.Getenv(childEnv + "_ID")
	l, err := ledger.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	// A few goroutines per process too, so in-process and cross-process locking mix.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < n/4; i++ {
				if err := incr(l, 1, fmt.Sprintf("p%s-g%d", id, g)); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = err
					}
					mu.Unlock()
					return
				}
			}
		}(g)
	}
	wg.Wait()
	if firstErr != nil {
		t.Fatal(firstErr)
	}
}

// 4 OS processes hammer the same database file (as three agents + a sweep would).
func TestStressProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("stress")
	}
	if os.Getenv(childEnv) != "" {
		t.Skip("child")
	}
	const procs, per = 4, 200 // per process (4 goroutines × 50)
	path := filepath.Join(t.TempDir(), "multi.sqlite")
	prepareCounter(t, path)
	start := time.Now()
	cmds := make([]*exec.Cmd, procs)
	outs := make([][]byte, procs)
	var wg sync.WaitGroup
	for i := range cmds {
		c := exec.Command(os.Args[0], "-test.run=^TestStressChildProcess$", "-test.count=1", "-test.v")
		c.Env = append(os.Environ(), childEnv+"="+path, childEnv+"_N="+strconv.Itoa(per), childEnv+"_ID="+strconv.Itoa(i))
		cmds[i] = c
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			out, err := cmds[i].CombinedOutput()
			outs[i] = out
			if err != nil {
				t.Errorf("child %d: %v\n%s", i, err, out)
			}
		}(i)
	}
	wg.Wait()
	el := time.Since(start)
	if t.Failed() {
		return
	}
	got, evs := readCounter(t, path)
	if got != procs*per || evs != procs*per {
		t.Fatalf("lost updates across processes: attempts=%d events=%d want %d", got, evs, procs*per)
	}
	t.Logf("processes: %d tx from %d processes in %v (incl. process start) = %.0f tx/s",
		procs*per, procs, el.Round(time.Millisecond), float64(procs*per)/el.Seconds())
}
