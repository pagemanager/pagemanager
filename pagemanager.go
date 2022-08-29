package pagemanager

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/template/parse"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	goldmarkhtml "github.com/yuin/goldmark/renderer/html"
)

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

const (
	ModeReadonly = iota
	ModeLocal
	ModeLive
)

var (
	bufpool   = sync.Pool{New: func() any { return &bytes.Buffer{} }}
	routePool = sync.Pool{New: func() any { return &Route{} }}
)

type contextKey struct {
	name string
}

var (
	RouteContextKey = &contextKey{name: "Route"}
)

type Route struct {
	Domain      string
	Subdomain   string
	TildePrefix string
	LangCode    string
	PathName    string
}

type Pagemanager struct {
	Mode     int
	FS       fs.FS
	Dialect  string
	DBMu     *sync.Mutex
	DB       *sql.DB
	handlers map[string]http.Handler
	sources  map[string]func(context.Context, ...any) (any, error)
}

var (
	initFuncsMu sync.RWMutex
	initFuncs   map[string]func(*Pagemanager) error
)

func RegisterInit(name string, initFunc func(*Pagemanager) error) {
	initFuncsMu.Lock()
	defer initFuncsMu.Unlock()
	if _, dup := initFuncs[name]; dup {
		panic(fmt.Sprintf("pagemanager: RegisterInit called twice for init function %q", name))
	}
	if initFunc == nil {
		panic(fmt.Sprintf("pagemanager: RegisterInit %q init function is nil", name))
	}
	initFuncs[name] = initFunc
}

var (
	sourcesMu sync.RWMutex
	sources   = make(map[string]func(*Pagemanager) func(context.Context, ...any) (any, error))
)

func RegisterSource(name string, constructor func(*Pagemanager) func(context.Context, ...any) (any, error)) {
	sourcesMu.Lock()
	defer sourcesMu.Unlock()
	if _, dup := sources[name]; dup {
		panic(fmt.Sprintf("pagemanager: RegisterSource called twice for source %q", name))
	}
	if constructor == nil {
		panic(fmt.Sprintf("pagemanager: RegisterSource %q source is nil", name))
	}
	sources[name] = constructor
}

var (
	handlersMu sync.RWMutex
	handlers   = make(map[string]func(*Pagemanager) http.Handler)
)

func RegisterHandler(name string, constructor func(*Pagemanager) http.Handler) {
	handlersMu.Lock()
	defer handlersMu.Unlock()
	if _, dup := handlers[name]; dup {
		panic(fmt.Sprintf("pagemanager: RegisterHandler called twice for handler %q", name))
	}
	if constructor == nil {
		panic(fmt.Sprintf("pagemanager: RegisterHandler %q handler is nil", name))
	}
	handlers[name] = constructor
}

var (
	pmMode = flag.String("pm-mode", "", "pagemanager mode")
	pmDir  = flag.String("pm-dir", "", "pagemanager directory")
	pmDB   = flag.String("pm-db", "", "pagemanager database")
)

type Config struct {
	Mode       int
	FS         fs.FS
	Dialect    string
	DriverName string
	DB         *sql.DB
	Handlers   map[string]http.Handler
	Sources    map[string]func(context.Context, ...any) (any, error)
}

