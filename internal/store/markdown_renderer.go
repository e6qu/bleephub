package store

import (
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// MarkdownModeRenderer matches GitHub's `markdown` mode: GFM syntax
// extensions minus task lists and hard line breaks. Shared by the REST
// markdown endpoint and the GraphQL discussion body renderer (moved from
// the server package in ARCH-003).
var MarkdownModeRenderer = goldmark.New(
	goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.Linkify),
)
