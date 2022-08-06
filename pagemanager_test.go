package pagemanager

import (
	"io/fs"
	"net/url"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/pagemanager/pagemanager/internal/testutil"
)

func TestPagemanagerTemplate(t *testing.T) {
	pm := New(os.DirFS("."), 0)
	dirEntries, err := fs.ReadDir(os.DirFS("."), "testdata")
	if err != nil {
		t.Fatal(testutil.Callers(), err)
	}

	// TODO: this test is panicking and I don't know why. Find out.
	for _, d := range dirEntries {
		name := d.Name()
		if !strings.HasSuffix(name, ".html") {
			continue
		}
		name = strings.ReplaceAll(name, "~", "/")
		tmpl, err := pm.template(Site{}, "", path.Join("pm-src", name))
		if err != nil {
			t.Fatal(testutil.Callers(), err)
		}
		file, err := os.OpenFile(path.Join("testdata", name), os.O_TRUNC|os.O_WRONLY, 0644)
		if err != nil {
			t.Fatal(testutil.Callers(), err)
		}
		defer file.Close()
		err = tmpl.Execute(file, map[string]any{
			"URL": &url.URL{Path: "/"},
		})
		if err != nil {
			t.Fatal(testutil.Callers(), err)
		}
	}
}
