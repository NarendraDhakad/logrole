// Package server responds to incoming HTTP requests and renders the site.
//
// There are a number of smaller servers in this package, each of which takes
// only the configuration necessary to serve it.
package server

import (
	"bytes"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/kevinburke/handlers"
	"github.com/kevinburke/rest"
	"github.com/saintpete/logrole/assets"
	"github.com/saintpete/logrole/config"
	"github.com/saintpete/logrole/services"
	"github.com/saintpete/logrole/views"
)

// Server version, run "make release" to increase this value
const Version = "0.54"

func authUserHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r, _, err := config.AuthUser(r)
		if err != nil {
			rest.Forbidden(w, r, &rest.Error{
				Title: err.Error(),
			})
			return
		}
		h.ServeHTTP(w, r)
	})
}

func UpgradeInsecureHandler(h http.Handler, allowUnencryptedTraffic bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if allowUnencryptedTraffic == false {
			if r.Header.Get("X-Forwarded-Proto") == "http" {
				u := r.URL
				u.Scheme = "https"
				u.Host = r.Host
				http.Redirect(w, r, u.String(), http.StatusMovedPermanently)
				return
			}
		}
		// This header doesn't mean anything when served over HTTP, but
		// detecting HTTPS is a general way is hard, so let's just send it
		// every time.
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload")
		h.ServeHTTP(w, r)
	})
}

// Static file HTTP server; all assets are packaged up in the assets directory
// with go-bindata.
type static struct {
	modTime time.Time
}

func (s *static) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/favicon.ico" {
		r.URL.Path = "/static/favicon.ico"
	}
	bits, err := assets.Asset(strings.TrimPrefix(r.URL.Path, "/"))
	if err != nil {
		rest.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, r.URL.Path, s.modTime, bytes.NewReader(bits))
}

type indexServer struct{}

type indexData struct {
	baseData
}

func (i *indexData) Title() string {
	return "Homepage"
}

func (i *indexServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := &baseData{Data: &indexData{}}
	if err := render(w, r, indexTemplate, "base", data); err != nil {
		rest.ServerError(w, r, err)
	}
}

type Server struct {
	http.Handler
	vc       views.Client
	DoneChan chan bool
	PageSize uint
}

func (s *Server) Close() error {
	return nil
}

func (s *Server) CacheCommonQueries() {
	go s.vc.CacheCommonQueries(s.PageSize, s.DoneChan)
}

