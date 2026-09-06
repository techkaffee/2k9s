package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Item: Key is the returned value, Cols are the display columns (aligned automatically).
type Item struct {
	Key  string
	Cols []string
}

var errNoSelection = fmt.Errorf("nothing selected")

// Pick shows a list to choose from. Prefers fzf (type-to-filter),
// falling back to a numbered list when fzf isn't available.
func Pick(prompt, header string, items []Item) (string, error) {
	if len(items) == 0 {
		return "", fmt.Errorf("list is empty")
	}
	if len(items) == 1 {
		return items[0].Key, nil
	}
	if path, err := exec.LookPath("fzf"); err == nil {
		return pickFzf(path, prompt, header, items)
	}
	return pickNumbered(prompt, header, items)
}

// alignCols pads columns to their widest value. Uses rune count so multi-byte
// characters don't throw off the alignment.
func alignCols(items []Item) []string {
	n := 0
	for _, it := range items {
		if len(it.Cols) > n {
			n = len(it.Cols)
		}
	}
	w := make([]int, n)
	for _, it := range items {
		for i, c := range it.Cols {
			if l := len([]rune(c)); l > w[i] {
				w[i] = l
			}
		}
	}

	out := make([]string, len(items))
	for idx, it := range items {
		var sb strings.Builder
		for i := 0; i < n; i++ {
			c := ""
			if i < len(it.Cols) {
				c = it.Cols[i]
			}
			if i == n-1 {
				sb.WriteString(c) // don't pad the last column
			} else {
				pad := w[i] - len([]rune(c))
				sb.WriteString(c)
				sb.WriteString(strings.Repeat(" ", pad))
				sb.WriteString("  ")
			}
		}
		out[idx] = strings.TrimRight(sb.String(), " ")
	}
	return out
}

func pickFzf(fzfPath, prompt, header string, items []Item) (string, error) {
	display := alignCols(items)

	var in bytes.Buffer
	for i := range items {
		// format: <index>\t<display>  -> fzf only shows/filters the display part
		fmt.Fprintf(&in, "%d\t%s\n", i, display[i])
	}

	args := []string{
		"--delimiter=\t",
		"--with-nth=2..",
		"--layout=reverse",
		"--height=60%",
		"--border",
		"--cycle",
		"--prompt=" + prompt + " > ",
	}
	if header != "" {
		args = append(args, "--header="+header)
	}

	cmd := exec.Command(fzfPath, args...)
	cmd.Stdin = &in
	cmd.Stderr = os.Stderr
	var out bytes.Buffer
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		// fzf exit 130 = user pressed Esc/Ctrl-C
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 130 {
			return "", errNoSelection
		}
		return "", fmt.Errorf("fzf: %w", err)
	}

	line := strings.TrimRight(out.String(), "\n")
	if line == "" {
		return "", errNoSelection
	}
	idxStr := line
	if i := strings.Index(line, "\t"); i >= 0 {
		idxStr = line[:i]
	}
	idx, err := strconv.Atoi(strings.TrimSpace(idxStr))
	if err != nil || idx < 0 || idx >= len(items) {
		return "", fmt.Errorf("invalid fzf result: %q", line)
	}
	return items[idx].Key, nil
}

func pickNumbered(prompt, header string, items []Item) (string, error) {
	display := alignCols(items)

	if header != "" {
		fmt.Fprintf(os.Stderr, "\n%s\n", header)
	}
	fmt.Fprintln(os.Stderr)
	width := len(strconv.Itoa(len(items)))
	for i, d := range display {
		fmt.Fprintf(os.Stderr, "  %*d. %s\n", width, i+1, d)
	}
	fmt.Fprintf(os.Stderr, "\n%s [1-%d, Enter = cancel]: ", prompt, len(items))

	sc := bufio.NewScanner(os.Stdin)
	if !sc.Scan() {
		return "", errNoSelection
	}
	s := strings.TrimSpace(sc.Text())
	if s == "" {
		return "", errNoSelection
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > len(items) {
		return "", fmt.Errorf("invalid selection: %q", s)
	}
	return items[n-1].Key, nil
}
