package dashboard

import "strings"

// diffPart is one run of a word diff: text kept, removed or added.
type diffPart struct {
	Text string
	Op   string // "same", "del" or "ins"
}

// wordDiff compares two short texts word by word (longest common
// subsequence), for comparing versions of a fact. Facts are one sentence, so
// the quadratic table is small.
func wordDiff(a, b string) []diffPart {
	aw, bw := strings.Fields(a), strings.Fields(b)
	n, m := len(aw), len(bw)
	lcs := make([][]int, n+1)
	for i := range lcs {
		lcs[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if aw[i] == bw[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out []diffPart
	add := func(op, word string) {
		if len(out) > 0 && out[len(out)-1].Op == op {
			out[len(out)-1].Text += " " + word
			return
		}
		out = append(out, diffPart{Text: word, Op: op})
	}
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case aw[i] == bw[j]:
			add("same", aw[i])
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			add("del", aw[i])
			i++
		default:
			add("ins", bw[j])
			j++
		}
	}
	for ; i < n; i++ {
		add("del", aw[i])
	}
	for ; j < m; j++ {
		add("ins", bw[j])
	}
	return out
}
