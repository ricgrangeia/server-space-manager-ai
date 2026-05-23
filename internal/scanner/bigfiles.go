package scanner

import (
	"container/heap"
	"sort"
)

// bigFile is one candidate for the "top N largest files" set.
type bigFile struct {
	path string
	size int64
}

// minHeap implements container/heap over bigFile slices, ordered by size
// ascending. The smallest entry is always at index 0, so when the heap is
// full we can decide in O(1) whether a new file is large enough to displace
// it, and pop the smallest with the next push.
type minHeap []bigFile

func (m minHeap) Len() int           { return len(m) }
func (m minHeap) Less(i, j int) bool { return m[i].size < m[j].size }
func (m minHeap) Swap(i, j int)      { m[i], m[j] = m[j], m[i] }

func (m *minHeap) Push(x any) { *m = append(*m, x.(bigFile)) }
func (m *minHeap) Pop() any {
	old := *m
	n := len(old)
	x := old[n-1]
	*m = old[:n-1]
	return x
}

// bigFilesHeap collects the N largest files observed across a scan without
// retaining the full file list in memory. consider() is O(log N), called
// once per file walked — cheap even on million-file hosts.
type bigFilesHeap struct {
	cap int
	h   *minHeap
}

func newBigFilesHeap(capacity int) *bigFilesHeap {
	h := &minHeap{}
	heap.Init(h)
	return &bigFilesHeap{cap: capacity, h: h}
}

// consider pushes a new file into the heap if it's larger than the current
// smallest entry (or if the heap isn't full yet).
func (b *bigFilesHeap) consider(path string, size int64) {
	if b.cap <= 0 {
		return
	}
	if b.h.Len() < b.cap {
		heap.Push(b.h, bigFile{path, size})
		return
	}
	if size > (*b.h)[0].size {
		(*b.h)[0] = bigFile{path, size}
		heap.Fix(b.h, 0)
	}
}

// sorted returns the collected files in descending order by size, suitable
// for direct rendering as the dashboard's "biggest files" list.
func (b *bigFilesHeap) sorted() []bigFile {
	out := make([]bigFile, b.h.Len())
	copy(out, *b.h)
	sort.Slice(out, func(i, j int) bool { return out[i].size > out[j].size })
	return out
}
