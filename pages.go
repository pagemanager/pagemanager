package pagemanager

import "context"

func init() {
	RegisterSource("github.com/pagemanager/pagemanager.Pages", Pages)
}

func Pages(pm *Pagemanager) func(context.Context, ...any) (any, error) {
	return func(ctx context.Context, args ...any) (any, error) {
		route := ctx.Value(RouteContextKey).(*Route)
		if route == nil {
			route = &Route{}
		}
		// source "github.com/pagemanager/pagemanager.Index" "hasSuffix url (red)"
		// TODO: if pagemanager.Index can't find the front matter in content.zh.md, it will look in content.md instead.
		// Each folder than contains an index.html is considered an entry.
		// TODO: each index entry contains:
		// - name
		// - url
		// - modtime
		// - anything else inside content.md (title, summary, published, etc) (need to parse from content.md as necessary)
		// TODO: source "github.com/pagemanager/pagemanager.Index" "URL ASC" "Name DESC"
		// TODO: what happens if you want to index really deep? like recursively index? what happens if you only want pages that contain a certain tag? or a certain taxonomy?
		return nil, nil
	}
}
