package pagemanager

import (
	"bytes"
	"database/sql"
	"fmt"
	"io/fs"
	"net/http"
	"runtime"
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
