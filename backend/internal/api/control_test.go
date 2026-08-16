package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The handler turns what the daemon said into a status code, and it does so by
// matching on words. That is honest about the transport - two halves of one
// program talking over a socket in sentences - and it is exactly the kind of
// thing that rots silently when somebody rewords an error. These tests are the
// thing that notices.
func steerRequest(t *testing.T, handler http.Handler, command, body string) *httptest.ResponseRecorder {
	t.Helper()
	cookie, token := csrf(t, handler)
	request := httptest.NewRequest(http.MethodPost, "/api/grbl/control/"+command, bytes.NewReader([]byte(body)))
	request.AddCookie(cookie)
	request.Header.Set("X-CSRF-Token", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestSteeringNeedsTheBackendThatKnowsWhatItIsCarrying(t *testing.T) {
	handler, store, _ := testServer(t)
	cfg, _ := store.Load()
	cfg.GRBL.Backend = "ser2net"
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	response := steerRequest(t, handler, "jog", `{"axis":"X","distance":1,"feed":1000}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: ser2net has nowhere to put a command", response.Code)
	}
	if !strings.Contains(response.Body.String(), "ser2net") {
		t.Errorf("the reason does not name the backend: %s", response.Body.String())
	}
}

func TestSteeringWithoutADaemonSaysSoRatherThanPretending(t *testing.T) {
	handler, store, _ := testServer(t)
	cfg, _ := store.Load()
	cfg.GRBL.Backend = "laserbridged"
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	// No daemon is listening on the socket in a test server.
	response := steerRequest(t, handler, "home", `{}`)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", response.Code)
	}
}

func TestSteeringRefusesACallerWithoutASession(t *testing.T) {
	handler, _, _ := testServer(t)
	request := httptest.NewRequest(http.MethodPost, "/api/grbl/control/stop", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want the CSRF guard to refuse it", response.Code)
	}
}

func TestSteeringRejectsAMalformedBody(t *testing.T) {
	handler, store, _ := testServer(t)
	cfg, _ := store.Load()
	cfg.GRBL.Backend = "laserbridged"
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	response := steerRequest(t, handler, "jog", `{"axis":"X","surprise":true}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want an unknown field refused", response.Code)
	}
}

// The console is a read and answers like the journal does: with ser2net in
// charge there is nothing to show, and saying so is not the same as an error.
func TestTheConsoleSaysNothingIsRecordedUnderSer2net(t *testing.T) {
	handler, store, _ := testServer(t)
	cfg, _ := store.Load()
	cfg.GRBL.Backend = "ser2net"
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/grbl/console?after=12", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	if !strings.Contains(response.Body.String(), `"available":false`) {
		t.Fatalf("body = %s", response.Body.String())
	}
}

// A typed command travels the same road as a jog, so it meets the same guards.
func TestATypedCommandIsRefusedUnderSer2netLikeEverythingElse(t *testing.T) {
	handler, store, _ := testServer(t)
	cfg, _ := store.Load()
	cfg.GRBL.Backend = "ser2net"
	if err := store.Save(cfg); err != nil {
		t.Fatal(err)
	}
	response := steerRequest(t, handler, "send", `{"line":"$$"}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", response.Code)
	}
}
