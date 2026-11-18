package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
)

// MCP: JSON-RPC 2.0 over Streamable HTTP (POST /mcp), modern era 2026-07-28
// plus the legacy initialize handshake. Answers are application/json; no SSE.

var mcpSupportedVersions = []string{
	"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05",
}

var mcpServerInfo = map[string]any{"name": "forge", "version": "1.0.0"}

// mcpToolsListResult is the precomputed tools/list result: byte-identical on
// every call (the suite compares two calls).
var mcpToolsListResult = func() []byte {
	tool := func(name, desc string, props map[string]any, required []string) map[string]any {
		return map[string]any{
			"name":        name,
			"description": desc,
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": props,
				"required":   required,
			},
		}
	}
	str := map[string]any{"type": "string"}
	tools := []map[string]any{
		tool("create_repo", "Create a repository for the viewer.", map[string]any{
			"name":        str,
			"description": str,
			"private":     map[string]any{"type": "boolean"},
		}, []string{"name"}),
		tool("get_repo", "Get a repository by owner and name.", map[string]any{
			"owner": str,
			"repo":  str,
		}, []string{"owner", "repo"}),
		tool("create_issue", "Create an issue in a repository.", map[string]any{
			"owner": str,
			"repo":  str,
			"title": str,
			"body":  str,
		}, []string{"owner", "repo", "title"}),
		tool("list_issues", "List issues in a repository.", map[string]any{
			"owner": str,
			"repo":  str,
			"state": str,
		}, []string{"owner", "repo"}),
		tool("create_pull_request", "Create a pull request.", map[string]any{
			"owner": str,
			"repo":  str,
			"title": str,
			"body":  str,
			"head":  str,
			"base":  str,
		}, []string{"owner", "repo", "title", "head", "base"}),
		tool("merge_pull_request", "Merge a pull request with squash.", map[string]any{
			"owner":       str,
			"repo":        str,
			"pull_number": map[string]any{"type": "integer"},
		}, []string{"owner", "repo", "pull_number"}),
	}
	body, _ := json.Marshal(map[string]any{"tools": tools})
	return body
}()

type jsonrpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func rpcResultBytes(id json.RawMessage, result json.RawMessage) []byte {
	var b bytes.Buffer
	b.WriteString(`{"jsonrpc":"2.0","id":`)
	if id == nil {
		b.WriteString("null")
	} else {
		b.Write(id)
	}
	b.WriteString(`,"result":`)
	b.Write(result)
	b.WriteString("}")
	return b.Bytes()
}

func rpcErrorBytes(id json.RawMessage, code int, message string) []byte {
	msg, _ := json.Marshal(message)
	var b bytes.Buffer
	b.WriteString(`{"jsonrpc":"2.0","id":`)
	if id == nil {
		b.WriteString("null")
	} else {
		b.Write(id)
	}
	b.WriteString(`,"error":{"code":`)
	b.WriteString(strconv.Itoa(code))
	b.WriteString(`,"message":`)
	b.Write(msg)
	b.WriteString("}}")
	return b.Bytes()
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if !s.authed(r) {
		s.writeErr(w, http.StatusUnauthorized, "Requires authentication")
		return
	}
	if r.Method != "POST" {
		s.writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	body, err := readBody(r, 16<<20)
	if err != nil {
		s.writeJSONBytes(w, http.StatusBadRequest, rpcErrorBytes(nil, -32700, "Parse error"))
		return
	}
	var req jsonrpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		s.writeJSONBytes(w, http.StatusBadRequest, rpcErrorBytes(nil, -32700, "Parse error"))
		return
	}
	// Notifications (no id) get an empty 202.
	if req.ID == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := s.dispatchMCP(&req)
	s.writeJSONBytes(w, http.StatusOK, resp)
}

func (s *Server) dispatchMCP(req *jsonrpcRequest) []byte {
	switch req.Method {
	case "server/discover":
		result, _ := json.Marshal(map[string]any{
			"supportedVersions": mcpSupportedVersions,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        mcpServerInfo,
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": mcpServerInfo,
			},
		})
		return rpcResultBytes(req.ID, result)
	case "initialize":
		version := "2026-07-28"
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(req.Params) > 0 {
			json.Unmarshal(req.Params, &params)
		}
		if params.ProtocolVersion != "" {
			version = params.ProtocolVersion
		}
		result, _ := json.Marshal(map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      mcpServerInfo,
		})
		return rpcResultBytes(req.ID, result)
	case "ping":
		return rpcResultBytes(req.ID, []byte("{}"))
	case "tools/list":
		return rpcResultBytes(req.ID, mcpToolsListResult)
	case "tools/call":
		return s.mcpToolsCall(req)
	default:
		return rpcErrorBytes(req.ID, -32601, "method not found: "+req.Method)
	}
}

