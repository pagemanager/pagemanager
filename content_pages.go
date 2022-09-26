package pagemanager

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
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
			if rv.Kind() == reflect.Slice {
				if rv.Len() == 0 {
					args[i] = ""
					continue
				}
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
		// TODO: when would I ever want to pull pages from multiple disparate
		// urls? If not I could just provide a -root and a -recursive flag and
		// be done with it.
		flagset.Var(&sourceFlag{srcs: &srcs, recursive: false}, "url", "")
		flagset.Var(&sourceFlag{srcs: &srcs, recursive: true}, "recursive-url", "")
		root := flagset.String("root", "", "")
		recursive := flagset.Bool("recursive", false, "")
		err := flagset.Parse(args)
		if err != nil {
			return nil, err
		}
		fsys, err := fs.Sub(pm.FS, "pm-src")
		if err != nil {
			return nil, err
		}
		for _, src := range srcs {
			var dirNames []string
			if src.recursive {
				err = fs.WalkDir(fsys, src.path, func(name string, d fs.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if d.IsDir() && name != src.path {
						dirNames = append(dirNames, name)
					}
					return nil
				})
				if err != nil {
					return nil, err
				}
			} else {
				// TODO: if recursive, each page must have an additional field "pages" e.g. $page.pages.
				// TODO: oh god this means I must handle calling fs.ReadDir manually in a recursive fashion.
				dirEntries, err := fs.ReadDir(fsys, src.path)
				if err != nil {
					return nil, err
				}
				for _, d := range dirEntries {
					dirNames = append(dirNames, path.Join(src.path, d.Name()))
				}
			}
			for _, dirName := range dirNames {
				file, err := fsys.Open(path.Join(dirName, "content.md"))
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				if err != nil {
					return nil, err
				}
				buf.Reset()
				_, err = buf.ReadFrom(file)
				if err != nil {
					return nil, err
				}
				entry, err := parseFrontMatter(buf.Bytes())
				if err != nil {
					return nil, err
				}
				fileinfo, err := file.Stat()
				if err != nil {
					return nil, err
				}
				entry["lastModified"] = fileinfo.ModTime()
				entry["href"] = path.Join(route.TildePrefix, route.LangCode, route.PathName)
				includeEntry := true
				for _, filter := range filters {
					value := entry[filter.key]
					if filter.operator == "contains" {
						includeEntry, err = contains(value, filter.args)
						if err != nil {
							return nil, err
						}
						if !includeEntry {
							break
						}
					}
					switch filter.operator {
					case "eq":
					case "lt":
					case "le":
					case "gt":
					case "ge":
					case "contains":
					}
				}
				if includeEntry {
					entries = append(entries, entry)
				}
			}
			// for _, dirEntry := range dirEntries {
			// 	file, err := fsys.Open(path.Join(pathname, "content.md"))
			// 	if err != nil {
			// 		return err
			// 	}
			// 	defer file.Close()
			// 	buf.Reset()
			// 	_, err = buf.ReadFrom(file)
			// 	if err != nil {
			// 		return err
			// 	}
			// 	entry, err := parseFrontMatter(buf.Bytes())
			// 	if err != nil {
			// 		return err
			// 	}
			// 	fileinfo, err := file.Stat()
			// 	if err != nil {
			// 		return err
			// 	}
			// 	if _, ok := entry["lastModified"]; !ok {
			// 		entry["lastModified"] = fileinfo.ModTime()
			// 	}
			// 	entry["path"] = pathname // TODO: change to url? link? permalink?
			// 	ok, err := func() (bool, error) {
			// 		for _, f := range filters {
			// 			value := entry[f.key]
			// 			if f.operator == "contains" {
			// 				ok, err := contains(value, f.record)
			// 				if err != nil {
			// 					return false, err
			// 				}
			// 				if !ok {
			// 					return false, nil
			// 				}
			// 			}
			// 			for _, field := range f.record {
			// 				n, err := cmp(value, field)
			// 				if err != nil {
			// 					return false, err
			// 				}
			// 				switch f.operator {
			// 				case "eq":
			// 				case "lt":
			// 				case "le":
			// 				case "gt":
			// 				case "ge":
			// 				case "contains":
			// 				}
			// 			}
			// 			// Short-circuit evaluation if false.
			// 			if !ok {
			// 				break
			// 			}
			// 		}
			// 	}()
			// 	if ok {
			// 		entries = append(entries, entry)
			// 	}
			// 	return nil
			// }
		}
		if len(filters) > 0 {
			sort.Slice(entries, func(i, j int) bool {
				return false // TODO: loop filters and determine sort order.
			})
		}
		return nil, nil
	}
}

var sqliteTimestampFormats = []string{
	"2006-01-02 15:04:05.999999999-07:00",
	"2006-01-02T15:04:05.999999999-07:00",
	"2006-01-02 15:04:05.999999999",
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04:05",
	"2006-01-02 15:04",
	"2006-01-02T15:04",
	"2006-01-02",
}

