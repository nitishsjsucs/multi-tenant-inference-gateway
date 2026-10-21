package prefixhash

import (
	"strings"
	"testing"
)

func TestBlocksChain(t *testing.T) {
	shared := strings.Repeat("s", 128)
	a := Blocks("m", shared+strings.Repeat("a", 64)+"tail", 64, 0)
	b := Blocks("m", shared+strings.Repeat("b", 64), 64, 0)
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("partial trailing block must be ignored: %d, %d blocks", len(a), len(b))
	}
	if a[0] != b[0] || a[1] != b[1] || a[2] == b[2] {
		t.Errorf("shared prefix blocks should match and the divergent block differ: %x %x", a, b)
	}
	// Identical block content at a different position hashes differently.
	if c := Blocks("m", strings.Repeat("s", 64), 64, 0); c[0] != a[0] {
		t.Error("first block should depend only on seed and content")
	}
	if d := Blocks("m", strings.Repeat("x", 64)+shared, 64, 0); d[1] == a[1] {
		t.Error("a block's hash must depend on everything before it")
	}
	if e := Blocks("adapter", shared, 64, 0); e[0] == a[0] {
		t.Error("the seed (model/adapter) must change every hash")
	}
	if f := Blocks("m", strings.Repeat("s", 640), 64, 4); len(f) != 4 {
		t.Errorf("maxBlocks not applied: %d", len(f))
	}
}
