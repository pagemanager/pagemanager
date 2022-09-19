package pagemanager

import (
	"context"
	"flag"
	"fmt"
)

func init() {
	RegisterSource("github.com/pagemanager/pagemanager.Pages", Pages)
}

func Pages(pm *Pagemanager) func(context.Context, ...any) (any, error) {
	return func(ctx context.Context, args ...any) (any, error) {
		route := ctx.Value(RouteContextKey).(*Route)
		if route == nil {
			route = &Route{}
		}
		arguments := make([]string, len(args))
		for i, v := range args {
			str, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("not a string: %#v", v)
			}
			arguments[i] = str
		}
		// .title
		// .summary
		// .lastModified
		// .path (includes langCode)
		//
		// -ascending
		// -descending
		// -url
		// -recursive-url
		// -eq
		// -gt
		// -ge
		// -lt
		// -le
		// -contains
		flagset := flag.NewFlagSet("", flag.ContinueOnError)
		err := flagset.Parse(arguments)
		if err != nil {
			return nil, err
		}
		return nil, nil
	}
}
