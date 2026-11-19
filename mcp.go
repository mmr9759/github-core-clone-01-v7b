// mcp.go: the MCP endpoint at /mcp (JSON-RPC 2.0 Streamable HTTP).
//
// The suite probes server/discover first; a result there makes the session
// modern (revision 2026-07-28). The legacy initialize flow is supported too.
// tools/list returns one constant document so two calls are byte-identical.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

const toolsListJSON = `{"tools":[` +
	`{"name":"create_repo","description":"Create a new repository for the viewer.","inputSchema":{"type":"object","properties":{"name":{"type":"string","description":"Repository name"},"description":{"type":"string","description":"Repository description"},"private":{"type":"boolean","description":"Whether the repository is private"}},"required":["name"]}},` +
	`{"name":"get_repo","description":"Get a repository by owner and name.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"}},"required":["repo"]}},` +
	`{"name":"create_issue","description":"Create an issue in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"},"title":{"type":"string","description":"Issue title"},"body":{"type":"string","description":"Issue body"}},"required":["repo","title"]}},` +
	`{"name":"list_issues","description":"List issues in a repository.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"},"state":{"type":"string","description":"Issue state: open, closed or all"}},"required":["repo"]}},` +
	`{"name":"create_pull_request","description":"Create a pull request.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"},"title":{"type":"string","description":"Pull request title"},"head":{"type":"string","description":"Head branch name"},"base":{"type":"string","description":"Base branch name"},"body":{"type":"string","description":"Pull request body"}},"required":["repo","title","head","base"]}},` +
	`{"name":"merge_pull_request","description":"Merge a pull request.","inputSchema":{"type":"object","properties":{"owner":{"type":"string","description":"Repository owner"},"repo":{"type":"string","description":"Repository name"},"pull_number":{"type":"integer","description":"Pull request number"},"merge_method":{"type":"string","description":"Merge method: merge, squash or rebase"}},"required":["repo","pull_number"]}}` +
	`]}`

func (s *server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodDelete {
		writeJSON(w, 200, []byte(`{"jsonrpc":"2.0","result":{}}`))
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, 200, []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32601,"message":"method not found"}}`))
		return
	}
	body, _ := readBody(r, 1<<22)
	var req map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(body), &req); err != nil {
		// Non-JSON body: HTTP 400 with a -32700 parse error.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-GitHub-Request-Id", nextReqID())
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`))
		return
	}
	id := req["id"]
	method, _ := req["method"].(string)
	params := map[string]any{}
	if p, ok := req["params"].(map[string]any); ok {
		params = p
	}
	result := s.execMCP(method, params)
	var out bytes.Buffer
	out.WriteString(`{"jsonrpc":"2.0",`)
	if id != nil {
		out.WriteString(`"id":`)
		enc, _ := json.Marshal(id)
		out.Write(enc)
		out.WriteString(`,`)
	}
	if result.isError {
		out.WriteString(`"error":{"code":`)
		out.WriteString(strconv.Itoa(result.code))
		out.WriteString(`,"message":`)
		jstr(&out, result.message)
		out.WriteString(`}`)
	} else {
		out.WriteString(`"result":`)
		out.Write(result.payload)
	}
	out.WriteString(`}`)
	writeJSON(w, 200, out.Bytes())
}

type mcpResult struct {
	isError bool
	code    int
	message string
	payload []byte
}

func mcpOK(payload []byte) mcpResult { return mcpResult{payload: payload} }

func mcpErr(code int, msg string) mcpResult { return mcpResult{isError: true, code: code, message: msg} }

