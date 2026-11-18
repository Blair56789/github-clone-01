package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// --- object ids -----------------------------------------------------------

// hashObject computes git's object id: SHA-1 over "<type> <size>\0" + data.
func hashObject(typ string, data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d\x00", typ, len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

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

func oidBytes(s string) ([]byte, error) {
	if !isOID(s) {
		return nil, fmt.Errorf("bad object id %q", s)
	}
	return hex.DecodeString(s)
}

// --- commits --------------------------------------------------------------

func parseCommitBytes(data []byte) *CommitView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	msg := ""
	if blank >= 0 {
		headerEnd = blank
		msg = string(data[blank+2:])
	}
	c := &CommitView{Message: msg}
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
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
			c.Tree = val
		case "parent":
			c.Parents = append(c.Parents, val)
		case "author":
			c.Author = val
		case "committer":
			c.Committer = val
		}
	}
	if !isOID(c.Tree) {
		return nil
	}
	for _, p := range c.Parents {
		if !isOID(p) {
			return nil
		}
	}
	return c
}

// buildCommitBytes renders a commit object. author/committer are full ident
// lines without the timestamp ("Name <email>"); epoch supplies the timestamp.
func buildCommitBytes(tree string, parents []string, author, committer string, epoch int64, message string) []byte {
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		b.WriteString("parent " + p + "\n")
	}
	ts := strconv.FormatInt(epoch, 10) + " +0000"
	b.WriteString("author " + author + " " + ts + "\n")
	b.WriteString("committer " + committer + " " + ts + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	return []byte(b.String())
}

// --- trees ----------------------------------------------------------------

func parseTreeBytes(data []byte) []TreeEntry {
	var entries []TreeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			return nil
		}
		sp += off
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul < 0 {
			return nil
		}
		nul += sp + 1
		if nul+21 > len(data) {
			return nil
		}
		mode := string(data[off:sp])
		for _, c := range mode {
			if c < '0' || c > '7' {
				return nil
			}
		}
		entries = append(entries, TreeEntry{
			Mode: mode,
			Name: string(data[sp+1 : nul]),
			OID:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		off = nul + 21
	}
	return entries
}

// compareTreeNames is git's tree sort: byte order, directories as name+"/".
func compareTreeNames(a, b TreeEntry) int {
	an, bn := a.Name, b.Name
	if a.Mode == "40000" {
		an += "/"
	}
	if b.Mode == "40000" {
		bn += "/"
	}
	return strings.Compare(an, bn)
}

func buildTreeBytes(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return compareTreeNames(sorted[i], sorted[j]) < 0 })
	var b bytes.Buffer
	for _, e := range sorted {
		raw, err := hex.DecodeString(e.OID)
		if err != nil || len(raw) != 20 {
			continue
		}
		fmt.Fprintf(&b, "%s %s\x00", e.Mode, e.Name)
		b.Write(raw)
	}
	return b.Bytes()
}

// --- tags -----------------------------------------------------------------

func parseTagBytes(data []byte) *TagView {
	blank := bytes.Index(data, []byte("\n\n"))
	headerEnd := len(data)
	msg := ""
	if blank >= 0 {
		headerEnd = blank
		msg = string(data[blank+2:])
	}
	t := &TagView{Message: msg}
	for _, line := range strings.Split(string(data[:headerEnd]), "\n") {
		if line == "" {
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			return nil
		}
		key, val := line[:sp], line[sp+1:]
		switch key {
		case "object":
			t.Object = val
		case "type":
			t.Type = val
		case "tag":
			t.Tag = val
		case "tagger":
			t.Tagger = val
		}
	}
	if !isOID(t.Object) {
		return nil
	}
	switch t.Type {
	case "commit", "tree", "blob", "tag":
	default:
		return nil
	}
	return t
}

// --- packfile reading -----------------------------------------------------

// deltaError is a pack/delta corruption error.
type packError struct{ msg string }

func (e *packError) Error() string { return e.msg }

