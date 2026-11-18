// Package main implements forge: a GitHub-core clone (git smart HTTP, REST v3,
// GraphQL v4, MCP) in one static Go binary, written from the kit's sealed spec.
package main

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// The sealed scenario's viewer (wire.ts SCENARIO_SEED is a kit constant, so the
// viewer login is the same in every trial: users[0] of buildScenario()).
const (
	viewerLogin = "adaiversen"
	viewerID    = 1000
	viewerName  = "ada iversen"
	viewerEmail = "adaiversen@example.invalid"
)

// Identity lines for commits the server itself creates (PUT /contents, merges).
func serverIdent(epoch int64) string {
	return fmt.Sprintf("%s <%s> %d +0000", viewerName, viewerEmail, epoch)
}

const zeroOID = "0000000000000000000000000000000000000000"

// Object is one git object: its type and exact bytes (no loose header).
type Object struct {
	Type string // commit | tree | blob | tag
	Data []byte
}

// TreeEntry is one parsed tree entry.
type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

// CommitView is a parsed commit object.
type CommitView struct {
	Tree      string
	Parents   []string
	Author    string
	Committer string
	Message   string
}

// TagView is a parsed annotated tag object.
type TagView struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

// Label is a repository label.
type Label struct {
	Name  string
	Color string
}

// Comment is an issue comment.
type Comment struct {
	ID  int64
	Body string
	Seq int64
}

// Issue is one issue. Number shares the repo's counter with pull requests
// (GitHub's numbering: issues and PRs draw from one sequence).
type Issue struct {
	ID         int64
	Number     int
	Title      string
	Body       string
	State      string // open | closed
	Labels     []string
	Seq        int64
	Comments   []*Comment
	cachedJSON []byte // marshaled REST JSON; nil means dirty
}

// Pull is one pull request.
type Pull struct {
	ID         int64
	Number     int
	Title      string
	Body       string
	State      string // open | closed
	HeadRef    string
	BaseRef    string
	HeadSha    string // tip captured at creation (fallback if the branch is gone)
	BaseSha    string
	Merged     bool
	MergedAt   string // "" until merged
	MergeSha   string // "" until merged
	Seq        int64
}

// Repo is one repository: its git objects and refs plus its issues/PRs/labels.
type Repo struct {
	ID            int64
	Name          string
	Owner         string
	Private       bool
	Description   string
	DefaultBranch string
	CreatedSeq    int64

	Objects map[string]*Object
	Refs    map[string]string // refname -> oid

	Labels     map[string]*Label
	LabelOrder []string
	Issues     map[int]*Issue
	IssueOrder []*Issue
	Pulls      map[int]*Pull
	PullOrder  []*Pull
	NextNumber int

	// version bumps on every mutation; caches key on it.
	Version uint64

	// parsed-object caches (cleared never: objects are immutable). sync.Map
	// because fetch/parse runs under the read lock with many goroutines.
	commitCache sync.Map // oid -> *CommitView
	treeCache   sync.Map // oid -> []TreeEntry
	tagCache    sync.Map // oid -> *TagView

	// fetch/ls-refs response caches, keyed by request shape + version.
	fetchCache  map[string][]byte
	lsrefsCache map[string][]byte
}

func newRepo(id int64, name string, private bool, description string, seq int64) *Repo {
	return &Repo{
		ID:            id,
		Name:          name,
		Owner:         viewerLogin,
		Private:       private,
		Description:   description,
		DefaultBranch: "main",
		CreatedSeq:    seq,
		Objects:       make(map[string]*Object),
		Refs:          make(map[string]string),
		Labels:        make(map[string]*Label),
		Issues:        make(map[int]*Issue),
		Pulls:         make(map[int]*Pull),
		NextNumber:    1,
		fetchCache:    make(map[string][]byte),
		lsrefsCache:   make(map[string][]byte),
	}
}

// bump invalidates the repo's response caches after a mutation.
func (r *Repo) bump() {
	r.Version++
	r.fetchCache = make(map[string][]byte)
	r.lsrefsCache = make(map[string][]byte)
}

// lookup returns the object for oid, or nil.
func (r *Repo) lookup(oid string) *Object { return r.Objects[oid] }

// parseCommit returns the cached parsed commit, or nil when absent/unparsable.
func (r *Repo) parseCommit(oid string) *CommitView {
	if v, ok := r.commitCache.Load(oid); ok {
		return v.(*CommitView)
	}
	obj := r.Objects[oid]
	if obj == nil || obj.Type != "commit" {
		return nil
	}
	c := parseCommitBytes(obj.Data)
	if c == nil {
		return nil
	}
	r.commitCache.Store(oid, c)
	return c
}

// parseTree returns the cached parsed tree, or nil when absent/unparsable.
func (r *Repo) parseTree(oid string) []TreeEntry {
	if v, ok := r.treeCache.Load(oid); ok {
		return v.([]TreeEntry)
	}
	obj := r.Objects[oid]
	if obj == nil || obj.Type != "tree" {
		return nil
	}
	t := parseTreeBytes(obj.Data)
	if t == nil {
		return nil
	}
	r.treeCache.Store(oid, t)
	return t
}

