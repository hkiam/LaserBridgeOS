package runtime

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
)

type recordingRunner struct {
	mu       sync.Mutex
	calls    []string
	statusOK bool
}

func (r *recordingRunner) Run(name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	call := strings.Join(append([]string{name}, args...), " ")
	r.calls = append(r.calls, call)
	if strings.HasSuffix(call, "status") && !r.statusOK {
		return []byte("stopped"), errStopped
	}
	return nil, nil
}

func (r *recordingRunner) count(substring string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, call := range r.calls {
		if strings.Contains(call, substring) {
			n++
		}
	}
	return n
}

func (r *recordingRunner) restarts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, call := range r.calls {
		if strings.Contains(call, "laserbridged restart") {
			n++
		}
	}
	return n
}

var errStopped = &stoppedError{}

type stoppedError struct{}

func (*stoppedError) Error() string { return "service is stopped" }

// answeringSocket stands in for a healthy daemon; wedged is one that accepts
// the connection and then says nothing, which is exactly what a deadlocked
// process looks like from outside - the kernel does the accepting.
func answeringSocket(t *testing.T, path string, answer bool) {
	t.Helper()
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if !answer {
				// Hold it open and say nothing.
				continue
			}
			go func() {
				defer conn.Close()
				_ = json.NewEncoder(conn).Encode(map[string]any{"state": "LISTENING"})
			}()
		}
	}()
}

func watchFor(t *testing.T, socket string, runner *recordingRunner) *BridgeWatch {
	t.Helper()
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.yaml"))
	if _, err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	cfg, _ := store.Load()
	cfg.GRBL.Backend = config.BackendLaserbridged
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	return &BridgeWatch{
		Store: store, Runner: runner, SocketPath: socket,
		Interval: 20 * time.Millisecond, Timeout: 50 * time.Millisecond,
		Failures: 3, Cooldown: 10 * time.Second,
	}
}

func TestWedgedBridgeIsRestarted(t *testing.T) {
	// The failure this exists for: the process is alive, its listening socket
	// still accepts, and it has stopped supervising the machine. Nothing else
	// in the appliance would notice.
	socket := filepath.Join(t.TempDir(), "bridge.sock")
	answeringSocket(t, socket, false)
	runner := &recordingRunner{statusOK: true}
	watch := watchFor(t, socket, runner)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watch.Watch(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && runner.restarts() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if runner.restarts() == 0 {
		t.Fatal("a bridge that accepts connections and never answers was left alone")
	}
	if watch.Responsive() {
		t.Error("Responsive() called a wedged daemon healthy")
	}
}

func TestAnsweringBridgeIsLeftAlone(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "bridge.sock")
	answeringSocket(t, socket, true)
	runner := &recordingRunner{statusOK: true}
	watch := watchFor(t, socket, runner)
	// The other tests want a bridge to be given up on quickly; this one wants it
	// left alone, and fifty milliseconds to answer a Unix socket is not a
	// generous allowance under the race detector, which slows everything down
	// several times over. Three unlucky round trips in a row would restart a
	// perfectly healthy bridge and fail this test for the wrong reason - and a
	// test that flakes under -race is a test that gets -race switched off.
	watch.Timeout = 500 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watch.Watch(ctx)
	time.Sleep(300 * time.Millisecond)

	if n := runner.restarts(); n != 0 {
		t.Errorf("restarted a working bridge %d times", n)
	}
	if !watch.Responsive() {
		t.Error("Responsive() should say yes")
	}
}

func TestARestartThatLeavesItStoppedIsFollowedByAStart(t *testing.T) {
	// The worst outcome observed on a real appliance: stopping a wedged daemon
	// leaves OpenRC's supervisor gone and the service marked stopped, so an
	// earlier version of this saw "not running, so not a hang" and walked away
	// from a machine with no bridge at all. Being unable to fix it is one
	// thing; leaving it worse is another.
	socket := filepath.Join(t.TempDir(), "absent.sock")
	runner := &recordingRunner{statusOK: false}
	watch := watchFor(t, socket, runner)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watch.Watch(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && runner.count("laserbridged start") == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if runner.restarts() == 0 {
		t.Fatal("an unresponsive bridge was never restarted")
	}
	if runner.count("laserbridged start") == 0 {
		t.Error("the restart left the service stopped and nothing started it")
	}
}

func TestSer2netIsNotWatched(t *testing.T) {
	// There is nothing to ask, and asking a socket that will never exist must
	// not turn into a restart loop for a service that is deliberately idle.
	socket := filepath.Join(t.TempDir(), "absent.sock")
	runner := &recordingRunner{statusOK: true}
	watch := watchFor(t, socket, runner)
	cfg, _ := watch.Store.Load()
	cfg.GRBL.Backend = config.BackendSer2net
	if err := watch.Store.Save(cfg); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watch.Watch(ctx)
	time.Sleep(300 * time.Millisecond)

	if n := runner.restarts(); n != 0 {
		t.Errorf("restarted laserbridged %d times while ser2net was in charge", n)
	}
}

func TestRestartsDoNotRepeatQuickly(t *testing.T) {
	// A restart costs the client its connection. A bridge that stays wedged
	// must not turn into a machine that drops its client every few seconds.
	socket := filepath.Join(t.TempDir(), "bridge.sock")
	answeringSocket(t, socket, false)
	runner := &recordingRunner{statusOK: true}
	watch := watchFor(t, socket, runner)
	watch.Cooldown = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go watch.Watch(ctx)
	time.Sleep(700 * time.Millisecond)

	if n := runner.restarts(); n != 1 {
		t.Errorf("restarts = %d, want exactly one within the cooldown", n)
	}
}
