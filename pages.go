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
	RegisterSource("github.com/pagemanager/pagemanager.Pages", Pages)
}

func Pages(pm *Pagemanager) func(context.Context, ...any) (any, error) {
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
		var root string
		fsys, err := fs.Sub(pm.FS, "pm-src")
		if err != nil {
			return nil, err
		}
		src := &pageSource{
			fsys:  fsys,
			route: route,
		}
		// TODO: I could probably make this code a lot clearer by constructing
		// a new *pageSource from the given args, rather than relying on args
		// side effects to initialize it. I could use a catch-all flagVar that
		// looks like struct{flag string; values []string} to accumulate all
		// flag arguments into a slice, then parse from there.
		flagset := flag.NewFlagSet("", flag.ContinueOnError)
		flagset.StringVar(&root, "root", "", "")
		flagset.BoolVar(&src.recursive, "recursive", false, "")
		flagset.Var(src.predicateFlag("eq"), "eq", "")
		flagset.Var(src.predicateFlag("lt"), "lt", "")
		flagset.Var(src.predicateFlag("le"), "le", "")
		flagset.Var(src.predicateFlag("gt"), "gt", "")
		flagset.Var(src.predicateFlag("ge"), "ge", "")
		flagset.Var(src.predicateFlag("contains"), "contains", "")
		flagset.Var(src.sortFlag(false), "ascending", "")
		flagset.Var(src.sortFlag(true), "descending", "")
		err = flagset.Parse(args)
		if err != nil {
			return nil, err
		}
		return src.pages(root)
	}
}

type pageSource struct {
	fsys       fs.FS
	route      *Route
	recursive  bool
	predicates []predicate
	order      []sortField
}

