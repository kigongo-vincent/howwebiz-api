package search

import "github.com/kigongo-vincent/howwebiz-api/modules/base"

func RouterRegister(r *base.RouteRegister) {
	s := NewService(r.DB)
	// howwe.ug search; the aggregated catalog search is /songs?q=.
	r.Router.Get("/howwe/search", s.Search)
}
