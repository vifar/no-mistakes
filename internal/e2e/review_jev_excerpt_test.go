//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// jevRequest is the wire shape of one System One evaluation as the daemon
// sends it: the pre-brief state (change digest plus candidates) and the
// per-candidate questions. Only the fields the excerpt feature can change
// are decoded.
type jevRequest struct {
	State struct {
		Candidates []struct {
			Path    string  `json:"path"`
			Excerpt *string `json:"excerpt"`
		} `json:"candidates"`
	} `json:"state"`
	Questions map[string]struct {
		Instructions string `json:"instructions"`
	} `json:"questions"`
}

// typesafeInterceptor is an HTTPS proxy that terminates TLS for the pinned
// TypeSafe endpoint with a certificate the daemon trusts through
// SSL_CERT_FILE, records every evaluation request body verbatim, and answers
// each question as "relevant context" so the pre-brief lists every candidate.
// Nothing is forwarded: no change content leaves the machine.
type typesafeInterceptor struct {
	mu     sync.Mutex
	bodies [][]byte
}

func startTypesafeInterceptor(t *testing.T) *typesafeInterceptor {
	t.Helper()
	caPEM, cert := issueTypesafeCert(t)
	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0o644); err != nil {
		t.Fatalf("write ca: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	r := &typesafeInterceptor{}
	tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go r.serve(conn, tlsCfg)
		}
	}()
	t.Setenv("HTTPS_PROXY", "http://"+ln.Addr().String())
	t.Setenv("https_proxy", "http://"+ln.Addr().String())
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	t.Setenv("SSL_CERT_FILE", caPath)
	return r
}

func (r *typesafeInterceptor) serve(conn net.Conn, tlsCfg *tls.Config) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	req, err := http.ReadRequest(bufio.NewReader(conn))
	if err != nil || req.Method != http.MethodConnect || req.Host != jevEndpointHostPort {
		_, _ = conn.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	tlsConn := tls.Server(conn, tlsCfg)
	defer tlsConn.Close()
	reader := bufio.NewReader(tlsConn)
	for {
		inner, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		body, err := io.ReadAll(inner.Body)
		_ = inner.Body.Close()
		if err != nil {
			return
		}
		r.mu.Lock()
		r.bodies = append(r.bodies, body)
		r.mu.Unlock()
		var decoded jevRequest
		_ = json.Unmarshal(body, &decoded)
		answers := map[string]any{}
		for id := range decoded.Questions {
			answers[id] = map[string]any{
				"type":          "score",
				"score":         2.6,
				"probabilities": map[string]float64{"0": 0.05, "1": 0.15, "2": 0.5, "3": 0.3},
				"confidence":    0.9,
			}
		}
		payload, _ := json.Marshal(map[string]any{
			"model":   "jev-1.13.0",
			"answers": answers,
			"usage":   map[string]int{"input_tokens": 1234, "output_tokens": 0},
		})
		resp := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(payload), payload)
		if _, err := tlsConn.Write([]byte(resp)); err != nil {
			return
		}
	}
}

// requests returns the recorded evaluation bodies, decoded.
func (r *typesafeInterceptor) requests(t *testing.T) []jevRequest {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]jevRequest, 0, len(r.bodies))
	for _, body := range r.bodies {
		var decoded jevRequest
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("decode jev request: %v\n%s", err, body)
		}
		out = append(out, decoded)
	}
	return out
}

func (r *typesafeInterceptor) rawBodies() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.bodies...)
}

// issueTypesafeCert mints a throwaway CA and a leaf for api.typesafe.ai.
func issueTypesafeCert(t *testing.T) ([]byte, tls.Certificate) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "nm-e2e jev test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse ca: %v", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "api.typesafe.ai"},
		DNSNames:     []string{"api.typesafe.ai"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("leaf cert: %v", err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	return caPEM, tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}
}

const jevExcerptBudget = 40

