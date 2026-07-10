package main

import (
	"bytes"
	"html/template"
	"log/slog"

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
