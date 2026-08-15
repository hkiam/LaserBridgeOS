package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
	"github.com/laserbridgeos/laserbridgeos/backend/internal/proxy"
	lbruntime "github.com/laserbridgeos/laserbridgeos/backend/internal/runtime"
)

// grblServer is testServer with a say in where the bridge socket lives.
func grblServer(t *testing.T, socket string) (http.Handler, *config.Store) {
	t.Helper()
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.yaml"))
	if _, err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	// One root for the whole server: nothing here may read - or write - the
	// machine running the test.
	root := filepath.Join(dir, "root")
	manager := &lbruntime.Manager{Store: store, Dir: filepath.Join(dir, "run"), DataDir: filepath.Join(dir, "data"), Root: root}
	handler := (&Server{Store: store, Runtime: manager, BridgeSocket: socket, Root: root}).Handler()
	return handler, store
}

func getGRBL(t *testing.T, handler http.Handler) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/grbl", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/grbl = %d %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestGRBLStatusSaysSoWhenSer2netIsInCharge(t *testing.T) {
	handler, _ := grblServer(t, filepath.Join(t.TempDir(), "absent.sock"))
	body := getGRBL(t, handler)
	if body["available"] != false {
		t.Fatalf("available = %v, want false with ser2net selected", body["available"])
	}
	if body["backend"] != config.BackendSer2net {
		t.Errorf("backend = %v", body["backend"])
	}
}

func TestGRBLStatusSurvivesAnAbsentDaemon(t *testing.T) {
	// The daemon may be restarting. The page should say the reading is
	// unavailable rather than show an error where a machine state belongs.
	handler, store := grblServer(t, filepath.Join(t.TempDir(), "absent.sock"))
	cfg, _ := store.Load()
	cfg.GRBL.Backend = config.BackendLaserbridged
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	body := getGRBL(t, handler)
	if body["available"] != false {
		t.Fatalf("available = %v, want false", body["available"])
	}
	if body["reason"] == "" {
		t.Error("no reason given for the missing reading")
	}
}

func TestGRBLStatusPassesTheBridgeReadingOn(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "laserbridged.sock")
	bridge := proxy.New(proxy.Config{Port: 23, Device: "/dev/null"}, nil)
	server := proxy.NewStatusSocket(socket, bridge)
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if err := server.Serve(ctx); err != nil {
			t.Errorf("status socket: %v", err)
		}
	}()
	t.Cleanup(func() { cancel(); <-finished })

	handler, store := grblServer(t, socket)
	cfg, _ := store.Load()
	cfg.GRBL.Backend = config.BackendLaserbridged
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}

	body := waitForAvailable(t, handler)
	reading, ok := body["bridge"].(map[string]any)
	if !ok {
		t.Fatalf("no bridge reading in %v", body)
	}
	if reading["state"] != string(proxy.StateStopped) {
		t.Errorf("state = %v, want %v", reading["state"], proxy.StateStopped)
	}
	if _, ok := reading["machine"]; !ok {
		t.Error("the reading carries no machine field")
	}
}

func waitForAvailable(t *testing.T, handler http.Handler) map[string]any {
	t.Helper()
	var body map[string]any
	for i := 0; i < 50; i++ {
		body = getGRBL(t, handler)
		if body["available"] == true {
			return body
		}
	}
	t.Fatalf("the bridge reading never became available: %v", body)
	return nil
}
