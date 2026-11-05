package http

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/arksnorman/open-kase/api/package/fault"
)

// reservedPrefixes are never served by the SPA fallback.
var reservedPrefixes = []string{"/api", "/mcp", "/health"}

func isReserved(p string) bool {
	for _, pre := range reservedPrefixes {
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}

// spa serves a built single-page application from a directory: existing
// files are served as-is and other GET/HEAD paths fall back to index.html.
// API, MCP and health paths never fall through to the SPA.
//
// index.html is not cached for the process lifetime: it is re-read whenever
// its modification time or size changes, so a UI-only deploy (replacing the
// built files) is served without restarting the API.
type spa struct {
	root string

	mu      sync.Mutex
	index   []byte
	csp     string
	modTime time.Time
	size    int64
}

var inlineScript = regexp.MustCompile(`(?is)<script(?:\s[^>]*)?>(.*?)</script>`)

// newSPA validates dir and prepares index.html with a CSP that allows only
// same-origin resources plus the hashes of index.html's inline scripts.
func newSPA(dir string) (*spa, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	s := &spa{root: root}
	if _, _, err := s.current(); err != nil {
		return nil, fmt.Errorf("ui directory %s: %w", root, err)
	}
	return s, nil
}

// current returns index.html and its CSP, reloading them when the file
// changed on disk. If a reload fails (e.g. mid-deploy), the last good copy
// is served.
func (s *spa) current() ([]byte, string, error) {
	path := filepath.Join(s.root, "index.html")
	info, statErr := os.Stat(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	if statErr == nil && s.index != nil && info.ModTime().Equal(s.modTime) && info.Size() == s.size {
		return s.index, s.csp, nil
	}
	if statErr != nil {
		if s.index != nil {
			return s.index, s.csp, nil
		}
		return nil, "", statErr
	}
	index, err := os.ReadFile(path)
	if err != nil {
		if s.index != nil {
			return s.index, s.csp, nil
		}
		return nil, "", err
	}
	s.index, s.csp, s.modTime, s.size = index, cspFor(index), info.ModTime(), info.Size()
	return s.index, s.csp, nil
}

func cspFor(index []byte) string {
	var hashes []string
	for _, m := range inlineScript.FindAllSubmatch(index, -1) {
		tag := m[0][:strings.IndexByte(string(m[0]), '>')]
		if strings.Contains(strings.ToLower(string(tag)), "src=") || len(m[1]) == 0 {
			continue
		}
		sum := sha256.Sum256(m[1])
		hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	scriptSrc := strings.Join(append([]string{"'self'"}, hashes...), " ")
	return "default-src 'self'; script-src " + scriptSrc + "; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; " +
		"font-src 'self' data:; connect-src 'self'; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'"
}

// serve handles a request that matched no API route. It returns false when
// the request is not the SPA's to answer.
func (s *spa) serve(c *gin.Context) bool {
	if c.Request.Method != http.MethodGet && c.Request.Method != http.MethodHead {
		return false
	}
	clean := path.Clean("/" + c.Request.URL.Path)
	if isReserved(clean) {
		return false
	}
	index, csp, err := s.current()
	if err != nil {
		writeError(c, fault.Internal(err))
		return true
	}
	h := c.Writer.Header()
	h.Set("Content-Security-Policy", csp)
	if clean != "/" && clean != "/index.html" {
		full := filepath.Join(s.root, filepath.FromSlash(clean))
		if rel, err := filepath.Rel(s.root, full); err == nil && !strings.HasPrefix(rel, "..") {
			if info, err := os.Stat(full); err == nil && info.Mode().IsRegular() {
				if strings.HasPrefix(clean, "/assets/") {
					h.Set("Cache-Control", "public, max-age=31536000, immutable")
				} else {
					h.Set("Cache-Control", "no-cache")
				}
				http.ServeFile(c.Writer, c.Request, full)
				return true
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				writeError(c, fault.Internal(err))
				return true
			}
		}
		// A missing file with an extension is a genuine 404, not a route.
		if path.Ext(clean) != "" {
			return false
		}
	}
	h.Set("Cache-Control", "no-cache")
	c.Data(http.StatusOK, "text/html; charset=utf-8", index)
	return true
}
