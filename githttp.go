// githttp.go: git smart HTTP — info/refs, upload-pack (v2 + v0 fallback)
// and receive-pack (v0).
//
// Performance shape: the v0/v2 advertisements are cached per repository
// revision, ls-refs answers are cached per revision + arguments, and whole
// fetch responses are cached per revision + request key. The benchmark's 50
// concurrent depth-1 clones of one tip therefore build the pack once and
// serve cached bytes 49 times.
package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
)

// logUnpack writes a one-line diagnostic to stderr. Kept minimal: it only
// fires when a push's pack cannot be unpacked.
func logUnpack(msg string, packLen int) {
	fmt.Fprintf(os.Stderr, "[forge] %s (pack %d bytes)\n", msg, packLen)
}

func (s *server) handleGit(w http.ResponseWriter, r *http.Request) {
	// /{owner}/{repo}.git/<rest>
	path := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(path, ".git/", 2)
	if len(parts) != 2 {
		writeErr(w, 404, "Not Found")
		return
	}
	ownerRepo := strings.SplitN(parts[0], "/", 2)
	if len(ownerRepo) != 2 {
		writeErr(w, 404, "Not Found")
		return
	}
	owner, repoName := ownerRepo[0], ownerRepo[1]
	rest := parts[1]

	s.store.mu.RLock()
	repo := s.store.lookupRepo(owner, repoName)
	s.store.mu.RUnlock()
	if repo == nil {
		// A missing repository is 404 on both services' GET /info/refs, and
		// the server stays up.
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-GitHub-Request-Id", nextReqID())
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		return
	}

	switch {
	case rest == "info/refs" && r.Method == http.MethodGet:
		service := r.URL.Query().Get("service")
		s.gitInfoRefs(w, r, repo, service)
	case rest == "git-upload-pack" && r.Method == http.MethodPost:
		s.gitUploadPack(w, r, repo)
	case rest == "git-receive-pack" && r.Method == http.MethodPost:
		s.gitReceivePack(w, r, repo)
	default:
		writeErr(w, 404, "Not Found")
	}
}

