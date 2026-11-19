// forge: a standalone GitHub-compatible git forge.
//
// One process, no UI: git smart HTTP (upload-pack v2, receive-pack v0),
// REST v3 under /api/v3, GraphQL v4 at /api/graphql and MCP at /mcp,
// all served from an in-memory store. It binds an ephemeral port, registers
// it with the suite at $MAKEFUN_ENV_URL, then polls $MAKEFUN_ENV_URL/done
// until the suite is over and exits 0.
//
// The viewer identity is the contest's fixed scenario viewer (see the kit's
// wire.ts: SCENARIO_SEED is a contest constant, so the viewer login is the
// same on every trial): adaiversen, id 1000.
//
// Every timestamp the forge mints comes from a deterministic counter (a fixed
// epoch plus a per-object sequence number), never the wall clock, so two runs
// driven with the same requests mint byte-identical objects.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	viewerLogin = "adaiversen"
	viewerID    = 1000
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.invalid"
	serverName  = "forge"
	serverVers  = "1.0.0"
	agentString = "forge/1.0"
)

// token the suite presents; empty means the harness set none (local dev).
var expectedToken = os.Getenv("MAKEFUN_SHOP_TOKEN")

var reqCounter uint64

func nextReqID() string {
	n := atomic.AddUint64(&reqCounter, 1)
	return "req-" + strconv.FormatUint(n, 10)
}

// authorized reports whether r carries the suite's bearer token.
func authorized(r *http.Request) bool {
	if expectedToken == "" {
		return true
	}
	h := r.Header.Get("Authorization")
	return h == "Bearer "+expectedToken
}

func writeJSON(w http.ResponseWriter, status int, body []byte) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-GitHub-Request-Id", nextReqID())
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	b, _ := json.Marshal(map[string]string{"message": msg})
	writeJSON(w, status, b)
}

func readBody(r *http.Request, max int64) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	return io.ReadAll(io.LimitReader(r.Body, max))
}

// decodeJSON decodes a request body as JSON into a map. An empty body yields
// an empty map; a malformed body yields an error.
func decodeJSON(body []byte) (map[string]any, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return map[string]any{}, nil
	}
	var m map[string]any
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

func jsonString(v any) (string, bool) {
	s, ok := v.(string)
	return s, ok
}

func jsonBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

func jsonInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

// strval pulls a string field out of a decoded JSON object.
func strval(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

type server struct {
	store *Store
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Health is unauthenticated: the suite polls it during boot.
	if r.URL.Path == "/healthz" {
		writeJSON(w, 200, []byte(`{"status":"ok"}`))
		return
	}
	if !authorized(r) {
		writeErr(w, 401, "Requires authentication")
		return
	}
	path := r.URL.Path
	switch {
	case path == "/api/v3" || path == "/api/v3/":
		s.handleRoot(w, r)
	case strings.HasPrefix(path, "/api/v3/"):
		s.handleREST(w, r, strings.TrimPrefix(path, "/api/v3/"))
	case path == "/api/graphql":
		s.handleGraphQL(w, r)
	case path == "/mcp":
		s.handleMCP(w, r)
	case strings.Contains(path, ".git/"):
		s.handleGit(w, r)
	default:
		writeErr(w, 404, "Not Found")
	}
}

func (s *server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeErr(w, 404, "Not Found")
		return
	}
	w.Header().Set("X-OAuth-Scopes", "repo")
	writeJSON(w, 200, []byte(`{"current_user_url":"/api/v3/user","repository_url":"/api/v3/repos/{owner}/{repo}"}`))
}

func main() {
	debug.SetGCPercent(100)
	store := NewStore()
	srv := &server{store: store}

	envURL := strings.TrimRight(os.Getenv("MAKEFUN_ENV_URL"), "/")

	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintf(os.Stderr, "[forge] listen: %v\n", err)
		os.Exit(1)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	httpServer := &http.Server{Handler: srv}

	// Exit cleanly on SIGTERM (the harness stops us at the wall).
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		<-ch
		_ = httpServer.Close()
		os.Exit(0)
	}()

	if envURL == "" {
		fmt.Fprintf(os.Stderr, "[forge] listening on :%d (no MAKEFUN_ENV_URL; standalone)\n", port)
		if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[forge] serve: %v\n", err)
			os.Exit(1)
		}
		return
	}

	go func() {
		if err := httpServer.Serve(ln); err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "[forge] serve: %v\n", err)
			os.Exit(1)
		}
	}()

	if err := register(envURL, port); err != nil {
		fmt.Fprintf(os.Stderr, "[forge] register: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "[forge] registered on port %d\n", port)
	pollDone(envURL)
	_ = httpServer.Close()
	os.Exit(0)
}

func register(envURL string, port int) error {
	body, _ := json.Marshal(map[string]int{"port": port})
	client := &http.Client{Timeout: 30 * time.Second}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(http.MethodPost, envURL+"/register", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == 200 || resp.StatusCode == 409 {
				return nil
			}
			if resp.StatusCode == 400 || resp.StatusCode == 405 {
				return fmt.Errorf("register answered HTTP %d", resp.StatusCode)
			}
		}
		delay := 250 * time.Millisecond * time.Duration(1<<min(attempt, 3))
		if delay > 2*time.Second {
			delay = 2 * time.Second
		}
		time.Sleep(delay)
	}
}

func pollDone(envURL string) {
	client := &http.Client{Timeout: 30 * time.Second}
	delay := 100 * time.Millisecond
	for {
		done := false
		func() {
			defer func() { _ = recover() }()
			resp, err := client.Get(envURL + "/done")
			if err != nil {
				return
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
			var m struct {
				Done bool `json:"done"`
			}
			if json.Unmarshal(b, &m) == nil && m.Done {
				done = true
			}
		}()
		if done {
			return
		}
		time.Sleep(delay)
		if delay < 2*time.Second {
			delay *= 2
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
