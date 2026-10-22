package tokenizer

const (
	noSymbol        = -1
	binaryHeapArity = 2
)

type mergeScratch struct {
	symbols    []symbol
	candidates []candidate
}

type symbol struct {
	token      uint32
	prev, next int32
	version    uint32
	merged     bool
}

type candidate struct {
	rank    uint32
	left    int32
	version uint32
}

func (s *mergeScratch) merge(c *Counter, piece string) int {
	s.symbols = s.symbols[:0]
	for index := range len(piece) {
		s.symbols = append(s.symbols, symbol{token: c.byteSymbol[piece[index]], prev: int32(index - 1), next: int32(index + 1)})
	}
	s.symbols[len(s.symbols)-1].next = noSymbol
	s.candidates = s.candidates[:0]
	for left := range int32(len(s.symbols) - 1) {
		if found, ok := c.merges[pairKey(s.symbols[left].token, s.symbols[left+1].token)]; ok {
			s.candidates = append(s.candidates, candidate{rank: found.rank, left: left})
		}
	}
	for parent := len(s.candidates)/binaryHeapArity - 1; parent >= 0; parent-- {
		s.siftDown(parent)
	}
	remaining := len(s.symbols)
	for len(s.candidates) > 0 {
		next := s.pop()
		left := &s.symbols[next.left]
		if left.merged || left.version != next.version || left.next == noSymbol {
			continue
		}
		right := &s.symbols[left.next]
		found, ok := c.merges[pairKey(left.token, right.token)]
		if !ok || found.rank != next.rank {
			continue
		}
		left.token = found.result
		left.version++
		right.merged = true
		left.next = right.next
		if right.next != noSymbol {
			s.symbols[right.next].prev = next.left
		}
		remaining--
		if left.prev != noSymbol {
			s.symbols[left.prev].version++
			s.offer(c, left.prev)
		}
		s.offer(c, next.left)
	}
	return remaining
}

func (s *mergeScratch) offer(c *Counter, left int32) {
	current := s.symbols[left]
	if current.next == noSymbol {
		return
	}
	found, ok := c.merges[pairKey(current.token, s.symbols[current.next].token)]
	if !ok {
		return
	}
	s.push(candidate{rank: found.rank, left: left, version: current.version})
}

func (a candidate) before(b candidate) bool {
	if a.rank != b.rank {
		return a.rank < b.rank
	}
	return a.left < b.left
}

func (s *mergeScratch) push(entry candidate) {
	s.candidates = append(s.candidates, entry)
	child := len(s.candidates) - 1
	for child > 0 {
		parent := (child - 1) / binaryHeapArity
		if !s.candidates[child].before(s.candidates[parent]) {
			break
		}
		s.candidates[child], s.candidates[parent] = s.candidates[parent], s.candidates[child]
		child = parent
	}
}

func (s *mergeScratch) pop() candidate {
	top := s.candidates[0]
	last := len(s.candidates) - 1
	s.candidates[0] = s.candidates[last]
	s.candidates = s.candidates[:last]
	s.siftDown(0)
	return top
}

func (s *mergeScratch) siftDown(parent int) {
	size := len(s.candidates)
	for {
		smallest, left, right := parent, binaryHeapArity*parent+1, binaryHeapArity*parent+binaryHeapArity
		if left < size && s.candidates[left].before(s.candidates[smallest]) {
			smallest = left
		}
		if right < size && s.candidates[right].before(s.candidates[smallest]) {
			smallest = right
		}
		if smallest == parent {
			return
		}
		s.candidates[parent], s.candidates[smallest] = s.candidates[smallest], s.candidates[parent]
		parent = smallest
	}
}
