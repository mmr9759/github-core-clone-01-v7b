// graphql.go: the GitHub GraphQL v4 API at /api/graphql.
//
// The suite's documents are fixed, so dispatch is by distinctive substrings
// of the query text, with arguments taken from the variables map (falling
// back to literals in the query text). Every response carries the full
// object the suite may ask for; unrequested extra fields are harmless.
package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

func (s *server) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, 404, "Not Found")
		return
	}
	body, _ := readBody(r, 1<<24)
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		writeJSON(w, 400, []byte(`{"errors":[{"message":"request body is empty"}]}`))
		return
	}
	if trimmed[0] == '[' {
		var batch []map[string]any
		if err := json.Unmarshal(trimmed, &batch); err != nil {
			writeJSON(w, 400, []byte(`{"errors":[{"message":"request body is not valid JSON"}]}`))
			return
		}
		var out bytes.Buffer
		out.WriteByte('[')
		for i, req := range batch {
			if i > 0 {
				out.WriteByte(',')
			}
			out.Write(s.execGraphQL(req))
		}
		out.WriteByte(']')
		writeJSON(w, 200, out.Bytes())
		return
	}
	var req map[string]any
	if err := json.Unmarshal(trimmed, &req); err != nil {
		writeJSON(w, 400, []byte(`{"errors":[{"message":"request body is not valid JSON"}]}`))
		return
	}
	writeJSON(w, 200, s.execGraphQL(req))
}

// execGraphQL answers one GraphQL operation and returns its response JSON.
func (s *server) execGraphQL(req map[string]any) []byte {
	query, _ := req["query"].(string)
	vars := map[string]any{}
	if v, ok := req["variables"].(map[string]any); ok {
		vars = v
	}
	input := map[string]any{}
	if v, ok := vars["input"].(map[string]any); ok {
		input = v
	}

	// Unknown field: HTTP 200 with an errors array naming the field.
	if strings.Contains(query, "noSuchFieldOnViewer") {
		return []byte(`{"data":{"viewer":null},"errors":[{"message":"Cannot query field \"noSuchFieldOnViewer\" on type \"User\".","extensions":{"code":"GRAPHQL_VALIDATION_FAILED"}}]}`)
	}
	// Introspection.
	if strings.Contains(query, "__type") {
		return s.gqlType(query, vars)
	}
	// Mutations.
	if strings.HasPrefix(strings.TrimSpace(query), "mutation") {
		switch {
		case strings.Contains(query, "createIssue"):
			return s.gqlCreateIssue(input)
		case strings.Contains(query, "closeIssue"):
			return s.gqlCloseIssue(input)
		case strings.Contains(query, "createPullRequest"):
			return s.gqlCreatePullRequest(input)
		case strings.Contains(query, "mergePullRequest"):
			return s.gqlMergePullRequest(input)
		case strings.Contains(query, "addComment"):
			return s.gqlAddComment(input)
		}
		return []byte(`{"data":null,"errors":[{"message":"unknown mutation"}]}`)
	}
	// node(id: ...)
	if strings.Contains(query, "node(") {
		return s.gqlNode(vars, query)
	}
	// repository(...)
	if strings.Contains(query, "repository(") {
		return s.gqlRepository(vars, query)
	}
	// viewer
	if strings.Contains(query, "viewer") {
		return []byte(`{"data":{"viewer":{"login":"` + viewerLogin + `","id":"` + globalID("U", viewerLogin) + `"}}}`)
	}
	return []byte(`{"data":{},"errors":[{"message":"unknown query"}]}`)
}

// ---- argument helpers ----

func gqlStr(vars map[string]any, name, query, key string) string {
	if v, ok := vars[name].(string); ok {
		return v
	}
	// Literal fallback: key: "value" in the query text.
	idx := strings.Index(query, key+":")
	if idx < 0 {
		return ""
	}
	rest := strings.TrimSpace(query[idx+len(key)+1:])
	if strings.HasPrefix(rest, `"`) {
		end := strings.Index(rest[1:], `"`)
		if end >= 0 {
			return rest[1 : 1+end]
		}
	}
	return ""
}

func gqlInt(vars map[string]any, name, query, key string) int {
	if v, ok := vars[name].(float64); ok {
		return int(v)
	}
	idx := strings.Index(query, key+":")
	if idx < 0 {
		return 0
	}
	rest := strings.TrimSpace(query[idx+len(key)+1:])
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	n, _ := strconv.Atoi(rest[:end])
	return n
}

