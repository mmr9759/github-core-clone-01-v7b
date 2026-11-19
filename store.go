// store.go: the in-memory forge store.
//
// One Store holds every repository. A single RWMutex guards it; the timed
// workloads are sequential per workload (push/read/write) or read-heavy
// (clone), so one lock is the fastest simple shape.
//
// Hot-path rule: anything the benchmark reads in a tight loop is served from
// pre-serialized bytes. An issue's GET response is built once at creation and
// rebuilt only when the issue mutates; the same holds for a repository's
// object. The git advertisements and the depth-1 fetch responses are cached
// per repository revision the same way.
package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// baseEpoch is the fixed epoch every minted timestamp derives from.
	baseEpoch = 1700000000
	zeroOID   = "0000000000000000000000000000000000000000"
)

func isOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// globalID mints an opaque, stable Relay-style id for a node.
func globalID(kind, key string) string {
	h := sha1.Sum([]byte(kind + ":" + key))
	return kind + "_" + hex.EncodeToString(h[:12])
}

func isoTime(sec int64) string {
	return time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05Z")
}

// ---- objects ----

type Object struct {
	typ  byte // 1 commit, 2 tree, 3 blob, 4 tag
	data []byte
}

func (o *Object) typName() string {
	switch o.typ {
	case 1:
		return "commit"
	case 2:
		return "tree"
	case 3:
		return "blob"
	case 4:
		return "tag"
	}
	return ""
}

func typeCode(name string) byte {
	switch name {
	case "commit":
		return 1
	case "tree":
		return 2
	case "blob":
		return 3
	case "tag":
		return 4
	}
	return 0
}

