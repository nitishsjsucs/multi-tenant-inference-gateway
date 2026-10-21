// Package prefixhash splits text into fixed-size blocks and hashes each block
// chained to its predecessor, so block i's hash identifies the whole prefix
// up to and including block i. This mirrors how vLLM's automatic prefix
// caching keys KV blocks, and how the Gateway API Inference Extension's
// prefix scorer approximates it on the routing side.
package prefixhash

import (
	"encoding/binary"
	"hash/fnv"
)

// Blocks returns the chained hashes of the first maxBlocks full blocks of
// text (a trailing partial block is ignored, as only full KV blocks are
// cached). The seed, typically the model or LoRA adapter name, keeps caches
// of different adapters apart. maxBlocks <= 0 means no limit.
func Blocks(seed, text string, blockSize, maxBlocks int) []uint64 {
	if blockSize <= 0 {
		return nil
	}
	n := len(text) / blockSize
	if maxBlocks > 0 && n > maxBlocks {
		n = maxBlocks
	}
	out := make([]uint64, n)
	h := fnv.New64a()
	h.Write([]byte(seed))
	prev := h.Sum64()
	var buf [8]byte
	for i := range n {
		h.Reset()
		binary.LittleEndian.PutUint64(buf[:], prev)
		h.Write(buf[:])
		h.Write([]byte(text[i*blockSize : (i+1)*blockSize]))
		prev = h.Sum64()
		out[i] = prev
	}
	return out
}
