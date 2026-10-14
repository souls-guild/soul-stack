package applyrun

// lineEdit is one maximal run where a text departs from the base: base lines
// [s, e) are replaced by the text's lines [hs, he). s == e is an insertion,
// hs == he a deletion.
type lineEdit struct{ s, e, hs, he int }

// diffLines is Myers' O(ND) shortest edit script from a to b, lines compared as
// interned ids. It gives up — ok false — once the script needs more than
// maxEdits edits or the search has spent *budget steps, so a text whose hosts
// differ on most of their lines costs a bounded amount: the caller compares it
// whole instead. budget is shared by every host of one leaf; a compared line and
// an allocated trace slot each cost one.
func diffLines(a, b []int, maxEdits int, budget *int) ([]lineEdit, bool) {
	n, m := len(a), len(b)
	if maxEdits > n+m {
		maxEdits = n + m
	}
	off := maxEdits + 1
	v := make([]int, 2*maxEdits+3)
	var trace [][]int
	for d := 0; d <= maxEdits; d++ {
		// v as the previous round left it, for k in [-d-1, d+1]: what the walk
		// back needs to find the step that reached each diagonal.
		trace = append(trace, append([]int(nil), v[off-d-1:off+d+2]...))
		*budget -= 2*d + 3
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
				*budget--
			}
			*budget--
			if *budget < 0 {
				return nil, false
			}
			v[off+k] = x
			if x >= n && y >= m {
				return editsFromTrace(trace, d, n, m), true
			}
		}
	}
	return nil, false
}

// editsFromTrace walks the Myers trace back from (n, m) and folds the path into
// runs of consecutive non-equal steps.
func editsFromTrace(trace [][]int, dEnd, n, m int) []lineEdit {
	const (
		equal = iota
		del
		ins
	)
	var back []int
	x, y := n, m
	for d := dEnd; d >= 0; d-- {
		snap := trace[d]
		at := func(k int) int { return snap[k+d+1] }
		k := x - y
		prevK := k - 1
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			prevK = k + 1
		}
		prevX := at(prevK)
		prevY := prevX - prevK
		for x > prevX && y > prevY {
			back = append(back, equal)
			x--
			y--
		}
		if d > 0 {
			if x == prevX {
				back = append(back, ins)
			} else {
				back = append(back, del)
			}
		}
		x, y = prevX, prevY
	}

	var out []lineEdit
	x, y = 0, 0
	open := false
	var cur lineEdit
	for i := len(back) - 1; i >= 0; i-- {
		switch back[i] {
		case equal:
			if open {
				cur.e, cur.he = x, y
				out = append(out, cur)
				open = false
			}
			x++
			y++
		case del, ins:
			if !open {
				cur = lineEdit{s: x, hs: y}
				open = true
			}
			if back[i] == del {
				x++
			} else {
				y++
			}
		}
	}
	if open {
		cur.e, cur.he = x, y
		out = append(out, cur)
	}
	return out
}