func normalizeDSN(c *Config, dsn string) (normalizedDSN string) {
	if strings.HasPrefix(dsn, "file:") {
		filename := strings.TrimPrefix(strings.TrimPrefix(dsn, "file:"), "//")
		file, err := os.Open(filename)
		if err != nil {
			return ""
		}
		defer file.Close()
		r := bufio.NewReader(file)
		// SQLite databases may also start with a 'file:' prefix. Treat the
		// contents of the file as a dsn only if the file isn't already an
		// SQLite database i.e. the first 16 bytes isn't the SQLite file
		// header. https://www.sqlite.org/fileformat.html#the_database_header
		header, err := r.Peek(16)
		if err != nil {
			return dsn
		}
		if string(header) == "SQLite format 3\x00" {
			dsn = "sqlite:" + dsn
		} else {
			var b strings.Builder
			_, err = r.WriteTo(&b)
			if err != nil {
				return ""
			}
			dsn = strings.TrimSpace(b.String())
		}
	}
	trimmedDSN, _, _ := strings.Cut(dsn, "?")
	if c.Dialect == "" {
		if strings.HasPrefix(dsn, "sqlite:") {
			c.Dialect = "sqlite"
		} else if strings.HasPrefix(dsn, "postgres://") {
			c.Dialect = "postgres"
		} else if strings.HasPrefix(dsn, "mysql://") {
			c.Dialect = "mysql"
		} else if strings.HasPrefix(dsn, "sqlserver://") {
			c.Dialect = "sqlserver"
		} else if strings.Contains(dsn, "@tcp(") || strings.Contains(dsn, "@unix(") {
			c.Dialect = "mysql"
		} else if strings.HasSuffix(trimmedDSN, ".sqlite") ||
			strings.HasSuffix(trimmedDSN, ".sqlite3") ||
			strings.HasSuffix(trimmedDSN, ".db") ||
			strings.HasSuffix(trimmedDSN, ".db3") {
			c.Dialect = "sqlite"
		} else {
			return dsn
		}
	}
	if (c.DriverName == "" && c.Dialect == "sqlite") || c.DriverName == "sqlite3" {
		if c.DriverName == "" {
			c.DriverName = "sqlite3"
		}
		dsn = strings.TrimPrefix(strings.TrimPrefix(dsn, "sqlite:"), "//")
		before, after, _ := strings.Cut(dsn, "?")
		q, err := url.ParseQuery(after)
		if err != nil {
			return dsn
		}
		if !q.Has("_foreign_keys") && !q.Has("_fk") {
			q.Set("_foreign_keys", "true")
		}
		return before + "?" + q.Encode()
	}
	if (c.DriverName == "" && c.Dialect == "postgres") || (c.DriverName == "postgres" || c.DriverName == "pgx") {
		if c.DriverName == "" {
			c.DriverName = "postgres"
		}
		before, after, _ := strings.Cut(dsn, "?")
		q, err := url.ParseQuery(after)
		if err != nil {
			return dsn
		}
		if !q.Has("sslmode") {
			q.Set("sslmode", "disable")
		}
		return before + "?" + q.Encode()
	}
	if (c.DriverName == "" && c.Dialect == "mysql") || c.DriverName == "mysql" {
		if c.DriverName == "" {
			c.DriverName = "mysql"
		}
		if strings.HasPrefix(dsn, "mysql://") {
			u, err := url.Parse(dsn)
			if err != nil {
				dsn = strings.TrimPrefix(dsn, "mysql://")
			} else {
				var b strings.Builder
				b.Grow(len(dsn))
				if u.User != nil {
					username := u.User.Username()
					password, ok := u.User.Password()
					b.WriteString(username)
					if ok {
						b.WriteString(":" + password)
					}
				}
				if u.Host != "" {
					if b.Len() > 0 {
						b.WriteString("@")
					}
					b.WriteString("tcp(" + u.Host + ")")
				}
				b.WriteString("/" + strings.TrimPrefix(u.Path, "/"))
				if u.RawQuery != "" {
					b.WriteString("?" + u.RawQuery)
				}
				dsn = b.String()
			}
		}
		before, after, _ := strings.Cut(dsn, "?")
		q, err := url.ParseQuery(after)
		if err != nil {
			return dsn
		}
		if !q.Has("allowAllFiles") {
			q.Set("allowAllFiles", "true")
		}
		if !q.Has("multiStatements") {
			q.Set("multiStatements", "true")
		}
		if !q.Has("parseTime") {
			q.Set("parseTime", "true")
		}
		return before + "?" + q.Encode()
	}
	if (c.DriverName == "" && c.Dialect == "sqlserver") || c.DriverName == "sqlserver" {
		if c.DriverName == "" {
			c.DriverName = "sqlserver"
		}
		u, err := url.Parse(dsn)
		if err != nil {
			return dsn
		}
		if u.Path != "" {
			before, after, _ := strings.Cut(dsn, "?")
			q, err := url.ParseQuery(after)
			if err != nil {
				return dsn
			}
			q.Set("database", u.Path[1:])
			dsn = strings.TrimSuffix(before, u.Path) + "?" + q.Encode()
		}
		return dsn
	}
	return dsn
}

