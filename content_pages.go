package pagemanager

import (
	"bytes"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io/fs"
	"path"
	"reflect"
	"strings"
)

func init() {
	RegisterSource("github.com/pagemanager/pagemanager.ContentPages", ContentPages)
}

func ContentPages(pm *Pagemanager) func(context.Context, ...any) (any, error) {
	return func(ctx context.Context, a ...any) (any, error) {
		route := ctx.Value(RouteContextKey).(*Route)
		if route == nil {
			route = &Route{}
		}
		args := make([]string, len(a))
		buf := bufpool.Get().(*bytes.Buffer)
		buf.Reset()
		defer bufpool.Put(buf)
		for i, v := range a {
			if v, ok := v.(string); ok {
				args[i] = v
				continue
			}
			rv := reflect.ValueOf(v)
			if rv.Kind() == reflect.Slice && rv.Len() > 0 {
				record := make([]string, rv.Len())
				for i := 0; i <= rv.Len(); i++ {
					record[i] = fmt.Sprint(rv.Index(i).Interface())
				}
				buf.Reset()
				w := csv.NewWriter(buf)
				err := w.Write(record)
				if err != nil {
					return nil, err
				}
				w.Flush()
				err = w.Error()
				if err != nil {
					return nil, err
				}
				args[i] = buf.String()
				continue
			}
			return nil, fmt.Errorf("unsupported type: %#v", v)
		}
		// TODO: "-eq" `name, red, green, blue` "-gt" `age, 5` "-descending" "published"
		var entries []map[string]any
		var srcs []contentSource
		var filters []filter
		var order []sortField
		flagset := flag.NewFlagSet("", flag.ContinueOnError)
		flagset.Var(&sortFlag{order: &order, desc: false}, "ascending", "")
		flagset.Var(&sortFlag{order: &order, desc: true}, "descending", "")
		flagset.Var(&filterFlag{filters: &filters, op: "eq"}, "eq", "")
		flagset.Var(&filterFlag{filters: &filters, op: "lt"}, "lt", "")
		flagset.Var(&filterFlag{filters: &filters, op: "le"}, "le", "")
		flagset.Var(&filterFlag{filters: &filters, op: "gt"}, "gt", "")
		flagset.Var(&filterFlag{filters: &filters, op: "ge"}, "ge", "")
		flagset.Var(&filterFlag{filters: &filters, op: "contains"}, "contains", "")
		flagset.Var(&sourceFlag{srcs: &srcs, recursive: false}, "url", "")
		flagset.Var(&sourceFlag{srcs: &srcs, recursive: true}, "recursive-url", "")
		err := flagset.Parse(args)
		if err != nil {
			return nil, err
		}
		fsys, err := fs.Sub(pm.FS, "pm-src")
		if err != nil {
			return nil, err
		}
		for _, src := range srcs {
			fn := func(pathname string, d fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if pathname == src.path || !d.IsDir() {
					return nil
				}
				file, err := fsys.Open(path.Join(pathname, "content.md"))
				if err != nil {
					return err
				}
				defer file.Close()
				buf.Reset()
				_, err = buf.ReadFrom(file)
				if err != nil {
					return err
				}
				entry, err := parseFrontMatter(buf.Bytes())
				if err != nil {
					return err
				}
				fileinfo, err := file.Stat()
				if err != nil {
					return err
				}
				if _, ok := entry["lastModified"]; !ok {
					entry["lastModified"] = fileinfo.ModTime()
				}
				entry["path"] = pathname
				var exclude bool
				for _, f := range filters {
					value := entry[f.key]
					_ = value
					switch f.operator {
					case "eq":
					case "lt":
					case "le":
					case "gt":
					case "ge":
					case "contains":
					}
				}
				if exclude {
					return nil
				}
				entries = append(entries, entry)
				return nil
			}
			if src.recursive {
			} else {
			}
			fs.WalkDir(fsys, src.path, fn)
		}
		return nil, nil
	}
}

type contentSource struct {
	recursive bool
	path      string
}

type sourceFlag struct {
	recursive bool
	srcs      *[]contentSource
}

var _ flag.Value = (*sourceFlag)(nil)

func newSourceFlag(srcs *[]contentSource, recursive bool) *sourceFlag {
	return &sourceFlag{srcs: srcs, recursive: recursive}
}

func (f *sourceFlag) String() string {
	return fmt.Sprint(*f.srcs)
}

func (f *sourceFlag) Set(s string) error {
	r := csv.NewReader(strings.NewReader(s))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.TrimLeadingSpace = true
	r.ReuseRecord = true
	record, err := r.Read()
	if err != nil {
		return err
	}
	for _, path := range record {
		*f.srcs = append(*f.srcs, contentSource{
			recursive: f.recursive,
			path:      path,
		})
	}
	return nil
}

type filter struct {
	operator string
	key      string
	record   []string
}

type filterFlag struct {
	op      string
	filters *[]filter
}

var _ flag.Value = (*filterFlag)(nil)

func (f *filterFlag) String() string {
	return fmt.Sprint(*f.filters)
}

func (f *filterFlag) Set(s string) error {
	r := csv.NewReader(strings.NewReader(s))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.TrimLeadingSpace = true
	r.ReuseRecord = true
	record, err := r.Read()
	if err != nil {
		return err
	}
	if len(record) == 0 {
		return fmt.Errorf("%s: key not found", f.op)
	}
	*f.filters = append(*f.filters, filter{
		operator: f.op,
		key:      record[0],
		record:   record[1:],
	})
	return nil
}

type sortField struct {
	name string
	desc bool
}

type sortFlag struct {
	desc  bool
	order *[]sortField
}

var _ flag.Value = (*sortFlag)(nil)

func (f *sortFlag) String() string {
	return fmt.Sprint(*f.order)
}

func (f *sortFlag) Set(s string) error {
	r := csv.NewReader(strings.NewReader(s))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	r.TrimLeadingSpace = true
	r.ReuseRecord = true
	record, err := r.Read()
	if err != nil {
		return err
	}
	for _, name := range record {
		*f.order = append(*f.order, sortField{
			name: name,
			desc: f.desc,
		})
	}
	return nil
}
