package board

import (
	"bytes"
	"html/template"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// md renders GitHub-flavoured markdown; raw HTML is escaped (goldmark's default).
// "#N" links to board item N ("PR #N" doesn't).
var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithASTTransformers(util.Prioritized(itemRefs{}, 100))),
)

// itemRefRE matches #N not preceded by a word character, &, / or #
// (so "a#1", "&#35;" and "page/#2" stay text).
var itemRefRE = regexp.MustCompile(`(?:^|[^\w&/#])(#([0-9]{1,9}))\b`)

// prRefRE matches text ending in "PR " or "pull request ".
var prRefRE = regexp.MustCompile(`(?i)\b(?:PR|pull request) $`)

// itemRefs turns "#N" in plain text into a link to /items/N, leaving code,
// links and autolinks alone.
type itemRefs struct{}

func (itemRefs) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	source := reader.Source()
	var texts []*ast.Text
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch n := n.(type) {
		case *ast.Link, *ast.AutoLink, *ast.CodeSpan, *ast.Image:
			return ast.WalkSkipChildren, nil
		case *ast.Text:
			texts = append(texts, n)
		}
		return ast.WalkContinue, nil
	})
	for _, t := range texts {
		linkItemRefs(t, source)
	}
}

// linkItemRefs splits t around each #N, inserting links in its place.
func linkItemRefs(t *ast.Text, source []byte) {
	seg := t.Segment
	val := seg.Value(source)
	matches := itemRefRE.FindAllSubmatchIndex(val, -1)
	if matches == nil {
		return
	}
	if prev, ok := t.PreviousSibling().(*ast.Text); ok && matches[0][2] == 0 {
		if pv := prev.Segment.Value(source); len(pv) > 0 && isWordByte(pv[len(pv)-1]) {
			matches = matches[1:]
		}
	}
	// "PR #34" is a pull request, not an item.
	kept := matches[:0]
	for _, m := range matches {
		if !prRefRE.Match(val[:m[2]]) {
			kept = append(kept, m)
		}
	}
	matches = kept
	if len(matches) == 0 {
		return
	}
	parent := t.Parent()
	at := 0
	for _, m := range matches {
		start, end := m[2], m[3]
		if start > at {
			parent.InsertBefore(parent, t, ast.NewTextSegment(text.NewSegment(seg.Start+at, seg.Start+start)))
		}
		link := ast.NewLink()
		link.Destination = []byte("/items/" + string(val[m[4]:m[5]]))
		link.AppendChild(link, ast.NewTextSegment(text.NewSegment(seg.Start+start, seg.Start+end)))
		parent.InsertBefore(parent, t, link)
		at = end
	}
	if at < len(val) {
		rest := ast.NewTextSegment(text.NewSegment(seg.Start+at, seg.Stop))
		rest.SetSoftLineBreak(t.SoftLineBreak())
		rest.SetHardLineBreak(t.HardLineBreak())
		parent.InsertBefore(parent, t, rest)
	} else if t.SoftLineBreak() || t.HardLineBreak() {
		empty := ast.NewTextSegment(text.NewSegment(seg.Stop, seg.Stop))
		empty.SetSoftLineBreak(t.SoftLineBreak())
		empty.SetHardLineBreak(t.HardLineBreak())
		parent.InsertBefore(parent, t, empty)
	}
	parent.RemoveChild(parent, t)
}

func isWordByte(b byte) bool {
	return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
}

// Markdown renders untrusted text to safe HTML.
func Markdown(s string) template.HTML {
	var buf bytes.Buffer
	if err := md.Convert([]byte(s), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(s))
	}
	return template.HTML(buf.String())
}

// mentionRE matches @login where login follows GitHub's rules, not inside an email.
var mentionRE = regexp.MustCompile(`(?:^|[^\w@.])@([A-Za-z0-9](?:[A-Za-z0-9]|-[A-Za-z0-9]){0,38})\b`)

// Mentions returns the distinct logins @-mentioned in s, lower-cased.
func Mentions(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range mentionRE.FindAllStringSubmatch(s, -1) {
		l := strings.ToLower(m[1])
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	return out
}

// tagRE matches #tag at the start of a word: a letter, then letters, digits,
// - or _. "#12" (an item number) and "page#anchor" are not tags.
var tagRE = regexp.MustCompile(`(?:^|[\s(\[,;])#([A-Za-z][A-Za-z0-9_-]{0,29})\b`)

// maxTags caps how many tags one item keeps.
const maxTags = 5

// Tags returns the distinct #tags in s, lower-cased, in order of appearance.
func Tags(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range tagRE.FindAllStringSubmatch(s, -1) {
		t := strings.ToLower(m[1])
		if !seen[t] && len(out) < maxTags {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}