// readPack parses a version 2/3 pack and resolves every object, including
// OFS_DELTA and REF_DELTA. resolveBase supplies a thin pack's external base.
func readPack(pack []byte, resolveBase func(oid string) *Object) ([]*Object, error) {
	if len(pack) < 32 {
		return nil, &packError{"pack is too short"}
	}
	if string(pack[:4]) != "PACK" {
		return nil, &packError{"pack does not start with PACK"}
	}
	version := binary.BigEndian.Uint32(pack[4:8])
	if version != 2 && version != 3 {
		return nil, &packError{fmt.Sprintf("unsupported pack version %d", version)}
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	trailerStart := len(pack) - 20
	// Verify the trailer checksum.
	sum := sha1.Sum(pack[:trailerStart])
	if !bytes.Equal(sum[:], pack[trailerStart:]) {
		return nil, &packError{"pack trailer checksum mismatch"}
	}

	type rawEntry struct {
		offset     int
		typeCode   int
		size       int
		data       []byte // inflated delta data, or the plain object data
		baseOffset int    // ofs-delta only
		baseOID    string // ref-delta only
		resolved   *Object
		resolving  bool
	}
	raw := make([]*rawEntry, 0, count)
	byOffset := make(map[int]*rawEntry)
	off := 12
	for i := 0; i < count; i++ {
		entryOffset := off
		if off >= trailerStart {
			return nil, &packError{"pack entry header past the trailer"}
		}
		// Size varint: low 4 bits in the first byte, then 7 bits per byte.
		first := pack[off]
		off++
		size := int(first & 0x0f)
		shift := uint(4)
		b := first
		for b&0x80 != 0 {
			if off >= trailerStart {
				return nil, &packError{"pack entry size varint truncated"}
			}
			b = pack[off]
			off++
			size += int(b&0x7f) << shift
			shift += 7
		}
		typeCode := int(first>>4) & 0x07
		e := &rawEntry{offset: entryOffset, typeCode: typeCode, size: size}
		switch typeCode {
		case 6: // ofs-delta
			if off >= trailerStart {
				return nil, &packError{"ofs-delta has no base offset"}
			}
			b := pack[off]
			off++
			value := int64(b & 0x7f)
			for b&0x80 != 0 {
				if off >= trailerStart {
					return nil, &packError{"ofs-delta base offset truncated"}
				}
				b = pack[off]
				off++
				value = ((value + 1) << 7) + int64(b&0x7f)
			}
			if value <= 0 || int(value) > entryOffset {
				return nil, &packError{"ofs-delta base offset invalid"}
			}
			e.baseOffset = entryOffset - int(value)
		case 7: // ref-delta
			if off+20 > trailerStart {
				return nil, &packError{"ref-delta base id truncated"}
			}
			e.baseOID = hex.EncodeToString(pack[off : off+20])
			off += 20
		case 1, 2, 3, 4:
		default:
			return nil, &packError{fmt.Sprintf("pack entry has unknown type code %d", typeCode)}
		}
		data, next, err := inflateAt(pack[:trailerStart], off)
		if err != nil {
			return nil, err
		}
		if len(data) != size {
			return nil, &packError{"pack entry inflated size mismatch"}
		}
		e.data = data
		off = next
		raw = append(raw, e)
		byOffset[entryOffset] = e
	}
	if off != trailerStart {
		return nil, &packError{"pack has stray bytes before its trailer"}
	}

	typeNames := map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}
	resolvedByOID := make(map[string]*rawEntry)
	var resolve func(e *rawEntry) (*Object, error)
	resolve = func(e *rawEntry) (*Object, error) {
		if e.resolved != nil {
			return e.resolved, nil
		}
		if e.resolving {
			return nil, &packError{"delta cycle in pack"}
		}
		e.resolving = true
		var typ string
		var data []byte
		switch e.typeCode {
		case 6:
			base := byOffset[e.baseOffset]
			if base == nil {
				return nil, &packError{"ofs-delta base not in pack"}
			}
			bobj, err := resolve(base)
			if err != nil {
				return nil, err
			}
			out, err := applyDelta(bobj.Data, e.data)
			if err != nil {
				return nil, err
			}
			typ, data = bobj.Type, out
		case 7:
			var bobj *Object
			if be := resolvedByOID[e.baseOID]; be != nil {
				var err error
				bobj, err = resolve(be)
				if err != nil {
					return nil, err
				}
			} else if resolveBase != nil {
				bobj = resolveBase(e.baseOID)
			}
			if bobj == nil {
				return nil, &packError{"ref-delta base not available"}
			}
			out, err := applyDelta(bobj.Data, e.data)
			if err != nil {
				return nil, err
			}
			typ, data = bobj.Type, out
		default:
			typ = typeNames[e.typeCode]
			data = e.data
		}
		obj := &Object{Type: typ, Data: data}
		e.resolved = obj
		e.resolving = false
		resolvedByOID[hashObject(typ, data)] = e
		return obj, nil
	}

	out := make([]*Object, 0, len(raw))
	seen := make(map[string]bool)
	for _, e := range raw {
		obj, err := resolve(e)
		if err != nil {
			return nil, err
		}
		oid := hashObject(obj.Type, obj.Data)
		if seen[oid] {
			continue
		}
		seen[oid] = true
		out = append(out, obj)
	}
	return out, nil
}

