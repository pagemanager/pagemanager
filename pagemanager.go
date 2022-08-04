package pagemanager

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/template/parse"

	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
)

var (
	ErrHandlerNotRegistered = errors.New("handler was not registered") // returned by pm.Handler() if neither index.html nor handler.txt exist.
)

const (
	ModeReadonly = iota
	ModeOffline
	ModeOnline
)

var bufpool = sync.Pool{
	New: func() any { return &bytes.Buffer{} },
}

var (
	handlersMu sync.RWMutex
	handlers   = make(map[string]http.Handler)
)

func RegisterHandler(name string, handler http.Handler) {
	handlersMu.Lock()
	defer handlersMu.Unlock()
	if handler == nil {
		panic("pagemanager: RegisterHandler " + strconv.Quote(name) + " handler is nil")
	}
	if _, dup := handlers[name]; dup {
		panic("pagemanager: RegisterHandler called twice for handler " + strconv.Quote(name))
	}
	handlers[name] = handler
}

var (
	templateQueriesMu sync.RWMutex
	templateQueries   = make(map[string]func(*url.URL, ...string) (any, error))
)

func RegisterTemplateQuery(name string, query func(*url.URL, ...string) (any, error)) {
	templateQueriesMu.Lock()
	defer templateQueriesMu.Unlock()
	if query == nil {
		panic("pagemanager: RegisterTemplateQuery " + strconv.Quote(name) + " query is nil")
	}
	if _, dup := templateQueries[name]; dup {
		panic("pagemanager: RegisterTemplateQuery called twice for query " + strconv.Quote(name))
	}
	templateQueries[name] = query
}

var funcmap = map[string]any{
	"list": func(args ...any) []any { return args },
	"dict": func(args ...any) (map[string]any, error) {
		if len(args)%2 != 0 {
			return nil, fmt.Errorf("odd number of args")
		}
		var ok bool
		var key string
		dict := make(map[string]any)
		for i, arg := range args {
			if i%2 != 0 {
				key, ok = arg.(string)
				if !ok {
					return nil, fmt.Errorf("argument %#v is not a string", arg)
				}
				continue
			}
			dict[key] = arg
		}
		return dict, nil
	},
	"joinPath": path.Join,
	"prefix": func(s string, prefix string) string {
		if s == "" {
			return ""
		}
		return prefix + s
	},
	"suffix": func(s string, suffix string) string {
		if s == "" {
			return ""
		}
		return s + suffix
	},
	"json": func(v any) (string, error) {
		buf := bufpool.Get().(*bytes.Buffer)
		buf.Reset()
		defer bufpool.Put(buf)
		enc := json.NewEncoder(buf)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		err := enc.Encode(v)
		if err != nil {
			return "", err
		}
		return buf.String(), nil
	},
	"img": func(u *url.URL, src string, attrs ...string) (template.HTML, error) {
		// Ugh this will not be available in markdown, I may have to remove
		// this entirely to ensure consistency between markdown and html.
		var b strings.Builder
		b.WriteString("<img")
		src = html.EscapeString(src)
		if !strings.HasPrefix(src, "/") && !strings.HasPrefix(src, "https://") && !strings.HasPrefix(src, "http://") {
			src = path.Join(html.EscapeString(u.Path), src)
		}
		b.WriteString(` src="` + src + `"`)
		for _, attr := range attrs {
			name, value, _ := strings.Cut(attr, " ")
			b.WriteString(" " + html.EscapeString(strings.TrimSpace(name)))
			if value != "" {
				b.WriteString(`="` + html.EscapeString(strings.TrimSpace(value)) + `"`)
			}
		}
		b.WriteString(">")
		return template.HTML(b.String()), nil
	},
}

type Pagemanager struct {
	// may have to create a config struct if more fields are added, like
	// *sql.DB and dialect.
	mode     int
	fsys     fs.FS
	handlers map[string]http.Handler
	funcmap  map[string]any
}

func New(fsys fs.FS, mode int) *Pagemanager {
	pm := &Pagemanager{
		fsys:     fsys,
		mode:     mode,
		handlers: make(map[string]http.Handler),
		funcmap:  make(map[string]any),
	}
	handlersMu.RLock()
	defer handlersMu.RUnlock()
	for name, handler := range handlers {
		pm.handlers[name] = handler
	}
	for name, fn := range funcmap {
		pm.funcmap[name] = fn
	}
	queries := make(map[string]func(*url.URL, ...string) (any, error))
	templateQueriesMu.RLock()
	defer templateQueriesMu.RUnlock()
	for name, query := range templateQueries {
		queries[name] = query
	}
	pm.funcmap["query"] = func(name string, p *url.URL, args ...string) (any, error) {
		fn := queries[name]
		if fn == nil {
			return nil, fmt.Errorf("no such query %q", name)
		}
		return fn(p, args...)
	}
	pm.funcmap["hasQuery"] = func(name string) bool {
		fn := queries[name]
		return fn != nil
	}
	return pm
}

