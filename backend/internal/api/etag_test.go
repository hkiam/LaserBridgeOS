package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Two tabs, or one tab and somebody at the SSH prompt: the whole document is
// sent on every save, so without this the second writer silently discards the
// first one's work and neither of them is told.
func TestASaveAgainstAStaleConfigurationIsRefused(t *testing.T) {
	handler, store, _ := testServer(t)
	cookie, token := csrf(t, handler)

	read := httptest.NewRecorder()
	handler.ServeHTTP(read, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	tag := read.Header().Get("ETag")
	if tag == "" {
		t.Fatal("GET /api/config did not name the configuration it returned")
	}

	// Somebody else saves in the meantime.
	meanwhile, _ := store.Load()
	meanwhile.Camera.FPS = 24
	if err := store.Save(meanwhile); err != nil {
		t.Fatal(err)
	}

	mine, _ := store.Load()
	mine.Camera.Quality = 55
	body, _ := json.Marshal(mine)
	request := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(body))
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", token)
	request.Header.Set("If-Match", tag)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	stored, _ := store.Load()
	if stored.Camera.FPS != 24 || stored.Camera.Quality == 55 {
		t.Fatalf("the other save was overwritten: %+v", stored.Camera)
	}
}

func TestASaveWithTheCurrentTagGoesThrough(t *testing.T) {
	handler, store, _ := testServer(t)
	cookie, token := csrf(t, handler)

	read := httptest.NewRecorder()
	handler.ServeHTTP(read, httptest.NewRequest(http.MethodGet, "/api/config", nil))
	tag := read.Header().Get("ETag")

	mine, _ := store.Load()
	mine.Camera.Quality = 55
	body, _ := json.Marshal(mine)
	request := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(body))
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", token)
	request.Header.Set("If-Match", tag)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	// And the answer names what was written, so the page can carry on editing
	// without reloading.
	if next := response.Header().Get("ETag"); next == "" || next == tag {
		t.Fatalf("ETag after the save = %q, want the new configuration named", next)
	}
	stored, _ := store.Load()
	if stored.Camera.Quality != 55 {
		t.Fatalf("quality = %d, want the save applied", stored.Camera.Quality)
	}
}

// A caller that says nothing about which version it edited is still served: a
// shell script with curl never loaded a page to be stale against.
func TestASaveWithoutATagIsStillAllowed(t *testing.T) {
	handler, store, _ := testServer(t)
	cookie, token := csrf(t, handler)
	cfg, _ := store.Load()
	cfg.Camera.Quality = 42
	body, _ := json.Marshal(cfg)
	request := httptest.NewRequest(http.MethodPut, "/api/config", bytes.NewReader(body))
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
}
