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
		fv := &flagValue{modifiers: &[][2]string{}}
		flagset := flag.NewFlagSet("", flag.ContinueOnError)
		flagset.Var(fv.Name("ascending"), "ascending", "")
		flagset.Var(fv.Name("descending"), "descending", "")
		flagset.Var(fv.Name("url"), "url", "")
		flagset.Var(fv.Name("recursive-url"), "recursive-url", "")
		flagset.Var(fv.Name("eq"), "eq", "")
		flagset.Var(fv.Name("gt"), "gt", "")
		flagset.Var(fv.Name("ge"), "ge", "")
		flagset.Var(fv.Name("lt"), "lt", "")
		flagset.Var(fv.Name("le"), "le", "")
		flagset.Var(fv.Name("contains"), "contains", "")
		err := flagset.Parse(args)
		if err != nil {
			return nil, err
		}
		return nil, nil
	}
}

type flagValue struct {
	name      string
	modifiers *[][2]string
}

func (f *flagValue) Name(name string) *flagValue {
	return &flagValue{name: name, modifiers: f.modifiers}
}

func (f *flagValue) String() string { return fmt.Sprint(*f.modifiers) }

func (f *flagValue) Set(s string) error {
	*f.modifiers = append(*f.modifiers, [2]string{f.name, s})
	return nil
}

type contentSource struct {
	recursive bool
	path      string
}

type contentSourceFlag struct {
	recursive      bool
	contentSources *[]contentSource
}

var _ flag.Value = (*contentSourceFlag)(nil)

func (f *contentSourceFlag) String() string {
	return fmt.Sprint(*f.contentSources)
}

func (f *contentSourceFlag) Set(s string) error {
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
		*f.contentSources = append(*f.contentSources, contentSource{
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
	operator string
	filters  *[]filter
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
		return fmt.Errorf("%s: key not found", f.operator)
	}
	*f.filters = append(*f.filters, filter{
		operator: f.operator,
		key:      record[0],
		values:   record[1:],
	})
	return nil
}

type sortField struct {
	name string
	desc bool
}

type sortFieldFlag struct {
	desc       bool
	sortFields *[]sortField
}

var _ flag.Value = (*sortFieldFlag)(nil)

func (f *sortFieldFlag) String() string {
	return fmt.Sprint(*f.sortFields)
}

func (f *sortFieldFlag) Set(s string) error {
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
		*f.sortFields = append(*f.sortFields, sortField{
			name: name,
			desc: f.desc,
		})
	}
	return nil
}
