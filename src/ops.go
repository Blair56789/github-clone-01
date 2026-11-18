package main

// Shared store operations used by REST, GraphQL and MCP alike. Every function
// here assumes the caller holds the server's write lock.

func (s *Server) allocID() int64 {
	id := s.nextEntityID
	s.nextEntityID++
	return id
}

func (s *Server) allocSeq() int64 {
	s.nextSeq++
	return s.nextSeq
}

// createIssueInternal creates an issue, drawing its number from the repo's
// counter shared with pull requests.
func (s *Server) createIssueInternal(repo *Repo, title, body string, labels []string) *Issue {
	issue := &Issue{
		ID:     s.allocID(),
		Number: repo.NextNumber,
		Title:  title,
		Body:   body,
		State:  "open",
		Labels: append([]string(nil), labels...),
		Seq:    s.allocSeq(),
	}
	repo.NextNumber++
	repo.Issues[issue.Number] = issue
	repo.IssueOrder = append(repo.IssueOrder, issue)
	return issue
}

// closeIssueInternal closes an issue and invalidates its cached REST JSON.
func closeIssueInternal(issue *Issue) {
	issue.State = "closed"
	issue.cachedJSON = nil
}

// createPullInternal creates a pull request. It returns an error message
// ("" on success) when a branch is missing or an open PR already covers the
// same head and base.
func (s *Server) createPullInternal(repo *Repo, title, body, head, base string) (*Pull, string) {
	headTip, headOK := repo.Refs["refs/heads/"+head]
	baseTip, baseOK := repo.Refs["refs/heads/"+base]
	if !headOK || !baseOK {
		return nil, "head or base branch does not exist"
	}
	for _, p := range repo.PullOrder {
		if p.State == "open" && p.HeadRef == head && p.BaseRef == base {
			return nil, "a pull request already exists for this head and base"
		}
	}
	pull := &Pull{
		ID:      s.allocID(),
		Number:  repo.NextNumber,
		Title:   title,
		Body:    body,
		State:   "open",
		HeadRef: head,
		BaseRef: base,
		HeadSha: headTip,
		BaseSha: baseTip,
		Seq:     s.allocSeq(),
	}
	repo.NextNumber++
	repo.Pulls[pull.Number] = pull
	repo.PullOrder = append(repo.PullOrder, pull)
	return pull, ""
}

// addCommentInternal appends a comment to an issue.
func (s *Server) addCommentInternal(issue *Issue, body string) *Comment {
	comment := &Comment{ID: s.allocID(), Body: body, Seq: s.allocSeq()}
	issue.Comments = append(issue.Comments, comment)
	return comment
}

// pullMergeable reports a PR's mergeability: MERGEABLE, CONFLICTING or
// UNKNOWN (closed/merged PRs, or missing branches).
func pullMergeable(repo *Repo, pr *Pull) string {
	if pr.State != "open" {
		return "UNKNOWN"
	}
	baseTip := repo.Refs["refs/heads/"+pr.BaseRef]
	headTip := repo.Refs["refs/heads/"+pr.HeadRef]
	if baseTip == "" || headTip == "" {
		return "UNKNOWN"
	}
	baseCommit := repo.parseCommit(baseTip)
	headCommit := repo.parseCommit(headTip)
	if baseCommit == nil || headCommit == nil {
		return "UNKNOWN"
	}
	base := mergeBase(repo, baseTip, headTip)
	baseTree := ""
	if base != "" {
		if bc := repo.parseCommit(base); bc != nil {
			baseTree = bc.Tree
		}
	}
	headChanges := diffTrees(repo, baseTree, headCommit.Tree)
	baseChanges := diffTrees(repo, baseTree, baseCommit.Tree)
	baseChanged := map[string]string{}
	for _, ch := range baseChanges {
		baseChanged[ch.Path] = ch.NewBlobOID
	}
	for _, ch := range headChanges {
		if boid, ok := baseChanged[ch.Path]; ok && boid != ch.NewBlobOID {
			return "CONFLICTING"
		}
	}
	return "MERGEABLE"
}

// pullStateName maps a PR to its GraphQL state name.
func pullStateName(pr *Pull) string {
	switch {
	case pr.Merged:
		return "MERGED"
	case pr.State == "closed":
		return "CLOSED"
	default:
		return "OPEN"
	}
}

// issueStateName maps an issue to its GraphQL state name.
func issueStateName(issue *Issue) string {
	if issue.State == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}
