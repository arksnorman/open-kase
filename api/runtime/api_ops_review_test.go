package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// slaOffReopen leaves #1 open with a closed SLA cycle 1 and no current
// cycle: SLA on at creation, off while the ticket is resolved and reopened,
// then on again.
func (f *slaFixture) slaOffReopen() {
	f.t.Helper()
	f.putPolicy()
	f.createTicket(f.dana.Access, map[string]any{"priority": 1})
	off := testPolicy()
	off["active"] = false
	f.do("PUT", "/api/v1/sla", off, bearer(f.admin.Access), ifMatchV(1)).expect(200)
	f.runDue() // the stop job ends cycle 1 (no longer current)
	f.do("POST", tpath("1", "/transitions"), map[string]any{"action": "resolve", "reason": "fixed"},
		bearer(f.dana.Access), ifMatchV(f.version("1"))).expect(200)
	f.do("POST", tpath("1", "/transitions"), map[string]any{"action": "reopen"},
		bearer(f.dana.Access), ifMatchV(f.version("1"))).expect(200)
	if s := f.get(f.admin.Access, "1").expect(200).JSON()["sla"]; s != nil {
		f.t.Fatalf("reopened with SLA off still has a current cycle: %v", s)
	}
	f.do("PUT", "/api/v1/sla", testPolicy(), bearer(f.admin.Access), ifMatchV(2)).expect(200)
}

// OC-OPS-02: the backfill inserted cycle 1 again for a ticket whose closed
// cycle 1 already existed; the duplicate-key error was swallowed inside the
// transaction, so the commit failed as transient and the driver re-ran the
// callback until its 120 s limit.
func TestOpsSLABackfillAfterClosedCycleDoesNotHang(t *testing.T) {
	f := newSLAFixture(t)
	f.slaOffReopen()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	scope, err := f.c.Access.Scope(ctx, f.principal(t, f.admin))
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.c.Tickets.BackfillSLA(ctx, scope)
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("backfill took %s (err %v)", took, err)
	}
	if err != nil || res.Started != 1 {
		t.Fatalf("backfill started %d, want 1 (err %v)", res.Started, err)
	}
	tk := f.get(f.admin.Access, "1").expect(200).JSON()
	if s := slaOf(tk); s == nil || s["cycle"] != float64(2) {
		t.Fatalf("backfilled sla %v", s)
	}
}

// OC-OPS-02 (second path): with SLA on again, resolving and reopening the
// same ticket tried to start cycle 1 again and failed with a duplicate key.
// Since SLA is first response only, a reopen starts no cycle at all.
func TestOpsSLAReopenAfterClosedCycle(t *testing.T) {
	f := newSLAFixture(t)
	f.slaOffReopen()
	f.do("POST", tpath("1", "/transitions"), map[string]any{"action": "resolve", "reason": "fixed again"},
		bearer(f.dana.Access), ifMatchV(f.version("1"))).expect(200)
	f.do("POST", tpath("1", "/transitions"), map[string]any{"action": "reopen"},
		bearer(f.dana.Access), ifMatchV(f.version("1"))).expect(200)
	if s := f.get(f.admin.Access, "1").expect(200).JSON()["sla"]; s != nil {
		t.Fatalf("reopened sla %v", s)
	}
}

// slowReader pauses after every chunk bytes, so a request body takes a
// while to arrive.
type slowReader struct {
	r     io.Reader
	chunk int
	pause time.Duration
	left  int
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.left <= 0 {
		time.Sleep(s.pause)
		s.left = s.chunk
	}
	if len(p) > s.left {
		p = p[:s.left]
	}
	n, err := s.r.Read(p)
	s.left -= n
	return n, err
}

// OC-OPS-04: the server's 60 s read/write timeouts covered whole attachment
// transfers: an upload or download slower than that (a 10 MiB file below
// ~1.4 Mbit/s; nginx is configured with proxy_buffering off) was cut off.
// The test serves the real router with 500 ms timeouts and transfers that
// take about 1.5 s.
func TestOpsAttachmentTransfersOutlastServerTimeouts(t *testing.T) {
	f := newTicketFixture(t)
	srv := httptest.NewUnstartedServer(f.h)
	srv.Config.ReadTimeout = 500 * time.Millisecond
	srv.Config.WriteTimeout = 500 * time.Millisecond
	srv.Start()
	defer srv.Close()
	// A small client receive buffer, so the server cannot hand the whole
	// download to the kernel and finish before the client reads.
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if tc, ok := conn.(*net.TCPConn); ok {
			_ = tc.SetReadBuffer(64 << 10)
		}
		return conn, err
	}}}

	content := bytes.Repeat([]byte("0123456789abcdef"), (8<<20)/16) // 8 MiB
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "large.bin")
	fw.Write(content)
	mw.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/api/v1/attachments",
		&slowReader{r: bytes.NewReader(body.Bytes()), chunk: 256 << 10, pause: 45 * time.Millisecond})
	req.ContentLength = int64(body.Len())
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+f.dana.Access)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("slow upload: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("slow upload: %d %s", resp.StatusCode, raw)
	}
	var att struct {
		Id string `json:"id"`
	}
	if err := json.Unmarshal(raw, &att); err != nil || att.Id == "" {
		t.Fatalf("upload response %s: %v", raw, err)
	}

	req, _ = http.NewRequest("GET", srv.URL+"/api/v1/attachments/"+att.Id+"/download", nil)
	req.Header.Set("Authorization", "Bearer "+f.dana.Access)
	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download: %d", resp.StatusCode)
	}
	time.Sleep(1500 * time.Millisecond) // a slow client
	got, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("slow download: %d of %d bytes, %v", len(got), len(content), err)
	}
}

// OC-OPS-07: init pointed at an initialized database with a missing key file
// generated a new master key there (and said to back it up) before finding
// that it does not match the installation, leaving a useless key file at the
// path the server reads.
func TestOpsInitDoesNotCreateAKeyForAnExistingInstallation(t *testing.T) {
	h := newHarness(t)
	keyPath := filepath.Join(t.TempDir(), "secrets", "master.key")
	t.Setenv("APP_ENV", "test")
	t.Setenv("MONGODB_URI", h.uri)
	t.Setenv("OPEN_KASE_MASTER_KEY_FILE", keyPath)
	t.Setenv("OPEN_KASE_LOG_LEVEL", "error")
	var out bytes.Buffer
	code := Execute(context.Background(), []string{"init"}, IO{In: devNull(t), Out: &out, Err: &out})
	if code == ExitOK {
		t.Fatalf("init succeeded without the installation's key: %s", out.String())
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("init created a master key for an existing installation (%v): %s", err, out.String())
	}
	if !strings.Contains(out.String(), "already holds an initialized installation") {
		t.Fatalf("init output: %s", out.String())
	}
}
