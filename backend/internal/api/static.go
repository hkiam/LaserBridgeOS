package api

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// The web interface is replaced wholesale by a system update, and the browser
// has to notice. Until this was here it did not, in two ways that compounded:
//
// http.FileServer sends Last-Modified and nothing else. With no Cache-Control a
// browser is free to invent a freshness lifetime from the age of the file -
// Chrome uses a tenth of it - and serve its copy without asking. That is how an
// appliance kept handing out the previous page after an update, while Safari,
// which had never seen it, showed the new one.
//
// And the obvious fix, telling it to revalidate, would not have worked either.
// The image build gives every file the same fixed timestamp so that builds are
// reproducible, so Last-Modified is identical in every version this appliance
// will ever run. A conditional request would have been answered "not modified"
// forever, and the browser would have kept the old page with our blessing.
//
// So the validator has to come from the content. http.ServeContent, which the
// file server uses underneath, honours an ETag already in the header and
// answers If-None-Match against it - an unchanged asset becomes a 304, a
// changed one a 200 carrying the new bytes.
//
// no-cache rather than no-store: the browser may keep its copy, it just has to
// ask first. On a workshop LAN that is one round trip per file per load.

// maxHashedAsset bounds what will be read to be hashed. The web interface is a
// handful of small files; anything larger has been put in the web root by
// mistake and should not be read on every request.
const maxHashedAsset = 4 << 20

// staticFiles serves the web interface so that a page from a previous version
// cannot outlive it.
func (s *Server) staticFiles() http.Handler {
	files := http.FileServer(http.Dir(s.WebRoot))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		if tag := assetETag(s.WebRoot, r.URL.Path); tag != "" {
			w.Header().Set("ETag", tag)
		}
		files.ServeHTTP(w, r)
	})
}

// assetETag names the file this request would be served, by its content. An
// empty answer means "no opinion" - a directory, a missing file, something too
// large to hash - and leaves the file server to answer as it did before.
//
// The content is hashed on every request rather than remembered. The first
// version of this cached the hash against the file's size and timestamp, which
// is wrong in exactly the situation it exists for: an update replaces a file
// whose timestamp is frozen by the reproducible build, and a replacement of the
// same length would have kept the old tag and the old page. A test caught it.
// The web interface is some tens of kilobytes, and this appliance serves a
// handful of requests a minute; there was nothing here worth optimising.
func assetETag(root, urlPath string) string {
	if root == "" {
		return ""
	}
	// Cleaned against a leading slash first, so a path trying to climb out of
	// the web root cannot be turned into one that does. The file server does its
	// own containment; this only decides what to read.
	name := path.Clean("/" + strings.TrimPrefix(urlPath, "/"))
	if strings.HasSuffix(urlPath, "/") || name == "/" {
		name = path.Join(name, "index.html")
	}
	full := filepath.Join(root, filepath.FromSlash(name))
	info, err := os.Stat(full)
	if err != nil || info.IsDir() || info.Size() > maxHashedAsset {
		return ""
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}
