package web

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

// Every data table sits in a .table-wrap, which is what widens the page on
// desktop and confines sideways scrolling to the table on phones.
func TestTablesWrapped(t *testing.T) {
	css, err := fs.ReadFile(FS, "static/style.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), "body:has(.table-wrap) :is(nav, main, footer) { max-width:") {
		t.Error("style.css no longer widens pages with a .table-wrap")
	}
	table := regexp.MustCompile(`(.{0,40})<table class="sites`)
	for _, dir := range []string{"templates", "hub"} {
		entries, err := fs.ReadDir(FS, dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			b, err := fs.ReadFile(FS, dir+"/"+e.Name())
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range table.FindAllStringSubmatch(string(b), -1) {
				if !strings.HasSuffix(m[1], `<div class="table-wrap">`) {
					t.Errorf("%s/%s: table.sites outside a .table-wrap", dir, e.Name())
				}
			}
		}
	}
}
