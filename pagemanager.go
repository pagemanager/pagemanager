package pagemanager

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"text/template/parse"
	"time"

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
	// TODO: because handlers can be dynamically added and removed, the end
	// user needs to know the details of each handler. So instead of a
	// http.Handler some kind of Handler struct will have to be registered,
	// which contains the handler metadata.
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

type OpenFirstFS interface {
	OpenFirst(names ...string) (fs.File, error)
}

func OpenFirst(fsys fs.FS, names ...string) (fs.File, error) {
	if fsys, ok := fsys.(OpenFirstFS); ok {
		return fsys.OpenFirst(names...)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("at least one name must be provided")
	}
	for _, name := range names {
		file, err := fsys.Open(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err == nil {
			return file, nil
		}
	}
	return nil, fs.ErrNotExist
}

type Pagemanager struct {
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

type Site struct {
	Domain      string
	Subdomain   string
	TildePrefix string
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

func (pm *Pagemanager) template(site Site, langCode, filename string) (*template.Template, error) {
	buf := bufpool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufpool.Put(buf)
	mdbuf := bufpool.Get().(*bytes.Buffer)
	mdbuf.Reset()
	defer bufpool.Put(mdbuf)
	name := path.Join(site.Domain, site.Subdomain, site.TildePrefix, filename)
	file, err := pm.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	_, err = buf.ReadFrom(file)
	if err != nil {
		return nil, err
	}
	body := buf.String()
	main, err := template.New(name).Funcs(pm.funcmap).Parse(body)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	workingDir := filepath.Dir(filename)

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
				basename := strings.TrimSuffix(node.Name, ".html")
				names := make([]string, 0, 8)
				if langCode != "" {
					// 1. <workingDir>/<basename>.<langCode>.html
					// 2. <workingDir>/<basename>.<langCode>.md
					names = append(names, path.Join(workingDir, basename)+"."+langCode+".html", basename+"."+langCode+".md")
				}
				// 3. <workingDir>/<basename>.html
				// 4. <workingDir>/<basename>.md
				names = append(names, path.Join(workingDir, basename)+".html", path.Join(workingDir, basename)+".md")
				sitePrefix := path.Join(site.Domain, site.Subdomain, site.TildePrefix)
				if sitePrefix != "" {
					if langCode != "" {
						// 5. <sitePrefix>/pm-template/<basename>.<langCode>.html
						names = append(names, path.Join(sitePrefix, "pm-template", basename+"."+langCode+".html"))
					}
					// 6. <sitePrefix>/pm-template/<basename>.html
					names = append(names, path.Join(sitePrefix, "pm-template", node.Name))
				}
				if langCode != "" {
					// 7. pm-template/<basename>.<langCode>.html
					names = append(names, path.Join("pm-template", basename+"."+langCode+".html"))
				}
				// 8. pm-template/<basename>.html
				names = append(names, path.Join("pm-template", node.Name))
				file, err := OpenFirst(pm.fsys, names...)
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) {
						errmsgs = append(errmsgs, fmt.Sprintf("%s: %s does not exist", tmpl.Name(), node.String()))
						continue
					}
					return nil, fmt.Errorf("%s: %s: %w", tmpl.Name(), node.String(), err)
				}
				fileinfo, err := file.Stat()
				if err != nil {
					return nil, fmt.Errorf("%s: %s: %w", tmpl.Name(), node.String(), err)
				}
				buf.Reset()
				_, err = buf.ReadFrom(file)
				if err != nil {
					return nil, fmt.Errorf("%s: %s: %w", tmpl.Name(), node.String(), err)
				}
				if strings.HasSuffix(fileinfo.Name(), ".md") {
					mdbuf.Reset()
					err = markdownConverter.Convert(buf.Bytes(), mdbuf)
					if err != nil {
						return nil, fmt.Errorf("%s: %s: %w", tmpl.Name(), node.String(), err)
					}
					b := make([]byte, mdbuf.Len())
					copy(b, mdbuf.Bytes())
					_, err = page.AddParseTree(node.Name, &parse.Tree{
						Root: &parse.ListNode{
							Nodes: []parse.Node{
								&parse.TextNode{Text: b},
							},
						},
					})
					if err != nil {
						return nil, fmt.Errorf("%s: %s: %w", tmpl.Name(), node.String(), err)
					}
					continue
				}
				body := buf.String()
				t, err := template.New(node.Name).Funcs(pm.funcmap).Parse(body)
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

func getSite(u *url.URL) (site Site, pathName string) {
	if u.Host != "localhost" && !strings.HasPrefix(u.Host, "localhost:") && u.Host != "127.0.0.1" && !strings.HasPrefix(u.Host, "127.0.0.1:") {
		if i := strings.LastIndex(u.Host, "."); i >= 0 {
			site.Domain = u.Host
			if j := strings.LastIndex(u.Host[:i], "."); j >= 0 {
				site.Subdomain = u.Host[:j]
				site.Domain = u.Host[j+1:]
			}
		}
	}
	pathName = strings.TrimPrefix(u.Path, "/")
	if strings.HasPrefix(pathName, "~") {
		if i := strings.Index(pathName, "/"); i >= 0 {
			site.TildePrefix = pathName[:i]
			pathName = pathName[i+1:]
		}
	}
	return site, pathName
}

func (pm *Pagemanager) err(w http.ResponseWriter, r *http.Request, msg string, code int) {
	statusCode := strconv.Itoa(code)
	errmsg := statusCode + " " + http.StatusText(code) + "\n\n" + msg
	if msg == "" {
		errmsg = errmsg[:len(errmsg)-2]
	}
	site, _ := getSite(r.URL)
	filename := path.Join(site.Domain, site.Subdomain, site.TildePrefix, "pm-template", statusCode+".html")
	tmpl, err := pm.template(site, "", filename)
	if err != nil {
		http.Error(w, errmsg, code)
		return
	}
	buf := bufpool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufpool.Put(buf)
	err = tmpl.ExecuteTemplate(buf, filename, map[string]any{
		"URL": r.URL,
		"Msg": msg,
	})
	if err != nil {
		http.Error(w, errmsg+"\n\n(error executing "+filename+": "+err.Error()+")", code)
		return
	}
	w.WriteHeader(code)
	http.ServeContent(w, r, statusCode+".html", time.Time{}, bytes.NewReader(buf.Bytes()))
}

func (pm *Pagemanager) notFound(w http.ResponseWriter, r *http.Request) {
	pm.err(w, r, path.Join(r.Host, r.URL.String()), 404)
}

func (pm *Pagemanager) internalServerError(w http.ResponseWriter, r *http.Request, err error) {
	pm.err(w, r, err.Error(), 500)
}

func (pm *Pagemanager) serveFile(w http.ResponseWriter, r *http.Request, file fs.File) {
	fileinfo, err := file.Stat()
	if err != nil {
		pm.internalServerError(w, r, err)
		return
	}
	if fileinfo.IsDir() {
		pm.notFound(w, r)
		return
	}
	fileSeeker, ok := file.(io.ReadSeeker)
	if !ok {
		ext := filepath.Ext(fileinfo.Name())
		w.Header().Set("Content-Type", mime.TypeByExtension(ext))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = io.Copy(w, file)
		return
	}
	http.ServeContent(w, r, fileinfo.Name(), fileinfo.ModTime(), fileSeeker)
}

func (pm *Pagemanager) Pagemanager(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		site, pathName := getSite(r.URL)
		// pm-static.
		if pathName == "pm-static" || strings.HasPrefix(pathName, "pm-static/") {
			names := make([]string, 0, 2)
			names = append(names, pathName)
			if strings.HasPrefix(pathName, "pm-static/pm-template") {
				names = append(names, strings.TrimPrefix(pathName, "pm-static"))
			}
			file, err := OpenFirst(pm.fsys, names...)
			if errors.Is(err, fs.ErrNotExist) {
				pm.notFound(w, r)
				return
			}
			if err != nil {
				pm.internalServerError(w, r, err)
				return
			}
			defer file.Close()
			pm.serveFile(w, r, file)
			return
		}
		// pm-site.
		names := []string{
			path.Join(site.Domain, site.Subdomain, site.TildePrefix, "pm-site", pathName, "index.html.gz"),
			path.Join(site.Domain, site.Subdomain, site.TildePrefix, "pm-site", pathName, "index.html"),
		}
		file, err := OpenFirst(pm.fsys, names...)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			pm.internalServerError(w, r, err)
			return
		}
		if err == nil {
			defer file.Close()
			pm.serveFile(w, r, file)
			return
		}
		// pm-src.
		ext := filepath.Ext(pathName)
		if ext != "" {
			name := path.Join(site.Domain, site.Subdomain, site.TildePrefix, "pm-src", pathName)
			file, err = pm.fsys.Open(name)
			if err != nil {
				pm.internalServerError(w, r, err)
				return
			}
			pm.serveFile(w, r, file)
			return
		}
		var langCode string
		if i := strings.Index(pathName, "/"); i >= 0 {
			name := path.Join(site.Domain, site.Subdomain, site.TildePrefix, "pm-lang", pathName[:i]+".txt")
			_, err = fs.Stat(pm.fsys, name)
			if err == nil {
				langCode = pathName[:i]
				pathName = pathName[i+1:]
			}
		}
		names = []string{
			path.Join(site.Domain, site.Subdomain, site.TildePrefix, "pm-src", pathName, "index.html"),
			path.Join(site.Domain, site.Subdomain, site.TildePrefix, "pm-src", pathName, "handler.txt"),
		}
		file, err = OpenFirst(pm.fsys, names...)
		if err != nil {
			pm.internalServerError(w, r, err)
			return
		}
		defer file.Close()
		fileinfo, err := file.Stat()
		if err != nil {
			pm.internalServerError(w, r, err)
			return
		}
		filename := fileinfo.Name()
		modtime := fileinfo.ModTime()
		if filename == "handler.txt" {
			var b strings.Builder
			_, err = io.Copy(&b, file)
			b.Grow(int(fileinfo.Size()))
			if err != nil {
				pm.internalServerError(w, r, err)
				return
			}
			handlerName := b.String()
			handler := pm.handlers[handlerName]
			if handler == nil {
				pm.internalServerError(w, r, fmt.Errorf("%s: handler %q does not exist", pathName, handlerName))
				return
			}
			handler.ServeHTTP(w, r)
			return
		}
		tmpl, err := pm.template(site, langCode, path.Join(pathName, filename))
		if err != nil {
			pm.internalServerError(w, r, err)
		}
		buf := bufpool.Get().(*bytes.Buffer)
		buf.Reset()
		defer bufpool.Put(buf)
		err = tmpl.Execute(buf, map[string]any{
			"URL": r.URL,
		})
		if err != nil {
			pm.internalServerError(w, r, err)
			return
		}
		http.ServeContent(w, r, filename, modtime, bytes.NewReader(buf.Bytes()))
	})
}
