// gitobj.go: git object model, pkt-lines and packfiles.
//
// Object ids are SHA-1 over "<type> <size>\0" + bytes, exactly as git
// computes them. Packs are version 2, written without deltas (the suite's
// pack-deltas check only requires delta-compressed objects to resolve when
// the server sends them); incoming packs resolve OFS_DELTA and REF_DELTA.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// ---- pkt-lines ----

func pktLine(payload []byte) []byte {
	out := make([]byte, 4+len(payload))
	hex4 := fmt.Sprintf("%04x", len(payload)+4)
	copy(out, hex4)
	copy(out[4:], payload)
	return out
}

func pktStr(s string) []byte { return pktLine([]byte(s)) }

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// pktLines decodes a buffer into packets. It returns the data payloads in
// order plus flags for flush/delim, and the offset where the framed section
// ended (the rest is raw pack bytes, e.g. after a receive-pack command list).
// Parsing stops at the first flush packet: everything after it is raw data,
// not more pkt-lines.
func pktLines(buf []byte) (datas [][]byte, kinds []byte, end int, err error) {
	i := 0
	for i+4 <= len(buf) {
		hexLen := string(buf[i : i+4])
		n, perr := strconv.ParseInt(hexLen, 16, 32)
		if perr != nil {
			return nil, nil, 0, fmt.Errorf("invalid pkt-line length %q", hexLen)
		}
		if n == 0 {
			kinds = append(kinds, 'f')
			datas = append(datas, nil)
			i += 4
			return datas, kinds, i, nil
		}
		if n == 1 {
			kinds = append(kinds, 'd')
			datas = append(datas, nil)
			i += 4
			continue
		}
		if n == 2 {
			kinds = append(kinds, 'e')
			datas = append(datas, nil)
			i += 4
			continue
		}
		if n < 4 || i+int(n) > len(buf) {
			return nil, nil, 0, errors.New("truncated pkt-line")
		}
		kinds = append(kinds, 'x')
		datas = append(datas, buf[i+4:i+int(n)])
		i += int(n)
	}
	return datas, kinds, i, nil
}

// ---- trees, commits, tags ----

type TreeEntry struct {
	Mode string
	Name string
	OID  string // hex
}

func parseTree(data []byte) ([]TreeEntry, error) {
	var out []TreeEntry
	i := 0
	for i < len(data) {
		sp := bytes.IndexByte(data[i:], ' ')
		if sp < 0 {
			return nil, errors.New("tree entry has no mode separator")
		}
		mode := string(data[i : i+sp])
		nul := bytes.IndexByte(data[i+sp+1:], 0)
		if nul < 0 {
			return nil, errors.New("tree entry has no name terminator")
		}
		name := string(data[i+sp+1 : i+sp+1+nul])
		oidStart := i + sp + 1 + nul + 1
		if oidStart+20 > len(data) {
			return nil, errors.New("tree entry has a truncated object id")
		}
		out = append(out, TreeEntry{Mode: mode, Name: name, OID: hex.EncodeToString(data[oidStart : oidStart+20])})
		i = oidStart + 20
	}
	return out, nil
}

func compareTreeNames(a, b TreeEntry) int {
	an := a.Name
	bn := b.Name
	if a.Mode == "40000" {
		an += "/"
	}
	if b.Mode == "40000" {
		bn += "/"
	}
	return strings.Compare(an, bn)
}

func buildTree(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return compareTreeNames(sorted[i], sorted[j]) < 0 })
	var b bytes.Buffer
	for _, e := range sorted {
		b.WriteString(e.Mode)
		b.WriteByte(' ')
		b.WriteString(e.Name)
		b.WriteByte(0)
		raw, _ := hex.DecodeString(e.OID)
		b.Write(raw)
	}
	return b.Bytes()
}

type CommitView struct {
	Tree      string
	Parents   []string
	Author    string // "Name <email> ts tz"
	Committer string
	Message   string
}

