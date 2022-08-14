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
	"strings"
	"sync"

	_ "github.com/mattn/go-sqlite3"
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
	initFuncsMu   sync.RWMutex
	initFuncs     []func(*Pagemanager) error // TODO: make this a map, sort the keys before iterating. If a plugin die die need to initialize first, they can give their name a very high sort priority.
	initFuncNames = make(map[string]struct{})
)

func RegisterInit(name string, initFunc func(*Pagemanager) error) {
	initFuncsMu.Lock()
	defer initFuncsMu.Unlock()
	if _, dup := initFuncNames[name]; dup {
		panic(fmt.Sprintf("pagemanager: RegisterInit called twice for init function %q", name))
	}
	if initFunc == nil {
		panic(fmt.Sprintf("pagemanager: RegisterInit %q init function is nil", name))
	}
	initFuncNames[name] = struct{}{}
	initFuncs = append(initFuncs, initFunc)
}

var (
	sourcesMu sync.RWMutex
	sources   = make(map[string]func(*Pagemanager) func(*Route, ...string) (any, error))
)

func RegisterSource(name string, constructor func(*Pagemanager) func(*Route, ...string) (any, error)) {
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
	pmDSN1 = flag.String("pm-dsn1", "", "pagemanager DSN 1")
	pmDSN2 = flag.String("pm-dsn1", "", "pagemanager DSN 2")
	pmDSN3 = flag.String("pm-dsn1", "", "pagemanager DSN 3")
)

type Config struct {
	Mode       int
	FS         fs.FS
	Dialect    string
	DriverName string
	DB1        *sql.DB
	DB2        *sql.DB
	DB3        *sql.DB
	Handlers   map[string]http.Handler
	Sources    map[string]func(route *Route, args ...string) (any, error)
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
	if c.DriverName == "" {
		switch c.Dialect {
		case "sqlite":
			c.DriverName = "sqlite3"
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
			c.DriverName = "postgres"
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
			c.DriverName = "mysql"
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
		case "sqlserver":
			c.DriverName = "sqlserver"
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
		dsn := normalizeDSN(c, *pmDSN1)
		if c.DriverName == "" {
			return nil, fmt.Errorf("could not identity dialect for -pm-dsn1 %q", *pmDSN1)
		}
		pm.DB1, err = sql.Open(c.DriverName, dsn)
		if err != nil {
			return nil, fmt.Errorf("error connecting to %q: %w", *pmDSN1, err)
		}
	}
	if pm.DB2 == nil && *pmDSN2 != "" {
		dsn := normalizeDSN(c, *pmDSN2)
		if c.DriverName == "" {
			return nil, fmt.Errorf("could not identity dialect for -pm-dsn2 %q", *pmDSN2)
		}
		pm.DB2, err = sql.Open(c.DriverName, dsn)
		if err != nil {
			return nil, fmt.Errorf("error connecting to %q: %w", *pmDSN2, err)
		}
	}
	if pm.DB3 == nil && *pmDSN3 != "" {
		dsn := normalizeDSN(c, *pmDSN3)
		if c.DriverName == "" {
			return nil, fmt.Errorf("could not identity dialect for -pm-dsn3 %q", *pmDSN3)
		}
		pm.DB3, err = sql.Open(c.DriverName, dsn)
		if err != nil {
			return nil, fmt.Errorf("error connecting to %q: %w", *pmDSN3, err)
		}
	}
	if pm.DB1 == nil && dir != "" {
		c.Dialect = "sqlite"
		c.DriverName = "sqlite3"
		dsn := filepath.ToSlash(dir + "/pagemanager.db")
		pm.DB1, err = sql.Open(c.DriverName, dsn)
		if err != nil {
			return nil, fmt.Errorf("error connecting to %q: %w", dsn, err)
		}
		pm.DB2 = pm.DB1
		pm.DB3 = pm.DB1
	}
	if pm.DB1 != nil {
		return nil, fmt.Errorf("database not provided")
	}
	if pm.DB2 == nil {
		pm.DB2 = pm.DB1
	}
	if pm.DB3 == nil {
		pm.DB3 = pm.DB1
	}
	if pm.handlers == nil || pm.sources == nil {
		initFuncsMu.RLock()
		defer initFuncsMu.RUnlock()
		for _, initFunc := range initFuncs {
			err = initFunc(pm)
			if err != nil {
				return nil, fmt.Errorf("error connecting to %q: %w", dsn, err)
			}
		}
	}
	if pm.handlers == nil {
	}
	if pm.sources == nil {
	}
	return pm, nil
}
