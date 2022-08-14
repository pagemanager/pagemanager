package pagemanager

import (
	"bytes"
	"database/sql"
	"io/fs"
	"net/http"
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
	DB1      *sql.DB
	DB2      *sql.DB
	DB3      *sql.DB
	handlers map[string]http.Handler
	sources  map[string]func(route *Route, args ...string) (any, error)
}

func RegisterInit(init func() error) {
}

func RegisterSource(name string, constructor func(*Pagemanager) func(*Route, ...string) (any, error)) {
}

func RegisterHandler(name string, constructor func(*Pagemanager) http.Handler) {
}
