package pagemanager

import (
	"bytes"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"strings"
)

func init() {
	RegisterSource("github.com/pagemanager/pagemanager.ContentPages", ContentPages)
}

func ContentPages(pm *Pagemanager) func(context.Context, ...any) (any, error) {
	return func(ctx context.Context, args ...any) (any, error) {
		route := ctx.Value(RouteContextKey).(*Route)
		if route == nil {
			route = &Route{}
		}
		arguments := make([]string, len(args))
		buf := bufpool.Get().(*bytes.Buffer)
		buf.Reset()
		defer bufpool.Put(buf)
		for i, v := range args {
			switch v := v.(type) {
			case string:
				arguments[i] = v
			case []string:
				buf.Reset()
				w := csv.NewWriter(buf)
				err := w.Write(v)
				if err != nil {
					return nil, err
				}
				w.Flush()
				err = w.Error()
				if err != nil {
					return nil, err
				}
				arguments[i] = buf.String()
			default:
				return nil, fmt.Errorf("unsupported type: %#v", v)
			}
		}
		// .title
		// .summary
		// .lastModified
		// .path (includes langCode)
		//
		// -ascending
		// -descending
		// TODO: -url, -recursive-url
		// -url
		// -recursive-url
		// -eq
		// -gt
		// -ge
		// -lt
		// -le
		// -contains
		// TODO: "-eq" `name, red, green, blue` "-gt" `age, 5` "-descending" "published"
		flagset := flag.NewFlagSet("", flag.ContinueOnError)
		var sortFields []sortField
		ascending := &sortFlag{fields: &sortFields, desc: false}
		descending := &sortFlag{fields: &sortFields, desc: true}
		flagset.Var(ascending, "ascending", "")
		flagset.Var(descending, "descending", "")
		err := flagset.Parse(arguments)
		if err != nil {
			return nil, err
		}
		return nil, nil
	}
}

type sortFlag struct {
	fields *[]sortField
	desc   bool
}

type sortField struct {
	field string
	desc  bool
}

func (f *sortFlag) String() string { return fmt.Sprint(*f.fields) }

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
	for _, field := range record {
		*f.fields = append(*f.fields, sortField{field: field, desc: f.desc})
	}
	return nil
}
