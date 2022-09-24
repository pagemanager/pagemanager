package pagemanager

import (
	"bytes"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
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
		// .title
		// .summary
		// .lastModified
		// .path (includes langCode)
		// TODO: "-eq" `name, red, green, blue` "-gt" `age, 5` "-descending" "published"
		var srcs []contentSource
		var filters []filter
		var fields []sortField
		flagset := flag.NewFlagSet("", flag.ContinueOnError)
		flagset.Var(&sortFlag{fields: &fields, desc: false}, "ascending", "")
		flagset.Var(&sortFlag{fields: &fields, desc: true}, "descending", "")
		flagset.Var(&filterFlag{filters: &filters, op: "eq"}, "eq", "")
		flagset.Var(&filterFlag{filters: &filters, op: "gt"}, "gt", "")
		flagset.Var(&filterFlag{filters: &filters, op: "ge"}, "ge", "")
		flagset.Var(&filterFlag{filters: &filters, op: "lt"}, "lt", "")
		flagset.Var(&filterFlag{filters: &filters, op: "le"}, "le", "")
		flagset.Var(&filterFlag{filters: &filters, op: "contains"}, "contains", "")
		flagset.Var(&sourceFlag{srcs: &srcs, recursive: false}, "url", "")
		flagset.Var(&sourceFlag{srcs: &srcs, recursive: true}, "recursive-url", "")
		err := flagset.Parse(args)
		if err != nil {
			return nil, err
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
	values   []string
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
		values:   record[1:],
	})
	return nil
}

type sortField struct {
	name string
	desc bool
}

type sortFlag struct {
	desc   bool
	fields *[]sortField
}

var _ flag.Value = (*sortFlag)(nil)

func (f *sortFlag) String() string {
	return fmt.Sprint(*f.fields)
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
		*f.fields = append(*f.fields, sortField{
			name: name,
			desc: f.desc,
		})
	}
	return nil
}
