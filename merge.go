// merge.go: reachability, shallow-fetch selection, tree diffs and squash merges.
package main

import (
	"bytes"
	"sort"
	"strings"
)

// reachable returns every object id reachable from roots (commits walk trees
// and parents, tags walk their target, trees walk entries). Missing objects
// are skipped when allowMissing is set.
func (r *Repo) reachable(roots []string, allowMissing bool) map[string]bool {
	seen := make(map[string]bool)
	var stack []string
	for _, root := range roots {
		if isOID(root) {
			stack = append(stack, root)
		}
	}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		obj := r.objects[oid]
		if obj == nil {
			if allowMissing {
				continue
			}
			continue
		}
		seen[oid] = true
		switch obj.typ {
		case 1:
			cv, err := parseCommit(obj.data)
			if err != nil {
				continue
			}
			stack = append(stack, cv.Tree)
			stack = append(stack, cv.Parents...)
		case 4:
			tv, err := parseTag(obj.data)
			if err != nil {
				continue
			}
			stack = append(stack, tv.Object)
		case 2:
			entries, err := parseTree(obj.data)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.Mode == "160000" {
					continue
				}
				stack = append(stack, e.OID)
			}
		}
	}
	return seen
}

// isAncestor reports whether anc is reachable from oid (or equal).
func (r *Repo) isAncestor(anc, oid string) bool {
	if anc == oid {
		return true
	}
	if anc == "" || anc == zeroOID || oid == "" || oid == zeroOID {
		return false
	}
	seen := make(map[string]bool)
	stack := []string{oid}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == anc {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := r.objects[cur]
		if obj == nil || obj.typ != 1 {
			continue
		}
		cv, err := parseCommit(obj.data)
		if err != nil {
			continue
		}
		stack = append(stack, cv.Parents...)
	}
	return false
}

// mergeBase finds the first common ancestor of a and b (linear histories:
// the nearest ancestor of a that is also an ancestor of b).
func (r *Repo) mergeBase(a, b string) string {
	if a == "" || b == "" {
		return ""
	}
	ancB := make(map[string]bool)
	stack := []string{b}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if ancB[cur] {
			continue
		}
		ancB[cur] = true
		obj := r.objects[cur]
		if obj == nil || obj.typ != 1 {
			continue
		}
		cv, err := parseCommit(obj.data)
		if err != nil {
			continue
		}
		stack = append(stack, cv.Parents...)
	}
	stack = []string{a}
	seen := make(map[string]bool)
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if ancB[cur] {
			return cur
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := r.objects[cur]
		if obj == nil || obj.typ != 1 {
			continue
		}
		cv, err := parseCommit(obj.data)
		if err != nil {
			continue
		}
		stack = append(stack, cv.Parents...)
	}
	return ""
}

// flattenTree returns path -> blob oid for every file under a tree.
func (r *Repo) flattenTree(treeOID string) map[string]string {
	out := make(map[string]string)
	var walk func(treeOID, prefix string)
	walk = func(treeOID, prefix string) {
		obj := r.objects[treeOID]
		if obj == nil || obj.typ != 2 {
			return
		}
		entries, err := parseTree(obj.data)
		if err != nil {
			return
		}
		for _, e := range entries {
			p := prefix + e.Name
			if e.Mode == "40000" {
				walk(e.OID, p+"/")
			} else if e.Mode != "160000" {
				out[p] = e.OID
			}
		}
	}
	walk(treeOID, "")
	return out
}

type fileChange struct {
	Path       string
	Status     string // "added" | "modified" | "removed"
	OldOID     string
	NewOID     string
	Additions  int
	Deletions  int
}

