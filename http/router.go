package http

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/egtann/yeoman"
	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
)

const (
	servicePrefix = "ym-srv-"
)

type Router struct {
	store yeoman.Store
}

func NewRouter(store yeoman.Store) *Router {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))

	rt := &Router{store: store}
	r.Route("/services", func(r chi.Router) {
		r.Post("/", e(rt.postService))
	})
}

func (rt *Router) postService(
	w http.ResponseWriter,
	r *http.Request,
) (interface{}, error) {
	var data struct {
		Name      string
		Container string
		Min       int `json:"min"`
		Max       int `json:"max"`
	}
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		return
	}
}
