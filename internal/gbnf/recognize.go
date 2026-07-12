package gbnf

import "slices"

// Accepts reports whether the grammar matches the whole of input. It is the
// in-process meaning of the grammar: the same answer a runtime's token mask would
// converge to if it only ever sampled grammar-permitted tokens and the result were
// checked against the rules. Tests use it to prove a compiled grammar accepts
// exactly the JSON values its source schema admits.
//
// The recognizer is a backtracking matcher over the rule AST. A node maps an input
// position to the set of positions it can advance to (more than one, because
// alternation and repetition branch), and the input is accepted when the root rule
// can advance from the start to the very end. Positions reached are memoized per
// (node, position) so shared subgrammars are not re-explored, which keeps the JSON
// grammars this package emits well within a fixed work budget. The budget counts
// matches attempted, including one per start position when advancing a set, so it
// bounds total work rather than node visits: an input that widens the position set
// pays for the width. It is a guard against a pathological grammar or input rather
// than a real limit for the inputs these grammars describe; exhausting it reports
// no match rather than looping or running long.
func (g *Grammar) Accepts(input string) bool {
	m := &matcher{g: g, runes: []rune(input), memo: map[memoKey][]int{}, budget: 1 << 20}
	root, ok := g.rules[g.root]
	if !ok {
		return false
	}
	for _, end := range m.match(root, 0) {
		if end == len(m.runes) {
			return true
		}
	}
	return false
}

type memoKey struct {
	node node
	pos  int
}

type matcher struct {
	g      *Grammar
	runes  []rune
	memo   map[memoKey][]int
	budget int
}

// match returns every input position the node can advance to from pos. An empty
// slice means the node does not match there. Results for a (node, pos) pair are
// memoized; only nodes that can recurse (ref) are keyed, since they are where
// sharing and repetition compound.
func (m *matcher) match(n node, pos int) []int {
	if m.budget <= 0 {
		return nil
	}
	m.budget--

	switch v := n.(type) {
	case lit:
		end := pos
		for _, r := range v.s {
			if end >= len(m.runes) || m.runes[end] != r {
				return nil
			}
			end++
		}
		return []int{end}

	case class:
		if pos >= len(m.runes) {
			return nil
		}
		if classMatch(v, m.runes[pos]) {
			return []int{pos + 1}
		}
		return nil

	case ref:
		key := memoKey{n, pos}
		if cached, ok := m.memo[key]; ok {
			return cached
		}
		// Reserve before recursing so a cyclic rule sees an in-progress empty result
		// instead of looping forever; the real positions overwrite it on return.
		m.memo[key] = nil
		got := m.match(m.g.rules[v.name], pos)
		m.memo[key] = got
		return got

	case seq:
		ends := []int{pos}
		for _, it := range v.items {
			ends = m.advanceAll(it, ends)
			if len(ends) == 0 {
				return nil
			}
		}
		return ends

	case alt:
		var ends []int
		for _, it := range v.items {
			ends = mergePositions(ends, m.match(it, pos))
		}
		return ends

	case opt:
		return mergePositions([]int{pos}, m.match(v.child, pos))

	case star:
		return m.repeat(v.child, pos)

	case plus:
		first := m.match(v.child, pos)
		var ends []int
		for _, p := range first {
			ends = mergePositions(ends, m.repeat(v.child, p))
		}
		return ends
	}
	return nil
}

// advanceAll matches node from each of the given start positions and merges the
// resulting positions, the set-valued step of matching a sequence. Advancing from
// a set of k positions costs k matches, so it charges the budget for all of them:
// the budget bounds the recognizer's total work, and a step whose cost grows with
// the input would otherwise escape it.
func (m *matcher) advanceAll(n node, starts []int) []int {
	m.budget -= len(starts)
	var ends []int
	for _, p := range starts {
		ends = mergePositions(ends, m.match(n, p))
	}
	return ends
}

// repeat returns every position reachable by matching child zero or more times
// from pos, the closure that defines star. A child that matches empty cannot add a
// new position, so the frontier strictly grows and the loop terminates. The result
// is sorted ascending, the order mergePositions relies on.
func (m *matcher) repeat(child node, pos int) []int {
	reached := []int{pos}
	seen := map[int]struct{}{pos: {}}
	frontier := []int{pos}
	for len(frontier) > 0 {
		var next []int
		for _, p := range frontier {
			for _, q := range m.match(child, p) {
				if _, ok := seen[q]; ok {
					continue
				}
				seen[q] = struct{}{}
				reached = append(reached, q)
				next = append(next, q)
			}
		}
		frontier = next
	}
	slices.Sort(reached)
	return reached
}

func classMatch(c class, r rune) bool {
	in := false
	for _, rg := range c.ranges {
		if r >= rg[0] && r <= rg[1] {
			in = true
			break
		}
	}
	return in != c.negated
}

// mergePositions unions two ascending position sets into a third, keeping the
// result ascending and duplicate-free so memoized results stay small. Both inputs
// are already sorted, so a two-pointer merge costs O(len(a)+len(b)); a pairwise
// scan would be quadratic in the number of positions, which is what lets a long
// input dominate the cost of a single step.
func mergePositions(a, b []int) []int {
	if len(a) == 0 {
		return b
	}
	if len(b) == 0 {
		return a
	}
	out := make([]int, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] < b[j]:
			out = append(out, a[i])
			i++
		case a[i] > b[j]:
			out = append(out, b[j])
			j++
		default:
			out = append(out, a[i])
			i++
			j++
		}
	}
	out = append(out, a[i:]...)
	return append(out, b[j:]...)
}
