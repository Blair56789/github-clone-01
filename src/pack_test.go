package main

import (
	"bytes"
	"compress/zlib"
	"testing"
)

// TestInflateAtExactConsumption verifies that inflateAt consumes exactly the
// zlib stream's bytes: two concatenated streams must inflate independently
// with the second starting exactly where the first ended.
func TestInflateAtExactConsumption(t *testing.T) {
	// Build a 2-object pack by hand and read it back through readPack.
	blob1 := []byte("hello world, this is blob one\n")
	blob2 := []byte("second blob with different content 12345\n")
	pack := writePack([]*Object{
		{Type: "blob", Data: blob1},
		{Type: "blob", Data: blob2},
	})
	objects, err := readPack(pack, nil)
	if err != nil {
		t.Fatalf("readPack failed: %v", err)
	}
	if len(objects) != 2 {
		t.Fatalf("expected 2 objects, got %d", len(objects))
	}
	if !bytes.Equal(objects[0].Data, blob1) || !bytes.Equal(objects[1].Data, blob2) {
		t.Fatalf("object data mismatch")
	}

	// Direct check: two zlib streams back to back.
	var buf bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&buf, zlib.DefaultCompression)
	zw.Write([]byte("first stream"))
	zw.Close()
	mid := buf.Len()
	zw2, _ := zlib.NewWriterLevel(&buf, zlib.DefaultCompression)
	zw2.Write([]byte("second stream"))
	zw2.Close()
	packed := buf.Bytes()
	data, next, err := inflateAt(packed, 0)
	if err != nil {
		t.Fatalf("inflateAt failed: %v", err)
	}
	if string(data) != "first stream" {
		t.Fatalf("first stream mismatch: %q", data)
	}
	if next != mid {
		t.Fatalf("inflateAt consumed %d bytes, expected %d (over-read by %d)", next, mid, next-mid)
	}
	data2, _, err := inflateAt(packed, next)
	if err != nil {
		t.Fatalf("second inflateAt failed: %v", err)
	}
	if string(data2) != "second stream" {
		t.Fatalf("second stream mismatch: %q", data2)
	}
}

// TestPackRoundTrip verifies a pack containing a commit, a tree and a blob
// survives writePack -> readPack with identical oids.
func TestPackRoundTrip(t *testing.T) {
	blob := []byte("content\n")
	blobOID := hashObject("blob", blob)
	tree := buildTreeBytes([]TreeEntry{{Mode: "100644", Name: "a.txt", OID: blobOID}})
	treeOID := hashObject("tree", tree)
	commit := buildCommitBytes(treeOID, nil, "ada iversen <a@x>", "ada iversen <a@x>", 1700000000, "init\n")
	commitOID := hashObject("commit", commit)
	pack := writePack([]*Object{
		{Type: "commit", Data: commit},
		{Type: "tree", Data: tree},
		{Type: "blob", Data: blob},
	})
	objects, err := readPack(pack, nil)
	if err != nil {
		t.Fatalf("readPack failed: %v", err)
	}
	seen := map[string]bool{}
	for _, o := range objects {
		seen[hashObject(o.Type, o.Data)] = true
	}
	for _, oid := range []string{commitOID, treeOID, blobOID} {
		if !seen[oid] {
			t.Fatalf("object %s missing after round trip", oid)
		}
	}
}
