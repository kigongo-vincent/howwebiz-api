package song

import "github.com/kigongo-vincent/howwebiz-api/modules/base"

func RouterRegister(r *base.RouteRegister) {
	s := NewService(r.DB)
	// howwe.ug songs; the aggregated catalog owns /songs.
	g := r.Router.Group("howwe/songs")
	g.Get("/", s.List)
	g.Get("/:id", s.Get)
}