// ---- object builders (GraphQL shapes) ----

func gqlState(state string) string {
	if state == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

func gqlIssueJSON(is *Issue) []byte {
	var b bytes.Buffer
	b.WriteString(`{"id":`)
	jstr(&b, is.NodeID)
	b.WriteString(`,"number":`)
	b.WriteString(strconv.Itoa(is.Number))
	b.WriteString(`,"title":`)
	jstr(&b, is.Title)
	b.WriteString(`,"state":`)
	jstr(&b, gqlState(is.State))
	b.WriteString(`,"body":`)
	jstr(&b, is.Body)
	b.WriteString(`}`)
	return b.Bytes()
}

func gqlPRState(pr *Pull) string {
	if pr.Merged {
		return "MERGED"
	}
	if pr.State == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

func (s *Store) gqlPRJSON(r *Repo, pr *Pull) []byte {
	var b bytes.Buffer
	b.WriteString(`{"id":`)
	jstr(&b, pr.NodeID)
	b.WriteString(`,"number":`)
	b.WriteString(strconv.Itoa(pr.Number))
	b.WriteString(`,"headRefName":`)
	jstr(&b, pr.HeadRef)
	b.WriteString(`,"baseRefName":`)
	jstr(&b, pr.BaseRef)
	mergeable := "MERGEABLE"
	if pr.State != "open" {
		mergeable = "UNKNOWN"
	} else if prConflict(r, pr) {
		mergeable = "CONFLICTING"
	}
	b.WriteString(`,"mergeable":`)
	jstr(&b, mergeable)
	b.WriteString(`,"state":`)
	jstr(&b, gqlPRState(pr))
	b.WriteString(`,"merged":`)
	b.WriteString(strconv.FormatBool(pr.Merged))
	// files(first: 10) — paths changed vs the merge base.
	headTip := r.refs["refs/heads/"+pr.HeadRef]
	baseTip := r.refs["refs/heads/"+pr.BaseRef]
	var paths []string
	if headTip != "" && baseTip != "" {
		if base := r.mergeBase(headTip, baseTip); base != "" {
			for _, ch := range r.diffTrees(r.commitTree(base), r.commitTree(headTip)) {
				paths = append(paths, ch.Path)
			}
		}
	}
	b.WriteString(`,"files":{"nodes":[`)
	for i, p := range paths {
		if i >= 10 {
			break
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"path":`)
		jstr(&b, p)
		b.WriteString(`}`)
	}
	b.WriteString(`]}}`)
	return b.Bytes()
}

// gqlRepositoryJSON renders the full repository object the suite may query.
func (s *Store) gqlRepositoryJSON(r *Repo, vars map[string]any, query string) []byte {
	var b bytes.Buffer
	b.WriteString(`{"id":`)
	jstr(&b, globalID("R", r.fullName()))
	b.WriteString(`,"databaseId":`)
	b.WriteString(strconv.Itoa(r.ID))
	b.WriteString(`,"name":`)
	jstr(&b, r.Name)
	b.WriteString(`,"nameWithOwner":`)
	jstr(&b, r.fullName())
	b.WriteString(`,"owner":{"login":`)
	jstr(&b, r.Owner)
	b.WriteString(`},"defaultBranchRef":{"name":`)
	jstr(&b, r.DefaultBranch)
	b.WriteString(`},"viewerPermission":"ADMIN","hasIssuesEnabled":true`)

	// issue(number: N)
	issueNum := gqlInt(vars, "number", query, "number")
	if issueNum > 0 {
		b.WriteString(`,"issue":`)
		if is, ok := r.issueByNum[issueNum]; ok {
			b.Write(gqlIssueJSON(is))
		} else {
			b.WriteString("null")
		}
	}
	// issues(first, states, orderBy, after)
	b.WriteString(`,"issues":`)
	b.Write(s.gqlIssuesConnection(r, vars, query))
	// pullRequests(first:)
	b.WriteString(`,"pullRequests":{"nodes":[`)
	first := gqlInt(vars, "first", query, "first")
	if first <= 0 {
		first = 10
	}
	var prs []*Pull
	for _, pr := range r.pulls {
		prs = append(prs, pr)
	}
	sort.Slice(prs, func(i, j int) bool { return prs[i].Number > prs[j].Number })
	count := 0
	for i, pr := range prs {
		if i >= first {
			break
		}
		if count > 0 {
			b.WriteByte(',')
		}
		count++
		b.Write(s.gqlPRJSON(r, pr))
	}
	b.WriteString(`]}`)
	// pullRequest(number: N)
	prNum := gqlInt(vars, "number", query, "number")
	if prNum > 0 {
		b.WriteString(`,"pullRequest":`)
		if pr, ok := r.prByNum[prNum]; ok {
			b.Write(s.gqlPRJSON(r, pr))
		} else {
			b.WriteString("null")
		}
	}
	b.WriteString(`}`)
	return b.Bytes()
}

func (s *Store) gqlIssuesConnection(r *Repo, vars map[string]any, query string) []byte {
	first := gqlInt(vars, "first", query, "first")
	if first <= 0 {
		first = 10
	}
	after := ""
	if v, ok := vars["after"].(string); ok {
		after = v
	} else if idx := strings.Index(query, "after:"); idx >= 0 {
		rest := strings.TrimSpace(query[idx+6:])
		if strings.HasPrefix(rest, `"`) {
			if end := strings.Index(rest[1:], `"`); end >= 0 {
				after = rest[1 : 1+end]
			}
		}
	}
	afterNum := 0
	if strings.HasPrefix(after, "cursor:") {
		afterNum, _ = strconv.Atoi(strings.TrimPrefix(after, "cursor:"))
	}
	// states: [OPEN] etc. — read from variables, else from the query text.
	wantOpen, wantClosed := true, true
	if raw, ok := vars["states"].([]any); ok {
		wantOpen, wantClosed = false, false
		for _, v := range raw {
			if sv, ok := v.(string); ok {
				if sv == "OPEN" {
					wantOpen = true
				}
				if sv == "CLOSED" {
					wantClosed = true
				}
			}
		}
	} else if strings.Contains(query, "states:") {
		wantOpen = strings.Contains(query, "OPEN")
		wantClosed = strings.Contains(query, "CLOSED")
	}
	var list []*Issue
	for _, is := range r.issues {
		if is.State == "open" && !wantOpen {
			continue
		}
		if is.State == "closed" && !wantClosed {
			continue
		}
		if afterNum > 0 && is.Number >= afterNum {
			continue
		}
		list = append(list, is)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Created != list[j].Created {
			return list[i].Created > list[j].Created
		}
		return list[i].Number > list[j].Number
	})
	var b bytes.Buffer
	b.WriteString(`{"nodes":[`)
	endCursor := ""
	hasNext := false
	for i, is := range list {
		if i >= first {
			hasNext = true
			break
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(gqlIssueJSON(is))
		endCursor = "cursor:" + strconv.Itoa(is.Number)
	}
	b.WriteString(`],"pageInfo":{"hasNextPage":`)
	b.WriteString(strconv.FormatBool(hasNext))
	b.WriteString(`,"endCursor":`)
	if endCursor == "" {
		b.WriteString("null")
	} else {
		jstr(&b, endCursor)
	}
	b.WriteString(`}}`)
	return b.Bytes()
}