func (src *pageSource) pages(root string) (pages []map[string]any, err error) {
	root = strings.TrimPrefix(root, "/")
	if root == "" {
		root = "."
	}
	dirEntries, err := fs.ReadDir(src.fsys, root)
	if err != nil {
		return nil, fmt.Errorf("reading directory %q: %w", root, err)
	}
	for _, d := range dirEntries {
		if !d.IsDir() {
			continue
		}
		dirName := d.Name()
		file, err := src.fsys.Open(path.Join(root, dirName, "content.md"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		var lastModified time.Time
		page := make(map[string]any)
		if file != nil {
			fileinfo, err := file.Stat()
			if err != nil {
				return nil, err
			}
			lastModified = fileinfo.ModTime()
			err = frontmatter(page, file)
			if err != nil {
				return nil, err
			}
			file.Close()
		}
		page["lastModified"] = lastModified
		page["path"] = "/" + path.Join(src.route.TildePrefix, src.route.LangCode, root, dirName) + "/"
		ok, err := src.evalPredicates(page)
		if err != nil {
			return nil, err
		}
		var subpages []map[string]any
		if src.recursive {
			subpages, err = src.pages(path.Join(root, dirName))
			if err != nil {
				return nil, err
			}
			page["pages"] = subpages
		}
		if ok || len(subpages) > 0 {
			pages = append(pages, page)
		}
	}
	if len(src.order) > 0 {
		var cmpErr error
		sort.SliceStable(pages, func(i, j int) bool {
			p1, p2 := pages[i], pages[j]
			for _, field := range src.order {
				v1, v2 := p1[field.name], p2[field.name]
				n, err := cmp(v1, v2)
				if err != nil {
					if cmpErr != nil {
						cmpErr = err
					}
					return false
				}
				if n == 0 {
					continue
				}
				if field.desc {
					return n > 0
				}
				return n < 0
			}
			return false
		})
		if cmpErr != nil {
			return nil, cmpErr
		}
	}
	return pages, nil
}

func (src *pageSource) evalPredicates(page map[string]any) (bool, error) {
	var ok bool
	for _, predicate := range src.predicates {
		value := page[predicate.key]
		// contains.
		if predicate.op == "contains" {
			rv := reflect.ValueOf(value)
			switch rv.Kind() {
			case reflect.Slice:
				length := rv.Len()
				if length > 0 {
					for _, arg := range predicate.args {
						for i := 0; i < length; i++ {
							n, err := cmp(rv.Index(i).Interface(), arg)
							if err != nil {
								return false, err
							}
							ok = n == 0
							if ok {
								break // Short-ciruit OR. If the slice contains an arg, stop looking further.
							}
						}
						if ok {
							break // Short-ciruit OR. If the slice contains an arg, stop looking further.
						}
					}
				}
			case reflect.Map:
				keys := rv.MapKeys()
				isBool := rv.Elem().Kind() == reflect.Bool
				if len(keys) > 0 {
					for _, arg := range predicate.args {
						for _, key := range keys {
							n, err := cmp(key.Interface(), arg)
							if err != nil {
								return false, err
							}
							ok = n == 0
							if ok && isBool {
								// Special case: if map value is bool type, the
								// bool must also be true for it to be ok.
								ok = rv.MapIndex(key).Bool()
							}
							if ok {
								break // Short-ciruit OR. If the map contains an arg, stop looking further.
							}
						}
						if ok {
							break // Short-ciruit OR. If the map contains an arg, stop looking further.
						}
					}
				}
			default:
				return false, fmt.Errorf("contains %s: %#v is not a slice or map", strings.Join(predicate.args, ","), value)
			}
			if !ok {
				return false, nil // Short-ciruit AND. If a predicate returns false, stop evaluating.
			}
			continue
		}

		// eq, lt, le, gt, ge.
		for _, arg := range predicate.args {
			n, err := cmp(value, arg)
			if err != nil {
				return false, err
			}
			switch predicate.op {
			case "eq":
				ok = n == 0
			case "lt":
				ok = n < 0
			case "le":
				ok = n <= 0
			case "gt":
				ok = n > 0
			case "ge":
				ok = n >= 0
			default:
				return false, fmt.Errorf("invalid operator %q (valid operators: eq, lt, le, gt, ge)", predicate.op)
			}
			if ok {
				break // Short-ciruit OR. If the value matches an arg for the given op, stop looking further.
			}
		}
		if !ok {
			return false, nil // Short-ciruit AND. If a predicate returns false, stop evaluating.
		}
	}
	return true, nil
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

func cmp(a, b any) (n int, err error) {
	v1, v2 := reflect.ValueOf(a), reflect.ValueOf(b)

	// If both are nil, return true.
	if a == nil && b == nil {
		return 0, nil
	}
	// If one of them is nil, set it to the zero value of the other type.
	if a == nil {
		v1 = reflect.Zero(v2.Type())
		a = v1.Interface()
	}
	if b == nil {
		v2 = reflect.Zero(v1.Type())
		b = v2.Interface()
	}

	// If both are strings, compare them and return.
	if v1.Kind() == reflect.String && v2.Kind() == reflect.String {
		s1, s2 := v1.String(), v2.String()
		if s1 < s2 {
			return -1, nil
		}
		if s1 > s2 {
			return 1, nil
		}
		return 0, nil
	}
	// If one of them is a string, cast it to the other type.
	if v1.Kind() == reflect.String {
		a, err = castStr(v1.String(), b)
		if err != nil {
			return 0, err
		}
		v1 = reflect.ValueOf(a)
	}
	if v2.Kind() == reflect.String {
		b, err = castStr(v2.String(), a)
		if err != nil {
			return 0, err
		}
		v2 = reflect.ValueOf(b)
	}

	// If both are time.Time, compare them and return.
	if t1, ok := a.(time.Time); ok {
		t2, ok := b.(time.Time)
		if !ok {
			return 0, fmt.Errorf("cannot be compared: %#v, %#v", a, b)
		}
		if t1.Before(t2) {
			return -1, nil
		}
		if t1.After(t2) {
			return 1, nil
		}
		return 0, nil
	}

	if v1.Kind() != v2.Kind() {
		return 0, fmt.Errorf("cannot be compared: %#v, %#v", a, b)
	}
	switch v1.Kind() {
	case reflect.Bool:
		b1, b2 := v1.Bool(), v2.Bool()
		if !b1 && b2 {
			return -1, nil
		}
		if b1 && !b2 {
			return 1, nil
		}
		return 0, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n1, n2 := v1.Int(), v2.Int()
		if n1 < n2 {
			return -1, nil
		}
		if n1 > n2 {
			return 1, nil
		}
		return 0, nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n1, n2 := v1.Uint(), v2.Uint()
		if n1 < n2 {
			return -1, nil
		}
		if n1 > n2 {
			return 1, nil
		}
		return 0, nil
	case reflect.Float32, reflect.Float64:
		n1, n2 := v1.Float(), v2.Float()
		if n1 < n2 {
			return -1, nil
		}
		if n1 > n2 {
			return 1, nil
		}
		return 0, nil
	default:
		return 0, fmt.Errorf("cannot be compared: %#v, %#v", a, b)
	}
}

func castStr(s string, sample any) (any, error) {
	switch sample.(type) {
	case bool:
		return strconv.ParseBool(s)
	case int, int8, int16, int32, int64:
		return strconv.ParseInt(s, 10, 64)
	case uint, uint8, uint16, uint32, uint64:
		return strconv.ParseUint(s, 10, 64)
	case float32, float64:
		return strconv.ParseFloat(s, 64)
	case time.Time:
		s := strings.TrimSuffix(s, "Z")
		for _, format := range sqliteTimestampFormats {
			if t, err := time.ParseInLocation(format, s, time.UTC); err == nil {
				return t, nil
			}
		}
		return nil, fmt.Errorf("%s: not a valid time value", s)
	default:
		return nil, fmt.Errorf("unsupported type: %#v", sample)
	}
}

type predicate struct {
	op   string
	key  string
	args []string
}

type predicateFlag struct {
	op         string
	predicates *[]predicate
}

func (src *pageSource) predicateFlag(op string) *predicateFlag {
	return &predicateFlag{op: op, predicates: &src.predicates}
}

func (f *predicateFlag) String() string {
	return fmt.Sprint(*f.predicates)
}

func (f *predicateFlag) Set(s string) error {
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
	*f.predicates = append(*f.predicates, predicate{
		op:   f.op,
		key:  record[0],
		args: record[1:],
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

func (src *pageSource) sortFlag(desc bool) *sortFlag {
	return &sortFlag{desc: desc, order: &src.order}
}

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
