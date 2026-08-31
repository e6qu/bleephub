package bleephub

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/microcosm-cc/bluemonday"

	"github.com/e6qu/bleephub/internal/store"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	ghtml "github.com/yuin/goldmark/renderer/html"
	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// GitHub Markdown rendering API. `markdown` mode renders CommonMark + GFM
// extensions; `gfm` mode adds hard line breaks and links @mentions and
// #number references that resolve in the `context` repository.

var (
	// markdownModeRenderer lives in internal/store (ARCH-003) so the GraphQL
	// resolver layer renders discussion bodies identically.
	markdownModeRenderer = store.MarkdownModeRenderer
	gfmModeRenderer      = goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithRendererOptions(ghtml.WithHardWraps()),
	)
	// markdownSanitizer allowlists the element/attribute set GitHub's
	// html-pipeline produces and drops unsafe URL schemes and event handlers.
	// goldmark escapes raw HTML but does NOT strip javascript:/data: URLs from
	// link/image destinations, so this is what makes `[x](javascript:alert(1))`
	// safe. `class` is re-permitted for the linkifier's mention/issue classes
	// and fenced-code language-* hints.
	markdownSanitizer = newMarkdownSanitizer()
)

func newMarkdownSanitizer() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("class").Globally()
	return p
}

func (s *Server) registerGHMarkdownRoutes() {
	s.route("POST /api/v3/markdown", s.handleRenderMarkdown)
	s.route("POST /api/v3/markdown/raw", s.handleRenderMarkdownRaw)
}

func (s *Server) handleRenderMarkdown(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text    *string `json:"text"`
		Mode    string  `json:"mode"`
		Context string  `json:"context"`
	}
	// Bound the request body: the rendered pipeline (goldmark + bluemonday)
	// buffers and processes the whole input, so an unbounded body is a DoS
	// vector. The raw sibling caps identically.
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)).Decode(&req); err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return
	}
	if req.Text == nil {
		store.WriteGHValidationError(w, "Markdown", "text", "missing_field")
		return
	}
	switch req.Mode {
	case "", "markdown", "gfm":
	default:
		store.WriteGHValidationError(w, "Markdown", "mode", "invalid")
		return
	}
	rendered, err := s.renderMarkdown(*req.Text, req.Mode, req.Context, s.baseURL(r))
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "Markdown rendering failed")
		return
	}
	writeRenderedHTML(w, rendered)
}

// handleRenderMarkdownRaw renders the raw request body (text/plain or
// text/x-markdown) in `markdown` mode.
func (s *Server) handleRenderMarkdownRaw(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes))
	if err != nil {
		writeGHError(w, http.StatusBadRequest, "Could not read request body")
		return
	}
	rendered, err := s.renderMarkdown(string(body), "markdown", "", s.baseURL(r))
	if err != nil {
		writeGHError(w, http.StatusInternalServerError, "Markdown rendering failed")
		return
	}
	writeRenderedHTML(w, rendered)
}

func writeRenderedHTML(w http.ResponseWriter, rendered string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	// Sanitize at the response boundary so this sink is safe whichever caller
	// produced the HTML.
	// deepcode ignore XSS: The bytes are allowlist-sanitized by bluemonday (markdownSanitizer), which strips javascript:/data: URLs, inline event handlers, and disallowed elements — GitHub's own approach. Snyk's go/XSS only credits stdlib HTML-escaping, which would defeat markdown rendering. Verified by TestMarkdownSanitizesDangerousSchemes.
	_, _ = w.Write([]byte(markdownSanitizer.Sanitize(rendered)))
}

func (s *Server) renderMarkdown(text, mode, context, baseURL string) (string, error) {
	renderer := markdownModeRenderer
	if mode == "gfm" {
		renderer = gfmModeRenderer
	}
	var buf bytes.Buffer
	if err := renderer.Convert([]byte(text), &buf); err != nil {
		return "", err
	}
	rendered := buf.String()
	if mode == "gfm" {
		rendered = s.linkifyGFMReferences(rendered, context, baseURL)
	}
	return markdownSanitizer.Sanitize(rendered), nil
}

var (
	// group 1 is the leading boundary, group 2 the login / number.
	mentionRefRe = regexp.MustCompile(`(^|[^0-9A-Za-z_.])@([A-Za-z0-9][A-Za-z0-9-]{0,38})`)
	issueRefRe   = regexp.MustCompile(`(^|[^0-9A-Za-z_.])#([0-9]+)`)
)

