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
	"strings"
	"syscall"
	"time"
)

// timeFromEpoch formats a unix timestamp as ISO-8601 UTC.
func timeFromEpoch(epoch int64) string {
	return time.Unix(epoch, 0).UTC().Format("2006-01-02T15:04:05Z")
}

func main() {
	envURL := strings.TrimSuffix(os.Getenv("MAKEFUN_ENV_URL"), "/")
	if envURL == "" {
		fmt.Fprintln(os.Stderr, "forge: MAKEFUN_ENV_URL is not set")
		os.Exit(1)
	}
	trialNow := time.Date(2026, 11, 14, 6, 30, 0, 0, time.UTC)
	if raw := os.Getenv("MAKEFUN_TRIAL_NOW"); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			trialNow = t
		}
	}
	srv := newServer(os.Getenv("MAKEFUN_SHOP_TOKEN"), trialNow)
	serverNowISO = srv.trialNowISO

	listener, err := net.Listen("tcp", ":0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "forge: listen:", err)
		os.Exit(1)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	httpServer := &http.Server{
		Handler:           &forgeHandler{s: srv},
		ReadHeaderTimeout: 10 * time.Second,
	}
	go httpServer.Serve(listener)

	// Exit cleanly if the harness stops us.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		httpServer.Close()
		os.Exit(0)
	}()

	if err := register(envURL, port); err != nil {
		fmt.Fprintln(os.Stderr, "forge: register:", err)
		os.Exit(1)
	}
	waitDone(envURL)
	httpServer.Close()
	os.Exit(0)
}

// register posts the listening port to the env, retrying with backoff. 200 or
// 409 means registered; 400/405 is fatal; anything else is retried.
func register(envURL string, port int) error {
	body, _ := json.Marshal(map[string]any{"port": port})
	client := &http.Client{Timeout: 5 * time.Second}
	backoff := 100 * time.Millisecond
	deadline := time.Now().Add(15 * time.Second)
	for {
		req, err := http.NewRequest("POST", envURL+"/register", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusOK, http.StatusConflict:
				return nil
			case http.StatusBadRequest, http.StatusMethodNotAllowed:
				return fmt.Errorf("register rejected with status %d", resp.StatusCode)
			}
		}
		if time.Now().After(deadline) {
			if err != nil {
				return fmt.Errorf("register failed: %w", err)
			}
			return fmt.Errorf("register failed: status not accepted")
		}
		time.Sleep(backoff)
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}

// waitDone polls the env's /done endpoint until the trial is over.
func waitDone(envURL string) {
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		time.Sleep(500 * time.Millisecond)
		resp, err := client.Get(envURL + "/done")
		if err != nil {
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		var done struct {
			Done bool `json:"done"`
		}
		if json.Unmarshal(body, &done) == nil && done.Done {
			return
		}
	}
}

// forgeHandler routes every request to the git, REST, GraphQL or MCP handler.
type forgeHandler struct {
	s *Server
}

func (h *forgeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/healthz":
		if r.Method != "GET" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case path == "/api/v3" || strings.HasPrefix(path, "/api/v3/"):
		h.s.handleAPIv3(w, r)
	case path == "/api/graphql":
		h.s.handleGraphQL(w, r)
	case path == "/mcp":
		h.s.handleMCP(w, r)
	default:
		h.s.handleGit(w, r)
	}
}

// handleGit routes /<owner>/<repo>.git/<service>.
func (s *Server) handleGit(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.Split(path, "/")
	if len(parts) < 3 || !strings.HasSuffix(parts[1], ".git") {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	owner := parts[0]
	repoName := strings.TrimSuffix(parts[1], ".git")
	service := strings.Join(parts[2:], "/")
	switch {
	case service == "info/refs" && r.Method == "GET":
		s.handleGitInfoRefs(w, r, owner, repoName)
	case service == "git-upload-pack" && r.Method == "POST":
		s.handleGitUploadPack(w, r, owner, repoName)
	case service == "git-receive-pack" && r.Method == "POST":
		s.handleGitReceivePack(w, r, owner, repoName)
	default:
		s.writeErr(w, http.StatusNotFound, "Not Found")
	}
}