func cmp(value any, arg string) (n int, err error) {
	switch v := value.(type) {
	case bool:
	case int:
		value = int64(v)
	case int8:
		value = int64(v)
	case int16:
		value = int64(v)
	case int32:
		value = int64(v)
	case int64:
	case uint:
		value = uint64(v)
	case uint8:
		value = uint64(v)
	case uint16:
		value = uint64(v)
	case uint32:
		value = uint64(v)
	case uint64:
	case float32:
		value = float64(v)
	case float64:
	case string:
	case time.Time:
	default:
		return 0, fmt.Errorf("unsupported comparison type: %#v\n", value)
	}
	switch lhs := value.(type) {
	case bool:
		rhs, err := strconv.ParseBool(arg)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", arg, err)
		}
		if !lhs && rhs {
			return -1, nil
		}
		if lhs && !rhs {
			return 1, nil
		}
		return 0, nil
	case int64:
		rhs, err := strconv.ParseInt(arg, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", arg, err)
		}
		if lhs < rhs {
			return -1, nil
		}
		if lhs > rhs {
			return 1, nil
		}
		return 0, nil
	case uint64:
		rhs, err := strconv.ParseUint(arg, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", arg, err)
		}
		if lhs < rhs {
			return -1, nil
		}
		if lhs > rhs {
			return 1, nil
		}
		return 0, nil
	case float64:
		rhs, err := strconv.ParseFloat(arg, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", arg, err)
		}
		if lhs < rhs {
			return -1, nil
		}
		if lhs > rhs {
			return 1, nil
		}
		return 0, nil
	case string:
		if lhs < arg {
			return -1, nil
		}
		if lhs > arg {
			return 1, nil
		}
		return 0, nil
	case time.Time:
		s := strings.TrimSuffix(arg, "Z")
		var rhs time.Time
		ok := false
		for _, format := range sqliteTimestampFormats {
			if t, err := time.ParseInLocation(format, s, time.UTC); err == nil {
				rhs, ok = t, true
				break
			}
		}
		if !ok {
			return 0, fmt.Errorf("%s: not a valid time value", arg)
		}
		if lhs.Before(rhs) {
			return -1, nil
		}
		if lhs.After(rhs) {
			return 1, nil
		}
		return 0, nil
	}
	return 0, fmt.Errorf("unreachable")
}

func contains(value any, args []string) (bool, error) {
	rv := reflect.ValueOf(value)
	switch rv.Kind() {
	case reflect.Slice:
		length := rv.Len()
		if length > 0 {
			for _, arg := range args {
				for i := 0; i < length; i++ {
					n, err := cmp(rv.Index(i).Interface(), arg)
					if err != nil {
						return false, err
					}
					if n == 0 {
						return true, nil
					}
				}
			}
		}
		return false, nil
	case reflect.Map:
		keys := rv.MapKeys()
		isBool := rv.Elem().Kind() == reflect.Bool
		if len(keys) > 0 {
			for _, arg := range args {
				for _, key := range keys {
					n, err := cmp(key.Interface(), arg)
					if err != nil {
						return false, err
					}
					if n == 0 {
						if isBool && !rv.MapIndex(key).Bool() {
							return false, nil
						}
						return true, nil
					}
				}
			}
		}
		return false, nil
	default:
		return false, fmt.Errorf("contains %s: %#v is not a slice or map", strings.Join(args, ","), value)
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
	args     []string
}

func (f *filter) eval(entry map[string]any) (bool, error) {
	var err error
	includeEntry := true
	value := entry[f.key]
	if f.operator == "contains" {
		includeEntry, err = contains(value, f.args)
		if err != nil {
			return false, err
		}
	} else {
		for _, arg := range f.args {
			n, err := cmp(value, arg)
			_ = n
			if err != nil {
				return false, err
			}
			switch f.operator {
			case "eq":
			case "lt":
			case "le":
			case "gt":
			case "ge":
			}
		}
	}
	if !includeEntry {
		return false, nil
	}
	return true, nil
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
		args:     record[1:],
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

type contentSrc struct {
	fsys      fs.FS
	route     *Route
	recursive bool
	filters   []filter
	order     []sortField
}

// src.filterFlag("eq") *filterFlag
// src.orderFlag(true) *orderFlag
// &src.recursive
// &root

func (src *contentSrc) getPages(root string) (pages []map[string]any, err error) {
	dirEntries, err := fs.ReadDir(src.fsys, root)
	if err != nil {
		return nil, err
	}
	buf := bufpool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufpool.Put(buf)
	for _, d := range dirEntries {
		if !d.IsDir() {
			continue
		}
		dirName := d.Name()
		file, err := src.fsys.Open(path.Join(root, dirName, "content.md"))
		// TODO: Does it still make sense to continue if content.md is not
		// found? If no content.md exists, what do we display if we visit the
		// page?
		// TODO: If we continue crawling even if content.md doesn't exist, then
		// we can't call it ContentPages anymore. What do?
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		buf.Reset()
		_, err = buf.ReadFrom(file)
		if err != nil {
			return nil, err
		}
		page, err := parseFrontMatter(buf.Bytes())
		if err != nil {
			return nil, err
		}
		fileinfo, err := file.Stat()
		if err != nil {
			return nil, err
		}
		page["lastModified"] = fileinfo.ModTime()
		page["href"] = "/" + path.Join(src.route.TildePrefix, src.route.LangCode, root, dirName)
		// TODO: filter page.
		if src.recursive {
			page["pages"], err = src.getPages(path.Join(root, dirName))
			if err != nil {
				return nil, err
			}
		}
	}
	// TODO: sort pages.
	return pages, nil
}
