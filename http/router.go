package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi"
	"github.com/go-chi/chi/middleware"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
	"github.com/thankful-ai/yeoman"
)

const (
	servicePrefix = "ym-srv-"
)

type Router struct {
	store   yeoman.Store
	handler http.Handler
}

type RouterOpts struct {
	Log   zerolog.Logger
	Store yeoman.Store
}

func NewRouter(opts RouterOpts) *Router {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(hlog.NewHandler(opts.Log))
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))

	rt := &Router{store: opts.Store, handler: r}
	r.Get("/health", rt.getHealth)
	r.Route("/services", func(r chi.Router) {
		r.Get("/", e(rt.getServices))
		r.Post("/", e(rt.postService))
		r.Route("/{name}", func(r chi.Router) {
			r.Get("/", e(rt.getService))
			r.Delete("/", e(rt.deleteService))
		})
	})
	return rt
}

func (rt *Router) Handler() http.Handler { return rt.handler }

func (rt *Router) getHealth(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte("ok"))
}

func (rt *Router) getServices(
	w http.ResponseWriter,
	r *http.Request,
) (interface{}, error) {
	ctx := r.Context()
	services, err := rt.store.GetServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("get services: %w", err)
	}

	// TODO(egtann) should this also return current state? IPs, etc?
	return services, nil
}

// postService creates a config file in the Store recording the existence and
// settings of this service if needed. It deploys a container.
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
	services, err := rt.store.GetServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("get services: %w", err)
	}

	lg := hlog.FromRequest(r)
	_, updating := services[data.Name]
	if updating {
		lg.Info().Str("name", data.Name).Msg("updating service")
	} else {
		lg.Info().Str("name", data.Name).Msg("creating service")
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

type unprocessableError string

func (e unprocessableError) Error() string { return string(e) }

func unprocessable(err error) unprocessableError {
	return unprocessableError(fmt.Sprintf("unprocessable: %v", err))
}

func (e unprocessableError) Is(target error) bool {
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

	/*
		if name == "proxy" {
			return nil, errors.New("cannot delete proxy")
		}
	*/

	ctx := r.Context()
	services, err := rt.store.GetServices(ctx)
	if err != nil {
		return nil, fmt.Errorf("get services: %w", err)
	}

	if _, ok := services[name]; !ok {
		return nil, notFound(errors.New("service does not exist"))
	}
	if err = rt.store.DeleteService(ctx, name); err != nil {
		return nil, fmt.Errorf("delete service: %w", err)
	}
	delete(services, name)
	return nil, nil
}

func (rt *Router) getService(
	w http.ResponseWriter,
	r *http.Request,
) (interface{}, error) {
	return nil, errors.New("not implemented")

	/*
		name := chi.URLParam(r, "name")
		ctx := r.Context()
		services, err := rt.store.GetServices(ctx)
		if err != nil {
			return nil, fmt.Errorf("get services: %w", err)
		}
		srv, ok := services[name]
		if !ok {
			return nil, notFound(errors.New("service does not exist"))
		}
		// TODO(egtann) should this also return current state? IPs, etc?
		return srv, nil
	*/
}