func hashObject(typ byte, data []byte) string {
	h := sha1.New()
	h.Write([]byte(objectTypeName(typ) + " " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func objectTypeName(typ byte) string {
	switch typ {
	case 1:
		return "commit"
	case 2:
		return "tree"
	case 3:
		return "blob"
	case 4:
		return "tag"
	}
	return ""
}

// ---- labels, issues, comments, pulls ----

type Label struct {
	Name        string
	Color       string
	Description string
}

type Issue struct {
	repo      *Repo
	Number    int
	ID        int
	NodeID    string
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []string
	CreatedAt string
	UpdatedAt string
	Created   int64 // epoch seconds, deterministic
	jsonCache []byte
}

type Comment struct {
	ID        int
	NodeID    string
	Body      string
	CreatedAt string
	UpdatedAt string
}

type Pull struct {
	repo         *Repo
	Number       int
	ID           int
	NodeID       string
	Title        string
	Body         string
	State        string // "open" | "closed"
	HeadRef      string
	BaseRef      string
	Merged       bool
	MergeSHA     string
	MergedAt     string
	CreatedAt    string
	UpdatedAt    string
	ClosedAt     string
	Created      int64
}

type Repo struct {
	ID            int
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
	CreatedSeq    int
	CreatedAt     string

	refs    map[string]string // full ref name -> oid
	objects map[string]*Object

	issues     []*Issue
	issueByNum map[int]*Issue
	pulls      []*Pull
	prByNum    map[int]*Pull
	labels     map[string]*Label
	comments   map[int][]*Comment // issue number -> comments

	nextNumber    int // shared issue+PR counter
	nextCommentID int

	rev int // bumped on any mutation; invalidates the caches below

	repoJSON []byte // cached repository object
	advV0    []byte // cached v0 upload-pack advertisement
	advV2    []byte // cached v2 upload-pack advertisement
	advRecv  []byte // cached receive-pack advertisement
	lsCache  map[string][]byte
	fetchCache map[string][]byte
}

func (r *Repo) fullName() string { return r.Owner + "/" + r.Name }

// ---- store ----

type Store struct {
	mu sync.RWMutex

	repos     map[string]*Repo // "owner/name" -> repo
	order     []*Repo          // creation order
	seq       uint64           // deterministic timestamp counter
	nextRepo  int
	nextObjID int // numeric ids for issues/PRs/comments/repos share one counter
	nodeRepos map[string]*Repo
	nodeIssue map[string]*Issue
	nodePR    map[string]*Pull
	nodeCmt   map[string]*Comment
}

func NewStore() *Store {
	return &Store{
		repos:     make(map[string]*Repo),
		nodeRepos: make(map[string]*Repo),
		nodeIssue: make(map[string]*Issue),
		nodePR:    make(map[string]*Pull),
		nodeCmt:   make(map[string]*Comment),
	}
}

// nextTS mints the next deterministic timestamp (seconds since baseEpoch).
func (s *Store) nextTS() int64 {
	n := atomic.AddUint64(&s.seq, 1)
	return baseEpoch + int64(n)
}

func (s *Store) nextID() int {
	s.nextObjID++
	return s.nextObjID
}

// lookupRepo returns the repo or nil. Callers hold at least a read lock.
func (s *Store) lookupRepo(owner, name string) *Repo {
	if owner != viewerLogin {
		return nil
	}
	return s.repos[owner+"/"+name]
}

func (s *Store) createRepo(name, description string, private bool) (*Repo, bool) {
	key := viewerLogin + "/" + name
	if _, ok := s.repos[key]; ok {
		return nil, false
	}
	s.nextRepo++
	now := s.nextTS()
	r := &Repo{
		ID:            s.nextRepo,
		Owner:         viewerLogin,
		Name:          name,
		Description:   description,
		Private:       private,
		DefaultBranch: "main",
		CreatedSeq:    s.nextRepo,
		CreatedAt:     isoTime(now),
		refs:          make(map[string]string),
		objects:       make(map[string]*Object),
		issueByNum:    make(map[int]*Issue),
		prByNum:       make(map[int]*Pull),
		labels:        make(map[string]*Label),
		comments:      make(map[int][]*Comment),
		nextNumber:    1,
		nextCommentID: 1,
		lsCache:       make(map[string][]byte),
		fetchCache:    make(map[string][]byte),
	}
	s.repos[key] = r
	s.order = append(s.order, r)
	s.nodeRepos[globalID("R", r.fullName())] = r
	r.rebuildRepoJSON()
	return r, true
}

func (s *Store) deleteRepo(r *Repo) {
	delete(s.repos, r.fullName())
	for i, o := range s.order {
		if o == r {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	for id, repo := range s.nodeRepos {
		if repo == r {
			delete(s.nodeRepos, id)
		}
	}
}

// ---- JSON builders ----
// These build the exact bytes the API returns. Strings go through
// encoding/json for correct escaping; numbers are appended directly.

func jstr(b *bytes.Buffer, s string) {
	enc, _ := json.Marshal(s)
	b.Write(enc)
}

func (s *Store) userJSON() []byte {
	var b bytes.Buffer
	b.WriteString(`{"login":`)
	jstr(&b, viewerLogin)
	b.WriteString(`,"id":`)
	b.WriteString(strconv.Itoa(viewerID))
	b.WriteString(`,"node_id":`)
	jstr(&b, globalID("U", viewerLogin))
	b.WriteString(`,"type":"User"}`)
	return b.Bytes()
}

func (r *Repo) rebuildRepoJSON() {
	var b bytes.Buffer
	b.WriteString(`{"id":`)
	b.WriteString(strconv.Itoa(r.ID))
	b.WriteString(`,"node_id":`)
	jstr(&b, globalID("R", r.fullName()))
	b.WriteString(`,"name":`)
	jstr(&b, r.Name)
	b.WriteString(`,"full_name":`)
	jstr(&b, r.fullName())
	b.WriteString(`,"private":`)
	b.WriteString(strconv.FormatBool(r.Private))
	b.WriteString(`,"owner":{"login":`)
	jstr(&b, r.Owner)
	b.WriteString(`,"id":`)
	b.WriteString(strconv.Itoa(viewerID))
	b.WriteString(`,"node_id":`)
	jstr(&b, globalID("U", r.Owner))
	b.WriteString(`,"type":"User"},"description":`)
	if r.Description == "" {
		b.WriteString("null")
	} else {
		jstr(&b, r.Description)
	}
	b.WriteString(`,"default_branch":`)
	jstr(&b, r.DefaultBranch)
	b.WriteString(`,"created_at":`)
	jstr(&b, r.CreatedAt)
	b.WriteString(`,"updated_at":`)
	jstr(&b, r.CreatedAt)
	b.WriteString(`,"has_issues":true,"has_projects":true,"has_wiki":true,"fork":false,"archived":false,"disabled":false,"visibility":`)
	if r.Private {
		b.WriteString(`"private"`)
	} else {
		b.WriteString(`"public"`)
	}
	b.WriteString(`,"license":null}`)
	r.repoJSON = b.Bytes()
}

func (s *Store) buildIssueJSON(r *Repo, iss *Issue) []byte {
	var b bytes.Buffer
	b.WriteString(`{"id":`)
	b.WriteString(strconv.Itoa(iss.ID))
	b.WriteString(`,"node_id":`)
	jstr(&b, iss.NodeID)
	b.WriteString(`,"number":`)
	b.WriteString(strconv.Itoa(iss.Number))
	b.WriteString(`,"title":`)
	jstr(&b, iss.Title)
	b.WriteString(`,"body":`)
	jstr(&b, iss.Body)
	b.WriteString(`,"state":`)
	jstr(&b, iss.State)
	b.WriteString(`,"labels":[`)
	for i, name := range iss.Labels {
		if i > 0 {
			b.WriteByte(',')
		}
		lab := r.labels[name]
		color := "ededed"
		desc := ""
		if lab != nil {
			color = lab.Color
			desc = lab.Description
		}
		b.WriteString(`{"name":`)
		jstr(&b, name)
		b.WriteString(`,"color":`)
		jstr(&b, color)
		b.WriteString(`,"description":`)
		jstr(&b, desc)
		b.WriteString(`}`)
	}
	b.WriteString(`],"user":{"login":`)
	jstr(&b, viewerLogin)
	b.WriteString(`,"id":`)
	b.WriteString(strconv.Itoa(viewerID))
	b.WriteString(`,"node_id":`)
	jstr(&b, globalID("U", viewerLogin))
	b.WriteString(`,"type":"User"},"assignee":null,"assignees":[],"comments":`)
	b.WriteString(strconv.Itoa(len(r.comments[iss.Number])))
	b.WriteString(`,"created_at":`)
	jstr(&b, iss.CreatedAt)
	b.WriteString(`,"updated_at":`)
	jstr(&b, iss.UpdatedAt)
	b.WriteString(`,"closed_at":`)
	if iss.State == "closed" {
		jstr(&b, iss.UpdatedAt)
	} else {
		b.WriteString("null")
	}
	b.WriteString(`}`)
	return b.Bytes()
}

// refreshIssueJSON rebuilds an issue's cached response bytes.
func (s *Store) refreshIssueJSON(r *Repo, iss *Issue) {
	iss.jsonCache = s.buildIssueJSON(r, iss)
}

func (s *Store) buildPullJSON(r *Repo, pr *Pull) []byte {
	headSHA := r.refs["refs/heads/"+pr.HeadRef]
	baseSHA := r.refs["refs/heads/"+pr.BaseRef]
	var b bytes.Buffer
	b.WriteString(`{"id":`)
	b.WriteString(strconv.Itoa(pr.ID))
	b.WriteString(`,"node_id":`)
	jstr(&b, pr.NodeID)
	b.WriteString(`,"number":`)
	b.WriteString(strconv.Itoa(pr.Number))
	b.WriteString(`,"state":`)
	jstr(&b, pr.State)
	b.WriteString(`,"title":`)
	jstr(&b, pr.Title)
	b.WriteString(`,"body":`)
	jstr(&b, pr.Body)
	b.WriteString(`,"head":{"ref":`)
	jstr(&b, pr.HeadRef)
	b.WriteString(`,"sha":`)
	jstr(&b, headSHA)
	b.WriteString(`,"label":`)
	jstr(&b, r.Owner+":"+pr.HeadRef)
	b.WriteString(`,"user":{"login":`)
	jstr(&b, r.Owner)
	b.WriteString(`,"id":`)
	b.WriteString(strconv.Itoa(viewerID))
	b.WriteString(`},"repo":{"name":`)
	jstr(&b, r.Name)
	b.WriteString(`,"full_name":`)
	jstr(&b, r.fullName())
	b.WriteString(`}}`)
	b.WriteString(`,"base":{"ref":`)
	jstr(&b, pr.BaseRef)
	b.WriteString(`,"sha":`)
	jstr(&b, baseSHA)
	b.WriteString(`,"label":`)
	jstr(&b, r.Owner+":"+pr.BaseRef)
	b.WriteString(`,"user":{"login":`)
	jstr(&b, r.Owner)
	b.WriteString(`,"id":`)
	b.WriteString(strconv.Itoa(viewerID))
	b.WriteString(`},"repo":{"name":`)
	jstr(&b, r.Name)
	b.WriteString(`,"full_name":`)
	jstr(&b, r.fullName())
	b.WriteString(`}}`)
	b.WriteString(`,"merged":`)
	b.WriteString(strconv.FormatBool(pr.Merged))
	b.WriteString(`,"mergeable":`)
	b.WriteString(strconv.FormatBool(!prConflict(r, pr)))
	b.WriteString(`,"merge_commit_sha":`)
	if pr.MergeSHA == "" {
		b.WriteString("null")
	} else {
		jstr(&b, pr.MergeSHA)
	}
	b.WriteString(`,"merged_at":`)
	if pr.MergedAt == "" {
		b.WriteString("null")
	} else {
		jstr(&b, pr.MergedAt)
	}
	b.WriteString(`,"user":{"login":`)
	jstr(&b, viewerLogin)
	b.WriteString(`,"id":`)
	b.WriteString(strconv.Itoa(viewerID))
	b.WriteString(`},"created_at":`)
	jstr(&b, pr.CreatedAt)
	b.WriteString(`,"updated_at":`)
	jstr(&b, pr.UpdatedAt)
	b.WriteString(`,"closed_at":`)
	if pr.ClosedAt == "" {
		b.WriteString("null")
	} else {
		jstr(&b, pr.ClosedAt)
	}
	b.WriteString(`}`)
	return b.Bytes()
}

// bump invalidates a repo's derived caches after a mutation.
func (r *Repo) bump() {
	r.rev++
	r.advV0 = nil
	r.advV2 = nil
	r.advRecv = nil
	r.lsCache = make(map[string][]byte)
	r.fetchCache = make(map[string][]byte)
}

func stringsContain(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
