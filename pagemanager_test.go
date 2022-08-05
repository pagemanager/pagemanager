package pagemanager

import (
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/pagemanager/pagemanager/internal/testutil"
)

func TestPagemanagerTemplate(t *testing.T) {
	dirEntries, err := fs.ReadDir(os.DirFS("."), "testdata")
	if err != nil {
		t.Fatal(testutil.Callers(), err)
	}

	for _, d := range dirEntries {
		name := d.Name()
		if !strings.HasSuffix(name, ".html") {
			continue
		}
		name = strings.ReplaceAll(name, "~", "/")
	}
}
