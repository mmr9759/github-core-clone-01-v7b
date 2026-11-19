// rest.go: the GitHub REST v3 API under /api/v3.
package main

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

func (s *server) handleREST(w http.ResponseWriter, r *http.Request, rest string) {
	rest = strings.TrimSuffix(rest, "/")
	switch {
	case rest == "meta" && r.Method == http.MethodGet:
		writeJSON(w, 200, []byte(`{"installed_version":"1.0.0"}`))
	case rest == "user" && r.Method == http.MethodGet:
		writeJSON(w, 200, s.store.userJSON())
	case rest == "user/repos":
		s.handleUserRepos(w, r)
	case strings.HasPrefix(rest, "repos/"):
		s.handleRepoREST(w, r, strings.TrimPrefix(rest, "repos/"))
	default:
		writeErr(w, 404, "Not Found")
	}
}

func (s *server) handleUserRepos(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		q := r.URL.Query()
		perPage := atoiDefault(q.Get("per_page"), 30)
		if perPage < 1 {
			perPage = 1
		}
		if perPage > 100 {
			perPage = 100
		}
		page := atoiDefault(q.Get("page"), 1)
		if page < 1 {
			page = 1
		}
		s.store.mu.RLock()
		total := len(s.store.order)
		start := (page - 1) * perPage
		var out bytes.Buffer
		out.WriteByte('[')
		first := true
		for i := start; i < total && i < start+perPage; i++ {
			if !first {
				out.WriteByte(',')
			}
			first = false
			out.Write(s.store.order[i].repoJSON)
		}
		out.WriteByte(']')
		s.store.mu.RUnlock()
		if start+perPage < total {
			next := "http://" + r.Host + "/api/v3/user/repos?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page+1)
			w.Header().Set("Link", `<`+next+`>; rel="next"`)
		}
		writeJSON(w, 200, out.Bytes())
	case http.MethodPost:
		body, _ := readBody(r, 1<<22)
		m, err := decodeJSON(body)
		if err != nil {
			writeErr(w, 400, "Problems parsing JSON")
			return
		}
		name := strval(m, "name")
		if name == "" {
			writeErr(w, 422, "name is required")
			return
		}
		desc := strval(m, "description")
		private, _ := jsonBool(m["private"])
		s.store.mu.Lock()
		repo, ok := s.store.createRepo(name, desc, private)
		s.store.mu.Unlock()
		if !ok {
			writeErr(w, 422, "Repository already exists")
			return
		}
		writeJSON(w, 201, repo.repoJSON)
	default:
		writeErr(w, 404, "Not Found")
	}
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// handleRepoREST routes /repos/{owner}/{repo}[/...].
func (s *server) handleRepoREST(w http.ResponseWriter, r *http.Request, rest string) {
	segs := strings.Split(rest, "/")
	if len(segs) < 2 {
		writeErr(w, 404, "Not Found")
		return
	}
	owner, name := segs[0], segs[1]
	tail := segs[2:]

	s.store.mu.RLock()
	repo := s.store.lookupRepo(owner, name)
	s.store.mu.RUnlock()
	if repo == nil {
		writeErr(w, 404, "Not Found")
		return
	}

	// /repos/{o}/{r}
	if len(tail) == 0 {
		switch r.Method {
		case http.MethodGet:
			s.store.mu.RLock()
			b := repo.repoJSON
			s.store.mu.RUnlock()
			writeJSON(w, 200, b)
		case http.MethodPatch:
			body, _ := readBody(r, 1<<22)
			m, err := decodeJSON(body)
			if err != nil {
				writeErr(w, 400, "Problems parsing JSON")
				return
			}
			s.store.mu.Lock()
			if d, ok := m["description"]; ok {
				if ds, ok := d.(string); ok {
					repo.Description = ds
				}
			}
			repo.rebuildRepoJSON()
			b := repo.repoJSON
			s.store.mu.Unlock()
			writeJSON(w, 200, b)
		case http.MethodDelete:
			s.store.mu.Lock()
			s.store.deleteRepo(repo)
			s.store.mu.Unlock()
			w.Header().Set("X-GitHub-Request-Id", nextReqID())
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(204)
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}

	switch tail[0] {
	case "git":
		if len(tail) == 2 && tail[1] == "refs" && r.Method == http.MethodPost {
			s.restCreateRef(w, r, repo)
			return
		}
	case "branches":
		if len(tail) == 1 && r.Method == http.MethodGet {
			s.restBranches(w, r, repo)
			return
		}
	case "commits":
		if len(tail) == 1 && r.Method == http.MethodGet {
			s.restCommits(w, r, repo)
			return
		}
	case "contents":
		s.restContents(w, r, repo, tail[1:])
		return
	case "labels":
		if len(tail) == 1 {
			if r.Method == http.MethodPost {
				s.restCreateLabel(w, r, repo)
				return
			}
			if r.Method == http.MethodGet {
				s.restListLabels(w, r, repo)
				return
			}
		}
	case "issues":
		s.restIssues(w, r, repo, tail[1:])
		return
	case "pulls":
		s.restPulls(w, r, repo, tail[1:])
		return
	}
	writeErr(w, 404, "Not Found")
}

func (s *server) restCreateRef(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, _ := readBody(r, 1<<22)
	m, err := decodeJSON(body)
	if err != nil {
		writeErr(w, 400, "Problems parsing JSON")
		return
	}
	ref := strval(m, "ref")
	sha := strval(m, "sha")
	if ref == "" || sha == "" || !isOID(sha) {
		writeErr(w, 422, "ref and sha are required")
		return
	}
	if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
		writeErr(w, 422, "invalid ref")
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if _, exists := repo.refs[ref]; exists {
		writeErr(w, 422, "Reference already exists")
		return
	}
	obj := repo.objects[sha]
	if obj == nil {
		writeErr(w, 422, "Object does not exist")
		return
	}
	if strings.HasPrefix(ref, "refs/heads/") && obj.typ != 1 {
		writeErr(w, 422, "sha is not a commit")
		return
	}
	repo.refs[ref] = sha
	repo.bump()
	var b bytes.Buffer
	b.WriteString(`{"ref":`)
	jstr(&b, ref)
	b.WriteString(`,"object":{"sha":`)
	jstr(&b, sha)
	b.WriteString(`,"type":`)
	jstr(&b, obj.typName())
	b.WriteString(`},"url":""}`)
	writeJSON(w, 201, b.Bytes())
}

func (s *server) restBranches(w http.ResponseWriter, r *http.Request, repo *Repo) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	var names []string
	for ref := range repo.refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			names = append(names, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	sort.Strings(names)
	var b bytes.Buffer
	b.WriteByte('[')
	for i, name := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"name":`)
		jstr(&b, name)
		b.WriteString(`,"commit":{"sha":`)
		jstr(&b, repo.refs["refs/heads/"+name])
		b.WriteString(`,"url":""},"protected":false}`)
	}
	b.WriteByte(']')
	writeJSON(w, 200, b.Bytes())
}

func (s *server) restCommits(w http.ResponseWriter, r *http.Request, repo *Repo) {
	ref := r.URL.Query().Get("sha")
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	tip := ""
	if ref == "" {
		tip = repo.refs["refs/heads/"+repo.DefaultBranch]
	} else if isOID(ref) {
		tip = ref
	} else {
		tip = repo.refs["refs/heads/"+ref]
		if tip == "" {
			tip = repo.refs[ref]
		}
	}
	var b bytes.Buffer
	b.WriteByte('[')
	first := true
	if tip != "" {
		seen := make(map[string]bool)
		stack := []string{tip}
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[oid] {
				continue
			}
			seen[oid] = true
			obj := repo.objects[oid]
			if obj == nil || obj.typ != 1 {
				continue
			}
			cv, err := parseCommit(obj.data)
			if err != nil {
				continue
			}
			if !first {
				b.WriteByte(',')
			}
			first = false
			an, ae, ats := parseIdent(cv.Author)
			cn, ce, cts := parseIdent(cv.Committer)
			b.WriteString(`{"sha":`)
			jstr(&b, oid)
			b.WriteString(`,"commit":{"message":`)
			jstr(&b, cv.Message)
			b.WriteString(`,"author":{"name":`)
			jstr(&b, an)
			b.WriteString(`,"email":`)
			jstr(&b, ae)
			b.WriteString(`,"date":`)
			jstr(&b, isoTime(ats))
			b.WriteString(`},"committer":{"name":`)
			jstr(&b, cn)
			b.WriteString(`,"email":`)
			jstr(&b, ce)
			b.WriteString(`,"date":`)
			jstr(&b, isoTime(cts))
			b.WriteString(`}},"author":`)
			if ae == viewerEmail {
				b.WriteString(`{"login":`)
				jstr(&b, viewerLogin)
				b.WriteString(`,"id":`)
				b.WriteString(strconv.Itoa(viewerID))
				b.WriteString(`}`)
			} else {
				b.WriteString("null")
			}
			b.WriteString(`,"committer":`)
			if ce == viewerEmail {
				b.WriteString(`{"login":`)
				jstr(&b, viewerLogin)
				b.WriteString(`,"id":`)
				b.WriteString(strconv.Itoa(viewerID))
				b.WriteString(`}`)
			} else {
				b.WriteString("null")
			}
			b.WriteString(`,"parents":[`)
			for i, p := range cv.Parents {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteString(`{"sha":`)
				jstr(&b, p)
				b.WriteString(`}`)
			}
			b.WriteString(`]}`)
			stack = append(stack, cv.Parents...)
		}
	}
	b.WriteByte(']')
	writeJSON(w, 200, b.Bytes())
}

// ---- contents ----

// resolveRefTree returns the tree oid for a ref (branch name, full ref, oid
// or "" for the default branch).
func (r *Repo) resolveRefTree(ref string) string {
	if ref == "" {
		return r.commitTree(r.refs["refs/heads/"+r.DefaultBranch])
	}
	if isOID(ref) {
		return r.commitTree(ref)
	}
	if tip, ok := r.refs["refs/heads/"+ref]; ok {
		return r.commitTree(tip)
	}
	if tip, ok := r.refs[ref]; ok {
		if obj := r.objects[tip]; obj != nil {
			if obj.typ == 1 {
				return r.commitTree(tip)
			}
			if obj.typ == 2 {
				return tip
			}
		}
	}
	if tip, ok := r.refs["refs/tags/"+ref]; ok {
		if obj := r.objects[tip]; obj != nil && obj.typ == 4 {
			if tv, err := parseTag(obj.data); err == nil {
				return r.commitTree(tv.Object)
			}
		}
		return r.commitTree(tip)
	}
	return ""
}

func fileContentJSON(path, oid string, obj *Object) []byte {
	var b bytes.Buffer
	base := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		base = path[i+1:]
	}
	b.WriteString(`{"type":"file","encoding":"base64","size":`)
	b.WriteString(strconv.Itoa(len(obj.data)))
	b.WriteString(`,"name":`)
	jstr(&b, base)
	b.WriteString(`,"path":`)
	jstr(&b, path)
	b.WriteString(`,"sha":`)
	jstr(&b, oid)
	b.WriteString(`,"content":`)
	jstr(&b, base64.StdEncoding.EncodeToString(obj.data))
	b.WriteString(`,"url":"","html_url":"","download_url":""}`)
	return b.Bytes()
}

func (s *server) restContents(w http.ResponseWriter, r *http.Request, repo *Repo, tail []string) {
	path := strings.Join(tail, "/")
	switch r.Method {
	case http.MethodGet:
		s.store.mu.RLock()
		treeOID := repo.resolveRefTree(r.URL.Query().Get("ref"))
		if treeOID == "" {
			s.store.mu.RUnlock()
			writeErr(w, 404, "Not Found")
			return
		}
		oid, isDir, found := repo.lookupPath(treeOID, path)
		if !found {
			s.store.mu.RUnlock()
			writeErr(w, 404, "Not Found")
			return
		}
		if isDir {
			entries, _ := parseTree(repo.objects[oid].data)
			var b bytes.Buffer
			b.WriteByte('[')
			for i, e := range entries {
				if i > 0 {
					b.WriteByte(',')
				}
				typ := "file"
				if e.Mode == "40000" {
					typ = "dir"
				}
				b.WriteString(`{"type":`)
				jstr(&b, typ)
				b.WriteString(`,"name":`)
				jstr(&b, e.Name)
				b.WriteString(`,"path":`)
				if path == "" {
					jstr(&b, e.Name)
				} else {
					jstr(&b, path+"/"+e.Name)
				}
				b.WriteString(`,"sha":`)
				jstr(&b, e.OID)
				if typ == "file" {
					b.WriteString(`,"size":`)
					if obj := repo.objects[e.OID]; obj != nil {
						b.WriteString(strconv.Itoa(len(obj.data)))
					} else {
						b.WriteString("0")
					}
				}
				b.WriteString(`,"url":""}`)
			}
			b.WriteByte(']')
			s.store.mu.RUnlock()
			writeJSON(w, 200, b.Bytes())
			return
		}
		obj := repo.objects[oid]
		if obj == nil || obj.typ != 3 {
			s.store.mu.RUnlock()
			writeErr(w, 404, "Not Found")
			return
		}
		out := fileContentJSON(path, oid, obj)
		s.store.mu.RUnlock()
		writeJSON(w, 200, out)
	case http.MethodPut:
		s.restPutContents(w, r, repo, path)
	case http.MethodDelete:
		s.restDeleteContents(w, r, repo, path)
	default:
		writeErr(w, 404, "Not Found")
	}
}

func (s *server) restPutContents(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	body, _ := readBody(r, 1<<24)
	m, err := decodeJSON(body)
	if err != nil {
		writeErr(w, 400, "Problems parsing JSON")
		return
	}
	contentB64 := strval(m, "content")
	if contentB64 == "" {
		writeErr(w, 422, "content is required")
		return
	}
	content, err := base64.StdEncoding.DecodeString(contentB64)
	if err != nil {
		// Tolerate base64 with embedded newlines.
		cleaned := strings.Map(func(c rune) rune {
			if c == '\n' || c == '\r' || c == ' ' || c == '\t' {
				return -1
			}
			return c
		}, contentB64)
		content, err = base64.StdEncoding.DecodeString(cleaned)
		if err != nil {
			writeErr(w, 422, "content is not valid base64")
			return
		}
	}
	message := strval(m, "message")
	if message == "" {
		message = "Update " + path
	}
	branch := strval(m, "branch")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	sha := strval(m, "sha")

	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	tip := repo.refs["refs/heads/"+branch]
	var treeOID string
	if tip == "" {
		// Unborn branch: a first commit creates it.
		treeOID = ""
	} else {
		treeOID = repo.commitTree(tip)
	}
	// Whether the path already exists as a file decides create vs update —
	// not whether the branch already has commits.
	_, isDir, found := repo.lookupPath(treeOID, path)
	isUpdate := tip != "" && found && !isDir
	if found && isDir {
		writeErr(w, 422, "path is a directory")
		return
	}
	if isUpdate {
		curOID, _, _ := repo.lookupPath(treeOID, path)
		if sha == "" {
			writeErr(w, 422, "sha is required to update a file")
			return
		}
		if curOID != sha {
			writeErr(w, 409, "sha does not match")
			return
		}
	} else if sha != "" {
		writeErr(w, 422, "sha must not be provided for a new file")
		return
	}
	blobOID := s.store.putObject(repo, 3, content)
	newTree, err := s.store.updatePath(repo, treeOID, path, blobOID)
	if err != nil {
		writeErr(w, 500, "failed to update tree")
		return
	}
	parents := []string{}
	if tip != "" {
		parents = []string{tip}
	}
	commitOID := s.store.makeCommit(repo, newTree, parents, message)
	repo.refs["refs/heads/"+branch] = commitOID
	repo.bump()
	commitJSON := s.store.commitJSON(repo, commitOID, message)

	var b bytes.Buffer
	b.WriteString(`{"content":`)
	b.Write(fileContentJSON(path, blobOID, &Object{typ: 3, data: content}))
	b.WriteString(`,"commit":`)
	b.Write(commitJSON)
	b.WriteString(`}`)
	status := 201
	if isUpdate {
		status = 200
	}
	writeJSON(w, status, b.Bytes())
}

func (s *server) restDeleteContents(w http.ResponseWriter, r *http.Request, repo *Repo, path string) {
	body, _ := readBody(r, 1<<22)
	m, err := decodeJSON(body)
	if err != nil {
		writeErr(w, 400, "Problems parsing JSON")
		return
	}
	message := strval(m, "message")
	if message == "" {
		message = "Delete " + path
	}
	branch := strval(m, "branch")
	if branch == "" {
		branch = repo.DefaultBranch
	}
	sha := strval(m, "sha")

	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	tip := repo.refs["refs/heads/"+branch]
	if tip == "" {
		writeErr(w, 404, "Not Found")
		return
	}
	treeOID := repo.commitTree(tip)
	curOID, isDir, found := repo.lookupPath(treeOID, path)
	if !found || isDir {
		writeErr(w, 404, "Not Found")
		return
	}
	if sha != "" && curOID != sha {
		writeErr(w, 409, "sha does not match")
		return
	}
	newTree, err := s.store.updatePath(repo, treeOID, path, "")
	if err != nil {
		writeErr(w, 500, "failed to update tree")
		return
	}
	commitOID := s.store.makeCommit(repo, newTree, []string{tip}, message)
	repo.refs["refs/heads/"+branch] = commitOID
	repo.bump()
	commitJSON := s.store.commitJSON(repo, commitOID, message)
	var b bytes.Buffer
	b.WriteString(`{"commit":`)
	b.Write(commitJSON)
	b.WriteString(`}`)
	writeJSON(w, 200, b.Bytes())
}

// makeCommit creates a commit object (deterministic timestamp) and returns
// its oid.
func (s *Store) makeCommit(r *Repo, treeOID string, parents []string, message string) string {
	ts := s.nextTS()
	ident := viewerName + " <" + viewerEmail + "> " + strconv.Itoa(int(ts)) + " +0000"
	commit := buildCommit(treeOID, parents, ident, ident, message)
	return s.putObject(r, 1, commit)
}

func (s *Store) commitJSON(r *Repo, oid, message string) []byte {
	obj := r.objects[oid]
	var b bytes.Buffer
	if obj == nil {
		return []byte(`{"sha":"` + oid + `"}`)
	}
	cv, err := parseCommit(obj.data)
	if err != nil {
		return []byte(`{"sha":"` + oid + `"}`)
	}
	an, ae, ats := parseIdent(cv.Author)
	cn, ce, cts := parseIdent(cv.Committer)
	b.WriteString(`{"sha":`)
	jstr(&b, oid)
	b.WriteString(`,"message":`)
	jstr(&b, message)
	b.WriteString(`,"author":{"name":`)
	jstr(&b, an)
	b.WriteString(`,"email":`)
	jstr(&b, ae)
	b.WriteString(`,"date":`)
	jstr(&b, isoTime(ats))
	b.WriteString(`},"committer":{"name":`)
	jstr(&b, cn)
	b.WriteString(`,"email":`)
	jstr(&b, ce)
	b.WriteString(`,"date":`)
	jstr(&b, isoTime(cts))
	b.WriteString(`}}`)
	return b.Bytes()
}

// ---- labels ----

func (s *server) restCreateLabel(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, _ := readBody(r, 1<<22)
	m, err := decodeJSON(body)
	if err != nil {
		writeErr(w, 400, "Problems parsing JSON")
		return
	}
	name := strval(m, "name")
	if name == "" {
		writeErr(w, 422, "name is required")
		return
	}
	color := strval(m, "color")
	desc := strval(m, "description")
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if _, exists := repo.labels[name]; exists {
		writeErr(w, 422, "Label already exists")
		return
	}
	repo.labels[name] = &Label{Name: name, Color: color, Description: desc}
	var b bytes.Buffer
	b.WriteString(`{"name":`)
	jstr(&b, name)
	b.WriteString(`,"color":`)
	jstr(&b, color)
	b.WriteString(`,"description":`)
	jstr(&b, desc)
	b.WriteString(`}`)
	writeJSON(w, 201, b.Bytes())
}

func (s *server) restListLabels(w http.ResponseWriter, r *http.Request, repo *Repo) {
	s.store.mu.RLock()
	defer s.store.mu.RUnlock()
	var names []string
	for name := range repo.labels {
		names = append(names, name)
	}
	sort.Strings(names)
	var b bytes.Buffer
	b.WriteByte('[')
	for i, name := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		lb := repo.labels[name]
		b.WriteString(`{"name":`)
		jstr(&b, lb.Name)
		b.WriteString(`,"color":`)
		jstr(&b, lb.Color)
		b.WriteString(`,"description":`)
		jstr(&b, lb.Description)
		b.WriteString(`}`)
	}
	b.WriteByte(']')
	writeJSON(w, 200, b.Bytes())
}

// ---- issues ----

func (s *server) restIssues(w http.ResponseWriter, r *http.Request, repo *Repo, tail []string) {
	if len(tail) == 0 {
		switch r.Method {
		case http.MethodGet:
			s.restListIssues(w, r, repo)
		case http.MethodPost:
			s.restCreateIssue(w, r, repo)
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}
	num, err := strconv.Atoi(tail[0])
	if err != nil {
		writeErr(w, 404, "Not Found")
		return
	}
	if len(tail) == 1 {
		switch r.Method {
		case http.MethodGet:
			s.store.mu.RLock()
			is, ok := repo.issueByNum[num]
			s.store.mu.RUnlock()
			if !ok {
				writeErr(w, 404, "Not Found")
				return
			}
			writeJSON(w, 200, is.jsonCache)
		case http.MethodPatch:
			body, _ := readBody(r, 1<<22)
			m, err := decodeJSON(body)
			if err != nil {
				writeErr(w, 400, "Problems parsing JSON")
				return
			}
			s.store.mu.Lock()
			is, ok := repo.issueByNum[num]
			if !ok {
				s.store.mu.Unlock()
				writeErr(w, 404, "Not Found")
				return
			}
			if st, ok := m["state"]; ok {
				if sv, ok := st.(string); ok {
					is.State = sv
					is.UpdatedAt = isoTime(s.store.nextTS())
				}
			}
			if t, ok := m["title"]; ok {
				if tv, ok := t.(string); ok {
					is.Title = tv
					is.UpdatedAt = isoTime(s.store.nextTS())
				}
			}
			if bd, ok := m["body"]; ok {
				if bv, ok := bd.(string); ok {
					is.Body = bv
					is.UpdatedAt = isoTime(s.store.nextTS())
				}
			}
			s.store.refreshIssueJSON(repo, is)
			b := is.jsonCache
			s.store.mu.Unlock()
			writeJSON(w, 200, b)
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}
	if len(tail) == 2 && tail[1] == "comments" {
		switch r.Method {
		case http.MethodGet:
			s.store.mu.RLock()
			_, ok := repo.issueByNum[num]
			cmts := repo.comments[num]
			s.store.mu.RUnlock()
			if !ok {
				writeErr(w, 404, "Not Found")
				return
			}
			var b bytes.Buffer
			b.WriteByte('[')
			for i, c := range cmts {
				if i > 0 {
					b.WriteByte(',')
				}
				b.Write(buildCommentJSON(c))
			}
			b.WriteByte(']')
			writeJSON(w, 200, b.Bytes())
		case http.MethodPost:
			body, _ := readBody(r, 1<<22)
			m, err := decodeJSON(body)
			if err != nil {
				writeErr(w, 400, "Problems parsing JSON")
				return
			}
			text := strval(m, "body")
			s.store.mu.Lock()
			is, ok := repo.issueByNum[num]
			if !ok {
				s.store.mu.Unlock()
				writeErr(w, 404, "Not Found")
				return
			}
			c := &Comment{ID: s.store.nextID(), Body: text, CreatedAt: isoTime(s.store.nextTS())}
			c.NodeID = globalID("Comment", repo.fullName()+":"+strconv.Itoa(num)+":"+strconv.Itoa(c.ID))
			c.UpdatedAt = c.CreatedAt
			s.store.nodeCmt[c.NodeID] = c
			repo.comments[num] = append(repo.comments[num], c)
			s.store.refreshIssueJSON(repo, is)
			b := buildCommentJSON(c)
			s.store.mu.Unlock()
			writeJSON(w, 201, b)
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}
	writeErr(w, 404, "Not Found")
}

// buildCommentJSON renders one comment as REST JSON.
func buildCommentJSON(c *Comment) []byte {
	var b bytes.Buffer
	b.WriteString(`{"id":`)
	b.WriteString(strconv.Itoa(c.ID))
	b.WriteString(`,"node_id":`)
	jstr(&b, c.NodeID)
	b.WriteString(`,"body":`)
	jstr(&b, c.Body)
	b.WriteString(`,"user":{"login":`)
	jstr(&b, viewerLogin)
	b.WriteString(`,"id":`)
	b.WriteString(strconv.Itoa(viewerID))
	b.WriteString(`,"type":"User"},"created_at":`)
	jstr(&b, c.CreatedAt)
	b.WriteString(`,"updated_at":`)
	jstr(&b, c.UpdatedAt)
	b.WriteString(`}`)
	return b.Bytes()
}

func (s *server) restListIssues(w http.ResponseWriter, r *http.Request, repo *Repo) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	direction := q.Get("direction")
	if direction == "" {
		direction = "desc"
	}
	perPage := atoiDefault(q.Get("per_page"), 30)
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	page := atoiDefault(q.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	s.store.mu.RLock()
	var list []*Issue
	for _, is := range repo.issues {
		if state != "all" && is.State != state {
			continue
		}
		list = append(list, is)
	}
	sort.Slice(list, func(i, j int) bool {
		if direction == "asc" {
			return list[i].Created < list[j].Created
		}
		return list[i].Created > list[j].Created
	})
	total := len(list)
	start := (page - 1) * perPage
	var out bytes.Buffer
	out.WriteByte('[')
	first := true
	for i := start; i < total && i < start+perPage; i++ {
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.Write(list[i].jsonCache)
	}
	out.WriteByte(']')
	s.store.mu.RUnlock()
	writeJSON(w, 200, out.Bytes())
}

func (s *server) restCreateIssue(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, _ := readBody(r, 1<<22)
	m, err := decodeJSON(body)
	if err != nil {
		writeErr(w, 400, "Problems parsing JSON")
		return
	}
	title := strval(m, "title")
	if title == "" {
		writeErr(w, 422, "title is required")
		return
	}
	// Validate labels before creating anything.
	var labelNames []string
	if raw, ok := m["labels"]; ok {
		if arr, ok := raw.([]any); ok {
			for _, v := range arr {
				if name, ok := v.(string); ok {
					labelNames = append(labelNames, name)
				}
			}
		}
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	for _, name := range labelNames {
		if _, ok := repo.labels[name]; !ok {
			writeErr(w, 422, "Label does not exist")
			return
		}
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
		Body:    strval(m, "body"),
		State:   "open",
		Labels:  labelNames,
		Created: s.store.nextTS(),
	}
	is.CreatedAt = isoTime(is.Created)
	is.UpdatedAt = is.CreatedAt
	repo.issues = append(repo.issues, is)
	repo.issueByNum[num] = is
	s.store.nodeIssue[is.NodeID] = is
	s.store.refreshIssueJSON(repo, is)
	writeJSON(w, 201, is.jsonCache)
}

// ---- pulls ----

func (s *server) restPulls(w http.ResponseWriter, r *http.Request, repo *Repo, tail []string) {
	if len(tail) == 0 {
		switch r.Method {
		case http.MethodGet:
			s.restListPulls(w, r, repo)
		case http.MethodPost:
			s.restCreatePull(w, r, repo)
		default:
			writeErr(w, 404, "Not Found")
		}
		return
	}
	num, err := strconv.Atoi(tail[0])
	if err != nil {
		writeErr(w, 404, "Not Found")
		return
	}
	if len(tail) == 1 {
		if r.Method == http.MethodGet {
			s.store.mu.RLock()
			pr, ok := repo.prByNum[num]
			var b []byte
			if ok {
				b = s.store.buildPullJSON(repo, pr)
			}
			s.store.mu.RUnlock()
			if !ok {
				writeErr(w, 404, "Not Found")
				return
			}
			writeJSON(w, 200, b)
			return
		}
		writeErr(w, 404, "Not Found")
		return
	}
	if len(tail) == 2 && tail[1] == "files" && r.Method == http.MethodGet {
		s.store.mu.RLock()
		pr, ok := repo.prByNum[num]
		var out []byte
		if ok {
			out = s.store.prFilesJSON(repo, pr)
		}
		s.store.mu.RUnlock()
		if !ok {
			writeErr(w, 404, "Not Found")
			return
		}
		writeJSON(w, 200, out)
		return
	}
	if len(tail) == 2 && tail[1] == "merge" && r.Method == http.MethodPut {
		s.restMergePull(w, r, repo, num)
		return
	}
	writeErr(w, 404, "Not Found")
}

func (s *server) restListPulls(w http.ResponseWriter, r *http.Request, repo *Repo) {
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	perPage := atoiDefault(q.Get("per_page"), 30)
	if perPage < 1 {
		perPage = 1
	}
	if perPage > 100 {
		perPage = 100
	}
	page := atoiDefault(q.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	s.store.mu.RLock()
	var list []*Pull
	for _, pr := range repo.pulls {
		if state != "all" && pr.State != state {
			continue
		}
		list = append(list, pr)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Number > list[j].Number })
	total := len(list)
	start := (page - 1) * perPage
	var out bytes.Buffer
	out.WriteByte('[')
	first := true
	for i := start; i < total && i < start+perPage; i++ {
		if !first {
			out.WriteByte(',')
		}
		first = false
		out.Write(s.store.buildPullJSON(repo, list[i]))
	}
	out.WriteByte(']')
	s.store.mu.RUnlock()
	writeJSON(w, 200, out.Bytes())
}

func (s *server) restCreatePull(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, _ := readBody(r, 1<<22)
	m, err := decodeJSON(body)
	if err != nil {
		writeErr(w, 400, "Problems parsing JSON")
		return
	}
	title := strval(m, "title")
	head := strval(m, "head")
	base := strval(m, "base")
	if title == "" || head == "" || base == "" {
		writeErr(w, 422, "title, head and base are required")
		return
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	_, headOK := repo.refs["refs/heads/"+head]
	_, baseOK := repo.refs["refs/heads/"+base]
	if !headOK || !baseOK {
		writeErr(w, 422, "head or base branch does not exist")
		return
	}
	for _, pr := range repo.pulls {
		if pr.HeadRef == head && pr.BaseRef == base && pr.State == "open" {
			writeErr(w, 422, "a pull request already exists for this branch")
			return
		}
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
		Body:    strval(m, "body"),
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
	writeJSON(w, 201, s.store.buildPullJSON(repo, pr))
}

// mergePullLocked performs the merge of an open PR (caller holds the write
// lock). It returns the merge commit oid, or "" with an error message.
func (s *Store) mergePullLocked(r *Repo, pr *Pull, method string) (string, string) {
	headTip := r.refs["refs/heads/"+pr.HeadRef]
	baseTip := r.refs["refs/heads/"+pr.BaseRef]
	if headTip == "" || baseTip == "" {
		return "", "Pull request cannot be merged"
	}
	if prConflict(r, pr) {
		return "", "Pull request cannot be merged"
	}
	base := r.mergeBase(headTip, baseTip)
	if base == "" {
		return "", "Pull request cannot be merged"
	}
	headCommit, err1 := parseCommit(r.objects[headTip].data)
	baseCommit, err2 := parseCommit(r.objects[baseTip].data)
	baseBaseCommit, err3 := parseCommit(r.objects[base].data)
	if err1 != nil || err2 != nil || err3 != nil {
		return "", "Pull request cannot be merged"
	}
	newTree := s.applyDiffToTree(r, baseCommit.Tree, baseBaseCommit.Tree, headCommit.Tree)
	var mergeCommit string
	if method == "squash" {
		msg := pr.Title + " (#" + strconv.Itoa(pr.Number) + ")"
		mergeCommit = s.makeCommit(r, newTree, []string{baseTip}, msg)
	} else {
		mergeCommit = s.makeCommit(r, newTree, []string{baseTip, headTip}, "Merge "+pr.HeadRef+" into "+pr.BaseRef)
	}
	r.refs["refs/heads/"+pr.BaseRef] = mergeCommit
	pr.Merged = true
	pr.State = "closed"
	pr.MergedAt = isoTime(s.nextTS())
	pr.MergeSHA = mergeCommit
	pr.ClosedAt = pr.MergedAt
	pr.UpdatedAt = pr.MergedAt
	r.bump()
	return mergeCommit, ""
}

func (s *server) restMergePull(w http.ResponseWriter, r *http.Request, repo *Repo, num int) {
	body, _ := readBody(r, 1<<22)
	m, _ := decodeJSON(body)
	method := "merge"
	if m != nil {
		if v := strval(m, "merge_method"); v != "" {
			method = v
		}
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	pr, ok := repo.prByNum[num]
	if !ok {
		writeErr(w, 404, "Not Found")
		return
	}
	if pr.State != "open" || pr.Merged {
		writeErr(w, 405, "Pull request is already merged")
		return
	}
	mergeCommit, errMsg := s.store.mergePullLocked(repo, pr, method)
	if errMsg != "" {
		writeErr(w, 405, errMsg)
		return
	}
	var b bytes.Buffer
	b.WriteString(`{"sha":`)
	jstr(&b, mergeCommit)
	b.WriteString(`,"merged":true,"message":"Pull request successfully merged"}`)
	writeJSON(w, 200, b.Bytes())
}

// prFilesJSON renders a PR's changed files as REST JSON: the diff of the
// head tip against the merge base.
func (s *Store) prFilesJSON(r *Repo, pr *Pull) []byte {
	headTip := r.refs["refs/heads/"+pr.HeadRef]
	baseTip := r.refs["refs/heads/"+pr.BaseRef]
	var b bytes.Buffer
	b.WriteByte('[')
	if headTip != "" && baseTip != "" {
		base := r.mergeBase(headTip, baseTip)
		if base != "" {
			headTree := r.commitTree(headTip)
			baseTree := r.commitTree(base)
			changes := r.diffTrees(baseTree, headTree)
			for i, ch := range changes {
				if i > 0 {
					b.WriteByte(',')
				}
				status := ch.Status
				if status == "modified" {
					status = "modified"
				}
				b.WriteString(`{"filename":`)
				jstr(&b, ch.Path)
				b.WriteString(`,"status":`)
				jstr(&b, status)
				b.WriteString(`,"additions":`)
				b.WriteString(strconv.Itoa(ch.Additions))
				b.WriteString(`,"deletions":`)
				b.WriteString(strconv.Itoa(ch.Deletions))
				b.WriteString(`,"changes":`)
				b.WriteString(strconv.Itoa(ch.Additions + ch.Deletions))
				b.WriteString(`}`)
			}
		}
	}
	b.WriteByte(']')
	return b.Bytes()
}
