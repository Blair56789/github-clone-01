package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

// --- HTTP helpers ---------------------------------------------------------

// readBody reads a request body up to max bytes.
func readBody(r *http.Request, max int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r.Body, max+1))
}

// nextRequestID returns a deterministic request id (a monotonic counter).
func (s *Server) nextRequestID() string {
	n := atomic.AddUint64(&s.reqSeq, 1)
	return fmt.Sprintf("req-%016x", n)
}

// writeJSONBytes writes pre-marshaled JSON with the standard headers.
func (s *Server) writeJSONBytes(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-GitHub-Request-Id", s.nextRequestID())
	w.WriteHeader(status)
	w.Write(body)
}

// writeJSON marshals v and writes it.
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		s.writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.writeJSONBytes(w, status, body)
}

// writeErr writes a {"message": ...} error body.
func (s *Server) writeErr(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]string{"message": msg})
}

// authed reports whether the request carries the trial's bearer token. When
// MAKEFUN_SHOP_TOKEN is unset (local runs), any non-empty bearer is accepted.
func (s *Server) authed(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	token := strings.TrimSpace(h[len("Bearer "):])
	if token == "" {
		return false
	}
	if s.shopToken != "" {
		return token == s.shopToken
	}
	return true
}

// decodeJSONBody parses a request body as JSON into v. An empty body decodes
// as an empty object. Returns false (after writing 400) on malformed JSON.
func (s *Server) decodeJSONBody(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := readBody(r, 16<<20)
	if err != nil || int64(len(body)) > 16<<20 {
		s.writeErr(w, http.StatusBadRequest, "request body too large")
		return false
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return true // leave v at its zero value
	}
	if err := json.Unmarshal(body, v); err != nil {
		s.writeErr(w, http.StatusBadRequest, "malformed JSON body")
		return false
	}
	return true
}

// --- pagination -----------------------------------------------------------

type pageParams struct {
	page    int
	perPage int
}

func parsePage(r *http.Request) pageParams {
	p := pageParams{page: 1, perPage: 30}
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			p.page = n
		}
	}
	if v := r.URL.Query().Get("per_page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			p.perPage = n
			if p.perPage > 100 {
				p.perPage = 100
			}
		}
	}
	return p
}

// pageSlice returns the page's window of items plus whether a next page exists.
func pageSlice[T any](items []T, p pageParams) ([]T, bool) {
	start := (p.page - 1) * p.perPage
	if start >= len(items) {
		return nil, false
	}
	end := start + p.perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end], end < len(items)
}

// setLinkNext writes the Link header for the next page (absolute URL).
func (s *Server) setLinkNext(w http.ResponseWriter, r *http.Request, p pageParams, hasNext bool) {
	if !hasNext {
		return
	}
	q := r.URL.Query()
	q.Set("page", strconv.Itoa(p.page+1))
	q.Set("per_page", strconv.Itoa(p.perPage))
	next := "http://" + r.Host + r.URL.Path + "?" + q.Encode()
	w.Header().Set("Link", "<"+next+">; rel=\"next\"")
}

// --- JSON shapes ----------------------------------------------------------

func userJSON() map[string]any {
	return map[string]any{
		"login": viewerLogin,
		"id":    viewerID,
		"name":  viewerName,
		"type":  "User",
	}
}

func repoJSON(r *Repo) map[string]any {
	return map[string]any{
		"id":             r.ID,
		"name":           r.Name,
		"full_name":      r.Owner + "/" + r.Name,
		"owner":          userJSON(),
		"private":        r.Private,
		"description":    r.Description,
		"default_branch": r.DefaultBranch,
		"has_issues":     true,
		"created_at":     serverNowISO,
		"updated_at":     serverNowISO,
	}
}

// serverNowISO is set from the trial clock at startup (all timestamps are the
// trial's fixed now, never the wall clock, so replays are byte-identical).
var serverNowISO string

func labelJSON(l *Label) map[string]any {
	return map[string]any{"name": l.Name, "color": l.Color}
}

// issueJSON marshals an issue. When store is true (callers hold the write
// lock) the result is cached on the issue; readers under the read lock pass
// store=false and never write.
func issueJSON(s *Server, r *Repo, is *Issue, store bool) []byte {
	if is.cachedJSON != nil {
		return is.cachedJSON
	}
	labels := make([]any, 0, len(is.Labels))
	for _, name := range is.Labels {
		if l := r.Labels[name]; l != nil {
			labels = append(labels, labelJSON(l))
		} else {
			labels = append(labels, map[string]any{"name": name, "color": "ededed"})
		}
	}
	body, _ := json.Marshal(map[string]any{
		"id":         is.ID,
		"number":     is.Number,
		"title":      is.Title,
		"body":       is.Body,
		"state":      is.State,
		"labels":     labels,
		"user":       userJSON(),
		"created_at": serverNowISO,
		"updated_at": serverNowISO,
	})
	if store {
		is.cachedJSON = body
	}
	return body
}