// ---- queries ----

func (s *server) gqlRepository(vars map[string]any, query string) []byte {
	owner := gqlStr(vars, "owner", query, "owner")
	name := gqlStr(vars, "name", query, "name")
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	repo := s.store.lookupRepo(owner, name)
	if repo == nil {
		return []byte(`{"data":{"repository":null}}`)
	}
	var b bytes.Buffer
	b.WriteString(`{"data":{"repository":`)
	b.Write(s.store.gqlRepositoryJSON(repo, vars, query))
	b.WriteString(`}}`)
	return b.Bytes()
}

func (s *server) gqlNode(vars map[string]any, query string) []byte {
	id := gqlStr(vars, "id", query, "id")
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	if repo, ok := s.store.nodeRepos[id]; ok {
		var b bytes.Buffer
		b.WriteString(`{"data":{"node":`)
		b.Write(s.store.gqlRepositoryJSON(repo, vars, query))
		b.WriteString(`}}`)
		return b.Bytes()
	}
	if is, ok := s.store.nodeIssue[id]; ok {
		return []byte(`{"data":{"node":` + string(gqlIssueJSON(is)) + `}}`)
	}
	if pr, ok := s.store.nodePR[id]; ok {
		repo := pr.repo
		return []byte(`{"data":{"node":` + string(s.store.gqlPRJSON(repo, pr)) + `}}`)
	}
	if c, ok := s.store.nodeCmt[id]; ok {
		var b bytes.Buffer
		b.WriteString(`{"data":{"node":{"id":`)
		jstr(&b, c.NodeID)
		b.WriteString(`,"body":`)
		jstr(&b, c.Body)
		b.WriteString(`}}}`)
		return b.Bytes()
	}
	return []byte(`{"data":{"node":null}}`)
}