// inflateAt inflates one zlib stream starting at offset and returns the data
// plus the offset just past the stream. It relies on Go's flate reader
// consuming exactly the stream's bytes from an io.ByteReader (verified by
// test: bytes.Reader implements ReadByte, so flate never over-reads).
func inflateAt(pack []byte, offset int) ([]byte, int, error) {
	r := bytes.NewReader(pack[offset:])
	zr, err := zlib.NewReader(r)
	if err != nil {
		return nil, 0, &packError{"zlib stream did not open: " + err.Error()}
	}
	data, err := io.ReadAll(zr)
	if err != nil {
		return nil, 0, &packError{"zlib stream did not inflate: " + err.Error()}
	}
	zr.Close()
	consumed := len(pack[offset:]) - r.Len()
	return data, offset + consumed, nil
}

// applyDelta applies a git delta instruction stream to a base object's bytes.
func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readSize := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if off >= len(delta) {
				return 0, &packError{"delta size varint truncated"}
			}
			b := delta[off]
			off++
			value += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
			if shift > 63 {
				return 0, &packError{"delta size varint too long"}
			}
		}
		return value, nil
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
		return nil, &packError{"delta base size mismatch"}
	}
	out := make([]byte, 0, tgtSize)
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			copyOff, copySize := 0, 0
			shift := uint(0)
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if off >= len(delta) {
						return nil, &packError{"delta copy offset truncated"}
					}
					copyOff += int(delta[off]) << shift
					off++
				}
				shift += 8
			}
			shift = 0
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if off >= len(delta) {
						return nil, &packError{"delta copy size truncated"}
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
				return nil, &packError{"delta copy out of range"}
			}
			out = append(out, base[copyOff:copyOff+copySize]...)
		} else if op != 0 {
			lit := int(op & 0x7f)
			if off+lit > len(delta) || len(out)+lit > tgtSize {
				return nil, &packError{"delta insert out of range"}
			}
			out = append(out, delta[off:off+lit]...)
			off += lit
		} else {
			return nil, &packError{"delta opcode 0 is invalid"}
		}
	}
	if len(out) != tgtSize {
		return nil, &packError{"delta produced the wrong size"}
	}
	return out, nil
}

// --- packfile writing -----------------------------------------------------

