package diff

// myers computes the shortest edit script between a and b using the
// O(ND) Myers diff algorithm. It returns a slice of Ops in forward order.
func myers(a, b []string) []Op {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil
	}
	maxD := n + m
	offset := maxD
	v := make([]int, 2*maxD+1)
	v[offset+1] = 0

	var trace [][]int
	foundD := -1
	for d := 0; d <= maxD; d++ {
		snap := make([]int, len(v))
		copy(snap, v)
		trace = append(trace, snap)

		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[offset+k-1] < v[offset+k+1]) {
				x = v[offset+k+1]
			} else {
				x = v[offset+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[offset+k] = x
			if x >= n && y >= m {
				foundD = d
				break
			}
		}
		if foundD >= 0 {
			break
		}
	}

	if foundD < 0 {
		return nil
	}

	var rev []Op
	x, y := n, m
	for d := foundD; d > 0; d-- {
		k := x - y
		prev := trace[d]
		var prevK int
		if k == -d || (k != d && prev[offset+k-1] < prev[offset+k+1]) {
			prevK = k + 1
		} else {
			prevK = k - 1
		}
		prevX := prev[offset+prevK]
		prevY := prevX - prevK

		if prevK == k+1 {
			for i := x - prevX; i > 0; i-- {
				rev = append(rev, OpContext)
			}
			rev = append(rev, OpAdd)
		} else {
			for i := y - prevY; i > 0; i-- {
				rev = append(rev, OpContext)
			}
			rev = append(rev, OpDelete)
		}
		x, y = prevX, prevY
	}
	for i := x; i > 0; i-- {
		rev = append(rev, OpContext)
	}

	ops := make([]Op, len(rev))
	for i, j := 0, len(rev)-1; j >= 0; i, j = i+1, j-1 {
		ops[i] = rev[j]
	}
	return ops
}