// mcpToolsCall runs one tool call. The write lock is held for the whole call:
// tool calls are sequential in the suite, and serializing keeps the two
// concurrent calls in the id check independent (each echoes its own id).
func (s *Server) mcpToolsCall(req *jsonrpcRequest) []byte {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return rpcErrorBytes(req.ID, -32602, "invalid params")
		}
	}
	if params.Arguments == nil {
		params.Arguments = map[string]any{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, code, errMsg := s.runMCPTool(params.Name, params.Arguments)
	if code != 0 {
		return rpcErrorBytes(req.ID, code, errMsg)
	}
	text, _ := json.Marshal(result)
	resp, _ := json.Marshal(map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(text)},
		},
		"structuredContent": result,
	})
	return rpcResultBytes(req.ID, resp)
}

// argStr reads a string argument.
func argStr(args map[string]any, name string) string {
	if v, ok := args[name].(string); ok {
		return v
	}
	return ""
}

// argNum reads an integer argument (JSON number or digit string).
func argNum(args map[string]any, name string) (int, bool) {
	switch v := args[name].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			return n, true
		}
	}
	return 0, false
}

// runMCPTool executes a tool against the shared store (write lock held).
// It returns (result, 0, "") on success or (nil, rpcCode, message) on error.
func (s *Server) runMCPTool(name string, args map[string]any) (any, int, string) {
	switch name {
	case "create_repo":
		repoName := argStr(args, "name")
		if repoName == "" {
			return nil, -32602, "name is required"
		}
		if s.repos[repoKey(viewerLogin, repoName)] != nil {
			return nil, -32602, "name already exists on this account"
		}
		private := true
		if v, ok := args["private"].(bool); ok {
			private = v
		}
		s.nextSeq++
		repo := newRepo(s.nextRepoID, repoName, private, argStr(args, "description"), s.nextSeq)
		s.nextRepoID++
		s.repos[repoKey(viewerLogin, repoName)] = repo
		s.repoList = append(s.repoList, repo)
		return repoJSON(repo), 0, ""
	case "get_repo":
		owner, repoName := argStr(args, "owner"), argStr(args, "repo")
		if owner == "" || repoName == "" {
			return nil, -32602, "owner and repo are required"
		}
		repo := s.getRepo(owner, repoName)
		if repo == nil {
			return nil, -32602, "repository not found"
		}
		return repoJSON(repo), 0, ""
	case "create_issue":
		owner, repoName := argStr(args, "owner"), argStr(args, "repo")
		if owner == "" || repoName == "" || argStr(args, "title") == "" {
			return nil, -32602, "owner, repo and title are required"
		}
		repo := s.getRepo(owner, repoName)
		if repo == nil {
			return nil, -32602, "repository not found"
		}
		issue := s.createIssueInternal(repo, argStr(args, "title"), argStr(args, "body"), nil)
		var obj any
		json.Unmarshal(issueJSON(s, repo, issue, true), &obj)
		return obj, 0, ""
	case "list_issues":
		owner, repoName := argStr(args, "owner"), argStr(args, "repo")
		if owner == "" || repoName == "" {
			return nil, -32602, "owner and repo are required"
		}
		repo := s.getRepo(owner, repoName)
		if repo == nil {
			return nil, -32602, "repository not found"
		}
		state := argStr(args, "state")
		if state == "" {
			state = "open"
		}
		list := make([]any, 0)
		for _, is := range repo.IssueOrder {
			if state != "all" && is.State != state {
				continue
			}
			var obj any
			json.Unmarshal(issueJSON(s, repo, is, true), &obj)
			list = append(list, obj)
		}
		return list, 0, ""
	case "create_pull_request":
		owner, repoName := argStr(args, "owner"), argStr(args, "repo")
		head, base := argStr(args, "head"), argStr(args, "base")
		if owner == "" || repoName == "" || argStr(args, "title") == "" || head == "" || base == "" {
			return nil, -32602, "owner, repo, title, head and base are required"
		}
		repo := s.getRepo(owner, repoName)
		if repo == nil {
			return nil, -32602, "repository not found"
		}
		pr, errMsg := s.createPullInternal(repo, argStr(args, "title"), argStr(args, "body"), head, base)
		if errMsg != "" {
			return nil, -32602, errMsg
		}
		return s.pullJSON(repo, pr), 0, ""
	case "merge_pull_request":
		owner, repoName := argStr(args, "owner"), argStr(args, "repo")
		number, ok := argNum(args, "pull_number")
		if owner == "" || repoName == "" || !ok {
			return nil, -32602, "owner, repo and pull_number are required"
		}
		repo := s.getRepo(owner, repoName)
		if repo == nil {
			return nil, -32602, "repository not found"
		}
		pr := repo.Pulls[number]
		if pr == nil {
			return nil, -32602, "pull request not found"
		}
		sha, status, msg := s.mergePull(repo, pr)
		if status != 0 {
			return nil, -32602, msg
		}
		return map[string]any{
			"sha":     sha,
			"merged":  true,
			"number":  number,
			"message": "Pull Request successfully merged",
		}, 0, ""
	default:
		return nil, -32601, "unknown tool: " + name
	}
}