// parseTag returns the cached parsed tag, or nil when absent/unparsable.
func (r *Repo) parseTag(oid string) *TagView {
	if v, ok := r.tagCache.Load(oid); ok {
		return v.(*TagView)
	}
	obj := r.Objects[oid]
	if obj == nil || obj.Type != "tag" {
		return nil
	}
	t := parseTagBytes(obj.Data)
	if t == nil {
		return nil
	}
	r.tagCache.Store(oid, t)
	return t
}

// peel resolves a tag object (recursively) to the non-tag object it names.
func (r *Repo) peel(oid string) string {
	for i := 0; i < 16; i++ {
		obj := r.Objects[oid]
		if obj == nil || obj.Type != "tag" {
			return oid
		}
		t := r.parseTag(oid)
		if t == nil {
			return oid
		}
		oid = t.Object
	}
	return oid
}

// isAncestor reports whether anc is reachable from oid by following commit
// parents (oid itself counts). Tag tips peel to their commit first.
func (r *Repo) isAncestor(anc, oid string) bool {
	if anc == oid {
		return true
	}
	oid = r.peel(oid)
	if anc == oid {
		return true
	}
	seen := make(map[string]bool)
	queue := []string{oid}
	for len(queue) > 0 {
		cur := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[cur] {
			continue
		}
		seen[cur] = true
		c := r.parseCommit(cur)
		if c == nil {
			continue
		}
		for _, p := range c.Parents {
			if p == anc {
				return true
			}
			if !seen[p] {
				queue = append(queue, p)
			}
		}
	}
	return false
}

// refNames returns the ref names sorted (HEAD is not a stored ref).
func (r *Repo) refNames() []string {
	names := make([]string, 0, len(r.Refs))
	for name := range r.Refs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Server is the whole forge: the store plus request-scoped configuration.
type Server struct {
	mu           sync.RWMutex
	repos        map[string]*Repo // key: owner + "/" + name
	repoList     []*Repo          // creation order
	nextRepoID   int64
	nextEntityID int64 // global numeric id for issues, PRs and comments
	nextSeq      int64 // global creation sequence (ordering only)

	trialNow    time.Time
	trialNowISO string // RFC3339 UTC, e.g. 2026-11-14T06:30:00Z
	trialEpoch  int64

	shopToken string

	reqSeq uint64 // X-GitHub-Request-Id counter (atomic)

	fetchFlight singleflight // coalesces concurrent identical v2 fetches
}

func newServer(shopToken string, trialNow time.Time) *Server {
	if trialNow.IsZero() {
		trialNow = time.Date(2026, 11, 14, 6, 30, 0, 0, time.UTC)
	}
	return &Server{
		repos:        make(map[string]*Repo),
		nextRepoID:   1,
		nextEntityID: 1,
		trialNow:     trialNow,
		trialNowISO:  trialNow.UTC().Format("2006-01-02T15:04:05Z"),
		trialEpoch:   trialNow.Unix(),
		shopToken:    shopToken,
	}
}

// repoKey is the store key for a repository.
func repoKey(owner, name string) string { return owner + "/" + name }

// getRepo returns the repo, or nil. Callers hold at least a read lock.
func (s *Server) getRepo(owner, name string) *Repo {
	if owner != viewerLogin {
		return nil
	}
	return s.repos[repoKey(owner, name)]
}

// --- global ids (GraphQL Relay ids): opaque, stable, decodable -----------

func encodeGlobalID(kind string, parts ...string) string {
	raw := kind + ":" + strings.Join(parts, ":")
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeGlobalID(id string) (kind string, parts []string, ok bool) {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		// Tolerate standard base64 with padding too.
		raw, err = base64.StdEncoding.DecodeString(id)
		if err != nil {
			return "", nil, false
		}
	}
	fields := strings.Split(string(raw), ":")
	if len(fields) < 2 {
		return "", nil, false
	}
	return fields[0], fields[1:], true
}

func repoGlobalID(repoID int64) string {
	return encodeGlobalID("Repository", fmt.Sprintf("%d", repoID))
}

func issueGlobalID(repoID int64, number int) string {
	return encodeGlobalID("Issue", fmt.Sprintf("%d", repoID), fmt.Sprintf("%d", number))
}

func pullGlobalID(repoID int64, number int) string {
	return encodeGlobalID("PullRequest", fmt.Sprintf("%d", repoID), fmt.Sprintf("%d", number))
}

func commentGlobalID(repoID int64, number int, commentID int64) string {
	return encodeGlobalID("IssueComment", fmt.Sprintf("%d", repoID), fmt.Sprintf("%d", number), fmt.Sprintf("%d", commentID))
}

// findRepoByID locates a repo by its numeric database id.
func (s *Server) findRepoByID(id int64) *Repo {
	for _, r := range s.repoList {
		if r.ID == id {
			return r
		}
	}
	return nil
}
