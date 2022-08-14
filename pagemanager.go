package pagemanager

import (
	"bufio"
	"bytes"
	"database/sql"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

const (
	ModeReadonly = iota
	ModeLocal
	ModeLive
)

var bufpool = sync.Pool{
	New: func() any { return &bytes.Buffer{} },
}

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
	DB1      *sql.DB
	DB2      *sql.DB
	DB3      *sql.DB
	handlers map[string]http.Handler
	sources  map[string]func(route *Route, args ...string) (any, error)
}

var (
	initFuncsMu sync.RWMutex
	initFuncs   []func(*Pagemanager) error
)

func RegisterInit(init func(*Pagemanager) error) {
	initFuncsMu.Lock()
	defer initFuncsMu.Unlock()
	if init == nil {
		_, file, line, _ := runtime.Caller(1)
		panic(fmt.Sprintf("pagemanager: %s:%d: init function is nil", file, line))
	}
	initFuncs = append(initFuncs, init)
}

var (
	sourcesMu sync.RWMutex
	sources   = make(map[string]func(*Pagemanager) func(*Route, ...string) (any, error))
)

func RegisterSource(name string, constructor func(*Pagemanager) func(*Route, ...string) (any, error)) {
	sourcesMu.Lock()
	defer sourcesMu.Unlock()
	if constructor == nil {
		panic(fmt.Sprintf("pagemanager: RegisterSource %q source is nil", name))
	}
	if _, dup := sources[name]; dup {
		panic(fmt.Sprintf("pagemanager: RegisterSource called twice for source %q", name))
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
	if constructor == nil {
		panic(fmt.Sprintf("pagemanager: RegisterHandler %q handler is nil", name))
	}
	if _, dup := handlers[name]; dup {
		panic(fmt.Sprintf("pagemanager: RegisterHandler called twice for handler %q", name))
	}
	handlers[name] = constructor
}

var (
	pmMode = flag.String("pm-mode", "", "pagemanager mode")
	pmDir  = flag.String("pm-dir", "", "pagemanager directory")
	pmDSN1 = flag.String("pm-dsn1", "", "pagemanager DSN 1")
	pmDSN2 = flag.String("pm-dsn1", "", "pagemanager DSN 2")
	pmDSN3 = flag.String("pm-dsn1", "", "pagemanager DSN 3")
)

type Config struct {
	Mode     int
	FS       fs.FS
	Dialect  string
	Driver   string
	DB1      *sql.DB
	DB2      *sql.DB
	DB3      *sql.DB
	Handlers map[string]http.Handler
	Sources  map[string]func(route *Route, args ...string) (any, error)
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
	if c.Driver == "" {
		switch c.Dialect {
		case "sqlite":
			c.Driver = "sqlite3"
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
		case "postgres":
			c.Driver = "postgres"
			before, after, _ := strings.Cut(dsn, "?")
			q, err := url.ParseQuery(after)
			if err != nil {
				return dsn
			}
			if !q.Has("sslmode") {
				q.Set("sslmode", "disable")
			}
			return before + "?" + q.Encode()
		case "mysql":
			c.Driver = "mysql"
			return strings.TrimPrefix(dsn, "mysql://")
		case "sqlserver":
			c.Driver = "sqlserver"
		}
	}
	return dsn
}

func New(c *Config) (*Pagemanager, error) {
	pm := &Pagemanager{
		Mode:     c.Mode,
		FS:       c.FS,
		Dialect:  c.Dialect,
		DB1:      c.DB1,
		DB2:      c.DB2,
		DB3:      c.DB3,
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
	if pm.DB1 == nil && *pmDSN1 != "" {
		dsn1 := normalizeDSN(c, *pmDSN1)
		_ = dsn1
		if c.Dialect == "" {
		}
	}
	return pm, nil
}