func (s *Server) pullJSON(r *Repo, p *Pull) map[string]any {
	headSha := p.HeadSha
	if tip, ok := r.Refs["refs/heads/"+p.HeadRef]; ok {
		headSha = tip
	}
	baseSha := p.BaseSha
	if tip, ok := r.Refs["refs/heads/"+p.BaseRef]; ok {
		baseSha = tip
	}
	var mergedAt any
	if p.MergedAt != "" {
		mergedAt = p.MergedAt
	}
	var mergeSha any
	if p.MergeSha != "" {
		mergeSha = p.MergeSha
	}
	return map[string]any{
		"id":               p.ID,
		"number":           p.Number,
		"title":            p.Title,
		"body":             p.Body,
		"state":            p.State,
		"head":             map[string]any{"ref": p.HeadRef, "sha": headSha},
		"base":             map[string]any{"ref": p.BaseRef, "sha": baseSha},
		"merged":           p.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": mergeSha,
		"user":             userJSON(),
		"created_at":       serverNowISO,
		"updated_at":       serverNowISO,
	}
}

// --- REST router ----------------------------------------------------------

// handleAPIv3 routes /api/v3/... (auth required).
func (s *Server) handleAPIv3(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.writeErr(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v3")
	path = strings.TrimPrefix(path, "/")
	parts := []string{}
	if path != "" {
		parts = strings.Split(path, "/")
	}
	method := r.Method

	// Root and meta.
	if len(parts) == 0 {
		if method == "GET" {
			w.Header().Set("X-OAuth-Scopes", "repo, user, gist")
			s.writeJSON(w, http.StatusOK, map[string]any{})
			return
		}
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	switch parts[0] {
	case "meta":
		if method == "GET" && len(parts) == 1 {
			s.writeJSON(w, http.StatusOK, map[string]any{"installed_version": "1.0.0"})
			return
		}
	case "user":
		if len(parts) == 1 && method == "GET" {
			s.writeJSON(w, http.StatusOK, userJSON())
			return
		}
		if len(parts) == 2 && parts[1] == "repos" {
			if method == "GET" {
				s.listUserRepos(w, r)
				return
			}
			if method == "POST" {
				s.createRepo(w, r)
				return
			}
		}
	case "repos":
		if len(parts) >= 3 {
			s.handleRepoRoute(w, r, parts[1], parts[2], parts[3:])
			return
		}
	}
	s.writeErr(w, http.StatusNotFound, "Not Found")
}

// listUserRepos answers GET /api/v3/user/repos (paginated, creation order).
func (s *Server) listUserRepos(w http.ResponseWriter, r *http.Request) {
	p := parsePage(r)
	s.mu.RLock()
	list := make([]any, 0, len(s.repoList))
	for _, repo := range s.repoList {
		list = append(list, repoJSON(repo))
	}
	s.mu.RUnlock()
	window, hasNext := pageSlice(list, p)
	s.setLinkNext(w, r, p, hasNext)
	s.writeJSON(w, http.StatusOK, window)
}

// createRepo answers POST /api/v3/user/repos.
func (s *Server) createRepo(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Private     *bool  `json:"private"`
		Description string `json:"description"`
	}
	if !s.decodeJSONBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		s.writeErr(w, http.StatusUnprocessableEntity, "name is required")
		return
	}
	s.mu.Lock()
	if s.repos[repoKey(viewerLogin, body.Name)] != nil {
		s.mu.Unlock()
		s.writeErr(w, http.StatusUnprocessableEntity, "name already exists on this account")
		return
	}
	private := true
	if body.Private != nil {
		private = *body.Private
	}
	s.nextSeq++
	repo := newRepo(s.nextRepoID, body.Name, private, body.Description, s.nextSeq)
	s.nextRepoID++
	s.repos[repoKey(viewerLogin, body.Name)] = repo
	s.repoList = append(s.repoList, repo)
	resp := repoJSON(repo)
	s.mu.Unlock()
	s.writeJSON(w, http.StatusCreated, resp)
}