func New(c *Config) (*Pagemanager, error) {
	pm := &Pagemanager{
		Mode:     c.Mode,
		FS:       c.FS,
		Dialect:  c.Dialect,
		DBMu:     &sync.Mutex{},
		DB:       c.DB,
		handlers: c.Handlers,
		sources:  c.Sources,
	}
	var err error
	var dir string
	if pm.FS == nil {
		if *pmDir != "" {
			dir = *pmDir
		} else {
			homeDir, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			dir = filepath.ToSlash(homeDir + "/pagemanager-data")
		}
		err = os.MkdirAll(dir, 0755)
		if err != nil {
			return nil, err
		}
		pm.FS = os.DirFS(dir) // TODO: eventually need to replace this with custom DirFS that allows WriteFile and stuff.
	}
	if pm.DB == nil && *pmDB != "" {
		dsn := normalizeDSN(c, *pmDB)
		if c.DriverName == "" {
			return nil, fmt.Errorf("could not identity dialect for -pm-db %q", *pmDB)
		}
		pm.DB, err = sql.Open(c.DriverName, dsn)
		if err != nil {
			return nil, fmt.Errorf("error connecting to %q: %w", *pmDB, err)
		}
	}
	if pm.DB == nil && dir != "" {
		c.Dialect = "sqlite"
		c.DriverName = "sqlite3"
		dsn := filepath.ToSlash(dir + "/pagemanager.db")
		pm.DB, err = sql.Open(c.DriverName, dsn)
		if err != nil {
			return nil, fmt.Errorf("error connecting to %q: %w", dsn, err)
		}
	}
	if pm.DB != nil {
		return nil, fmt.Errorf("database not provided")
	}
	if pm.handlers == nil && pm.sources == nil {
		initFuncsMu.RLock()
		defer initFuncsMu.RUnlock()
		names := make([]string, 0, len(initFuncs))
		for name := range initFuncs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			initFunc := initFuncs[name]
			err = initFunc(pm)
			if err != nil {
				return nil, fmt.Errorf("init func %q: %w", name, err)
			}
		}
		if pm.handlers == nil {
			handlersMu.RLock()
			defer handlersMu.RUnlock()
			for name, constructor := range handlers {
				handler := constructor(pm)
				pm.handlers[name] = handler
			}
		}
		if pm.sources == nil {
			sourcesMu.RLock()
			defer sourcesMu.RUnlock()
			for name, constructor := range sources {
				source := constructor(pm)
				pm.sources[name] = source
			}
		}
	}
	return pm, nil
}

type OpenFirstFS interface {
	OpenFirst(names ...string) (name string, file fs.File, err error)
}

func OpenFirst(fsys fs.FS, names ...string) (name string, file fs.File, err error) {
	if fsys, ok := fsys.(OpenFirstFS); ok {
		return fsys.OpenFirst(names...)
	}
	if len(names) == 0 {
		return "", nil, fmt.Errorf("at least one name must be provided")
	}
	for _, name := range names {
		file, err := fsys.Open(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return name, nil, err
		}
		return name, file, nil
	}
	return "", nil, fs.ErrNotExist
}

func (pm *Pagemanager) FuncMap(ctx context.Context) template.FuncMap {
	route := ctx.Value(RouteContextKey).(*Route)
	if route == nil {
		route = &Route{}
	}
	return template.FuncMap{
		"route": func() *Route {
			return route
		},
		"load": func(filename string) (any, error) {
			var data map[string]any
			prefix := path.Join(route.Domain, route.Subdomain, route.TildePrefix, route.PathName)
			if prefix == "" {
				return data, nil
			}
			// TODO: load filename from where?
			return data, nil
		},
		"source": func(sourceName string, args ...any) (any, error) {
			source := pm.sources[sourceName]
			if source == nil {
				return nil, fmt.Errorf("no such source %q", sourceName)
			}
			return source(ctx, args...)
		},
	}
}