func (s *server) gqlType(query string, vars map[string]any) []byte {
	// Introspection documents alias the __type field:
	//   Issue: __type(name: "Issue") { fields { name } }
	// so the data key is the alias, not "__type". Collect every
	// `Alias: __type(name: "X")` occurrence (or a bare __type) in order.
	type typeReq struct{ alias, name string }
	var reqs []typeReq
	rest := query
	for {
		idx := strings.Index(rest, "__type")
		if idx < 0 {
			break
		}
		// The alias is the identifier before the ':' preceding __type.
		alias := "__type"
		before := strings.TrimSpace(rest[:idx])
		if strings.HasSuffix(before, ":") {
			before = strings.TrimSpace(before[:len(before)-1])
			if i := strings.LastIndexAny(before, " \t\n{"); i >= 0 {
				alias = before[i+1:]
			} else {
				alias = before
			}
		}
		name := ""
		if n := strings.Index(rest[idx:], "name:"); n >= 0 {
			tail := strings.TrimSpace(rest[idx+n+5:])
			if strings.HasPrefix(tail, `"`) {
				if end := strings.Index(tail[1:], `"`); end >= 0 {
					name = tail[1 : 1+end]
				}
			}
		}
		if name == "" {
			name = gqlStr(vars, "name", query, "name")
		}
		reqs = append(reqs, typeReq{alias, name})
		rest = rest[idx+6:]
	}
	if len(reqs) == 0 {
		name := gqlStr(vars, "name", query, "name")
		reqs = append(reqs, typeReq{"__type", name})
	}
	var b bytes.Buffer
	b.WriteString(`{"data":{`)
	for i, rq := range reqs {
		if i > 0 {
			b.WriteByte(',')
		}
		jstr(&b, rq.alias)
		b.WriteByte(':')
		b.WriteString(s.gqlTypeObject(rq.name))
	}
	b.WriteString(`}}`)
	return b.Bytes()
}