// handleRepoRoute routes /api/v3/repos/{owner}/{repo}/...
func (s *Server) handleRepoRoute(w http.ResponseWriter, r *http.Request, owner, repoName string, rest []string) {
	method := r.Method
	// The repo itself.
	if len(rest) == 0 {
		switch method {
		case "GET":
			s.mu.RLock()
			repo := s.getRepo(owner, repoName)
			var resp map[string]any
			if repo != nil {
				resp = repoJSON(repo)
			}
			s.mu.RUnlock()
			if repo == nil {
				s.writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			s.writeJSON(w, http.StatusOK, resp)
		case "PATCH":
			s.patchRepo(w, r, owner, repoName)
		case "DELETE":
			s.mu.Lock()
			repo := s.getRepo(owner, repoName)
			if repo == nil {
				s.mu.Unlock()
				s.writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			delete(s.repos, repoKey(owner, repoName))
			for i, rp := range s.repoList {
				if rp == repo {
					s.repoList = append(s.repoList[:i], s.repoList[i+1:]...)
					break
				}
			}
			s.mu.Unlock()
			w.Header().Set("X-GitHub-Request-Id", s.nextRequestID())
			w.WriteHeader(http.StatusNoContent)
		default:
			s.writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	switch rest[0] {
	case "branches":
		if len(rest) == 1 && method == "GET" {
			s.listBranches(w, r, owner, repoName)
			return
		}
		if len(rest) >= 2 && method == "GET" {
			s.getBranch(w, r, owner, repoName, strings.Join(rest[1:], "/"))
			return
		}
	case "commits":
		if len(rest) == 1 && method == "GET" {
			s.listCommits(w, r, owner, repoName)
			return
		}
	case "git":
		if len(rest) >= 2 && rest[1] == "refs" {
			if len(rest) == 2 && method == "POST" {
				s.createRef(w, r, owner, repoName)
				return
			}
			if len(rest) >= 3 && method == "GET" {
				s.getRef(w, r, owner, repoName, strings.Join(rest[2:], "/"))
				return
			}
		}
	case "contents":
		s.handleContents(w, r, owner, repoName, strings.Join(rest[1:], "/"))
		return
	case "labels":
		if len(rest) == 1 {
			if method == "POST" {
				s.createLabel(w, r, owner, repoName)
				return
			}
			if method == "GET" {
				s.listLabels(w, r, owner, repoName)
				return
			}
		}
	case "issues":
		s.handleIssues(w, r, owner, repoName, rest[1:])
		return
	case "pulls":
		s.handlePulls(w, r, owner, repoName, rest[1:])
		return
	}
	s.writeErr(w, http.StatusNotFound, "Not Found")
}

func (s *Server) patchRepo(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	var body struct {
		Description *string `json:"description"`
		Private     *bool   `json:"private"`
	}
	if !s.decodeJSONBody(w, r, &body) {
		return
	}
	s.mu.Lock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		s.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if body.Description != nil {
		repo.Description = *body.Description
	}
	if body.Private != nil {
		repo.Private = *body.Private
	}
	resp := repoJSON(repo)
	s.mu.Unlock()
	s.writeJSON(w, http.StatusOK, resp)
}

// --- branches -------------------------------------------------------------

func (s *Server) listBranches(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	s.mu.RLock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		s.mu.RUnlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	list := make([]any, 0)
	for _, name := range repo.refNames() {
		if strings.HasPrefix(name, "refs/heads/") {
			list = append(list, map[string]any{
				"name":   strings.TrimPrefix(name, "refs/heads/"),
				"commit": map[string]any{"sha": repo.Refs[name]},
			})
		}
	}
	s.mu.RUnlock()
	s.writeJSON(w, http.StatusOK, list)
}

func (s *Server) getBranch(w http.ResponseWriter, r *http.Request, owner, repoName, branch string) {
	s.mu.RLock()
	repo := s.getRepo(owner, repoName)
	var sha string
	var ok bool
	if repo != nil {
		sha, ok = repo.Refs["refs/heads/"+branch]
	}
	s.mu.RUnlock()
	if !ok {
		s.writeErr(w, http.StatusNotFound, "Branch not found")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"name":   branch,
		"commit": map[string]any{"sha": sha},
	})
}

// --- commits --------------------------------------------------------------

// resolveRef resolves a branch name or commit oid to a commit oid.
func resolveRef(repo *Repo, ref string) string {
	if ref == "" {
		return repo.Refs["refs/heads/"+repo.DefaultBranch]
	}
	if tip, ok := repo.Refs["refs/heads/"+ref]; ok {
		return tip
	}
	if tip, ok := repo.Refs[ref]; ok {
		return tip
	}
	if isOID(ref) {
		if obj := repo.Objects[ref]; obj != nil && obj.Type == "commit" {
			return ref
		}
		// A tag oid peels to its commit.
		if obj := repo.Objects[ref]; obj != nil && obj.Type == "tag" {
			return repo.peel(ref)
		}
	}
	return ""
}

func (s *Server) listCommits(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	ref := r.URL.Query().Get("sha")
	s.mu.RLock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		s.mu.RUnlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	tip := resolveRef(repo, ref)
	if tip == "" {
		s.mu.RUnlock()
		if ref == "" {
			// An unborn repo has no commits: an empty list, not a 404.
			s.writeJSON(w, http.StatusOK, []any{})
			return
		}
		s.writeErr(w, http.StatusNotFound, "No such ref")
		return
	}
	// Depth-first walk from the tip, newest first.
	list := make([]any, 0)
	seen := map[string]bool{}
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		c := repo.parseCommit(oid)
		if c == nil {
			continue
		}
		parents := make([]any, 0, len(c.Parents))
		for _, p := range c.Parents {
			parents = append(parents, map[string]any{"sha": p})
		}
		list = append(list, map[string]any{
			"sha": oid,
			"commit": map[string]any{
				"message":   c.Message,
				"author":    identJSON(c.Author),
				"committer": identJSON(c.Committer),
			},
			"parents": parents,
		})
		// Push parents in reverse so the first parent is visited first.
		for i := len(c.Parents) - 1; i >= 0; i-- {
			stack = append(stack, c.Parents[i])
		}
	}
	s.mu.RUnlock()
	s.writeJSON(w, http.StatusOK, list)
}

// identJSON parses "Name <email> ts tz" into GitHub's author shape.
func identJSON(ident string) map[string]any {
	name, email, date := ident, "", serverNowISO
	if lt := strings.Index(ident, "<"); lt >= 0 {
		name = strings.TrimSpace(ident[:lt])
		rest := ident[lt+1:]
		if gt := strings.Index(rest, ">"); gt >= 0 {
			email = rest[:gt]
			fields := strings.Fields(rest[gt+1:])
			if len(fields) >= 1 {
				if ts, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
					date = formatISO(ts)
				}
			}
		}
	}
	return map[string]any{"name": name, "email": email, "date": date}
}

func formatISO(epoch int64) string {
	// Format a unix timestamp as ISO-8601 UTC without importing time here.
	// (time is imported by store.go; this file uses the same package.)
	return timeFromEpoch(epoch)
}

// --- git refs -------------------------------------------------------------

func (s *Server) createRef(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	var body struct {
		Ref string `json:"ref"`
		Sha string `json:"sha"`
	}
	if !s.decodeJSONBody(w, r, &body) {
		return
	}
	if !strings.HasPrefix(body.Ref, "refs/") || !isOID(body.Sha) {
		s.writeErr(w, http.StatusUnprocessableEntity, "invalid ref or sha")
		return
	}
	s.mu.Lock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		s.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	obj := repo.Objects[body.Sha]
	if obj == nil {
		s.mu.Unlock()
		s.writeErr(w, http.StatusUnprocessableEntity, "object does not exist")
		return
	}
	repo.Refs[body.Ref] = body.Sha
	repo.bump()
	s.mu.Unlock()
	s.writeJSON(w, http.StatusCreated, map[string]any{
		"ref":    body.Ref,
		"object": map[string]any{"sha": body.Sha, "type": obj.Type},
	})
}

func (s *Server) getRef(w http.ResponseWriter, r *http.Request, owner, repoName, refSuffix string) {
	s.mu.RLock()
	repo := s.getRepo(owner, repoName)
	var sha, typ string
	var ok bool
	if repo != nil {
		ref := "refs/" + refSuffix
		sha, ok = repo.Refs[ref]
		if ok {
			if obj := repo.Objects[sha]; obj != nil {
				typ = obj.Type
			} else {
				typ = "commit"
			}
		}
		_ = ref
	}
	s.mu.RUnlock()
	if !ok {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ref":    "refs/" + refSuffix,
		"object": map[string]any{"sha": sha, "type": typ},
	})
}

