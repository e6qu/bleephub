package store

import (
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// MarkdownModeRenderer matches GitHub's `markdown` mode: GFM extensions minus
// task lists and hard line breaks.
var MarkdownModeRenderer = goldmark.New(
	goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.Linkify),
)
