// Package web provides the plumbing for Inbucket's web GUI and RESTful API
package web

import (
	"context"
	"expvar"
	"net"
	"net/http"
	"net/http/pprof"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/securecookie"
	"github.com/gorilla/sessions"
	"github.com/jhillyerd/inbucket/pkg/config"
	"github.com/jhillyerd/inbucket/pkg/message"
	"github.com/jhillyerd/inbucket/pkg/msghub"
	"github.com/rs/zerolog/log"
)

const (
	staticDir   = "static"
	templateDir = "templates"
)

var (
	// msgHub holds a reference to the message pub/sub system
	msgHub  *msghub.Hub
	manager message.Manager

	// Router is shared between httpd, webui and rest packages. It sends
	// incoming requests to the correct handler function
	Router = mux.NewRouter()

	rootConfig     *config.Root
	server         *http.Server
	listener       net.Listener
	sessionStore   sessions.Store
	globalShutdown chan bool

	// ExpWebSocketConnectsCurrent tracks the number of open WebSockets
	ExpWebSocketConnectsCurrent = new(expvar.Int)
)

func init() {
	m := expvar.NewMap("http")
	m.Set("WebSocketConnectsCurrent", ExpWebSocketConnectsCurrent)
}

// Initialize sets up things for unit tests or the Start() method
func Initialize(
	conf *config.Root,
	shutdownChan chan bool,
	mm message.Manager,
	mh *msghub.Hub) {

	rootConfig = conf
	globalShutdown = shutdownChan

	// NewContext() will use this DataStore for the web handlers
	msgHub = mh
	manager = mm

	// Content Paths
	log.Info().Str("module", "web").Str("phase", "startup").Str("path", conf.Web.UIDir).
		Msg("Web UI content mapped")
	Router.Handle("/debug/vars", expvar.Handler())
	if conf.Web.PProf {
		Router.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		Router.HandleFunc("/debug/pprof/profile", pprof.Profile)
		Router.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		Router.HandleFunc("/debug/pprof/trace", pprof.Trace)
		Router.PathPrefix("/debug/pprof/").HandlerFunc(pprof.Index)
		log.Warn().Str("module", "web").Str("phase", "startup").
			Msg("Go pprof tools installed to /debug/pprof")
	}
	// If no other route matches, attempt to service as UI element.
	Router.PathPrefix("/").Handler(http.StripPrefix("/", http.FileServer(http.Dir(conf.Web.UIDir))))
	Router.NotFoundHandler = noMatchHandler(http.StatusNotFound, "No route matches URI path")
	Router.MethodNotAllowedHandler = noMatchHandler(http.StatusMethodNotAllowed,
		"Method not allowed for URI path")

	// Session cookie setup
	if conf.Web.CookieAuthKey == "" {
		log.Info().Str("module", "web").Str("phase", "startup").
			Msg("Generating random cookie.auth.key")
		sessionStore = sessions.NewCookieStore(securecookie.GenerateRandomKey(64))
	} else {
		log.Info().Str("module", "web").Str("phase", "startup").
			Msg("Using configured cookie.auth.key")
		sessionStore = sessions.NewCookieStore([]byte(conf.Web.CookieAuthKey))
	}
}

// Start begins listening for HTTP requests
func Start(ctx context.Context) {
	server = &http.Server{
		Addr:         rootConfig.Web.Addr,
		Handler:      requestLoggingWrapper(Router),
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 60 * time.Second,
	}

	// We don't use ListenAndServe because it lacks a way to close the listener
	log.Info().Str("module", "web").Str("phase", "startup").Str("addr", server.Addr).
		Msg("HTTP listening on tcp4")
	var err error
	listener, err = net.Listen("tcp", server.Addr)
	if err != nil {
		log.Error().Str("module", "web").Str("phase", "startup").Err(err).
			Msg("HTTP failed to start TCP4 listener")
		emergencyShutdown()
		return
	}

	// Listener go routine
	go serve(ctx)

	// Wait for shutdown
	select {
	case _ = <-ctx.Done():
		log.Debug().Str("module", "web").Str("phase", "shutdown").
			Msg("HTTP server shutting down on request")
	}

	// Closing the listener will cause the serve() go routine to exit
	if err := listener.Close(); err != nil {
		log.Debug().Str("module", "web").Str("phase", "shutdown").Err(err).
			Msg("Failed to close HTTP listener")
	}
}

// serve begins serving HTTP requests
func serve(ctx context.Context) {
	// server.Serve blocks until we close the listener
	err := server.Serve(listener)

	select {
	case _ = <-ctx.Done():
		// Nop
	default:
		log.Error().Str("module", "web").Str("phase", "startup").Err(err).
			Msg("HTTP server failed")
		emergencyShutdown()
		return
	}
}

func emergencyShutdown() {
	// Shutdown Inbucket
	select {
	case _ = <-globalShutdown:
	default:
		close(globalShutdown)
	}
}