// sortedRefs returns refs sorted by name: heads first, then tags.
func (r *Repo) sortedRefs() [][2]string {
	var out [][2]string
	for name, oid := range r.refs {
		out = append(out, [2]string{name, oid})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

func (r *Repo) mainTip() string { return r.refs["refs/heads/main"] }

// v0Caps is the capability list on a v0 advertisement's first line.
func v0Caps(service string) string {
	if service == "git-receive-pack" {
		return "symref=HEAD:refs/heads/main report-status side-band-64k delete-refs ofs-delta agent=" + agentString
	}
	return "symref=HEAD:refs/heads/main side-band-64k multi_ack thin-pack ofs-delta shallow agent=" + agentString
}

// v0Advertisement builds a v0 ref advertisement (with the service preamble).
func (r *Repo) v0Advertisement(service string) []byte {
	var b bytes.Buffer
	if service == "git-upload-pack" {
		b.Write(pktStr("# service=git-upload-pack\n"))
	} else {
		b.Write(pktStr("# service=git-receive-pack\n"))
	}
	b.Write(pktFlush)
	refs := r.sortedRefs()
	caps := v0Caps(service)
	if len(refs) == 0 {
		b.Write(pktStr(zeroOID + " capabilities^{}\x00" + caps + "\n"))
		b.Write(pktFlush)
		return b.Bytes()
	}
	// HEAD first, carrying the capabilities.
	tip := r.mainTip()
	if tip == "" {
		tip = zeroOID
	}
	b.Write(pktStr(tip + " HEAD\x00" + caps + "\n"))
	for _, ref := range refs {
		b.Write(pktStr(ref[1] + " " + ref[0] + "\n"))
		// Peeled tag line for annotated tags.
		if strings.HasPrefix(ref[0], "refs/tags/") {
			if obj := r.objects[ref[1]]; obj != nil && obj.typ == 4 {
				if tv, err := parseTag(obj.data); err == nil {
					b.Write(pktStr(tv.Object + " " + ref[0] + "^{}\n"))
				}
			}
		}
	}
	b.Write(pktFlush)
	return b.Bytes()
}

// v2Advertisement builds the protocol-v2 capability advertisement.
func v2Advertisement() []byte {
	var b bytes.Buffer
	b.Write(pktStr("# service=git-upload-pack\n"))
	b.Write(pktFlush)
	b.Write(pktStr("version 2\n"))
	b.Write(pktStr("agent=" + agentString + "\n"))
	b.Write(pktStr("ls-refs=unborn\n"))
	b.Write(pktStr("fetch=shallow filter\n"))
	b.Write(pktStr("object-format=sha1\n"))
	b.Write(pktFlush)
	return b.Bytes()
}

func (s *server) gitInfoRefs(w http.ResponseWriter, r *http.Request, repo *Repo, service string) {
	if service != "git-upload-pack" && service != "git-receive-pack" {
		writeErr(w, 400, "invalid service")
		return
	}
	s.store.mu.RLock()
	var body []byte
	var ctype string
	if service == "git-upload-pack" && r.Header.Get("Git-Protocol") == "version=2" {
		body = repo.advV2
		ctype = "application/x-git-upload-pack-advertisement"
	} else if service == "git-upload-pack" {
		body = repo.advV0
		ctype = "application/x-git-upload-pack-advertisement"
	} else {
		body = repo.advRecv
		ctype = "application/x-git-receive-pack-advertisement"
	}
	if body == nil {
		s.store.mu.RUnlock()
		s.store.mu.Lock()
		if service == "git-upload-pack" && r.Header.Get("Git-Protocol") == "version=2" {
			if repo.advV2 == nil {
				repo.advV2 = v2Advertisement()
			}
			body = repo.advV2
		} else if service == "git-upload-pack" {
			if repo.advV0 == nil {
				repo.advV0 = repo.v0Advertisement(service)
			}
			body = repo.advV0
		} else {
			if repo.advRecv == nil {
				repo.advRecv = repo.v0Advertisement(service)
			}
			body = repo.advRecv
		}
		s.store.mu.Unlock()
	} else {
		s.store.mu.RUnlock()
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("X-GitHub-Request-Id", nextReqID())
	w.WriteHeader(200)
	_, _ = w.Write(body)
}

// ---- ls-refs ----

func (s *server) lsRefsResponse(repo *Repo, prefixes []string, peel, symrefs, unborn bool) []byte {
	key := strings.Join(prefixes, ",") + "|" + strconv.FormatBool(peel) + "|" + strconv.FormatBool(symrefs)
	s.store.mu.RLock()
	if cached, ok := repo.lsCache[key]; ok {
		s.store.mu.RUnlock()
		return cached
	}
	// Build under the read lock: the repo's refs and objects are only
	// mutated under the write lock.
	var b bytes.Buffer
	refs := repo.sortedRefs()
	mainTip := repo.mainTip()
	keep := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	if len(prefixes) == 0 && mainTip != "" {
		// HEAD with its symref target (only when unfiltered).
		b.Write(pktStr(mainTip + " HEAD symref-target:refs/heads/main\n"))
	}
	for _, ref := range refs {
		if !keep(ref[0]) {
			continue
		}
		line := ref[1] + " " + ref[0]
		if peel && strings.HasPrefix(ref[0], "refs/tags/") {
			if obj := repo.objects[ref[1]]; obj != nil && obj.typ == 4 {
				if tv, err := parseTag(obj.data); err == nil {
					line += " peeled:" + tv.Object
				}
			}
		}
		b.Write(pktStr(line + "\n"))
	}
	_ = symrefs
	_ = unborn
	out := b.Bytes()
	// The trailing flush: a v2 ls-refs answer ends with one flush packet.
	out = append(out, pktFlush...)
	s.store.mu.RUnlock()

	s.store.mu.Lock()
	repo.lsCache[key] = out
	s.store.mu.Unlock()
	return out
}

// ---- fetch ----

// sideband wraps pack bytes in band-1 sideband frames.
func sideband(pack []byte) []byte {
	var b bytes.Buffer
	const chunk = 32768
	for i := 0; i < len(pack); i += chunk {
		end := i + chunk
		if end > len(pack) {
			end = len(pack)
		}
		frame := make([]byte, 1+end-i)
		frame[0] = 1
		copy(frame[1:], pack[i:end])
		b.Write(pktLine(frame))
	}
	return b.Bytes()
}

func (s *server) fetchV2Response(repo *Repo, wants, haves, shallow []string, depth int, cacheKey string) []byte {
	s.store.mu.RLock()
	if cached, ok := repo.fetchCache[cacheKey]; ok {
		s.store.mu.RUnlock()
		return cached
	}
	// Build under the read lock: planFetch reads the object store, which
	// only mutates under the write lock.
	plan := repo.planFetch(wants, haves, shallow, depth)
	pack := writePack(plan.objects)

	var b bytes.Buffer
	// acknowledgments section
	b.Write(pktStr("acknowledgments\n"))
	if len(haves) == 0 {
		b.Write(pktStr("NAK\n"))
	} else {
		b.Write(pktStr("ACK " + haves[0] + "\n"))
		b.Write(pktStr("ready\n"))
	}
	b.Write(pktDelim)
	// shallow-info section
	if len(plan.newShallow) > 0 || len(plan.unshallow) > 0 {
		b.Write(pktStr("shallow-info\n"))
		for _, oid := range plan.newShallow {
			b.Write(pktStr("shallow " + oid + "\n"))
		}
		for _, oid := range plan.unshallow {
			b.Write(pktStr("unshallow " + oid + "\n"))
		}
		b.Write(pktDelim)
	}
	// packfile section (always present, even for an empty pack)
	b.Write(pktStr("packfile\n"))
	b.Write(sideband(pack))
	b.Write(pktFlush)
	out := b.Bytes()
	s.store.mu.RUnlock()

	s.store.mu.Lock()
	if len(repo.fetchCache) > 64 {
		repo.fetchCache = make(map[string][]byte)
	}
	repo.fetchCache[cacheKey] = out
	s.store.mu.Unlock()
	return out
}

func (s *server) fetchV0Response(repo *Repo, wants, haves, shallow []string, depth int) []byte {
	s.store.mu.RLock()
	plan := repo.planFetch(wants, haves, shallow, depth)
	pack := writePack(plan.objects)
	s.store.mu.RUnlock()
	var b bytes.Buffer
	if len(haves) == 0 {
		b.Write(pktStr("NAK\n"))
	} else {
		b.Write(pktStr("ACK " + haves[0] + "\n"))
	}
	for _, oid := range plan.newShallow {
		b.Write(pktStr("shallow " + oid + "\n"))
	}
	for _, oid := range plan.unshallow {
		b.Write(pktStr("unshallow " + oid + "\n"))
	}
	b.Write(sideband(pack))
	b.Write(pktFlush)
	return b.Bytes()
}

func (s *server) gitUploadPack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := readBody(r, 1<<26)
	if err != nil {
		writeErr(w, 400, "cannot read request body")
		return
	}
	datas, kinds, _, err := pktLines(body)
	if err != nil || len(datas) == 0 {
		// A malformed request must not take the server down.
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.WriteHeader(400)
		return
	}
	_ = kinds
	first := ""
	if len(datas) > 0 && datas[0] != nil {
		first = string(datas[0])
	}
	isV2 := r.Header.Get("Git-Protocol") == "version=2"
	if strings.HasPrefix(first, "command=ls-refs") {
		var prefixes []string
		peel, symrefs, unborn := true, true, true
		for _, d := range datas[1:] {
			if d == nil {
				continue
			}
			line := strings.TrimSuffix(string(d), "\n")
			switch {
			case strings.HasPrefix(line, "ref-prefix "):
				prefixes = append(prefixes, strings.TrimPrefix(line, "ref-prefix "))
			case line == "peel":
				peel = true
			case line == "symrefs":
				symrefs = true
			case line == "unborn":
				unborn = true
			}
		}
		out := s.lsRefsResponse(repo, prefixes, peel, symrefs, unborn)
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.WriteHeader(200)
		_, _ = w.Write(out)
		return
	}
	if !strings.HasPrefix(first, "command=fetch") {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
		w.WriteHeader(400)
		return
	}
	var wants, haves, shallow []string
	depth := 0
	for _, d := range datas[1:] {
		if d == nil {
			continue
		}
		line := strings.TrimSuffix(string(d), "\n")
		switch {
		case strings.HasPrefix(line, "want "):
			wants = append(wants, strings.TrimPrefix(line, "want "))
		case strings.HasPrefix(line, "have "):
			haves = append(haves, strings.TrimPrefix(line, "have "))
		case strings.HasPrefix(line, "shallow "):
			shallow = append(shallow, strings.TrimPrefix(line, "shallow "))
		case strings.HasPrefix(line, "deepen "):
			if n, err := strconv.Atoi(strings.TrimPrefix(line, "deepen ")); err == nil {
				depth = n
			}
		}
	}
	var out []byte
	if isV2 {
		key := "v2|" + strconv.Itoa(repo.rev) + "|" + strings.Join(wants, ",") + "|" + strings.Join(haves, ",") + "|" + strings.Join(shallow, ",") + "|" + strconv.Itoa(depth)
		out = s.fetchV2Response(repo, wants, haves, shallow, depth, key)
	} else {
		out = s.fetchV0Response(repo, wants, haves, shallow, depth)
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.WriteHeader(200)
	_, _ = w.Write(out)
}

// ---- receive-pack ----

func (s *server) gitReceivePack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := readBody(r, 1<<28)
	if err != nil {
		writeErr(w, 400, "cannot read request body")
		return
	}
	datas, kinds, end, err := pktLines(body)
	if err != nil {
		w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
		w.WriteHeader(400)
		return
	}
	type cmd struct{ old, new, ref string }
	var cmds []cmd
	for i, d := range datas {
		if kinds[i] == 'f' {
			break
		}
		if d == nil {
			continue
		}
		line := strings.TrimSuffix(string(d), "\n")
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			line = line[:nul]
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			continue
		}
		cmds = append(cmds, cmd{fields[0], fields[1], fields[2]})
	}
	packBytes := body[end:]

	s.store.mu.Lock()
	defer s.store.mu.Unlock()

	// Unpack first: a corrupt pack fails the whole exchange.
	unpackOK := true
	unpackDetail := "ok"
	if len(cmds) > 0 {
		needsPack := false
		for _, c := range cmds {
			if c.new != zeroOID {
				needsPack = true
			}
		}
		if needsPack {
			if len(packBytes) == 0 || !packTrailerOK(packBytes) {
				unpackOK = false
				unpackDetail = "unpacker error"
				logUnpack("receive-pack: empty pack or bad trailer", len(packBytes))
			} else {
				objs, perr := readPack(packBytes, func(oid string) *Object { return repo.objects[oid] })
				if perr != nil {
					unpackOK = false
					unpackDetail = "unpacker error"
					logUnpack("receive-pack: readPack failed: "+perr.Error(), len(packBytes))
				} else {
					for oid, obj := range objs {
						if _, ok := repo.objects[oid]; !ok {
							repo.objects[oid] = obj
						}
					}
				}
			}
		}
	}

	// Apply the commands in order, one report line each.
	var report bytes.Buffer
	if unpackOK {
		report.Write(pktStr("unpack ok\n"))
	} else {
		report.Write(pktStr("unpack " + unpackDetail + "\n"))
	}
	changed := false
	for _, c := range cmds {
		if !unpackOK {
			report.Write(pktStr("ng " + c.ref + " unpacker error\n"))
			continue
		}
		if c.new == zeroOID {
			// Delete.
			if _, ok := repo.refs[c.ref]; !ok {
				report.Write(pktStr("ng " + c.ref + " remote ref does not exist\n"))
				continue
			}
			delete(repo.refs, c.ref)
			changed = true
			report.Write(pktStr("ok " + c.ref + "\n"))
			continue
		}
		cur, exists := repo.refs[c.ref]
		if c.old == zeroOID {
			// Create.
			if exists {
				report.Write(pktStr("ng " + c.ref + " non-fast-forward\n"))
				continue
			}
			if repo.objects[c.new] == nil {
				report.Write(pktStr("ng " + c.ref + " object not found\n"))
				continue
			}
			repo.refs[c.ref] = c.new
			changed = true
			report.Write(pktStr("ok " + c.ref + "\n"))
			continue
		}
		// Update: old must match, and new must descend from old.
		if !exists || cur != c.old {
			report.Write(pktStr("ng " + c.ref + " non-fast-forward\n"))
			continue
		}
		if repo.objects[c.new] == nil || !repo.isAncestor(c.old, c.new) {
			report.Write(pktStr("ng " + c.ref + " non-fast-forward\n"))
			continue
		}
		repo.refs[c.ref] = c.new
		changed = true
		report.Write(pktStr("ok " + c.ref + "\n"))
	}
	if changed {
		repo.bump()
	}

	var out bytes.Buffer
	out.Write(pktLine(append([]byte{1}, report.Bytes()...)))
	out.Write(pktFlush)
	w.Header().Set("Content-Type", "application/x-git-receive-pack-result")
	w.WriteHeader(200)
	_, _ = w.Write(out.Bytes())
}