// writePack builds a version 2 pack of plain objects (no deltas), in the
// caller's order, with the SHA-1 trailer.
func writePack(objects []*Object) []byte {
	var buf bytes.Buffer
	header := make([]byte, 12)
	copy(header, "PACK")
	binary.BigEndian.PutUint32(header[4:8], 2)
	binary.BigEndian.PutUint32(header[8:12], uint32(len(objects)))
	buf.Write(header)
	typeCodes := map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
	for _, obj := range objects {
		code := typeCodes[obj.Type]
		// Type/size varint.
		size := len(obj.Data)
		first := byte(code<<4) | byte(size&0x0f)
		size >>= 4
		if size > 0 {
			first |= 0x80
		}
		buf.WriteByte(first)
		for size > 0 {
			b := byte(size & 0x7f)
			size >>= 7
			if size > 0 {
				b |= 0x80
			}
			buf.WriteByte(b)
		}
		zw, _ := zlib.NewWriterLevel(&buf, zlib.DefaultCompression)
		zw.Write(obj.Data)
		zw.Close()
	}
	sum := sha1.Sum(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes()
}

// emptyPack is a valid 0-object pack (used for fetch-nothing-new).
var emptyPack = func() []byte { return writePack(nil) }()

// --- reachability ---------------------------------------------------------

// collectReachable returns every object reachable from roots, following
// commit->tree/parents, tree->entries, tag->target. With maxDepth >= 0 it
// limits the COMMIT depth from the roots (0 = the roots themselves); trees
// and blobs of included commits are always fully included. It also returns
// the shallow boundary: included commits whose parents were cut by depth.
// stopAt names commits whose parents must not be followed (a shallow
// client's boundaries: it holds the boundary commit itself, not its
// ancestors); pass nil when the traversal should not stop early.
func collectReachable(r *Repo, roots []string, maxDepth int, stopAt map[string]bool) (objects map[string]bool, boundary []string) {
	objects = make(map[string]bool)
	boundarySet := make(map[string]bool)
	type item struct {
		oid   string
		depth int // commit depth; -1 for non-commit objects
	}
	queue := make([]item, 0, len(roots))
	for _, root := range roots {
		if isOID(root) && root != zeroOID {
			queue = append(queue, item{root, 0})
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if objects[cur.oid] {
			continue
		}
		obj := r.Objects[cur.oid]
		if obj == nil {
			continue
		}
		objects[cur.oid] = true
		switch obj.Type {
		case "commit":
			c := r.parseCommit(cur.oid)
			if c == nil {
				continue
			}
			queue = append(queue, item{c.Tree, -1})
			if stopAt != nil && stopAt[cur.oid] {
				// A shallow boundary: held, but its parents are not.
				continue
			}
			cut := false
			for _, p := range c.Parents {
				if maxDepth >= 0 && cur.depth+1 > maxDepth {
					cut = true
					continue
				}
				queue = append(queue, item{p, cur.depth + 1})
			}
			if cut && !boundarySet[cur.oid] {
				boundarySet[cur.oid] = true
				boundary = append(boundary, cur.oid)
			}
		case "tag":
			t := r.parseTag(cur.oid)
			if t != nil {
				queue = append(queue, item{t.Object, cur.depth})
			}
		case "tree":
			for _, e := range r.parseTree(cur.oid) {
				if e.Mode == "160000" {
					continue // gitlink: names a commit the repo does not hold
				}
				queue = append(queue, item{e.OID, -1})
			}
		}
	}
	sort.Strings(boundary)
	return objects, boundary
}

// --- tree helpers ---------------------------------------------------------

// flattenTree maps every file path in the tree to its blob oid.
func flattenTree(r *Repo, treeOID string) map[string]string {
	out := make(map[string]string)
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		for _, e := range r.parseTree(oid) {
			path := prefix + e.Name
			if e.Mode == "40000" {
				walk(e.OID, path+"/")
			} else {
				out[path] = e.OID
			}
		}
	}
	walk(treeOID, "")
	return out
}

// FileChange is one path's difference between two trees.
type FileChange struct {
	Path       string
	Status     string // added | modified | removed
	OldBlobOID string
	NewBlobOID string
}

// diffTrees compares two root trees ("" means the empty tree).
func diffTrees(r *Repo, baseTree, headTree string) []FileChange {
	base := map[string]string{}
	if baseTree != "" {
		base = flattenTree(r, baseTree)
	}
	head := map[string]string{}
	if headTree != "" {
		head = flattenTree(r, headTree)
	}
	var changes []FileChange
	for path, noid := range head {
		if boid, ok := base[path]; !ok {
			changes = append(changes, FileChange{Path: path, Status: "added", NewBlobOID: noid})
		} else if boid != noid {
			changes = append(changes, FileChange{Path: path, Status: "modified", OldBlobOID: boid, NewBlobOID: noid})
		}
	}
	for path, boid := range base {
		if _, ok := head[path]; !ok {
			changes = append(changes, FileChange{Path: path, Status: "removed", OldBlobOID: boid})
		}
	}
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })
	return changes
}

// countLines counts newline-terminated lines (a trailing partial line counts).
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

// lineDiff counts added/removed lines between two blobs (multiset line diff).
func lineDiff(oldData, newData []byte) (additions, deletions int) {
	oldLines := map[string]int{}
	for _, l := range strings.Split(string(oldData), "\n") {
		oldLines[l]++
	}
	for _, l := range strings.Split(string(newData), "\n") {
		if oldLines[l] > 0 {
			oldLines[l]--
		} else {
			additions++
		}
	}
	for _, c := range oldLines {
		deletions += c
	}
	return additions, deletions
}

// mergeBase finds a common ancestor of two commits (first found, BFS from
// both sides by walking a's ancestry then checking b's). Returns "" if none.
func mergeBase(r *Repo, a, b string) string {
	if a == "" || b == "" {
		return ""
	}
	a = r.peel(a)
	b = r.peel(b)
	ancestors := make(map[string]bool)
	queue := []string{a}
	for len(queue) > 0 {
		cur := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if ancestors[cur] {
			continue
		}
		ancestors[cur] = true
		if c := r.parseCommit(cur); c != nil {
			queue = append(queue, c.Parents...)
		}
	}
	queue = []string{b}
	seen := make(map[string]bool)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if ancestors[cur] {
			return cur
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		if c := r.parseCommit(cur); c != nil {
			queue = append(queue, c.Parents...)
		}
	}
	return ""
}

var errNoCommonAncestor = errors.New("no common ancestor")
