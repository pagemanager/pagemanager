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

type Pagemanager struct {
	Mode     int
	FS       fs.FS
	DB1      *sql.DB
	DB2      *sql.DB
	DB3      *sql.DB
	handlers map[string]http.Handler
	// sources (what type?)
}

func RegisterSource(name string) {
}

func RegisterHandler(name string) {
}
