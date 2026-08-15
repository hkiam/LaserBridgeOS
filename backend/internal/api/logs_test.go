package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
	lbruntime "github.com/laserbridgeos/laserbridgeos/backend/internal/runtime"
)

type logRunner struct{ out string }

func (r logRunner) Run(string, ...string) ([]byte, error) { return []byte(r.out), nil }

func logServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.yaml"))
	if _, err := store.Ensure(); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(filepath.Join(data, "log"), 0755); err != nil {
		t.Fatal(err)
	}
	manager := &lbruntime.Manager{Store: store, Dir: filepath.Join(dir, "run"), DataDir: data, Root: filepath.Join(dir, "root")}
	runner := logRunner{out: "from the ring buffer\n"}
	server := &Server{Store: store, Runtime: manager, Runner: runner, Root: filepath.Join(dir, "root")}
	return server.Handler(), filepath.Join(data, "log", "messages")
}

func readLines(t *testing.T, handler http.Handler, query string) []string {
	t.Helper()
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/logs"+query, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var body struct {
		Lines []string `json:"lines"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Lines
}

func TestLogsComeFromTheFileThatSurvivesAReboot(t *testing.T) {
	handler, path := logServer(t)
	// Before syslogd has written anything there is still the ring buffer, and
	// answering with nothing at all would look like an appliance with no logs.
	if lines := readLines(t, handler, ""); len(lines) != 1 || lines[0] != "from the ring buffer" {
		t.Fatalf("lines = %q, want the logread fallback", lines)
	}

	var written strings.Builder
	for i := 0; i < 500; i++ {
		written.WriteString("line ")
		written.WriteString(strconv.Itoa(i))
		written.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(written.String()), 0640); err != nil {
		t.Fatal(err)
	}
	lines := readLines(t, handler, "?limit=3")
	if len(lines) != 3 || lines[2] != "line 499" {
		t.Fatalf("lines = %q, want the last three lines of the file", lines)
	}
}
