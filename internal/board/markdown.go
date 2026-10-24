package board

import (
	"bytes"
	"html/template"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// md renders GitHub-flavoured markdown; raw HTML is escaped (goldmark's default).
var md = goldmark.New(goldmark.WithExtensions(extension.GFM))

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