// Siblings of widget/render.go that exercise each excerpt rule.
const (
	widgetUserContent  = "package widget\n\nfunc Header() string { return RenderWidget() + \"!\" }\n" // 59 bytes: cut at the last newline inside the budget
	widgetShortContent = "package widget\n"                                                           // 15 bytes: whole file fits
	widgetRuneContent  = "// ÄÖÜäöüÄÖÜäöüÄÖÜäöüÄÖÜäöüÄÖÜäöü one long line, no newline"                // multibyte, cut inside a rune without care
	widgetBinary       = "PK\x03\x04\x00\x00binary\x00payload\n"
)

// seedExcerptSiblings commits the widget package with one use site, a short
// sibling, a multibyte single-line sibling, a binary sibling, and a tracked
// symlink sibling pointing outside the repository, then publishes main.
func seedExcerptSiblings(t *testing.T, h *Harness) {
	t.Helper()
	h.CommitChange("main", "widget/render.go", "package widget\n\nfunc RenderWidget() string { return \"w\" }\n", "add widget renderer")
	h.CommitChange("main", "widget/user.go", widgetUserContent, "add widget user")
	h.CommitChange("main", "widget/doc.go", widgetShortContent, "add short sibling")
	h.CommitChange("main", "widget/notes.txt", widgetRuneContent, "add multibyte sibling")
	h.CommitChange("main", "widget/blob.bin", widgetBinary, "add binary sibling")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	link := filepath.Join(h.WorkDir, "widget", "latest")
	if err := os.Symlink("/etc/hostname", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "add", "widget/latest"); err != nil {
		t.Fatalf("git add symlink: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "commit", "-m", "add symlink sibling"); err != nil {
		t.Fatalf("git commit symlink: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push origin main: %v\n%s", err, out)
	}
}

func candidateExcerpt(req jevRequest, path string) (excerpt *string, found bool) {
	for _, c := range req.State.Candidates {
		if c.Path == path {
			return c.Excerpt, true
		}
	}
	return nil, false
}

func questionFor(req jevRequest, path string) string {
	for i, c := range req.State.Candidates {
		if c.Path == path {
			return req.Questions[fmt.Sprintf("ctx_%d", i)].Instructions
		}
	}
	return ""
}

// TestJevCandidateExcerptJourney drives the real daemon with the opt-in
// jev.candidate_excerpt_bytes and reads the exact evaluation request it sends
// to the (intercepted) TypeSafe endpoint: each candidate carries at most the
// configured leading bytes, cut at a line boundary and UTF-8 safe, binary and
// symlink siblings carry none, the question text tells Jev to judge from the
// excerpt, and the answered pre-brief still reaches the review prompt. With
// the field unset the request carries no excerpt at all, and a negative value
// fails the global config closed.
func TestJevCandidateExcerptJourney(t *testing.T) {
	t.Run("excerpts_on_request_carries_bounded_excerpts_per_rule", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{Agent: "claude", GlobalConfigExtra: fmt.Sprintf("jev:\n  review_assist: true\n  candidate_excerpt_bytes: %d\n", jevExcerptBudget)})
		t.Setenv("TYPESAFE_API_KEY", "nm-e2e-intercepted-key")
		proxy := startTypesafeInterceptor(t)
		seedExcerptSiblings(t, h)

		runID := runWidgetChange(t, h, "jev-excerpts-on")

		reqs := proxy.requests(t)
		if len(reqs) != 1 {
			t.Fatalf("daemon sent %d TypeSafe evaluation(s), want 1", len(reqs))
		}
		req := reqs[0]
		var paths []string
		for _, c := range req.State.Candidates {
			paths = append(paths, c.Path)
		}
		t.Logf("candidates: %v", paths)

		// Use site: 59 bytes, cut at the last newline inside the 40-byte budget.
		if ex, ok := candidateExcerpt(req, "widget/user.go"); !ok || ex == nil {
			t.Fatalf("widget/user.go excerpt missing (found=%v)", ok)
		} else if *ex != "package widget\n\n" {
			t.Errorf("widget/user.go excerpt = %q, want line-cut prefix %q", *ex, "package widget\n\n")
		}
		// Whole file within budget ships verbatim.
		if ex, ok := candidateExcerpt(req, "widget/doc.go"); !ok || ex == nil || *ex != widgetShortContent {
			t.Errorf("widget/doc.go excerpt = %v (found=%v), want whole file %q", ex, ok, widgetShortContent)
		}
		// Single multibyte line: hard cut, UTF-8 safe, within budget, a prefix.
		if ex, ok := candidateExcerpt(req, "widget/notes.txt"); !ok || ex == nil {
			t.Errorf("widget/notes.txt excerpt missing (found=%v)", ok)
		} else {
			if len(*ex) > jevExcerptBudget || len(*ex) == 0 || !utf8.ValidString(*ex) || !strings.HasPrefix(widgetRuneContent, *ex) {
				t.Errorf("widget/notes.txt excerpt = %q (%d bytes): want a non-empty valid UTF-8 prefix within %d bytes", *ex, len(*ex), jevExcerptBudget)
			}
			t.Logf("widget/notes.txt excerpt = %q (%d bytes)", *ex, len(*ex))
		}
		// Binary and symlink siblings are candidates with no excerpt key.
		for _, p := range []string{"widget/blob.bin", "widget/latest"} {
			ex, ok := candidateExcerpt(req, p)
			if !ok {
				t.Errorf("%s is not a candidate", p)
			} else if ex != nil {
				t.Errorf("%s carries an excerpt %q; want none", p, *ex)
			}
		}
		if _, ok := candidateExcerpt(req, "widget/render.go"); ok {
			t.Errorf("changed file widget/render.go listed as a candidate")
		}
		// The question basis follows the excerpt's presence.
		if q := questionFor(req, "widget/user.go"); !strings.Contains(q, "Judge from its path and its content excerpt") {
			t.Errorf("excerpted candidate question lacks the excerpt basis:\n%s", q)
		}
		if q := questionFor(req, "widget/blob.bin"); !strings.Contains(q, "Judge from its path and the change") {
			t.Errorf("path-only candidate question lacks the path basis:\n%s", q)
		}
		// The answered pre-brief reaches the reviewer.
		prompt := reviewPrompt(t, h)
		if !strings.Contains(prompt, jevPrebriefHeading) || !strings.Contains(prompt, "  - widget/user.go\n") {
			t.Errorf("review prompt lacks the pre-brief listing:\n%s", promptTail(prompt))
		}
		logs := reviewStepLog(t, h, runID)
		if strings.Contains(logs, "package widget") || strings.Contains(logs, "nm-e2e-intercepted-key") {
			t.Errorf("review step log leaks excerpt content or the key:\n%s", logs)
		}
		if i := strings.Index(prompt, jevPrebriefHeading); i >= 0 {
			t.Logf("review prompt pre-brief:\n%s", prompt[i:])
		}
		t.Logf("review step log:\n%s", logs)
		t.Logf("jev request body:\n%s", proxy.rawBodies()[0])
	})

	t.Run("pushed_ignore_patterns_still_exclude_candidates", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{Agent: "claude", GlobalConfigExtra: fmt.Sprintf("jev:\n  review_assist: true\n  candidate_excerpt_bytes: %d\n", jevExcerptBudget)})
		t.Setenv("TYPESAFE_API_KEY", "nm-e2e-intercepted-key")
		proxy := startTypesafeInterceptor(t)
		seedExcerptSiblings(t, h)
		pushMainRepoConfig(t, h, "ignore_patterns:\n  - '*.txt'\n  - 'widget/doc.go'\n")

		runWidgetChange(t, h, "jev-excerpts-ignored")

		reqs := proxy.requests(t)
		if len(reqs) != 1 {
			t.Fatalf("daemon sent %d TypeSafe evaluation(s), want 1", len(reqs))
		}
		for _, p := range []string{"widget/notes.txt", "widget/doc.go"} {
			if _, ok := candidateExcerpt(reqs[0], p); ok {
				t.Errorf("ignored path %s is still a candidate", p)
			}
		}
		if ex, ok := candidateExcerpt(reqs[0], "widget/user.go"); !ok || ex == nil {
			t.Errorf("widget/user.go excerpt missing (found=%v)", ok)
		}
		t.Logf("jev request body:\n%s", proxy.rawBodies()[0])
	})

	t.Run("per_request_ceiling_drops_lowest_coupling_excerpts_first", func(t *testing.T) {
		const budget = 9000
		h := NewHarness(t, SetupOpts{Agent: "claude", GlobalConfigExtra: fmt.Sprintf("jev:\n  review_assist: true\n  candidate_excerpt_bytes: %d\n", budget)})
		t.Setenv("TYPESAFE_API_KEY", "nm-e2e-intercepted-key")
		proxy := startTypesafeInterceptor(t)
		pad := strings.Repeat("// padding line to fill the excerpt budget\n", 250) // ~10.7 KiB
		h.CommitChange("main", "widget/render.go", "package widget\n\nfunc RenderWidget() string { return \"w\" }\n", "add widget renderer")
		h.CommitChange("main", "widget/user.go", widgetUserContent+pad, "add widget user")
		h.CommitChange("main", "widget/a.txt", pad, "add sibling a")
		h.CommitChange("main", "widget/b.txt", pad, "add sibling b")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if out, err := h.runGit(ctx, h.WorkDir, "push", "origin", "main"); err != nil {
			t.Fatalf("push origin main: %v\n%s", err, out)
		}

		runWidgetChange(t, h, "jev-excerpt-ceiling")

		reqs := proxy.requests(t)
		if len(reqs) != 1 {
			t.Fatalf("daemon sent %d TypeSafe evaluation(s), want 1", len(reqs))
		}
		total := 0
		for _, c := range reqs[0].State.Candidates {
			if c.Excerpt != nil {
				total += len(*c.Excerpt)
				t.Logf("%s excerpt: %d bytes", c.Path, len(*c.Excerpt))
			} else {
				t.Logf("%s excerpt: none", c.Path)
			}
		}
		if total > 16*1024 {
			t.Errorf("summed excerpts %d bytes exceed the 16 KiB ceiling", total)
		}
		if ex, ok := candidateExcerpt(reqs[0], "widget/user.go"); !ok || ex == nil || len(*ex) == 0 || len(*ex) > budget {
			t.Errorf("use site widget/user.go should keep its excerpt (found=%v ex=%v)", ok, ex != nil)
		}
		for _, p := range []string{"widget/a.txt", "widget/b.txt"} {
			if ex, ok := candidateExcerpt(reqs[0], p); !ok {
				t.Errorf("%s is not a candidate", p)
			} else if ex != nil {
				t.Errorf("%s (coupling 0) kept a %d-byte excerpt past the ceiling", p, len(*ex))
			}
		}
	})

	t.Run("excerpts_unset_request_stays_path_only", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{Agent: "claude", GlobalConfigExtra: jevReviewAssistOn})
		t.Setenv("TYPESAFE_API_KEY", "nm-e2e-intercepted-key")
		proxy := startTypesafeInterceptor(t)
		seedExcerptSiblings(t, h)

		runWidgetChange(t, h, "jev-excerpts-off")

		bodies := proxy.rawBodies()
		if len(bodies) != 1 {
			t.Fatalf("daemon sent %d TypeSafe evaluation(s), want 1", len(bodies))
		}
		if strings.Contains(string(bodies[0]), `"excerpt"`) || strings.Contains(string(bodies[0]), "content excerpt") {
			t.Errorf("path-only request carries excerpt material:\n%s", bodies[0])
		}
		reqs := proxy.requests(t)
		if _, ok := candidateExcerpt(reqs[0], "widget/user.go"); !ok {
			t.Errorf("widget/user.go is not a candidate in the path-only request")
		}
		if prompt := reviewPrompt(t, h); !strings.Contains(prompt, jevPrebriefHeading) {
			t.Errorf("path-only review prompt lacks the pre-brief:\n%s", promptTail(prompt))
		}
	})

	t.Run("negative_excerpt_bytes_fails_global_config_closed", func(t *testing.T) {
		h := NewHarness(t, SetupOpts{Agent: "claude", GlobalConfigExtra: "jev:\n  review_assist: true\n  candidate_excerpt_bytes: -1\n"})
		out, err := h.Run("init")
		const want = "jev.candidate_excerpt_bytes must be 0 (path-only candidates) or greater, got -1"
		if err == nil || !strings.Contains(out, want) {
			t.Errorf("nm init with a negative excerpt budget: err=%v want %q in output:\n%s", err, want, out)
		}
		t.Logf("nm init output:\n%s", out)
	})
}
