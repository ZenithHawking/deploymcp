// Package logs trims noisy container/build output into compact, AI-friendly lines.
package logs

import (
	"fmt"
	"regexp"
	"strings"
)

// Options control filtering.
type Options struct {
	Lines   int    // max lines returned (tail)
	Level   string // "" / "all" or "error"
	Compact bool   // strip noise, collapse repeats, shorten stack traces
}

// Result is the filtered output.
type Result struct {
	Lines     []string `json:"lines"`
	Total     int      `json:"total_lines"`
	Truncated bool     `json:"truncated"`
}

const maxLineLen = 400

var (
	ansiRe      = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	errorRe     = regexp.MustCompile(`(?i)\b(error|err!|exception|fatal|panic|traceback|failed|failure|cannot|unable to|segmentation fault|killed|oom|refused|denied|not found|undefined|exit code|exited with)\b`)
	stackRe     = regexp.MustCompile(`^\s+(at |File "|\.\.\.|goroutine |[a-zA-Z0-9_./-]+\.go:\d+)`)
	noiseRe     = regexp.MustCompile(`(?i)(node:internal|internal/modules|internal/process|node_modules/|site-packages/|<frozen |/usr/lib/python|runtime/proc\.go|runtime/asm_)`)
	progressRe  = regexp.MustCompile(`(?i)^(#\d+ (sha256:|\[internal\]|extracting|downloading|DONE \d)|\s*\d+(\.\d+)?\s*(kB|MB|GB)\s*/|.*\b\d{1,3}%\s*\|)`)
	timestampRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?Z\s+`)
)

// Filter applies Options to raw text.
func Filter(raw string, o Options) Result {
	if o.Lines <= 0 {
		o.Lines = 80
	}
	if o.Lines > 2000 {
		o.Lines = 2000
	}
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	total := 0
	var out []string
	if o.Compact {
		lines = compact(lines)
	} else {
		var kept []string
		for _, l := range lines {
			l = strings.TrimRight(ansiRe.ReplaceAllString(l, ""), " \t\r")
			if l != "" {
				kept = append(kept, l)
			}
		}
		lines = kept
	}
	total = len(lines)
	if strings.EqualFold(o.Level, "error") {
		lines = errorsOnly(lines)
	}
	truncated := false
	if len(lines) > o.Lines {
		lines = lines[len(lines)-o.Lines:]
		truncated = true
	}
	out = append(out, lines...)
	if out == nil {
		out = []string{}
	}
	return Result{Lines: out, Total: total, Truncated: truncated}
}

func compact(lines []string) []string {
	var out []string
	var last string
	repeat := 0
	stackRun := 0
	hiddenFrames := 0

	flushRepeat := func() {
		if repeat > 0 {
			out = append(out, fmt.Sprintf("  (previous line repeated %dx)", repeat))
			repeat = 0
		}
	}
	flushHidden := func() {
		if hiddenFrames > 0 {
			out = append(out, fmt.Sprintf("    ... %d more frames hidden", hiddenFrames))
			hiddenFrames = 0
		}
	}

	for _, l := range lines {
		l = ansiRe.ReplaceAllString(l, "")
		l = timestampRe.ReplaceAllString(l, "")
		l = strings.TrimRight(l, " \t\r")
		if strings.TrimSpace(l) == "" || progressRe.MatchString(l) {
			continue
		}
		if len(l) > maxLineLen {
			l = l[:maxLineLen] + " …[truncated]"
		}
		if stackRe.MatchString(l) {
			// Keep first 4 frames that are not from deps/runtime internals.
			if noiseRe.MatchString(l) || stackRun >= 4 {
				hiddenFrames++
				continue
			}
			stackRun++
		} else {
			flushHidden()
			stackRun = 0
		}
		if l == last {
			repeat++
			continue
		}
		flushRepeat()
		out = append(out, l)
		last = l
	}
	flushHidden()
	flushRepeat()
	return out
}

// errorsOnly keeps error lines plus the stack frames / context directly after them.
func errorsOnly(lines []string) []string {
	var out []string
	follow := 0
	for _, l := range lines {
		switch {
		case errorRe.MatchString(l):
			out = append(out, l)
			follow = 6
		case follow > 0 && (stackRe.MatchString(l) || strings.HasPrefix(l, "    ") || strings.HasPrefix(l, "  (previous")):
			out = append(out, l)
			follow--
		default:
			follow = 0
		}
	}
	return out
}
