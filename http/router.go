package http

import (
	"encoding/json"
	"errors"
	"fmt"
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
		r.Get("/", e(rt.getServices))
		r.Post("/", e(rt.postService))
		r.Route("/{name}", func(r chi.Router) {
			r.Get("/", e(rt.getService))
			r.Post("/deploy", e(rt.deployService))
			r.Delete("/", e(rt.deleteService))
		})
	})
}

func (rt *Router) getServices(
	w http.ResponseWriter,
	r *http.Request,
) (interface{}, error) {
	ctx := r.Context()
	services, err := rt.store.GetServices(ctx, data)
	if err != nil {
		return nil, fmt.Errorf("get services: %w", err)
	}
	// TODO(egtann) should this also return current state? IPs, etc?
	return services, nil
}

func (rt *Router) postService(
	w http.ResponseWriter,
	r *http.Request,
) (interface{}, error) {
	var data yeoman.ServiceOpts
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		return nil, badRequest(fmt.Errorf("decode: %w", err))
	}

	// It may be appropriate to set up locking around this in case many
	// people are making simultaneous changes, but that adds a lot of
	// complexity, so we're going to err on the side of simplicity for now.
	ctx := r.Context()
	services, err := rt.store.GetServices(ctx, data)
	if err != nil {
		return nil, fmt.Errorf("get services: %w", err)
	}
	services[data.Name] = data
	if err = rt.store.SetServices(ctx, services); err != nil {
		return nil, fmt.Errorf("set services: %w", err)
	}
	return nil, nil
}

type badRequestError string

func (e badRequestError) Error() string { return string(e) }

func badRequest(err error) badRequestError {
	return badRequestError(fmt.Sprintf("bad request: %v", err))
}

func (e badRequestError) Is(target error) bool {
	_, ok := target.(badRequestError)
	return ok
}

type notFoundError string

func (e notFoundError) Error() string { return string(e) }

func notFound(err error) notFoundError {
	return notFoundError(fmt.Sprintf("not found: %v", err))
}

type apiHandler func(http.ResponseWriter, *http.Request) (interface{}, error)

func e(h apiHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		x, err := h(w, r)
		switch {
		case errors.Is(err, notFoundError("")):
			http.Error(w, err.Error(), http.StatusNotFound)
			return
		case errors.Is(err, badRequestError("")):
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		case errors.Is(err, unprocessableError("")):
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if x == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		_ = json.NewEncoder(w).Encode(struct {
			Data interface{} `json:"data"`
		}{Data: x})
	}
}

func (rt *Router) deleteService(
	w http.ResponseWriter,
	r *http.Request,
) (interface{}, error) {
	name := chi.URLParam(r, "name")
	ctx := r.Context()
	services, err := rt.store.GetServices(ctx, data)
	if err != nil {
		return nil, fmt.Errorf("get services: %w", err)
	}
	if _, ok := services[name]; !ok {
		return nil, notFound(errors.New("service does not exist"))
	}
	delete(services, name)
	return nil, nil
}

func (rt *Router) getService(
	w http.ResponseWriter,
	r *http.Request,
) (interface{}, error) {
	name := chi.URLParam(r, "name")
	ctx := r.Context()
	services, err := rt.store.GetServices(ctx, data)
	if err != nil {
		return nil, fmt.Errorf("get services: %w", err)
	}
	srv, ok := services[name]
	if !ok {
		return nil, notFound(errors.New("service does not exist"))
	}
	// TODO(egtann) should this also return current state? IPs, etc?
	return srv, nil
}

func (rt *Router) deployService(
	w http.ResponseWriter,
	r *http.Request,
) (interface{}, error) {
	name := chi.URLParam(r, "name")
	ctx := r.Context()
	services, err := rt.store.GetServices(ctx, data)
	if err != nil {
		return nil, fmt.Errorf("get services: %w", err)
	}
	srv, ok := services[name]
	if !ok {
		return nil, notFound(errors.New("service does not exist"))
	}

	// TODO(egtann) set version

	// Create a new version of a service, but don't update the bucket until
	// we're sure we've succeeded. This way even if Yeoman crashes/reboots,
	// it'll see the currently-working version and shutdown the boxes from
	// the failed deploy.

	return nil, nil
}