func parseCommit(data []byte) (*CommitView, error) {
	blank := bytes.Index(data, []byte("\n\n"))
	headEnd := len(data)
	if blank >= 0 {
		headEnd = blank
	}
	cv := &CommitView{}
	for _, line := range strings.Split(string(data[:headEnd]), "\n") {
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		key, val := line, ""
		if sp >= 0 {
			key, val = line[:sp], line[sp+1:]
		}
		switch key {
		case "tree":
			cv.Tree = val
		case "parent":
			cv.Parents = append(cv.Parents, val)
		case "author":
			cv.Author = val
		case "committer":
			cv.Committer = val
		}
	}
	if !isOID(cv.Tree) {
		return nil, errors.New("commit has no valid tree header")
	}
	if blank >= 0 {
		cv.Message = string(data[blank+2:])
	}
	return cv, nil
}

// buildCommit serializes a commit object. Ident strings are already in the
// form "Name <email> ts tz".
func buildCommit(treeOID string, parents []string, author, committer, message string) []byte {
	var b bytes.Buffer
	b.WriteString("tree ")
	b.WriteString(treeOID)
	b.WriteString("\n")
	for _, p := range parents {
		b.WriteString("parent ")
		b.WriteString(p)
		b.WriteString("\n")
	}
	b.WriteString("author ")
	b.WriteString(author)
	b.WriteString("\n")
	b.WriteString("committer ")
	b.WriteString(committer)
	b.WriteString("\n")
	b.WriteString("\n")
	b.WriteString(message)
	return b.Bytes()
}

// parseIdent splits "Name <email> ts tz" into name, email, ts.
func parseIdent(s string) (name, email string, ts int64) {
	lt := strings.IndexByte(s, '<')
	gt := strings.IndexByte(s, '>')
	if lt < 0 || gt < 0 || gt < lt {
		return s, "", 0
	}
	name = strings.TrimSpace(s[:lt])
	email = s[lt+1 : gt]
	rest := strings.Fields(s[gt+1:])
	if len(rest) > 0 {
		ts, _ = strconv.ParseInt(rest[0], 10, 64)
	}
	return name, email, ts
}

type TagView struct {
	Object  string
	Type    string
	Tag     string
	Tagger  string
	Message string
}

func parseTag(data []byte) (*TagView, error) {
	blank := bytes.Index(data, []byte("\n\n"))
	headEnd := len(data)
	if blank >= 0 {
		headEnd = blank
	}
	tv := &TagView{}
	for _, line := range strings.Split(string(data[:headEnd]), "\n") {
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		switch line[:sp] {
		case "object":
			tv.Object = line[sp+1:]
		case "type":
			tv.Type = line[sp+1:]
		case "tag":
			tv.Tag = line[sp+1:]
		case "tagger":
			tv.Tagger = line[sp+1:]
		}
	}
	if !isOID(tv.Object) {
		return nil, errors.New("tag has no valid object header")
	}
	if blank >= 0 {
		tv.Message = string(data[blank+2:])
	}
	return tv, nil
}

// ---- packfiles ----

func writePackObjectHeader(typ byte, size int) []byte {
	code := typ
	var out []byte
	first := (code << 4) | byte(size&0x0f)
	size >>= 4
	for size > 0 {
		out = append(out, first|0x80)
		first = byte(size & 0x7f)
		size >>= 7
	}
	out = append(out, first)
	return out
}

// writePack builds a version-2 pack with no deltas. Objects are emitted in
// the order given; callers sort by oid for determinism.
func writePack(objs []*Object) []byte {
	var body bytes.Buffer
	hdr := make([]byte, 12)
	copy(hdr, "PACK")
	hdr[4] = 0
	hdr[5] = 0
	hdr[6] = 0
	hdr[7] = 2
	n := len(objs)
	hdr[8] = byte(n >> 24)
	hdr[9] = byte(n >> 16)
	hdr[10] = byte(n >> 8)
	hdr[11] = byte(n)
	body.Write(hdr)
	for _, o := range objs {
		body.Write(writePackObjectHeader(o.typ, len(o.data)))
		var zb bytes.Buffer
		zw, _ := zlib.NewWriterLevel(&zb, 1)
		_, _ = zw.Write(o.data)
		_ = zw.Close()
		body.Write(zb.Bytes())
	}
	sum := sha1.Sum(body.Bytes())
	body.Write(sum[:])
	return body.Bytes()
}