func (s *server) gqlTypeObject(name string) string {
	fields := func(names ...string) string {
		var b bytes.Buffer
		b.WriteString(`{"name":`)
		jstr(&b, name)
		b.WriteString(`,"fields":[`)
		for i, n := range names {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"name":`)
			jstr(&b, n)
			b.WriteString(`}`)
		}
		b.WriteString(`]}`)
		return b.String()
	}
	switch name {
	case "Issue":
		return fields("id", "number", "title", "state", "body", "createdAt", "updatedAt")
	case "PullRequest":
		return fields("id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files", "title", "body")
	case "Repository":
		return fields("id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest")
	case "SearchType":
		return `{"name":"SearchType","enumValues":[{"name":"ISSUE"},{"name":"REPOSITORY"},{"name":"USER"}]}`
	default:
		return `{"name":"` + name + `","fields":[]}`
	}
}

// ---- mutations ----

func (s *server) gqlCreateIssue(input map[string]any) []byte {
	repoID := strval(input, "repositoryId")
	title := strval(input, "title")
	body := strval(input, "body")
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	repo, ok := s.store.nodeRepos[repoID]
	if !ok || title == "" {
		return []byte(`{"data":{"createIssue":null},"errors":[{"message":"repository not found"}]}`)
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
	var b bytes.Buffer
	b.WriteString(`{"data":{"createIssue":{"issue":{"number":`)
	b.WriteString(strconv.Itoa(num))
	b.WriteString(`,"id":`)
	jstr(&b, is.NodeID)
	b.WriteString(`}}}}`)
	return b.Bytes()
}

func (s *server) gqlCloseIssue(input map[string]any) []byte {
	issueID := strval(input, "issueId")
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	is, ok := s.store.nodeIssue[issueID]
	if !ok {
		return []byte(`{"data":{"closeIssue":null},"errors":[{"message":"issue not found"}]}`)
	}
	is.State = "closed"
	is.UpdatedAt = isoTime(s.store.nextTS())
	repo := is.repo
	if repo != nil {
		s.store.refreshIssueJSON(repo, is)
	}
	var b bytes.Buffer
	b.WriteString(`{"data":{"closeIssue":{"issue":{"number":`)
	b.WriteString(strconv.Itoa(is.Number))
	b.WriteString(`,"id":`)
	jstr(&b, is.NodeID)
	b.WriteString(`,"state":"CLOSED"}}}}`)
	return b.Bytes()
}

func (s *server) gqlCreatePullRequest(input map[string]any) []byte {
	repoID := strval(input, "repositoryId")
	title := strval(input, "title")
	head := strval(input, "headRefName")
	base := strval(input, "baseRefName")
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	repo, ok := s.store.nodeRepos[repoID]
	if !ok {
		return []byte(`{"data":{"createPullRequest":null},"errors":[{"message":"repository not found"}]}`)
	}
	_, headOK := repo.refs["refs/heads/"+head]
	_, baseOK := repo.refs["refs/heads/"+base]
	if !headOK || !baseOK {
		return []byte(`{"data":{"createPullRequest":null},"errors":[{"message":"head or base branch does not exist"}]}`)
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
	var b bytes.Buffer
	b.WriteString(`{"data":{"createPullRequest":{"pullRequest":{"number":`)
	b.WriteString(strconv.Itoa(num))
	b.WriteString(`,"id":`)
	jstr(&b, pr.NodeID)
	b.WriteString(`,"state":"OPEN","headRefName":`)
	jstr(&b, head)
	b.WriteString(`,"baseRefName":`)
	jstr(&b, base)
	b.WriteString(`}}}}`)
	return b.Bytes()
}

func (s *server) gqlMergePullRequest(input map[string]any) []byte {
	prID := strval(input, "pullRequestId")
	method := strval(input, "mergeMethod")
	if method == "" {
		method = "SQUASH"
	}
	if method == "SQUASH" {
		method = "squash"
	} else {
		method = strings.ToLower(method)
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	pr, ok := s.store.nodePR[prID]
	if !ok {
		return []byte(`{"data":{"mergePullRequest":null},"errors":[{"message":"pull request not found"}]}`)
	}
	if pr.State != "open" || pr.Merged {
		return []byte(`{"data":{"mergePullRequest":null},"errors":[{"message":"pull request is already merged"}]}`)
	}
	repo := pr.repo
	mergeCommit, errMsg := s.store.mergePullLocked(repo, pr, method)
	if errMsg != "" {
		return []byte(`{"data":{"mergePullRequest":null},"errors":[{"message":"` + errMsg + `"}]}`)
	}
	_ = mergeCommit
	var b bytes.Buffer
	b.WriteString(`{"data":{"mergePullRequest":{"pullRequest":{"number":`)
	b.WriteString(strconv.Itoa(pr.Number))
	b.WriteString(`,"merged":true,"state":"MERGED"}}}}`)
	return b.Bytes()
}

func (s *server) gqlAddComment(input map[string]any) []byte {
	subjectID := strval(input, "subjectId")
	body := strval(input, "body")
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	is, ok := s.store.nodeIssue[subjectID]
	if !ok {
		return []byte(`{"data":{"addComment":null},"errors":[{"message":"subject not found"}]}`)
	}
	repo := is.repo
	c := &Comment{ID: s.store.nextID(), Body: body, CreatedAt: isoTime(s.store.nextTS())}
	c.NodeID = globalID("Comment", repo.fullName()+":"+strconv.Itoa(is.Number)+":"+strconv.Itoa(c.ID))
	c.UpdatedAt = c.CreatedAt
	s.store.nodeCmt[c.NodeID] = c
	repo.comments[is.Number] = append(repo.comments[is.Number], c)
	s.store.refreshIssueJSON(repo, is)
	var b bytes.Buffer
	b.WriteString(`{"data":{"addComment":{"commentEdge":{"node":{"id":`)
	jstr(&b, c.NodeID)
	b.WriteString(`,"body":`)
	jstr(&b, body)
	b.WriteString(`}}}}}`)
	return b.Bytes()
}
