package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// frozen is the timestamp every file in a built image carries. prepare-rootfs.sh
// sets it so that two builds of the same source produce the same bytes, and the
// consequence for caching is the point of these tests: Last-Modified cannot
// distinguish one version of the web interface from the next.
var frozen = time.Unix(1786579200, 0)

func webServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(full, frozen, frozen); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", "<!doctype html><title>version one</title>")
	write("assets/app.js", "console.log('version one');")
	return (&Server{WebRoot: root}).Handler(), root
}

func get(t *testing.T, handler http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestTheBrowserIsToldToAskBeforeReusingThePage(t *testing.T) {
	handler, _ := webServer(t)
	// "/index.html" is left out on purpose: the file server redirects it to
	// "/", which is the canonical path and the one a browser asks for.
	for _, path := range []string{"/", "/assets/app.js"} {
		response := get(t, handler, path, nil)
		if response.Code != http.StatusOK {
			t.Fatalf("%s = %d", path, response.Code)
		}
		// Without this a browser invents a freshness lifetime from the age of
		// the file and serves its copy without asking - which is how an
		// appliance that had just been updated kept handing out the old page.
		if got := response.Header().Get("Cache-Control"); got != "no-cache" {
			t.Errorf("%s Cache-Control = %q, want no-cache", path, got)
		}
		if response.Header().Get("ETag") == "" {
			t.Errorf("%s was served without an ETag", path)
		}
	}
}

func TestAnUnchangedPageCostsOneEmptyAnswer(t *testing.T) {
	handler, _ := webServer(t)
	first := get(t, handler, "/assets/app.js", nil)
	tag := first.Header().Get("ETag")

	again := get(t, handler, "/assets/app.js", map[string]string{"If-None-Match": tag})
	if again.Code != http.StatusNotModified {
		t.Fatalf("status = %d, want 304 for an unchanged file", again.Code)
	}
	if again.Body.Len() != 0 {
		t.Errorf("a 304 carried %d bytes", again.Body.Len())
	}
}

func TestAnUpdatedPageIsNotServedFromTheBrowsersCopy(t *testing.T) {
	// The failure this exists for. A system update replaces the web interface,
	// and every file in the new image carries the same frozen timestamp as the
	// old one - so Last-Modified says "not modified" about a file whose every
	// byte is different. Only a validator taken from the content can tell the
	// browser it is holding the wrong page.
	handler, root := webServer(t)
	first := get(t, handler, "/assets/app.js", nil)
	tag := first.Header().Get("ETag")

	replaced := filepath.Join(root, "assets", "app.js")
	if err := os.WriteFile(replaced, []byte("console.log('version two');"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replaced, frozen, frozen); err != nil {
		t.Fatal(err)
	}

	after := get(t, handler, "/assets/app.js", map[string]string{
		"If-None-Match":     tag,
		"If-Modified-Since": frozen.UTC().Format(http.TimeFormat),
	})
	if after.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: the browser would still be running the old interface", after.Code)
	}
	if body := after.Body.String(); body != "console.log('version two');" {
		t.Errorf("body = %q, want the new file", body)
	}
	if after.Header().Get("ETag") == tag {
		t.Error("the ETag did not change with the content")
	}
}

func TestTheETagFollowsTheContentAndNotTheClock(t *testing.T) {
	// Two files that differ only in content must not share a tag, and the same
	// content must keep its own across requests.
	handler, root := webServer(t)
	page := get(t, handler, "/index.html", nil).Header().Get("ETag")
	script := get(t, handler, "/assets/app.js", nil).Header().Get("ETag")
	if page == "" || page == script {
		t.Fatalf("index %q and script %q must have distinct tags", page, script)
	}
	if again := get(t, handler, "/index.html", nil).Header().Get("ETag"); again != page {
		t.Errorf("tag changed without the file changing: %q then %q", page, again)
	}
	// "/" and "/index.html" are the same file and must answer the same.
	if slash := get(t, handler, "/", nil).Header().Get("ETag"); slash != page {
		t.Errorf("/ = %q but /index.html = %q", slash, page)
	}
	_ = root
}

func TestAPathClimbingOutOfTheWebRootGetsNoTag(t *testing.T) {
	handler, _ := webServer(t)
	// The file server refuses these itself; this only makes sure the tag is not
	// computed from somewhere outside the web root on the way.
	response := get(t, handler, "/../../etc/passwd", nil)
	if response.Code == http.StatusOK && response.Header().Get("ETag") != "" {
		t.Errorf("served %d with a tag for a path outside the web root", response.Code)
	}
}