// --- labels ---------------------------------------------------------------

func (s *Server) createLabel(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	var body struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if !s.decodeJSONBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		s.writeErr(w, http.StatusUnprocessableEntity, "name is required")
		return
	}
	if body.Color == "" {
		body.Color = "ededed"
	}
	s.mu.Lock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		s.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	label, ok := repo.Labels[body.Name]
	if !ok {
		label = &Label{Name: body.Name, Color: body.Color}
		repo.Labels[body.Name] = label
		repo.LabelOrder = append(repo.LabelOrder, body.Name)
	}
	resp := labelJSON(label)
	s.mu.Unlock()
	s.writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) listLabels(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	s.mu.RLock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		s.mu.RUnlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	list := make([]any, 0, len(repo.LabelOrder))
	for _, name := range repo.LabelOrder {
		list = append(list, labelJSON(repo.Labels[name]))
	}
	s.mu.RUnlock()
	s.writeJSON(w, http.StatusOK, list)
}

// --- issues ---------------------------------------------------------------

func (s *Server) handleIssues(w http.ResponseWriter, r *http.Request, owner, repoName string, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case "GET":
			s.listIssues(w, r, owner, repoName)
		case "POST":
			s.createIssueREST(w, r, owner, repoName)
		default:
			s.writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	number, err := strconv.Atoi(rest[0])
	if err != nil || number < 1 {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(rest) == 1 {
		switch r.Method {
		case "GET":
			s.getIssue(w, r, owner, repoName, number)
		case "PATCH":
			s.patchIssue(w, r, owner, repoName, number)
		default:
			s.writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	if len(rest) == 2 && rest[1] == "comments" {
		switch r.Method {
		case "GET":
			s.listComments(w, r, owner, repoName, number)
		case "POST":
			s.createComment(w, r, owner, repoName, number)
		default:
			s.writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	s.writeErr(w, http.StatusNotFound, "Not Found")
}

func (s *Server) createIssueREST(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	var body struct {
		Title  string   `json:"title"`
		Body   string   `json:"body"`
		Labels []string `json:"labels"`
	}
	if !s.decodeJSONBody(w, r, &body) {
		return
	}
	s.mu.Lock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		s.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	for _, name := range body.Labels {
		if repo.Labels[name] == nil {
			s.mu.Unlock()
			s.writeErr(w, http.StatusUnprocessableEntity, fmt.Sprintf("label %q does not exist", name))
			return
		}
	}
	issue := s.createIssueInternal(repo, body.Title, body.Body, body.Labels)
	resp := issueJSON(s, repo, issue, true)
	s.mu.Unlock()
	s.writeJSONBytes(w, http.StatusCreated, resp)
}

func (s *Server) listIssues(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	sortBy := q.Get("sort")
	direction := q.Get("direction")
	p := parsePage(r)
	s.mu.RLock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		s.mu.RUnlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	list := make([]*Issue, 0, len(repo.IssueOrder))
	for _, is := range repo.IssueOrder {
		if state != "all" && is.State != state {
			continue
		}
		list = append(list, is)
	}
	// sort=created + direction; default is creation order ascending.
	if sortBy == "created" && direction == "desc" {
		for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
			list[i], list[j] = list[j], list[i]
		}
	}
	window, hasNext := pageSlice(list, p)
	// Marshal under the read lock (issueJSON caches on the issue).
	raw := make([]json.RawMessage, 0, len(window))
	for _, is := range window {
		raw = append(raw, issueJSON(s, repo, is, false))
	}
	s.mu.RUnlock()
	s.setLinkNext(w, r, p, hasNext)
	body, _ := json.Marshal(raw)
	s.writeJSONBytes(w, http.StatusOK, body)
}

func (s *Server) getIssue(w http.ResponseWriter, r *http.Request, owner, repoName string, number int) {
	s.mu.RLock()
	repo := s.getRepo(owner, repoName)
	var resp []byte
	if repo != nil {
		if is := repo.Issues[number]; is != nil {
			resp = issueJSON(s, repo, is, false)
		}
	}
	s.mu.RUnlock()
	if resp == nil {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	s.writeJSONBytes(w, http.StatusOK, resp)
}

func (s *Server) patchIssue(w http.ResponseWriter, r *http.Request, owner, repoName string, number int) {
	var body struct {
		State  *string  `json:"state"`
		Title  *string  `json:"title"`
		Body   *string  `json:"body"`
		Labels []string `json:"labels"`
	}
	if !s.decodeJSONBody(w, r, &body) {
		return
	}
	s.mu.Lock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		s.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	is := repo.Issues[number]
	if is == nil {
		s.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if body.Labels != nil {
		for _, name := range body.Labels {
			if repo.Labels[name] == nil {
				s.mu.Unlock()
				s.writeErr(w, http.StatusUnprocessableEntity, fmt.Sprintf("label %q does not exist", name))
				return
			}
		}
		is.Labels = append([]string(nil), body.Labels...)
	}
	if body.State != nil && (*body.State == "open" || *body.State == "closed") {
		is.State = *body.State
	}
	if body.Title != nil {
		is.Title = *body.Title
	}
	if body.Body != nil {
		is.Body = *body.Body
	}
	is.cachedJSON = nil
	resp := issueJSON(s, repo, is, true)
	s.mu.Unlock()
	s.writeJSONBytes(w, http.StatusOK, resp)
}

func commentJSON(c *Comment) map[string]any {
	return map[string]any{
		"id":         c.ID,
		"body":       c.Body,
		"user":       userJSON(),
		"created_at": serverNowISO,
		"updated_at": serverNowISO,
	}
}

func (s *Server) createComment(w http.ResponseWriter, r *http.Request, owner, repoName string, number int) {
	var body struct {
		Body string `json:"body"`
	}
	if !s.decodeJSONBody(w, r, &body) {
		return
	}
	s.mu.Lock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		s.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	is := repo.Issues[number]
	if is == nil {
		s.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	comment := s.addCommentInternal(is, body.Body)
	resp := commentJSON(comment)
	s.mu.Unlock()
	s.writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) listComments(w http.ResponseWriter, r *http.Request, owner, repoName string, number int) {
	s.mu.RLock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		s.mu.RUnlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	is := repo.Issues[number]
	if is == nil {
		s.mu.RUnlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	list := make([]any, 0, len(is.Comments))
	for _, c := range is.Comments {
		list = append(list, commentJSON(c))
	}
	s.mu.RUnlock()
	s.writeJSON(w, http.StatusOK, list)
}

// --- pull requests --------------------------------------------------------

func (s *Server) handlePulls(w http.ResponseWriter, r *http.Request, owner, repoName string, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case "GET":
			s.listPulls(w, r, owner, repoName)
		case "POST":
			s.createPullREST(w, r, owner, repoName)
		default:
			s.writeErr(w, http.StatusNotFound, "Not Found")
		}
		return
	}
	number, err := strconv.Atoi(rest[0])
	if err != nil || number < 1 {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(rest) == 1 {
		if r.Method == "GET" {
			s.getPull(w, r, owner, repoName, number)
			return
		}
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if len(rest) == 2 && rest[1] == "files" && r.Method == "GET" {
		s.listPullFiles(w, r, owner, repoName, number)
		return
	}
	if len(rest) == 2 && rest[1] == "merge" && r.Method == "PUT" {
		s.mergePullREST(w, r, owner, repoName, number)
		return
	}
	s.writeErr(w, http.StatusNotFound, "Not Found")
}

func (s *Server) createPullREST(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	var body struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Head  string `json:"head"`
		Base  string `json:"base"`
	}
	if !s.decodeJSONBody(w, r, &body) {
		return
	}
	if body.Head == "" || body.Base == "" {
		s.writeErr(w, http.StatusUnprocessableEntity, "head and base are required")
		return
	}
	s.mu.Lock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		s.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	pull, errMsg := s.createPullInternal(repo, body.Title, body.Body, body.Head, body.Base)
	if errMsg != "" {
		s.mu.Unlock()
		s.writeErr(w, http.StatusUnprocessableEntity, errMsg)
		return
	}
	resp := s.pullJSON(repo, pull)
	s.mu.Unlock()
	s.writeJSON(w, http.StatusCreated, resp)
}

func (s *Server) listPulls(w http.ResponseWriter, r *http.Request, owner, repoName string) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	p := parsePage(r)
	s.mu.RLock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		s.mu.RUnlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	list := make([]any, 0)
	for _, pr := range repo.PullOrder {
		if state != "all" && pr.State != state {
			continue
		}
		list = append(list, s.pullJSON(repo, pr))
	}
	s.mu.RUnlock()
	window, hasNext := pageSlice(list, p)
	s.setLinkNext(w, r, p, hasNext)
	s.writeJSON(w, http.StatusOK, window)
}

func (s *Server) getPull(w http.ResponseWriter, r *http.Request, owner, repoName string, number int) {
	s.mu.RLock()
	repo := s.getRepo(owner, repoName)
	var resp map[string]any
	if repo != nil {
		if pr := repo.Pulls[number]; pr != nil {
			resp = s.pullJSON(repo, pr)
		}
	}
	s.mu.RUnlock()
	if resp == nil {
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// pullTrees returns the base and head tip commits' trees for a PR.
func pullTrees(repo *Repo, pr *Pull) (baseTree, headTree string, ok bool) {
	baseTip := repo.Refs["refs/heads/"+pr.BaseRef]
	headTip := repo.Refs["refs/heads/"+pr.HeadRef]
	if baseTip == "" || headTip == "" {
		return "", "", false
	}
	bc := repo.parseCommit(baseTip)
	hc := repo.parseCommit(headTip)
	if bc == nil || hc == nil {
		return "", "", false
	}
	return bc.Tree, hc.Tree, true
}

func (s *Server) listPullFiles(w http.ResponseWriter, r *http.Request, owner, repoName string, number int) {
	s.mu.RLock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		s.mu.RUnlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	pr := repo.Pulls[number]
	if pr == nil {
		s.mu.RUnlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	baseTree, headTree, ok := pullTrees(repo, pr)
	if !ok {
		s.mu.RUnlock()
		s.writeJSON(w, http.StatusOK, []any{})
		return
	}
	changes := diffTrees(repo, baseTree, headTree)
	list := make([]any, 0, len(changes))
	for _, ch := range changes {
		var oldData, newData []byte
		if ch.OldBlobOID != "" {
			if obj := repo.Objects[ch.OldBlobOID]; obj != nil {
				oldData = obj.Data
			}
		}
		if ch.NewBlobOID != "" {
			if obj := repo.Objects[ch.NewBlobOID]; obj != nil {
				newData = obj.Data
			}
		}
		additions, deletions := lineDiff(oldData, newData)
		if ch.Status == "added" && additions == 0 {
			additions = 1
		}
		list = append(list, map[string]any{
			"filename":  ch.Path,
			"status":    ch.Status,
			"additions": additions,
			"deletions": deletions,
			"changes":   additions + deletions,
		})
	}
	s.mu.RUnlock()
	s.writeJSON(w, http.StatusOK, list)
}

// mergePull performs the squash merge of a PR. Callers hold the write lock.
// Returns the merge commit sha, or an error with (status, message).
func (s *Server) mergePull(repo *Repo, pr *Pull) (string, int, string) {
	if pr.State != "open" {
		return "", http.StatusMethodNotAllowed, "pull request is not open"
	}
	baseTip := repo.Refs["refs/heads/"+pr.BaseRef]
	headTip := repo.Refs["refs/heads/"+pr.HeadRef]
	if baseTip == "" || headTip == "" {
		return "", http.StatusUnprocessableEntity, "head or base branch is missing"
	}
	baseCommit := repo.parseCommit(baseTip)
	headCommit := repo.parseCommit(headTip)
	if baseCommit == nil || headCommit == nil {
		return "", http.StatusUnprocessableEntity, "head or base tip is not a commit"
	}
	base := mergeBase(repo, baseTip, headTip)
	baseTree := ""
	if base != "" {
		if bc := repo.parseCommit(base); bc != nil {
			baseTree = bc.Tree
		}
	}
	// Three-way conflict check: a path changed on both sides (vs the merge
	// base) to different contents conflicts.
	headChanges := diffTrees(repo, baseTree, headCommit.Tree)
	baseChanges := diffTrees(repo, baseTree, baseCommit.Tree)
	baseChanged := map[string]string{}
	for _, ch := range baseChanges {
		baseChanged[ch.Path] = ch.NewBlobOID
	}
	for _, ch := range headChanges {
		if boid, ok := baseChanged[ch.Path]; ok && boid != ch.NewBlobOID {
			return "", http.StatusMethodNotAllowed, "merge conflict"
		}
	}
	// Squash: the base tree plus the head's changes.
	merged := flattenTree(repo, baseCommit.Tree)
	for _, ch := range headChanges {
		if ch.Status == "removed" {
			delete(merged, ch.Path)
		} else {
			merged[ch.Path] = ch.NewBlobOID
		}
	}
	newTree := buildTreeFromMap(repo, merged)
	msg := fmt.Sprintf("Merge pull request #%d from %s\n\n%s\n", pr.Number, pr.HeadRef, pr.Title)
	commitData := buildCommitBytes(newTree, []string{baseTip},
		viewerName+" <"+viewerEmail+">", viewerName+" <"+viewerEmail+">",
		s.trialEpoch, msg)
	commitOID := hashObject("commit", commitData)
	repo.Objects[commitOID] = &Object{Type: "commit", Data: commitData}
	repo.Refs["refs/heads/"+pr.BaseRef] = commitOID
	pr.State = "closed"
	pr.Merged = true
	pr.MergedAt = serverNowISO
	pr.MergeSha = commitOID
	repo.bump()
	return commitOID, 0, ""
}

func (s *Server) mergePullREST(w http.ResponseWriter, r *http.Request, owner, repoName string, number int) {
	var body struct {
		MergeMethod string `json:"merge_method"`
	}
	// The body is optional; a malformed body is still a 400.
	raw, _ := readBody(r, 1<<20)
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			s.writeErr(w, http.StatusBadRequest, "malformed JSON body")
			return
		}
	}
	s.mu.Lock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		s.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	pr := repo.Pulls[number]
	if pr == nil {
		s.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	sha, status, msg := s.mergePull(repo, pr)
	s.mu.Unlock()
	if status != 0 {
		s.writeErr(w, status, msg)
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{
		"sha":     sha,
		"merged":  true,
		"message": "Pull Request successfully merged",
	})
}

// --- contents -------------------------------------------------------------

// buildTreeFromMap builds (and stores) the tree objects for a flat path map.
func buildTreeFromMap(repo *Repo, files map[string]string) string {
	type node struct {
		files map[string]string
		dirs  map[string]*node
	}
	mk := func() *node { return &node{files: map[string]string{}, dirs: map[string]*node{}} }
	root := mk()
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		segments := strings.Split(p, "/")
		n := root
		for _, seg := range segments[:len(segments)-1] {
			child, ok := n.dirs[seg]
			if !ok {
				child = mk()
				n.dirs[seg] = child
			}
			n = child
		}
		n.files[segments[len(segments)-1]] = files[p]
	}
	var build func(n *node) string
	build = func(n *node) string {
		entries := make([]TreeEntry, 0, len(n.files)+len(n.dirs))
		for name, oid := range n.files {
			entries = append(entries, TreeEntry{Mode: "100644", Name: name, OID: oid})
		}
		for name, child := range n.dirs {
			entries = append(entries, TreeEntry{Mode: "40000", Name: name, OID: build(child)})
		}
		data := buildTreeBytes(entries)
		oid := hashObject("tree", data)
		if repo.Objects[oid] == nil {
			repo.Objects[oid] = &Object{Type: "tree", Data: data}
		}
		return oid
	}
	return build(root)
}

// commitContents creates a commit on branch with files as the new tree state.
// tip is the branch's current tip ("" when unborn). Returns the commit oid.
func (s *Server) commitContents(repo *Repo, branch, message string, files map[string]string, tip string) string {
	treeOID := buildTreeFromMap(repo, files)
	var parents []string
	if tip != "" {
		parents = []string{tip}
	}
	if !strings.HasSuffix(message, "\n") {
		message += "\n"
	}
	ident := viewerName + " <" + viewerEmail + ">"
	data := buildCommitBytes(treeOID, parents, ident, ident, s.trialEpoch, message)
	oid := hashObject("commit", data)
	repo.Objects[oid] = &Object{Type: "commit", Data: data}
	repo.Refs["refs/heads/"+branch] = oid
	repo.bump()
	return oid
}

func (s *Server) handleContents(w http.ResponseWriter, r *http.Request, owner, repoName, filePath string) {
	switch r.Method {
	case "PUT":
		s.putContents(w, r, owner, repoName, filePath)
	case "GET":
		s.getContents(w, r, owner, repoName, filePath)
	case "DELETE":
		s.deleteContents(w, r, owner, repoName, filePath)
	default:
		s.writeErr(w, http.StatusNotFound, "Not Found")
	}
}

func (s *Server) putContents(w http.ResponseWriter, r *http.Request, owner, repoName, filePath string) {
	var body struct {
		Message string `json:"message"`
		Content string `json:"content"`
		Branch  string `json:"branch"`
		Sha     string `json:"sha"`
	}
	if !s.decodeJSONBody(w, r, &body) {
		return
	}
	if filePath == "" || body.Message == "" {
		s.writeErr(w, http.StatusUnprocessableEntity, "path and message are required")
		return
	}
	raw := strings.Map(func(c rune) rune {
		if c == '\n' || c == '\r' || c == '\t' || c == ' ' {
			return -1
		}
		return c
	}, body.Content)
	content, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		s.writeErr(w, http.StatusUnprocessableEntity, "content is not valid base64")
		return
	}
	s.mu.Lock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		s.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	branch := body.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	tip, exists := repo.Refs["refs/heads/"+branch]
	if !exists {
		// Creating a file on a missing branch creates the branch from the
		// default branch's tip (GitHub's contents-API behaviour).
		tip = repo.Refs["refs/heads/"+repo.DefaultBranch]
	}
	files := map[string]string{}
	if tip != "" {
		if c := repo.parseCommit(tip); c != nil {
			files = flattenTree(repo, c.Tree)
		}
	}
	blobOID := hashObject("blob", content)
	if repo.Objects[blobOID] == nil {
		repo.Objects[blobOID] = &Object{Type: "blob", Data: content}
	}
	current, fileExists := files[filePath]
	status := http.StatusCreated
	if fileExists {
		if body.Sha == "" || !strings.EqualFold(body.Sha, current) {
			s.mu.Unlock()
			s.writeErr(w, http.StatusConflict, "sha does not match the current file")
			return
		}
		status = http.StatusOK
	}
	files[filePath] = blobOID
	commitOID := s.commitContents(repo, branch, body.Message, files, tip)
	encoded := base64.StdEncoding.EncodeToString(content)
	name := filePath
	if idx := strings.LastIndex(filePath, "/"); idx >= 0 {
		name = filePath[idx+1:]
	}
	resp := map[string]any{
		"content": map[string]any{
			"name":     name,
			"path":     filePath,
			"sha":      blobOID,
			"size":     len(content),
			"encoding": "base64",
			"content":  encoded,
		},
		"commit": map[string]any{"sha": commitOID, "message": body.Message},
	}
	s.mu.Unlock()
	s.writeJSON(w, status, resp)
}

func (s *Server) getContents(w http.ResponseWriter, r *http.Request, owner, repoName, filePath string) {
	ref := r.URL.Query().Get("ref")
	s.mu.RLock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		s.mu.RUnlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	tip := resolveRef(repo, ref)
	if tip == "" {
		s.mu.RUnlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	commit := repo.parseCommit(tip)
	if commit == nil {
		s.mu.RUnlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	// Walk the tree by path segments.
	treeOID := commit.Tree
	if filePath != "" {
		segments := strings.Split(filePath, "/")
		for i, seg := range segments {
			entries := repo.parseTree(treeOID)
			var found *TreeEntry
			for j := range entries {
				if entries[j].Name == seg {
					found = &entries[j]
					break
				}
			}
			if found == nil {
				s.mu.RUnlock()
				s.writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			if i == len(segments)-1 {
				if found.Mode == "40000" {
					// Directory listing.
					list := make([]any, 0)
					for _, e := range repo.parseTree(found.OID) {
						typ := "file"
						if e.Mode == "40000" {
							typ = "dir"
						}
						list = append(list, map[string]any{
							"type": typ, "name": e.Name,
							"path": filePath + "/" + e.Name, "sha": e.OID,
						})
					}
					s.mu.RUnlock()
					s.writeJSON(w, http.StatusOK, list)
					return
				}
				obj := repo.Objects[found.OID]
				if obj == nil {
					s.mu.RUnlock()
					s.writeErr(w, http.StatusNotFound, "Not Found")
					return
				}
				name := seg
				resp := map[string]any{
					"type":     "file",
					"encoding": "base64",
					"content":  base64.StdEncoding.EncodeToString(obj.Data),
					"sha":      found.OID,
					"size":     len(obj.Data),
					"name":     name,
					"path":     filePath,
				}
				s.mu.RUnlock()
				s.writeJSON(w, http.StatusOK, resp)
				return
			}
			if found.Mode != "40000" {
				s.mu.RUnlock()
				s.writeErr(w, http.StatusNotFound, "Not Found")
				return
			}
			treeOID = found.OID
		}
	}
	// No path: the root listing.
	list := make([]any, 0)
	for _, e := range repo.parseTree(treeOID) {
		typ := "file"
		if e.Mode == "40000" {
			typ = "dir"
		}
		list = append(list, map[string]any{"type": typ, "name": e.Name, "path": e.Name, "sha": e.OID})
	}
	s.mu.RUnlock()
	s.writeJSON(w, http.StatusOK, list)
}

func (s *Server) deleteContents(w http.ResponseWriter, r *http.Request, owner, repoName, filePath string) {
	var body struct {
		Message string `json:"message"`
		Sha     string `json:"sha"`
		Branch  string `json:"branch"`
	}
	if !s.decodeJSONBody(w, r, &body) {
		return
	}
	s.mu.Lock()
	repo := s.getRepo(owner, repoName)
	if repo == nil {
		s.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	branch := body.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}
	tip := repo.Refs["refs/heads/"+branch]
	if tip == "" {
		s.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	commit := repo.parseCommit(tip)
	if commit == nil {
		s.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	files := flattenTree(repo, commit.Tree)
	current, ok := files[filePath]
	if !ok {
		s.mu.Unlock()
		s.writeErr(w, http.StatusNotFound, "Not Found")
		return
	}
	if body.Sha != "" && !strings.EqualFold(body.Sha, current) {
		s.mu.Unlock()
		s.writeErr(w, http.StatusConflict, "sha does not match the current file")
		return
	}
	delete(files, filePath)
	commitOID := s.commitContents(repo, branch, body.Message, files, tip)
	s.mu.Unlock()
	s.writeJSON(w, http.StatusOK, map[string]any{
		"commit": map[string]any{"sha": commitOID, "message": body.Message},
	})
}