// NewServer returns a new Handler that can serve the website.
func NewServer(settings *config.Settings) (*Server, error) {
	if settings.Reporter == nil {
		settings.Reporter = services.GetReporter("noop", "")
	}
	validKey := false
	for i := 0; i < len(settings.SecretKey); i++ {
		if settings.SecretKey[i] != 0x0 {
			validKey = true
			break
		}
	}
	if !validKey {
		return nil, errors.New("Invalid secret key (must initialize some bytes)")
	}
	if settings.Authenticator == nil {
		settings.Authenticator = &NoopAuthenticator{}
	}

	permission := config.NewPermission(settings.MaxResourceAge)
	vc := views.NewClient(handlers.Logger, settings.Client, settings.SecretKey, permission)
	mls, err := newMessageListServer(handlers.Logger, vc, settings.LocationFinder,
		settings.PageSize, settings.MaxResourceAge, settings.SecretKey)
	if err != nil {
		return nil, err
	}
	mis, err := newMessageInstanceServer(handlers.Logger, vc, settings.LocationFinder, settings.ShowMediaByDefault)
	if err != nil {
		return nil, err
	}
	cls, err := newCallListServer(handlers.Logger, vc, settings.LocationFinder,
		settings.PageSize, settings.MaxResourceAge, settings.SecretKey)
	if err != nil {
		return nil, err
	}
	cis, err := newCallInstanceServer(handlers.Logger, vc, settings.LocationFinder)
	if err != nil {
		return nil, err
	}
	confs, err := newConferenceListServer(handlers.Logger, vc,
		settings.LocationFinder, settings.PageSize, settings.MaxResourceAge,
		settings.SecretKey)
	if err != nil {
		return nil, err
	}
	confInstance, err := newConferenceInstanceServer(handlers.Logger, vc,
		settings.LocationFinder)
	if err != nil {
		return nil, err
	}
	ss := &searchServer{}
	o := &openSearchXMLServer{
		PublicHost:              settings.PublicHost,
		AllowUnencryptedTraffic: settings.AllowUnencryptedTraffic,
	}
	index := &indexServer{}
	image := &imageServer{
		secretKey: settings.SecretKey,
	}
	proxy, err := newAudioReverseProxy()
	if err != nil {
		return nil, err
	}
	audio := &audioServer{
		Client:    vc,
		Proxy:     proxy,
		secretKey: settings.SecretKey,
	}
	staticServer := &static{
		modTime: time.Now().UTC(),
	}
	logout := &logoutServer{
		Authenticator: settings.Authenticator,
	}
	tz := &tzServer{
		Logger:                  handlers.Logger,
		AllowUnencryptedTraffic: settings.AllowUnencryptedTraffic,
		LocationFinder:          settings.LocationFinder,
	}

	e := &errorServer{
		Mailto:   settings.Mailto,
		Reporter: settings.Reporter,
	}
	registerErrorHandlers(e)

	authR := new(handlers.Regexp)
	authR.Handle(regexp.MustCompile(`^/$`), []string{"GET"}, index)
	authR.Handle(imageRoute, []string{"GET"}, image)
	authR.Handle(audioRoute, []string{"GET"}, audio)
	authR.Handle(regexp.MustCompile(`^/search$`), []string{"GET"}, ss)
	authR.Handle(regexp.MustCompile(`^/messages$`), []string{"GET"}, mls)
	authR.Handle(regexp.MustCompile(`^/conferences$`), []string{"GET"}, confs)
	authR.Handle(regexp.MustCompile(`^/calls$`), []string{"GET"}, cls)
	authR.Handle(regexp.MustCompile(`^/tz$`), []string{"POST"}, tz)
	authR.Handle(conferenceInstanceRoute, []string{"GET"}, confInstance)
	authR.Handle(callInstanceRoute, []string{"GET"}, cis)
	authR.Handle(messageInstanceRoute, []string{"GET"}, mis)
	authH := config.AddAuthenticator(authR, settings.Authenticator)
	authH = handlers.Log(authH)

	r := new(handlers.Regexp)
	// TODO - don't protect static routes with basic auth
	r.Handle(regexp.MustCompile(`(^/static|^/favicon.ico$)`), []string{"GET"}, handlers.GZip(staticServer))
	r.Handle(regexp.MustCompile(`^/opensearch.xml$`), []string{"GET"}, o)
	r.Handle(regexp.MustCompile(`^/auth/logout$`), []string{"POST"}, logout)
	// todo awkward using HTTP methods here
	r.Handle(regexp.MustCompile(`^/`), []string{"GET", "POST", "PUT", "DELETE"}, authH)
	h := UpgradeInsecureHandler(r, settings.AllowUnencryptedTraffic)
	//if len(settings.Users) > 0 {
	//// TODO database, remove duplication
	//h = AuthUserHandler(h)
	//h = handlers.BasicAuth(h, "logrole", settings.Users)
	//}

	// Innermost handlers are first.
	h = handlers.Server(h, "logrole/"+Version)
	h = handlers.UUID(h)
	h = handlers.TrailingSlashRedirect(h)
	h = handlers.Debug(h)
	h = handlers.Log(h)
	h = handlers.WithTimeout(h, 32*time.Second)
	h = settings.Reporter.ReportPanics(h)
	h = handlers.Duration(h)
	return &Server{
		Handler:  h,
		PageSize: settings.PageSize,
		vc:       vc,
		DoneChan: make(chan bool, 1),
	}, nil
}
