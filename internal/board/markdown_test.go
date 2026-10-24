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