func (s *server) execMCP(method string, params map[string]any) mcpResult {
	switch method {
	case "server/discover":
		return mcpOK([]byte(`{"supportedVersions":["2026-07-28","2025-11-25","2025-06-18","2025-03-26","2024-11-05"],"capabilities":{"tools":{}},"serverInfo":{"name":"forge","version":"1.0.0"}}`))
	case "initialize":
		ver := "2025-11-25"
		if v, ok := params["protocolVersion"].(string); ok && v != "" {
			ver = v
		}
		var b bytes.Buffer
		b.WriteString(`{"protocolVersion":`)
		jstr(&b, ver)
		b.WriteString(`,"capabilities":{"tools":{}},"serverInfo":{"name":"forge","version":"1.0.0"}}`)
		return mcpOK(b.Bytes())
	case "notifications/initialized", "notifications/cancelled", "notifications/progress":
		return mcpOK([]byte(`{}`))
	case "ping":
		return mcpOK([]byte(`{}`))
	case "tools/list":
		return mcpOK([]byte(toolsListJSON))
	case "tools/call":
		name, _ := params["name"].(string)
		args := map[string]any{}
		if a, ok := params["arguments"].(map[string]any); ok {
			args = a
		}
		return s.mcpCallTool(name, args)
	}
	if strings.HasPrefix(method, "notifications/") {
		return mcpOK([]byte(`{}`))
	}
	return mcpErr(-32601, "method not found: "+method)
}

// mcpArg reads an argument, trying several aliases.
func mcpArg(args map[string]any, keys ...string) (any, bool) {
	for _, k := range keys {
		if v, ok := args[k]; ok && v != nil {
			return v, true
		}
	}
	return nil, false
}

func mcpStr(args map[string]any, keys ...string) string {
	if v, ok := mcpArg(args, keys...); ok {
		if sv, ok := v.(string); ok {
			return sv
		}
	}
	return ""
}

func mcpInt(args map[string]any, keys ...string) int {
	if v, ok := mcpArg(args, keys...); ok {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case string:
			i, _ := strconv.Atoi(n)
			return i
		}
	}
	return 0
}

// toolText wraps a JSON value as a text content block (plus structuredContent).
func toolText(payload []byte) []byte {
	var b bytes.Buffer
	b.WriteString(`{"content":[{"type":"text","text":`)
	jstr(&b, string(payload))
	b.WriteString(`}],"structuredContent":`)
	b.Write(payload)
	b.WriteString(`}`)
	return b.Bytes()
}

func toolErrText(msg string) []byte {
	var b bytes.Buffer
	b.WriteString(`{"content":[{"type":"text","text":`)
	jstr(&b, msg)
	b.WriteString(`}],"isError":true}`)
	return b.Bytes()
}

