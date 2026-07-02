package sievestore

import (
	"fmt"
	"strings"
)

// Unified renders a line-based unified diff (3 lines of context) between two
// texts, or "" when they are identical. Small LCS implementation — sieve
// documents are tiny, so O(n·m) is fine and we avoid a vendored dependency.
func Unified(aLabel, bLabel, aText, bText string) string {
	if aText == bText {
		return ""
	}
	ops := diffOps(splitLines(aText), splitLines(bText))
	hunks := buildHunks(ops, 3)
	if len(hunks) == 0 {
		// The texts differ but split to identical lines: a trailing-newline
		// or line-ending-only change. Saying "identical" here would
		// contradict the store's byte-exact pending flag — say what it is.
		return fmt.Sprintf("--- %s\n+++ %s\n\\ texts differ only in trailing newline or line endings (no line content changed)\n", aLabel, bLabel)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "--- %s\n+++ %s\n", aLabel, bLabel)
	for _, h := range hunks {
		sb.WriteString(h)
	}
	return sb.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

type diffOp struct {
	kind byte // ' ' equal, '-' only in a, '+' only in b
	text string
}

func diffOps(a, b []string) []diffOp {
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				dp[i][j] = dp[i+1][j+1] + 1
			case dp[i+1][j] >= dp[i][j+1]:
				dp[i][j] = dp[i+1][j]
			default:
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	ops := make([]diffOp, 0, n+m)
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			ops = append(ops, diffOp{' ', a[i]})
			i++
			j++
		case dp[i+1][j] >= dp[i][j+1]:
			ops = append(ops, diffOp{'-', a[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', b[j]})
			j++
		}
	}
	for ; i < n; i++ {
		ops = append(ops, diffOp{'-', a[i]})
	}
	for ; j < m; j++ {
		ops = append(ops, diffOp{'+', b[j]})
	}
	return ops
}

// buildHunks groups changes into unified hunks with ctx lines of context,
// merging changes separated by at most 2·ctx equal lines.
func buildHunks(ops []diffOp, ctx int) []string {
	// Line numbers (1-based) in a and b at each op.
	aPos := make([]int, len(ops))
	bPos := make([]int, len(ops))
	aLine, bLine := 1, 1
	for idx, op := range ops {
		aPos[idx], bPos[idx] = aLine, bLine
		if op.kind != '+' {
			aLine++
		}
		if op.kind != '-' {
			bLine++
		}
	}

	var hunks []string
	idx := 0
	for idx < len(ops) {
		if ops[idx].kind == ' ' {
			idx++
			continue
		}
		// A change starts here; find the last change of this hunk group.
		last := idx
		k := idx + 1
		for k < len(ops) {
			if ops[k].kind != ' ' {
				last = k
				k++
				continue
			}
			run := 0
			for k+run < len(ops) && ops[k+run].kind == ' ' {
				run++
			}
			if k+run < len(ops) && run <= 2*ctx {
				k += run // the next change is close enough — merge
				continue
			}
			break
		}
		lo := max(idx-ctx, 0)
		hi := min(last+ctx, len(ops)-1)

		aCount, bCount := 0, 0
		var body strings.Builder
		for t := lo; t <= hi; t++ {
			body.WriteByte(ops[t].kind)
			body.WriteString(ops[t].text)
			body.WriteByte('\n')
			if ops[t].kind != '+' {
				aCount++
			}
			if ops[t].kind != '-' {
				bCount++
			}
		}
		aStart, bStart := aPos[lo], bPos[lo]
		if aCount == 0 {
			aStart--
		}
		if bCount == 0 {
			bStart--
		}
		hunks = append(hunks, fmt.Sprintf("@@ -%d,%d +%d,%d @@\n%s", aStart, aCount, bStart, bCount, body.String()))
		idx = hi + 1
	}
	return hunks
}
