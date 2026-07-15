package main

import (
	"bytes"
	"html"
	"html/template"
	"log/slog"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// md renders Conventional Comments bodies. Raw HTML in the source is
// omitted by goldmark's default renderer (no WithUnsafe), which matters
// because review bodies can quote attacker-controlled PR content.
var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
)

// RenderMarkdown converts a comment body to sanitized HTML. On render
// failure it falls back to the escaped plain text.
func RenderMarkdown(src string) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		slog.Warn("markdown render", "err", err)
		return template.HTML(template.HTMLEscapeString(src))
	}
	return template.HTML(buf.String())
}

// RenderDiffHunk renders a GitHub-style diff_hunk (an "@@ ... @@" header
// followed by unified-diff lines) as a compact colored block. Hunk text is
// arbitrary PR source code, so every line is HTML-escaped before any markup
// wraps it. Returns "" for empty input — pre-migration rows and unresolvable
// findings carry no hunk and must render nothing at all.
//
// Lines are spans with `block` so the output stays valid HTML inside both
// div (detail card) and span (dashboard popover) parents.
func RenderDiffHunk(hunk string) template.HTML {
	hunk = strings.TrimRight(hunk, "\n")
	if hunk == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<span data-diff-hunk class="block mono text-xs rounded border border-zinc-200 overflow-x-auto">`)
	for _, line := range strings.Split(hunk, "\n") {
		var cls string
		switch {
		case strings.HasPrefix(line, "@@"):
			cls = "text-zinc-500 bg-zinc-50"
		case strings.HasPrefix(line, "+"):
			cls = "bg-green-50 text-green-800"
		case strings.HasPrefix(line, "-"):
			cls = "bg-red-50 text-red-800"
		default:
			cls = "text-zinc-600"
		}
		b.WriteString(`<span class="block px-2 whitespace-pre ` + cls + `">`)
		b.WriteString(html.EscapeString(line))
		b.WriteString(`</span>`)
	}
	b.WriteString(`</span>`)
	return template.HTML(b.String())
}