var markdownConverter = goldmark.New(
	goldmark.WithParserOptions(
		parser.WithAttribute(),
	),
	goldmark.WithExtensions(
		extension.Table,
		highlighting.NewHighlighting(
			highlighting.WithStyle("dracula"), // TODO: eventually this will have to be user-configurable. Maybe even dynamically configurable from the front end (this will have to become a property on Pagemanager itself.
		),
	),
	goldmark.WithRendererOptions(
		goldmarkhtml.WithUnsafe(),
	),
)

// TODO: write tests for this immediately. Don't put it off, don't write
// pm.Handler until you are absolutely sure pm.Template works.
func (pm *Pagemanager) Template(fsys fs.FS, name string) (*template.Template, error) {
	buf := bufpool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufpool.Put(buf)
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return nil, err
	}
	main, err := template.New(name).Funcs(pm.funcmap).Parse(string(b))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}

	dirEntries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	for _, d := range dirEntries {
		if d.IsDir() {
			continue
		}
		filename := d.Name()
		if filename == "" || filename[0] < 'A' || filename[0] > 'Z' {
			continue
		}
		isAllCaps := true
		for _, char := range filename[1:] {
			if char >= 'a' && char <= 'z' {
				isAllCaps = false
				break
			}
		}
		if isAllCaps {
			continue
		}
		ext := filepath.Ext(filename)
		if ext != ".html" && ext != ".md" && ext != ".txt" {
			continue
		}
		b, err := fs.ReadFile(fsys, filename)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filename, err)
		}
		if ext == ".html" {
			_, err = main.New(strings.TrimSuffix(filename, ext)).Parse(string(b))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", filename, err)
			}
			continue
		}
		if ext == ".md" {
			buf.Reset()
			err = markdownConverter.Convert(b, buf)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", filename, err)
			}
			b = make([]byte, buf.Len())
			copy(b, buf.Bytes())
		}
		_, err = main.AddParseTree(strings.TrimSuffix(filename, ext), &parse.Tree{
			Root: &parse.ListNode{
				Nodes: []parse.Node{
					&parse.TextNode{Text: b},
				},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filename, err)
		}
	}

	visited := make(map[string]struct{})
	page := template.New("").Funcs(pm.funcmap)
	tmpls := main.Templates()
	var tmpl *template.Template
	var nodes []parse.Node
	var node parse.Node
	var errmsgs []string
	for len(tmpls) > 0 {
		tmpl, tmpls = tmpls[len(tmpls)-1], tmpls[:len(tmpls)-1]
		if tmpl.Tree == nil {
			continue
		}
		if cap(nodes) < len(tmpl.Tree.Root.Nodes) {
			nodes = make([]parse.Node, 0, len(tmpl.Tree.Root.Nodes))
		}
		for i := len(tmpl.Tree.Root.Nodes) - 1; i >= 0; i-- {
			nodes = append(nodes, tmpl.Tree.Root.Nodes[i])
		}
		for len(nodes) > 0 {
			node, nodes = nodes[len(nodes)-1], nodes[:len(nodes)-1]
			switch node := node.(type) {
			case *parse.ListNode:
				for i := len(node.Nodes) - 1; i >= 0; i-- {
					nodes = append(nodes, node.Nodes[i])
				}
			case *parse.BranchNode:
				nodes = append(nodes, node.List)
				if node.ElseList != nil {
					nodes = append(nodes, node.ElseList)
				}
			case *parse.RangeNode:
				nodes = append(nodes, node.List)
				if node.ElseList != nil {
					nodes = append(nodes, node.ElseList)
				}
			case *parse.TemplateNode:
				if !strings.HasSuffix(node.Name, ".html") {
					continue
				}
				if _, ok := visited[node.Name]; ok {
					continue
				}
				visited[node.Name] = struct{}{}
				b, err = fs.ReadFile(pm.fsys, path.Join("pm-template", node.Name))
				if err != nil {
					body := tmpl.Tree.Root.String()
					pos := int(node.Position())
					line := 1 + strings.Count(body[:pos], "\n")
					if errors.Is(err, fs.ErrNotExist) {
						errmsgs = append(errmsgs, fmt.Sprintf("%s line %d: %s does not exist", tmpl.Name(), line, node.String()))
						continue
					}
					return nil, fmt.Errorf("%s line %d: %s: %w", tmpl.Name(), line, node.String(), err)
				}
				t, err := template.New(node.Name).Funcs(pm.funcmap).Parse(string(b))
				if err != nil {
					return nil, fmt.Errorf("%s: %w", node.Name, err)
				}
				for _, t := range t.Templates() {
					_, err = page.AddParseTree(t.Name(), t.Tree)
					if err != nil {
						return nil, fmt.Errorf("%s: adding %s: %w", node.Name, t.Name(), err)
					}
					tmpls = append(tmpls, t)
				}
			}
		}
	}
	if len(errmsgs) > 0 {
		return nil, fmt.Errorf("invalid template references:\n" + strings.Join(errmsgs, "\n"))
	}

	for _, t := range main.Templates() {
		_, err = page.AddParseTree(t.Name(), t.Tree)
		if err != nil {
			return nil, fmt.Errorf("%s: adding %s: %w", name, t.Name(), err)
		}
	}
	page = page.Lookup(name)
	return page, nil
}