func (s *server) mcpCallTool(name string, args map[string]any) mcpResult {
	switch name {
	case "create_repo":
		repoName := mcpStr(args, "name", "repo", "repository")
		if repoName == "" {
			return mcpOK(toolErrText("missing required argument: name"))
		}
		desc := mcpStr(args, "description")
		private := false
		if v, ok := mcpArg(args, "private"); ok {
			if bv, ok := v.(bool); ok {
				private = bv
			}
		}
		s.store.mu.Lock()
		repo, ok := s.store.createRepo(repoName, desc, private)
		s.store.mu.Unlock()
		if !ok {
			return mcpOK(toolErrText("repository already exists: " + repoName))
		}
		var b bytes.Buffer
		b.WriteString(`{"full_name":`)
		jstr(&b, repo.fullName())
		b.WriteString(`,"name":`)
		jstr(&b, repo.Name)
		b.WriteString(`,"private":`)
		b.WriteString(strconv.FormatBool(private))
		b.WriteString(`,"description":`)
		jstr(&b, desc)
		b.WriteString(`}`)
		return mcpOK(toolText(b.Bytes()))
	case "get_repo":
		repoName := mcpStr(args, "repo", "name", "repository")
		if repoName == "" {
			return mcpOK(toolErrText("missing required argument: repo"))
		}
		owner := mcpStr(args, "owner")
		if owner == "" {
			owner = viewerLogin
		}
		s.store.mu.RLock()
		repo := s.store.lookupRepo(owner, repoName)
		s.store.mu.RUnlock()
		if repo == nil {
			return mcpOK(toolErrText("repository not found: " + owner + "/" + repoName))
		}
		var b bytes.Buffer
		b.WriteString(`{"full_name":`)
		jstr(&b, repo.fullName())
		b.WriteString(`,"name":`)
		jstr(&b, repo.Name)
		b.WriteString(`,"private":`)
		b.WriteString(strconv.FormatBool(repo.Private))
		b.WriteString(`,"description":`)
		jstr(&b, repo.Description)
		b.WriteString(`}`)
		return mcpOK(toolText(b.Bytes()))
	case "create_issue":
		repoName := mcpStr(args, "repo", "name", "repository")
		title := mcpStr(args, "title")
		if repoName == "" {
			return mcpOK(toolErrText("missing required argument: repo"))
		}
		if title == "" {
			return mcpOK(toolErrText("missing required argument: title"))
		}
		owner := mcpStr(args, "owner")
		if owner == "" {
			owner = viewerLogin
		}
		body := mcpStr(args, "body")
		s.store.mu.Lock()
		repo := s.store.lookupRepo(owner, repoName)
		if repo == nil {
			s.store.mu.Unlock()
			return mcpOK(toolErrText("repository not found: " + owner + "/" + repoName))
		}
		num := repo.nextNumber
		repo.nextNumber++
		id := s.store.nextID()
		is := &Issue{
			repo:    repo,
			Number:  num,
			ID:      id,
			NodeID:  globalID("Issue", repo.fullName()+":"+strconv.Itoa(num)),
			Title:   title,
			Body:    body,
			State:   "open",
			Created: s.store.nextTS(),
		}
		is.CreatedAt = isoTime(is.Created)
		is.UpdatedAt = is.CreatedAt
		repo.issues = append(repo.issues, is)
		repo.issueByNum[num] = is
		s.store.nodeIssue[is.NodeID] = is
		s.store.refreshIssueJSON(repo, is)
		s.store.mu.Unlock()
		var b bytes.Buffer
		b.WriteString(`{"number":`)
		b.WriteString(strconv.Itoa(num))
		b.WriteString(`,"id":`)
		b.WriteString(strconv.Itoa(id))
		b.WriteString(`,"title":`)
		jstr(&b, title)
		b.WriteString(`,"state":"open"}`)
		return mcpOK(toolText(b.Bytes()))
	case "list_issues":
		repoName := mcpStr(args, "repo", "name", "repository")
		if repoName == "" {
			return mcpOK(toolErrText("missing required argument: repo"))
		}
		owner := mcpStr(args, "owner")
		if owner == "" {
			owner = viewerLogin
		}
		state := mcpStr(args, "state")
		if state == "" {
			state = "open"
		}
		s.store.mu.RLock()
		repo := s.store.lookupRepo(owner, repoName)
		var list []*Issue
		if repo != nil {
			for _, is := range repo.issues {
				if state != "all" && is.State != state {
					continue
				}
				list = append(list, is)
			}
		}
		s.store.mu.RUnlock()
		if repo == nil {
			return mcpOK(toolErrText("repository not found: " + owner + "/" + repoName))
		}
		var b bytes.Buffer
		b.WriteByte('[')
		for i, is := range list {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"number":`)
			b.WriteString(strconv.Itoa(is.Number))
			b.WriteString(`,"title":`)
			jstr(&b, is.Title)
			b.WriteString(`,"state":`)
			jstr(&b, is.State)
			b.WriteString(`,"id":`)
			b.WriteString(strconv.Itoa(is.ID))
			b.WriteString(`}`)
		}
		b.WriteByte(']')
		return mcpOK(toolText(b.Bytes()))
	case "create_pull_request":
		repoName := mcpStr(args, "repo", "name", "repository")
		title := mcpStr(args, "title")
		head := mcpStr(args, "head", "head_ref", "headRefName")
		base := mcpStr(args, "base", "base_ref", "baseRefName")
		if repoName == "" {
			return mcpOK(toolErrText("missing required argument: repo"))
		}
		if title == "" {
			return mcpOK(toolErrText("missing required argument: title"))
		}
		if head == "" || base == "" {
			return mcpOK(toolErrText("missing required argument: head and base"))
		}
		owner := mcpStr(args, "owner")
		if owner == "" {
			owner = viewerLogin
		}
		body := mcpStr(args, "body")
		s.store.mu.Lock()
		repo := s.store.lookupRepo(owner, repoName)
		if repo == nil {
			s.store.mu.Unlock()
			return mcpOK(toolErrText("repository not found: " + owner + "/" + repoName))
		}
		_, headOK := repo.refs["refs/heads/"+head]
		_, baseOK := repo.refs["refs/heads/"+base]
		if !headOK || !baseOK {
			s.store.mu.Unlock()
			return mcpOK(toolErrText("head or base branch does not exist"))
		}
		num := repo.nextNumber
		repo.nextNumber++
		id := s.store.nextID()
		pr := &Pull{
			repo:    repo,
			Number:  num,
			ID:      id,
			NodeID:  globalID("PullRequest", repo.fullName()+":"+strconv.Itoa(num)),
			Title:   title,
			Body:    body,
			State:   "open",
			HeadRef: head,
			BaseRef: base,
			Created: s.store.nextTS(),
		}
		pr.CreatedAt = isoTime(pr.Created)
		pr.UpdatedAt = pr.CreatedAt
		repo.pulls = append(repo.pulls, pr)
		repo.prByNum[num] = pr
		s.store.nodePR[pr.NodeID] = pr
		s.store.mu.Unlock()
		var b bytes.Buffer
		b.WriteString(`{"number":`)
		b.WriteString(strconv.Itoa(num))
		b.WriteString(`,"pull_number":`)
		b.WriteString(strconv.Itoa(num))
		b.WriteString(`,"id":`)
		b.WriteString(strconv.Itoa(id))
		b.WriteString(`,"title":`)
		jstr(&b, title)
		b.WriteString(`,"head":`)
		jstr(&b, head)
		b.WriteString(`,"base":`)
		jstr(&b, base)
		b.WriteString(`,"state":"open"}`)
		return mcpOK(toolText(b.Bytes()))
	case "merge_pull_request":
		repoName := mcpStr(args, "repo", "name", "repository")
		num := mcpInt(args, "pull_number", "pullNumber", "number")
		if repoName == "" {
			return mcpOK(toolErrText("missing required argument: repo"))
		}
		if num == 0 {
			return mcpOK(toolErrText("missing required argument: pull_number"))
		}
		owner := mcpStr(args, "owner")
		if owner == "" {
			owner = viewerLogin
		}
		method := mcpStr(args, "merge_method", "mergeMethod")
		if method == "" {
			method = "merge"
		}
		s.store.mu.Lock()
		repo := s.store.lookupRepo(owner, repoName)
		if repo == nil {
			s.store.mu.Unlock()
			return mcpOK(toolErrText("repository not found: " + owner + "/" + repoName))
		}
		pr, ok := repo.prByNum[num]
		if !ok {
			s.store.mu.Unlock()
			return mcpOK(toolErrText("pull request not found"))
		}
		if pr.State != "open" || pr.Merged {
			s.store.mu.Unlock()
			return mcpOK(toolErrText("pull request is already merged"))
		}
		mergeCommit, errMsg := s.store.mergePullLocked(repo, pr, strings.ToLower(method))
		s.store.mu.Unlock()
		if errMsg != "" {
			return mcpOK(toolErrText(errMsg))
		}
		var b bytes.Buffer
		b.WriteString(`{"number":`)
		b.WriteString(strconv.Itoa(num))
		b.WriteString(`,"merged":true,"sha":`)
		jstr(&b, mergeCommit)
		b.WriteString(`}`)
		return mcpOK(toolText(b.Bytes()))
	}
	return mcpErr(-32601, "unknown tool: "+name)
}