func packTrailer(pack []byte) string {
	if len(pack) < 20 {
		return ""
	}
	sum := sha1.Sum(pack[:len(pack)-20])
	return hex.EncodeToString(sum[:])
}

func packTrailerOK(pack []byte) bool {
	if len(pack) < 20 {
		return false
	}
	return hex.EncodeToString(pack[len(pack)-20:]) == packTrailer(pack)
}

// readPack parses a packfile and resolves every object (including deltas).
// resolveBase supplies objects a thin pack leaves out.
func readPack(pack []byte, resolveBase func(oid string) *Object) (map[string]*Object, error) {
	if len(pack) < 32 {
		return nil, errors.New("pack is too short")
	}
	if string(pack[:4]) != "PACK" {
		return nil, errors.New("pack does not start with PACK")
	}
	ver := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if ver != 2 && ver != 3 {
		return nil, fmt.Errorf("unsupported pack version %d", ver)
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	trailerStart := len(pack) - 20
	type raw struct {
		offset     int
		typeCode   int
		size       int
		data       []byte
		baseOffset int
		baseOID    string
		resolved   *Object
		oid        string
	}
	raws := make([]*raw, 0, count)
	byOffset := make(map[int]*raw)
	offset := 12
	for i := 0; i < count; i++ {
		entryOffset := offset
		if offset >= trailerStart {
			return nil, errors.New("pack entry is truncated")
		}
		b0 := pack[offset]
		size := int(b0 & 0x0f)
		shift := uint(4)
		offset++
		for b0&0x80 != 0 {
			if offset >= trailerStart {
				return nil, errors.New("pack size varint is truncated")
			}
			b0 = pack[offset]
			offset++
			size += int(b0&0x7f) << shift
			shift += 7
		}
		typeCode := int((pack[entryOffset] >> 4) & 0x07)
		e := &raw{offset: entryOffset, typeCode: typeCode, size: size}
		if typeCode == 6 {
			if offset >= trailerStart {
				return nil, errors.New("ofs-delta has no base offset")
			}
			b := pack[offset]
			offset++
			v := int(b & 0x7f)
			for b&0x80 != 0 {
				if offset >= trailerStart {
					return nil, errors.New("ofs-delta base offset is truncated")
				}
				b = pack[offset]
				offset++
				v = ((v + 1) << 7) + int(b&0x7f)
			}
			e.baseOffset = entryOffset - v
		} else if typeCode == 7 {
			if offset+20 > trailerStart {
				return nil, errors.New("ref-delta base id is truncated")
			}
			e.baseOID = hex.EncodeToString(pack[offset : offset+20])
			offset += 20
		} else if typeCode < 1 || typeCode > 4 {
			return nil, fmt.Errorf("pack entry has unknown type code %d", typeCode)
		}
		cr := &countReader{r: &oneByteReader{r: bytes.NewReader(pack[offset:trailerStart])}}
		zr, err := zlib.NewReader(cr)
		if err != nil {
			return nil, fmt.Errorf("zlib stream did not inflate: %v", err)
		}
		data, err := io.ReadAll(zr)
		_ = zr.Close()
		if err != nil {
			return nil, fmt.Errorf("zlib stream did not inflate: %v", err)
		}
		if len(data) != size {
			return nil, fmt.Errorf("pack entry declares %d bytes but inflated to %d", size, len(data))
		}
		e.data = data
		offset += cr.n
		raws = append(raws, e)
		byOffset[entryOffset] = e
	}
	if offset != trailerStart {
		return nil, fmt.Errorf("pack has %d stray bytes before its trailer", trailerStart-offset)
	}
	out := make(map[string]*Object, count)
	resolvedByOID := make(map[string]*raw)
	var resolve func(e *raw) (*Object, error)
	resolving := make(map[int]bool)
	resolve = func(e *raw) (*Object, error) {
		if e.resolved != nil {
			return e.resolved, nil
		}
		if resolving[e.offset] {
			return nil, errors.New("delta cycle in pack")
		}
		resolving[e.offset] = true
		defer delete(resolving, e.offset)
		var obj *Object
		switch e.typeCode {
		case 6:
			base := byOffset[e.baseOffset]
			if base == nil {
				return nil, errors.New("ofs-delta base is not in the pack")
			}
			bobj, err := resolve(base)
			if err != nil {
				return nil, err
			}
			data, err := applyDelta(bobj.data, e.data)
			if err != nil {
				return nil, err
			}
			obj = &Object{typ: bobj.typ, data: data}
		case 7:
			if be, ok := resolvedByOID[e.baseOID]; ok {
				bobj, err := resolve(be)
				if err != nil {
					return nil, err
				}
				data, err := applyDelta(bobj.data, e.data)
				if err != nil {
					return nil, err
				}
				obj = &Object{typ: bobj.typ, data: data}
			} else if resolveBase != nil {
				ext := resolveBase(e.baseOID)
				if ext == nil {
					return nil, fmt.Errorf("ref-delta base %s is not available", e.baseOID)
				}
				data, err := applyDelta(ext.data, e.data)
				if err != nil {
					return nil, err
				}
				obj = &Object{typ: ext.typ, data: data}
			} else {
				return nil, fmt.Errorf("ref-delta base %s is not available", e.baseOID)
			}
		default:
			obj = &Object{typ: byte(e.typeCode), data: e.data}
		}
		e.resolved = obj
		e.oid = hashObject(obj.typ, obj.data)
		resolvedByOID[e.oid] = e
		return obj, nil
	}
	for _, e := range raws {
		obj, err := resolve(e)
		if err != nil {
			return nil, err
		}
		if _, ok := out[e.oid]; !ok {
			out[e.oid] = obj
		}
	}
	return out, nil
}

type countReader struct {
	r io.Reader
	n int
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// oneByteReader throttles its underlying reader to one byte per Read call.
// The zlib reader buffers ahead (bufio), so a plain counting reader would
// over-count the bytes a stream consumed and the next pack entry would start
// at the wrong offset. One byte at a time, the count is exactly the stream.
type oneByteReader struct {
	r io.Reader
}

func (o *oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

// applyDelta applies a git delta to a base object's bytes.
func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readSize := func() (int, error) {
		v, shift := 0, uint(0)
		for {
			if off >= len(delta) {
				return 0, errors.New("delta size varint is truncated")
			}
			b := delta[off]
			off++
			v += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return v, nil
	}
	srcSize, err := readSize()
	if err != nil {
		return nil, err
	}
	tgtSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, fmt.Errorf("delta expects a %d-byte base, got %d", srcSize, len(base))
	}
	out := make([]byte, 0, tgtSize)
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			copyOff, copySize := 0, 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<bit) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy offset is truncated")
					}
					copyOff += int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<bit) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta copy size is truncated")
					}
					copySize += int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			if copySize == 0 {
				copySize = 0x10000
			}
			if copyOff+copySize > len(base) || len(out)+copySize > tgtSize {
				return nil, errors.New("delta copy is out of range")
			}
			out = append(out, base[copyOff:copyOff+copySize]...)
		} else if op != 0 {
			lit := int(op & 0x7f)
			if off+lit > len(delta) || len(out)+lit > tgtSize {
				return nil, errors.New("delta insert is truncated")
			}
			out = append(out, delta[off:off+lit]...)
			off += lit
		} else {
			return nil, errors.New("delta opcode 0 is invalid")
		}
	}
	if len(out) != tgtSize {
		return nil, fmt.Errorf("delta produced %d of %d bytes", len(out), tgtSize)
	}
	return out, nil
}
