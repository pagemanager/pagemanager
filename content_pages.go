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
		var root string
		fsys, err := fs.Sub(pm.FS, "pm-src")
		if err != nil {
			return nil, err
		}
		src := &pageSource{
			fsys:  fsys,
			route: route,
		}
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
		return src.contentPages(root)
	}
}

type pageSource struct {
	fsys       fs.FS
	route      *Route
	recursive  bool
	predicates []predicate
	order      []sortField
}

func (src *pageSource) contentPages(root string) (pages []map[string]any, err error) {
	root = strings.TrimPrefix(root, "/")
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
		file.Close()
		if err != nil {
			return nil, err
		}
		page["lastModified"] = fileinfo.ModTime()
		page["path"] = "/" + path.Join(src.route.TildePrefix, src.route.LangCode, root, dirName)
		ok, err := src.evalPredicates(page)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if src.recursive {
			page["pages"], err = src.contentPages(path.Join(root, dirName))
			if err != nil {
				return nil, err
			}
		}
		pages = append(pages, page)
	}
	if len(src.order) > 0 {
		sort.SliceStable(pages, func(i, j int) bool {
			p1, p2 := pages[i], pages[j]
			for _, field := range src.order {
				v1, v2 := p1[field.name], p2[field.name]
				_, _ = v1, v2
				// Only strings, numbers (int, uint, float) and time are comparable.
				// TODO: use reflection to determine the type we are comparing
				// on. If any value is nil, it will follow the other value's
				// type (using the zero value) unless both values are nil in
				// which case both values are equal. If values are equal, we
				// proceed by evaluating the next field. If there are no more
				// fields to evaluate, we just return false.
			}
			return false
		})
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
								// If map value is bool type, the bool must also be true for it to be ok.
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

func eval(op string, value any, args []string) (bool, error) {
	ok := true
	for _, arg := range args {
		n, err := cmp(value, arg)
		if err != nil {
			return false, err
		}
		switch op {
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
		}
		if ok {
			break
		}
	}
	return ok, nil
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