// linkifyGFMReferences links @mention and #number references in plain text
// (skipping <a>, <code>, <pre>), but only when the mention resolves to a real
// user and the number to a real issue or PR in the context repository.
func (s *Server) linkifyGFMReferences(rendered, context, baseURL string) string {
	var contextRepo *store.Repo
	if owner, name, found := strings.Cut(context, "/"); found {
		contextRepo = s.store.GetRepo(owner, name)
	}

	bodyCtx := &xhtml.Node{Type: xhtml.ElementNode, Data: "body", DataAtom: atom.Body}
	nodes, err := xhtml.ParseFragment(strings.NewReader(rendered), bodyCtx)
	if err != nil {
		return rendered
	}

	var walk func(n *xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.ElementNode {
			switch n.DataAtom {
			case atom.A, atom.Code, atom.Pre:
				return
			}
		}
		for c := n.FirstChild; c != nil; {
			next := c.NextSibling
			if c.Type == xhtml.TextNode {
				s.linkifyTextNode(n, c, contextRepo, baseURL)
			} else {
				walk(c)
			}
			c = next
		}
	}
	root := &xhtml.Node{Type: xhtml.ElementNode, Data: "div", DataAtom: atom.Div}
	for _, n := range nodes {
		root.AppendChild(n)
	}
	walk(root)

	var out bytes.Buffer
	for c := root.FirstChild; c != nil; c = c.NextSibling {
		if err := xhtml.Render(&out, c); err != nil {
			return rendered
		}
	}
	return out.String()
}

// linkifyTextNode replaces mention/issue references in one text node with
// anchor elements, splicing the replacements in place.
func (s *Server) linkifyTextNode(parent, textNode *xhtml.Node, contextRepo *store.Repo, baseURL string) {
	type ref struct {
		start, end int // bounds of the replaced token (@login / #n)
		href       string
		class      string
		label      string
	}
	text := textNode.Data
	var refs []ref

	for _, m := range mentionRefRe.FindAllStringSubmatchIndex(text, -1) {
		login := text[m[4]:m[5]]
		if s.store.LookupUserByLogin(login) == nil {
			continue
		}
		refs = append(refs, ref{
			start: m[4] - 1, end: m[5],
			href:  baseURL + "/" + login,
			class: "user-mention",
			label: "@" + login,
		})
	}
	if contextRepo != nil {
		for _, m := range issueRefRe.FindAllStringSubmatchIndex(text, -1) {
			numStr := text[m[4]:m[5]]
			n, err := strconv.Atoi(numStr)
			if err != nil {
				continue
			}
			var href string
			if s.store.GetIssueByNumber(contextRepo.ID, n) != nil {
				href = baseURL + "/" + contextRepo.FullName + "/issues/" + numStr
			} else if s.store.GetPullRequestByNumber(contextRepo.ID, n) != nil {
				href = baseURL + "/" + contextRepo.FullName + "/pull/" + numStr
			} else {
				continue
			}
			refs = append(refs, ref{
				start: m[4] - 1, end: m[5],
				href:  href,
				class: "issue-link js-issue-link",
				label: "#" + numStr,
			})
		}
	}
	if len(refs) == 0 {
		return
	}
	// The two scans cover disjoint token shapes and never overlap; order by
	// position for in-order splicing.
	for i := 1; i < len(refs); i++ {
		for j := i; j > 0 && refs[j].start < refs[j-1].start; j-- {
			refs[j], refs[j-1] = refs[j-1], refs[j]
		}
	}

	pos := 0
	insert := func(n *xhtml.Node) { parent.InsertBefore(n, textNode) }
	for _, rf := range refs {
		if rf.start > pos {
			insert(&xhtml.Node{Type: xhtml.TextNode, Data: text[pos:rf.start]})
		}
		a := &xhtml.Node{
			Type: xhtml.ElementNode, Data: "a", DataAtom: atom.A,
			Attr: []xhtml.Attribute{
				{Key: "class", Val: rf.class},
				{Key: "href", Val: rf.href},
			},
		}
		a.AppendChild(&xhtml.Node{Type: xhtml.TextNode, Data: rf.label})
		insert(a)
		pos = rf.end
	}
	if pos < len(text) {
		insert(&xhtml.Node{Type: xhtml.TextNode, Data: text[pos:]})
	}
	parent.RemoveChild(textNode)
}