// if filename is .html, the dir will be used. if filename is a dir, index.html will be used.
// .html files are always sourced relative to $site/pm-template.
// .md files are always sourced relative to the workingDir.
// In order to
func (pm *Pagemanager) Template(ctx context.Context, filename string) (*template.Template, error) {
	route := ctx.Value(RouteContextKey).(*Route)
	if route == nil {
		route = &Route{}
	}
	buf := bufpool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufpool.Put(buf)
	mdbuf := bufpool.Get().(*bytes.Buffer)
	mdbuf.Reset()
	defer bufpool.Put(mdbuf)
	workingDir := filepath.ToSlash(filepath.Dir(filename))
	name := path.Join(route.Domain, route.Subdomain, route.TildePrefix, filename)
	file, err := pm.FS.Open(name)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	fileinfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if fileinfo.IsDir() {
		file.Close()
		workingDir = filename
		name = path.Join(filename, "index.html")
		file, err = pm.FS.Open(name)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	_, err = buf.ReadFrom(file)
	if err != nil {
		return nil, err
	}
	text := buf.String()
	funcMap := pm.FuncMap(ctx)
	main, err := template.New(filename).Funcs(funcMap).Parse(text)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", filename, err)
	}

	visited := make(map[string]struct{})
	page := template.New("").Funcs(funcMap)
	tmpls := main.Templates()
	sort.Slice(tmpls, func(i, j int) bool {
		return tmpls[i].Name() < tmpls[j].Name()
	})
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
				ext := filepath.Ext(node.Name)
				if ext != ".html" && ext != ".md" {
					continue
				}
				if _, ok := visited[node.Name]; ok {
					continue
				}
				visited[node.Name] = struct{}{}
				switch ext {
				case ".html":
					name := path.Join(route.Domain, route.Subdomain, route.TildePrefix, "pm-template", node.Name)
					file, err := pm.FS.Open(name)
					if errors.Is(err, fs.ErrNotExist) {
						errmsgs = append(errmsgs, fmt.Sprintf("%s: %s does not exist", tmpl.Name(), node.String()))
						continue
					}
					if err != nil {
						return nil, fmt.Errorf("%s: %w", name, err)
					}
					buf.Reset()
					_, err = buf.ReadFrom(file)
					if err != nil {
						return nil, fmt.Errorf("%s: %w", name, err)
					}
					file.Close()
					body := buf.String()
					t, err := template.New(node.Name).Funcs(funcMap).Parse(body)
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
				case ".md":
					names := make([]string, 0, 2)
					if route.LangCode != "" {
						names = append(names, path.Join(workingDir, strings.TrimSuffix(node.Name, ".md")+"."+route.LangCode+".md"))
					}
					names = append(names, path.Join(workingDir, node.Name))
					_, file, err := OpenFirst(pm.FS, names...)
					if err != nil && !errors.Is(err, fs.ErrNotExist) {
						return nil, fmt.Errorf("%s: %w", node.Name, err)
					}
					buf.Reset()
					var b []byte
					if file != nil {
						_, err = buf.ReadFrom(file)
						if err != nil {
							return nil, fmt.Errorf("%s: %w", node.Name, err)
						}
						file.Close()
						mdbuf.Reset()
						err = markdownConverter.Convert(buf.Bytes(), mdbuf)
						if err != nil {
							return nil, fmt.Errorf("%s: %s: %w", tmpl.Name(), node.String(), err)
						}
						b = make([]byte, mdbuf.Len())
						copy(b, mdbuf.Bytes())
					} else {
						b = []byte("<p>Lorem ipsum dolor sit amet</p>")
					}
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

func (pm *Pagemanager) Error(w http.ResponseWriter, r *http.Request, msg string, code int) {
	statusCode := strconv.Itoa(code)
	errmsg := statusCode + " " + http.StatusText(code) + "\n\n" + msg
	if msg == "" {
		errmsg = errmsg[:len(errmsg)-2]
	}
	ctx := r.Context()
	route := ctx.Value(RouteContextKey).(*Route)
	if route == nil {
		route = &Route{}
	}
	filename := path.Join(route.Domain, route.Subdomain, route.TildePrefix, "pm-template", statusCode+".html")
	tmpl, err := pm.Template(ctx, filename)
	if err != nil {
		http.Error(w, errmsg, code)
		return
	}
	buf := bufpool.Get().(*bytes.Buffer)
	buf.Reset()
	data := map[string]any{"Msg": msg}
	defer bufpool.Put(buf)
	err = tmpl.ExecuteTemplate(buf, filename, data)
	if err != nil {
		http.Error(w, errmsg+"\n\n(error executing "+filename+": "+err.Error()+")", code)
		return
	}
	w.WriteHeader(code)
	http.ServeContent(w, r, filename, time.Time{}, bytes.NewReader(buf.Bytes()))
}

func (pm *Pagemanager) NotFound(w http.ResponseWriter, r *http.Request) {
	pm.Error(w, r, path.Join(r.Host, r.URL.String()), 404)
}

func (pm *Pagemanager) InternalServerError(w http.ResponseWriter, r *http.Request, err error) {
	pm.Error(w, r, err.Error(), 500)
}

func (pm *Pagemanager) ServeFile(w http.ResponseWriter, r *http.Request, file fs.File) {
	fileinfo, err := file.Stat()
	if err != nil {
		pm.InternalServerError(w, r, err)
		return
	}
	if fileinfo.IsDir() {
		pm.NotFound(w, r)
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
		var err error
		route := routePool.Get().(*Route)
		route.Domain = ""
		route.Subdomain = ""
		route.TildePrefix = ""
		route.LangCode = ""
		route.PathName = ""
		defer routePool.Put(route)
		ctx := context.WithValue(r.Context(), RouteContextKey, route)
		r = r.WithContext(ctx)
		if r.URL.Host != "localhost" && !strings.HasPrefix(r.URL.Host, "localhost:") &&
			r.URL.Host != "127.0.0.1" && !strings.HasPrefix(r.URL.Host, "127.0.0.1:") {
			if i := strings.LastIndex(r.URL.Host, "."); i >= 0 {
				route.Domain = r.URL.Host
				if j := strings.LastIndex(r.URL.Host[:i], "."); j >= 0 {
					route.Subdomain = r.URL.Host[:j]
					route.Domain = r.URL.Host[j+1:]
				}
			}
		}
		route.PathName = strings.TrimPrefix(r.URL.Path, "/")
		if strings.HasPrefix(route.PathName, "~") {
			if i := strings.Index(route.PathName, "/"); i >= 0 {
				route.TildePrefix = route.PathName[:i]
				route.PathName = route.PathName[i+1:]
			}
		}
		// pm-static.
		if route.PathName == "pm-static" || strings.HasPrefix(route.PathName, "pm-static/") {
			names := make([]string, 0, 2)
			names = append(names, route.PathName)
			if strings.HasPrefix(route.PathName, "pm-static/pm-template") {
				names = append(names, strings.TrimPrefix(route.PathName, "pm-static/"))
			}
			_, file, err := OpenFirst(pm.FS, names...)
			if errors.Is(err, fs.ErrNotExist) {
				pm.NotFound(w, r)
				return
			}
			if err != nil {
				pm.InternalServerError(w, r, err)
				return
			}
			defer file.Close()
			pm.ServeFile(w, r, file)
			return
		}
		// pm-site.
		names := []string{
			path.Join(route.Domain, route.Subdomain, route.TildePrefix, "pm-site", route.PathName, "index.html.gz"),
			path.Join(route.Domain, route.Subdomain, route.TildePrefix, "pm-site", route.PathName, "index.html"),
		}
		_, file, err := OpenFirst(pm.FS, names...)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			pm.InternalServerError(w, r, err)
			return
		}
		if err == nil {
			defer file.Close()
			pm.ServeFile(w, r, file)
			return
		}
		// pm-src.
		ext := filepath.Ext(route.PathName)
		if ext != "" {
			name := path.Join(route.Domain, route.Subdomain, route.TildePrefix, "pm-src", route.PathName)
			file, err = pm.FS.Open(name)
			if err != nil {
				pm.InternalServerError(w, r, err)
				return
			}
			defer file.Close()
			pm.ServeFile(w, r, file)
			return
		}
		if i := strings.Index(route.PathName, "/"); i >= 0 {
			name := path.Join(route.Domain, route.Subdomain, route.TildePrefix, "pm-lang", route.PathName[:i]+".txt")
			_, err = fs.Stat(pm.FS, name)
			if err == nil {
				route.LangCode = route.PathName[:i]
				route.PathName = route.PathName[i+1:]
			}
		}
		names = []string{
			path.Join(route.Domain, route.Subdomain, route.TildePrefix, "pm-src", route.PathName, "index.html"),
			path.Join(route.Domain, route.Subdomain, route.TildePrefix, "pm-src", route.PathName, "handler.txt"),
		}
		name, file, err := OpenFirst(pm.FS, names...)
		if err != nil {
			pm.InternalServerError(w, r, err)
			return
		}
		defer file.Close()
		fileinfo, err := file.Stat()
		if err != nil {
			pm.InternalServerError(w, r, err)
			return
		}
		if strings.HasSuffix(name, "/handler.txt") {
			var b strings.Builder
			b.Grow(int(fileinfo.Size()))
			_, err = io.Copy(&b, file)
			if err != nil {
				pm.InternalServerError(w, r, err)
				return
			}
			handlerName := b.String()
			handler := pm.handlers[handlerName]
			if handler == nil {
				pm.InternalServerError(w, r, fmt.Errorf("%s: handler %q does not exist", route.PathName, handlerName))
				return
			}
			handler.ServeHTTP(w, r)
			return
		}
		tmpl, err := pm.Template(ctx, name)
		if err != nil {
			pm.InternalServerError(w, r, err)
		}
		buf := bufpool.Get().(*bytes.Buffer)
		buf.Reset()
		defer bufpool.Put(buf)
		err = tmpl.Execute(buf, map[string]any{})
		if err != nil {
			pm.InternalServerError(w, r, err)
			return
		}
		http.ServeContent(w, r, name, fileinfo.ModTime(), bytes.NewReader(buf.Bytes()))
	})
}
