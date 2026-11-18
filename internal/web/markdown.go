package web

import (
	"bytes"
	"html/template"
	"net/url"
	"path"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// linkRewriter points relative links in a README at the browser's own
// blob/raw URLs. Raw HTML stays escaped (goldmark's safe default).
type linkRewriter struct {
	blobBase, rawBase string // e.g. /group/proj/-/blob/main/docs
	uploadsBase       string // e.g. /group/proj/-/uploads ("" = leave /uploads links)
}

func (l *linkRewriter) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.Link:
			v.Destination = l.rewrite(v.Destination, l.blobBase)
		case *ast.Image:
			v.Destination = l.rewrite(v.Destination, l.rawBase)
		}
		return ast.WalkContinue, nil
	})
}

func (l *linkRewriter) rewrite(dest []byte, base string) []byte {
	s := string(dest)
	// GitLab attachments: "/uploads/<secret>/<file>" is relative to the project.
	if rest, ok := strings.CutPrefix(s, "/uploads/"); ok && l.uploadsBase != "" {
		return []byte(l.uploadsBase + "/" + rest)
	}
	if s == "" || strings.HasPrefix(s, "#") || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "//") {
		return dest
	}
	if u, err := url.Parse(s); err != nil || u.Scheme != "" {
		return dest
	}
	frag := ""
	if i := strings.IndexByte(s, '#'); i >= 0 {
		s, frag = s[:i], s[i:]
	}
	return []byte(path.Join(base, s) + frag)
}

// renderMarkdown renders md to HTML. dir is the directory of the file
// within the repo; blobBase/rawBase are URL prefixes for the ref.
func renderMarkdown(md []byte, blobBase, rawBase, dir string) template.HTML {
	// blobBase is /<project>/-/blob/<ref>: attachments live at /<project>/-/uploads.
	uploads := ""
	if i := strings.Index(blobBase, "/-/blob/"); i > 0 {
		uploads = blobBase[:i] + "/-/uploads"
	}
	lr := &linkRewriter{blobBase: path.Join(blobBase, dir), rawBase: path.Join(rawBase, dir), uploadsBase: uploads}
	g := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
		goldmark.WithParserOptions(
			parser.WithAutoHeadingID(),
			parser.WithASTTransformers(util.Prioritized(lr, 100)),
		),
	)
	var buf bytes.Buffer
	if err := g.Convert(md, &buf); err != nil {
		return template.HTML("<pre>" + template.HTMLEscapeString(string(md)) + "</pre>")
	}
	return template.HTML(buf.String()) // goldmark escapes raw HTML by default
}
