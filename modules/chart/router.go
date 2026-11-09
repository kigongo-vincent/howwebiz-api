package chart

import "github.com/kigongo-vincent/howwebiz-api/modules/base"

func RouterRegister(r *base.RouteRegister) {
	s := NewService(r.DB)
	g := r.Router.Group("lists")
	g.Get("/", s.Index)
	g.Get("/:list", s.Show)
	g.Get("/:list/snapshots", s.History)

	r.Router.Get("/charts/hot100", s.Hot100)
}