// diffTrees lists the file changes from base tree to head tree.
func (r *Repo) diffTrees(baseTree, headTree string) []fileChange {
	base := r.flattenTree(baseTree)
	head := r.flattenTree(headTree)
	paths := make(map[string]bool)
	for p := range base {
		paths[p] = true
	}
	for p := range head {
		paths[p] = true
	}
	var out []fileChange
	for p := range paths {
		b, inBase := base[p]
		h, inHead := head[p]
		switch {
		case !inBase && inHead:
			add := 0
			if obj := r.objects[h]; obj != nil {
				add = countLines(obj.data)
			}
			out = append(out, fileChange{Path: p, Status: "added", NewOID: h, Additions: add})
		case inBase && !inHead:
			del := 0
			if obj := r.objects[b]; obj != nil {
				del = countLines(obj.data)
			}
			out = append(out, fileChange{Path: p, Status: "removed", OldOID: b, Deletions: del})
		case b != h:
			add, del := lineDiff(r.objects[b], r.objects[h])
			out = append(out, fileChange{Path: p, Status: "modified", OldOID: b, NewOID: h, Additions: add, Deletions: del})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func countLines(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	n := bytes.Count(data, []byte("\n"))
	if data[len(data)-1] != '\n' {
		n++
	}
	return n
}

// lineDiff counts added/removed lines between two blobs (multiset difference).
func lineDiff(oldObj, newObj *Object) (add, del int) {
	oldLines := map[string]int{}
	newLines := map[string]int{}
	for _, l := range bytes.Split(oldObj.data, []byte("\n")) {
		oldLines[string(l)]++
	}
	for _, l := range bytes.Split(newObj.data, []byte("\n")) {
		newLines[string(l)]++
	}
	for l, n := range newLines {
		if d := n - oldLines[l]; d > 0 {
			add += d
		}
	}
	for l, n := range oldLines {
		if d := n - newLines[l]; d > 0 {
			del += d
		}
	}
	return add, del
}

// prConflict reports whether merging pr would conflict: a path changed on
// the head side since the merge base and changed differently on the base side.
func prConflict(r *Repo, pr *Pull) bool {
	headTip := r.refs["refs/heads/"+pr.HeadRef]
	baseTip := r.refs["refs/heads/"+pr.BaseRef]
	if headTip == "" || baseTip == "" {
		return false
	}
	base := r.mergeBase(headTip, baseTip)
	if base == "" {
		return false
	}
	headCommit, err1 := parseCommit(r.objects[headTip].data)
	baseCommit, err2 := parseCommit(r.objects[baseTip].data)
	baseBaseCommit, err3 := parseCommit(r.objects[base].data)
	if err1 != nil || err2 != nil || err3 != nil {
		return false
	}
	headFiles := r.flattenTree(headCommit.Tree)
	baseFiles := r.flattenTree(baseCommit.Tree)
	mergeFiles := r.flattenTree(baseBaseCommit.Tree)
	paths := make(map[string]bool)
	for p := range headFiles {
		paths[p] = true
	}
	for p := range baseFiles {
		paths[p] = true
	}
	for p := range paths {
		h, hOk := headFiles[p]
		b, bOk := baseFiles[p]
		m, mOk := mergeFiles[p]
		headChanged := hOk != mOk || h != m
		baseChanged := bOk != mOk || b != m
		if headChanged && baseChanged && h != b {
			return true
		}
	}
	return false
}

// applyDiffToTree returns a new tree oid: baseTipTree with every change from
// diff(baseTree, headTree) applied (head's version wins per path).
func (s *Store) applyDiffToTree(r *Repo, baseTipTree, baseTree, headTree string) string {
	changes := r.diffTrees(baseTree, headTree)
	treeOID := baseTipTree
	for _, ch := range changes {
		var err error
		if ch.Status == "removed" {
			treeOID, err = s.updatePath(r, treeOID, ch.Path, "")
		} else {
			treeOID, err = s.updatePath(r, treeOID, ch.Path, ch.NewOID)
		}
		if err != nil {
			return treeOID
		}
	}
	return treeOID
}

// updatePath returns a new tree oid with path set to blobOID (or deleted when
// blobOID is ""). Intermediate trees are created as needed.
func (s *Store) updatePath(r *Repo, treeOID, path, blobOID string) (string, error) {
	segments := strings.Split(path, "/")
	return s.updatePathRec(r, treeOID, segments, blobOID)
}

func (s *Store) updatePathRec(r *Repo, treeOID string, segs []string, blobOID string) (string, error) {
	var entries []TreeEntry
	if treeOID != "" {
		if obj := r.objects[treeOID]; obj != nil && obj.typ == 2 {
			parsed, err := parseTree(obj.data)
			if err != nil {
				return "", err
			}
			entries = parsed
		}
	}
	name := segs[0]
	if len(segs) == 1 {
		out := entries[:0]
		found := false
		for _, e := range entries {
			if e.Name == name {
				found = true
				if blobOID != "" {
					out = append(out, TreeEntry{Mode: "100644", Name: name, OID: blobOID})
				}
				continue
			}
			out = append(out, e)
		}
		if !found && blobOID != "" {
			out = append(out, TreeEntry{Mode: "100644", Name: name, OID: blobOID})
		}
		return s.putObject(r, 2, buildTree(out)), nil
	}
	// Descend into (or create) the subdirectory.
	var childOID string
	out := entries[:0]
	found := false
	for _, e := range entries {
		if e.Name == name && e.Mode == "40000" {
			found = true
			childOID = e.OID
			continue
		}
		out = append(out, e)
	}
	_ = found
	newChild, err := s.updatePathRec(r, childOID, segs[1:], blobOID)
	if err != nil {
		return "", err
	}
	if newChild != "" {
		// An empty tree means the directory became empty: drop it, unless it
		// was just created with content (then it is non-empty by definition).
		if obj := r.objects[newChild]; obj != nil {
			parsed, _ := parseTree(obj.data)
			if len(parsed) == 0 {
				return s.putObject(r, 2, buildTree(out)), nil
			}
		}
		out = append(out, TreeEntry{Mode: "40000", Name: name, OID: newChild})
	}
	return s.putObject(r, 2, buildTree(out)), nil
}

// putObject stores an object and returns its oid.
func (s *Store) putObject(r *Repo, typ byte, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := r.objects[oid]; !ok {
		r.objects[oid] = &Object{typ: typ, data: data}
	}
	return oid
}

// lookupPath resolves a path in a tree to (oid, isDir, found).
func (r *Repo) lookupPath(treeOID, path string) (string, bool, bool) {
	if path == "" {
		return treeOID, true, true
	}
	segs := strings.Split(path, "/")
	cur := treeOID
	for i, seg := range segs {
		obj := r.objects[cur]
		if obj == nil || obj.typ != 2 {
			return "", false, false
		}
		entries, err := parseTree(obj.data)
		if err != nil {
			return "", false, false
		}
		var found *TreeEntry
		for j := range entries {
			if entries[j].Name == seg {
				found = &entries[j]
				break
			}
		}
		if found == nil {
			return "", false, false
		}
		if i == len(segs)-1 {
			return found.OID, found.Mode == "40000", true
		}
		if found.Mode != "40000" {
			return "", false, false
		}
		cur = found.OID
	}
	return "", false, false
}

// commitTree returns the tree oid of a commit, or "".
func (r *Repo) commitTree(oid string) string {
	obj := r.objects[oid]
	if obj == nil || obj.typ != 1 {
		return ""
	}
	cv, err := parseCommit(obj.data)
	if err != nil {
		return ""
	}
	return cv.Tree
}

// ---- fetch selection ----

type fetchPlan struct {
	objects    []*Object // sorted by oid
	newShallow []string
	unshallow  []string
}

// planFetch computes what a fetch sends: reachable-from-wants minus
// reachable-from-haves, with an optional depth limit (deepen). oldShallow
// lists the client's current shallow boundaries.
func (r *Repo) planFetch(wants, haves, oldShallow []string, depth int) fetchPlan {
	// haveSet: everything the client already holds. Traversal from a have
	// stops at a shallow boundary (the client lacks its parents).
	haveSet := make(map[string]bool)
	{
		shallowSet := make(map[string]bool)
		for _, o := range oldShallow {
			shallowSet[o] = true
		}
		var stack []string
		for _, h := range haves {
			if isOID(h) {
				stack = append(stack, h)
			}
		}
		for len(stack) > 0 {
			oid := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if haveSet[oid] {
				continue
			}
			obj := r.objects[oid]
			if obj == nil {
				continue
			}
			haveSet[oid] = true
			switch obj.typ {
			case 1:
				if shallowSet[oid] {
					// The client holds this commit's tree/blobs but not its parents.
					cv, err := parseCommit(obj.data)
					if err == nil {
						stack = append(stack, cv.Tree)
					}
					continue
				}
				cv, err := parseCommit(obj.data)
				if err != nil {
					continue
				}
				stack = append(stack, cv.Tree)
				stack = append(stack, cv.Parents...)
			case 4:
				tv, err := parseTag(obj.data)
				if err == nil {
					stack = append(stack, tv.Object)
				}
			case 2:
				entries, err := parseTree(obj.data)
				if err == nil {
					for _, e := range entries {
						if e.Mode != "160000" {
							stack = append(stack, e.OID)
						}
					}
				}
			}
		}
	}

	// Walk from the wants with depths (want itself is depth 1).
	depthOf := make(map[string]int)
	type item struct {
		oid   string
		depth int
	}
	var queue []item
	for _, w := range wants {
		if !isOID(w) {
			continue
		}
		if _, ok := depthOf[w]; !ok {
			depthOf[w] = 1
			queue = append(queue, item{w, 1})
		}
	}
	included := make(map[string]bool)
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		obj := r.objects[it.oid]
		if obj == nil {
			continue
		}
		switch obj.typ {
		case 1:
			cv, err := parseCommit(obj.data)
			if err != nil {
				continue
			}
			if !haveSet[it.oid] {
				included[it.oid] = true
			}
			// Traverse the tree (always) and parents (up to the depth limit).
			if !haveSet[cv.Tree] {
				included[cv.Tree] = true
			}
			if depth == 0 || it.depth < depth {
				for _, p := range cv.Parents {
					if _, ok := depthOf[p]; !ok {
						depthOf[p] = it.depth + 1
						queue = append(queue, item{p, it.depth + 1})
					}
				}
			}
		case 4:
			if !haveSet[it.oid] {
				included[it.oid] = true
			}
			tv, err := parseTag(obj.data)
			if err == nil {
				if _, ok := depthOf[tv.Object]; !ok {
					depthOf[tv.Object] = it.depth
					queue = append(queue, item{tv.Object, it.depth})
				}
			}
		case 2:
			if !haveSet[it.oid] {
				included[it.oid] = true
			}
		default: // blob
			if !haveSet[it.oid] {
				included[it.oid] = true
			}
		}
	}
	// Trees/blobs under included commits: walk every included commit's tree.
	{
		visited := make(map[string]bool)
		var tstack []string
		for oid := range included {
			if obj := r.objects[oid]; obj != nil && obj.typ == 1 {
				cv, err := parseCommit(obj.data)
				if err == nil {
					tstack = append(tstack, cv.Tree)
				}
			}
		}
		for len(tstack) > 0 {
			oid := tstack[len(tstack)-1]
			tstack = tstack[:len(tstack)-1]
			if visited[oid] {
				continue
			}
			visited[oid] = true
			obj := r.objects[oid]
			if obj == nil {
				continue
			}
			if !haveSet[oid] {
				included[oid] = true
			}
			if obj.typ == 2 {
				entries, err := parseTree(obj.data)
				if err != nil {
					continue
				}
				for _, e := range entries {
					if e.Mode != "160000" {
						tstack = append(tstack, e.OID)
					}
				}
			}
		}
	}

	plan := fetchPlan{}
	if depth > 0 {
		// New shallow boundaries: included commits at exactly the depth.
		for oid := range included {
			if obj := r.objects[oid]; obj != nil && obj.typ == 1 && depthOf[oid] == depth {
				plan.newShallow = append(plan.newShallow, oid)
			}
		}
		sort.Strings(plan.newShallow)
		// Unshallow: old boundaries now interior (depth < N from a want).
		for _, b := range oldShallow {
			if d, ok := depthOf[b]; ok && d < depth {
				plan.unshallow = append(plan.unshallow, b)
			}
		}
		sort.Strings(plan.unshallow)
	}
	oids := make([]string, 0, len(included))
	for oid := range included {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	for _, oid := range oids {
		if obj := r.objects[oid]; obj != nil {
			plan.objects = append(plan.objects, obj)
		}
	}
	return plan
}
