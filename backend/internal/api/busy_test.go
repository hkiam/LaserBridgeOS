package api

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
	"github.com/laserbridgeos/laserbridgeos/backend/internal/proxy"
	lbruntime "github.com/laserbridgeos/laserbridgeos/backend/internal/runtime"
)

// busyServer wires an API server to a real bridge and its status socket, with
// a pseudo-terminal standing in for the controller so the machine can be told
// to look busy.
func busyServer(t *testing.T) (http.Handler, *config.Store, *os.File) {
	t.Helper()
	dir := t.TempDir()
	socket := filepath.Join(dir, "bridge.sock")

	controller, devicePath := newPTY(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	bridge := proxy.New(proxy.Config{
		Device: devicePath, Baudrate: 115200, Port: port,
		DeviceRetry: 20 * time.Millisecond, OnDisconnect: proxy.DisconnectNone,
	}, nil)
	done := make(chan struct{})
	ready := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_ = bridge.Run(done, ready)
	}()
	<-ready
	socketDone := make(chan struct{})
	socketFinished := make(chan struct{})
	go func() {
		defer close(socketFinished)
		_ = proxy.NewStatusSocket(socket, bridge).Serve(socketDone)
	}()
	t.Cleanup(func() {
		close(socketDone)
		<-socketFinished
		close(done)
		<-finished
	})

	store := config.NewStore(filepath.Join(dir, "config.yaml"))
	if _, err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	cfg, _ := store.Load()
	cfg.GRBL.Backend = config.BackendLaserbridged
	cfg.System.SetupComplete = true
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{}
	manager := &lbruntime.Manager{Store: store, Dir: filepath.Join(dir, "run"), DataDir: filepath.Join(dir, "data"), Run: runner}
	handler := (&Server{Store: store, Runtime: manager, Runner: runner, BridgeSocket: socket}).Handler()
	return handler, store, controller
}

// tellMachine has the controller announce a state and waits for the bridge to
// have read it, so the test never races the parser.
func tellMachine(t *testing.T, handler http.Handler, controller *os.File, line string) {
	t.Helper()
	if _, err := controller.Write([]byte(line + "\r\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/grbl", nil))
		var body struct {
			Bridge struct {
				Machine struct {
					State string `json:"state"`
				} `json:"machine"`
			} `json:"bridge"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if body.Bridge.Machine.State != "" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the bridge never read the controller's report")
}

func post(t *testing.T, handler http.Handler, method, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	cookie, token := csrf(t, handler)
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.AddCookie(cookie)
	req.Header.Set("X-CSRF-Token", token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestRebootIsRefusedWhileTheMachineIsCutting(t *testing.T) {
	handler, _, controller := busyServer(t)
	tellMachine(t, handler, controller, "<Run|MPos:12.000,4.000,0.000|FS:600,255>")

	rec := post(t, handler, http.MethodPost, "/api/system/reboot", nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("reboot returned %d, want 409:\n%s", rec.Code, rec.Body.String())
	}
	// The refusal has to say what the machine is doing and where, in a
	// sentence; an operator reading it should not have to go and look, and
	// GRBL's own word for the state does not survive being lowercased into
	// one ("the machine is run").
	body := rec.Body.String()
	if !bytes.Contains([]byte(body), []byte("X 12")) {
		t.Errorf("the refusal does not say where the machine is: %s", body)
	}
	if !bytes.Contains([]byte(body), []byte("the machine is cutting")) {
		t.Errorf("the refusal does not read as a sentence: %s", body)
	}
}

func TestRebootGoesAheadWhenForced(t *testing.T) {
	handler, _, controller := busyServer(t)
	tellMachine(t, handler, controller, "<Run|MPos:1.000,1.000,0.000|FS:600,255>")

	rec := post(t, handler, http.MethodPost, "/api/system/reboot?force=true", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("forced reboot returned %d, want 202:\n%s", rec.Code, rec.Body.String())
	}
}

func TestAnIdleMachineBlocksNothing(t *testing.T) {
	handler, _, controller := busyServer(t)
	tellMachine(t, handler, controller, "<Idle|MPos:0.000,0.000,0.000|FS:0,0>")

	rec := post(t, handler, http.MethodPost, "/api/system/reboot", nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("reboot returned %d, want 202:\n%s", rec.Code, rec.Body.String())
	}
}

func TestGRBLSettingsAreRefusedWhileCutting(t *testing.T) {
	handler, store, controller := busyServer(t)
	tellMachine(t, handler, controller, "<Run|MPos:5.000,5.000,0.000|FS:600,255>")

	cfg, _ := store.Load()
	cfg.GRBL.Baudrate = 250000
	body, _ := json.Marshal(cfg)
	rec := post(t, handler, http.MethodPut, "/api/config", body)
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT /api/config returned %d, want 409:\n%s", rec.Code, rec.Body.String())
	}
	// And nothing was written.
	if after, _ := store.Load(); after.GRBL.Baudrate == 250000 {
		t.Error("the configuration was saved despite the refusal")
	}
}

func TestOtherSettingsAreNotBlockedByARunningJob(t *testing.T) {
	// Only the GRBL section restarts the bridge. Someone adjusting the camera
	// while a job runs is not doing anything dangerous, and being told
	// otherwise would teach them to reach for force=true out of habit.
	handler, store, controller := busyServer(t)
	tellMachine(t, handler, controller, "<Run|MPos:5.000,5.000,0.000|FS:600,255>")

	cfg, _ := store.Load()
	cfg.Camera.FPS = 15
	body, _ := json.Marshal(cfg)
	rec := post(t, handler, http.MethodPut, "/api/config", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/config returned %d, want 200:\n%s", rec.Code, rec.Body.String())
	}
}

func TestNotKnowingDoesNotBlock(t *testing.T) {
	// With ser2net in charge there is no reading, and an appliance that
	// refused to reboot because it could not tell would be worse than one that
	// never asked.
	handler, store := grblServer(t, filepath.Join(t.TempDir(), "absent.sock"))
	cfg, _ := store.Load()
	cfg.System.SetupComplete = true
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	rec := post(t, handler, http.MethodPost, "/api/system/reboot", nil)
	// grblServer has no Runner, so the honest answer is that reboot is
	// unavailable - what matters is that it is not a 409.
	if rec.Code == http.StatusConflict {
		t.Fatalf("refused with no way of knowing the machine was busy:\n%s", rec.Body.String())
	}
}
