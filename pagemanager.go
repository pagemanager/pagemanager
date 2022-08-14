package pagemanager

import (
	"bufio"
	"bytes"
	"database/sql"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/bokwoon95/sq"
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
	DB1      *sql.DB
	DB2      *sql.DB
	DB3      *sql.DB
	Handlers map[string]http.Handler
	Sources  map[string]func(route *Route, args ...string) (any, error)
}

func normalizeDSN(dsn string) (dialect, driverName, normalizedDSN string) {
	if strings.HasPrefix(dsn, "file:") {
		filename := strings.TrimPrefix(strings.TrimPrefix(dsn, "file:"), "//")
		file, err := os.Open(filename)
		if err != nil {
			return "", "", ""
		}
		defer file.Close()
		r := bufio.NewReader(file)
		// SQLite databases may also start with a 'file:' prefix. Treat the
		// contents of the file as a dsn only if the file isn't already an
		// SQLite database i.e. the first 16 bytes isn't the SQLite file
		// header. https://www.sqlite.org/fileformat.html#the_database_header
		header, err := r.Peek(16)
		if err != nil {
			return "", "", ""
		}
		if string(header) == "SQLite format 3\x00" {
			dsn = "sqlite:" + dsn
		} else {
			var b strings.Builder
			_, err = r.WriteTo(&b)
			if err != nil {
				return "", "", ""
			}
			dsn = strings.TrimSpace(b.String())
		}
	}
	trimmedDSN, _, _ := strings.Cut(dsn, "?")
	if strings.HasPrefix(dsn, "sqlite:") {
		dialect = sq.DialectSQLite
	} else if strings.HasPrefix(dsn, "postgres://") {
		dialect = sq.DialectPostgres
	} else if strings.HasPrefix(dsn, "mysql://") {
		dialect = sq.DialectMySQL
	} else if strings.HasPrefix(dsn, "sqlserver://") {
		dialect = sq.DialectSQLServer
	} else if strings.Contains(dsn, "@tcp(") || strings.Contains(dsn, "@unix(") {
		dialect = sq.DialectMySQL
	} else if strings.HasSuffix(trimmedDSN, ".sqlite") ||
		strings.HasSuffix(trimmedDSN, ".sqlite3") ||
		strings.HasSuffix(trimmedDSN, ".db") ||
		strings.HasSuffix(trimmedDSN, ".db3") {
		dialect = sq.DialectSQLite
	} else {
		return "", "", ""
	}
	if driver, ok := getDriver(dialect); ok {
		driverName = driver.DriverName
		if driver.PreprocessDSN != nil {
			return dialect, driver.DriverName, driver.PreprocessDSN(dsn)
		}
		return dialect, driver.DriverName, dsn
	}
	switch dialect {
	case sq.DialectSQLite:
		return dialect, "sqlite3", strings.TrimPrefix(strings.TrimPrefix(dsn, "sqlite:"), "//")
	case sq.DialectPostgres:
		return dialect, "postgres", dsn
	case sq.DialectMySQL:
		return dialect, "mysql", strings.TrimPrefix(dsn, "mysql://")
	case sq.DialectSQLServer:
		return dialect, "sqlserver", dsn
	}
	return "", "", ""
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
	}
	return pm, nil
}
