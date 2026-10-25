package board

import (
	"reflect"
	"strings"
	"testing"
)

func TestMarkdownEscapesHTML(t *testing.T) {
	out := string(Markdown("hi <script>alert(1)</script> **bold** [x](javascript:alert(1))"))
	if strings.Contains(out, "<script>") {
		t.Fatalf("raw HTML passed through: %s", out)
	}
	if strings.Contains(out, `href="javascript:`) {
		t.Fatalf("javascript link passed through: %s", out)
	}
	if !strings.Contains(out, "<strong>bold</strong>") {
		t.Fatalf("markdown not rendered: %s", out)
	}
}

func TestMentions(t *testing.T) {
	got := Mentions("@nkirsch can you ask @Octo-Cat? cc @nkirsch, mail me@example.com, @-bad")
	want := []string{"nkirsch", "octo-cat"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestTags(t *testing.T) {
	got := Tags("Dark mode #Feature (#ui) see #12 and page#anchor, #feature again #a-b_c\n#bug")
	want := []string{"feature", "ui", "a-b_c", "bug"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := Tags("#a #b #c #d #e #f"); len(got) != maxTags {
		t.Fatalf("got %v, want %d tags", got, maxTags)
	}
}

func TestMarkdownLinksItemRefs(t *testing.T) {
	out := string(Markdown("split into #29 because (#7), see a#1, &#35;, `#3` and [#4](/x)\nends #12\n\n#tag #5x"))
	for _, want := range []string{
		`split into <a href="/items/29">#29</a> because`,
		`(<a href="/items/7">#7</a>)`,
		"see a#1,",
		"<code>#3</code>",
		`<a href="/x">#4</a>`,
		`ends <a href="/items/12">#12</a></p>`,
		"#tag #5x",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	if strings.Count(out, "<a ") != 4 {
		t.Errorf("want 4 links: %s", out)
	}
}
