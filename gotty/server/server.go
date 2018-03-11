package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"html/template"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"regexp"
	noesctmpl "text/template"
	"time"

	"github.com/elazarl/go-bindata-assetfs"
	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/pkg/errors"

	"github.com/wrfly/container-web-tty/container"
	"github.com/wrfly/container-web-tty/gotty/pkg/homedir"
	"github.com/wrfly/container-web-tty/gotty/webtty"
)

// Server provides a webtty HTTP endpoint.
type Server struct {
	factory      Factory
	options      *Options
	containerCli container.Cli

	upgrader      *websocket.Upgrader
	indexTemplate *template.Template
	listTemplate  *template.Template
	titleTemplate *noesctmpl.Template
}

// New creates a new instance of Server.
// Server will use the New() of the factory provided to handle each request.
func New(factory Factory, options *Options, containerCli container.Cli) (*Server, error) {
	indexData, err := Asset("static/index.html")
	if err != nil {
		panic("index not found") // must be in bindata
	}
	indexTemplate, err := template.New("index").Parse(string(indexData))
	if err != nil {
		panic("index template parse failed") // must be valid
	}

	listIndexData, err := Asset("static/list.html")
	if err != nil {
		panic("list index not found") // must be in bindata
	}
	listTemplate, err := template.New("list").Parse(string(listIndexData))
	if err != nil {
		panic("list template parse failed") // must be valid
	}

	titleTemplate, err := noesctmpl.New("title").Parse(options.TitleFormat)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to parse window title format `%s`", options.TitleFormat)
	}

	var originChekcer func(r *http.Request) bool
	if options.WSOrigin != "" {
		matcher, err := regexp.Compile(options.WSOrigin)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to compile regular expression of Websocket Origin: %s", options.WSOrigin)
		}
		originChekcer = func(r *http.Request) bool {
			return matcher.MatchString(r.Header.Get("Origin"))
		}
	}

	return &Server{
		factory:      factory,
		options:      options,
		containerCli: containerCli,

		upgrader: &websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			Subprotocols:    webtty.Protocols,
			CheckOrigin:     originChekcer,
		},
		indexTemplate: indexTemplate,
		titleTemplate: titleTemplate,
		listTemplate:  listTemplate,
	}, nil
}

// Run starts the main process of the Server.
// The cancelation of ctx will shutdown the server immediately with aborting
// existing connections. Use WithGracefullContext() to support gracefull shutdown.
func (server *Server) Run(ctx context.Context, options ...RunOption) error {
	cctx, cancel := context.WithCancel(ctx)
	opts := &RunOptions{gracefullCtx: context.Background()}
	for _, opt := range options {
		opt(opts)
	}

	counter := newCounter(time.Duration(server.options.Timeout) * time.Second)

	path := "/"

	handlers := server.setupHandlers(cctx, cancel, path, counter)
	srv, err := server.setupHTTPServer(handlers)
	if err != nil {
		return errors.Wrapf(err, "failed to setup an HTTP server")
	}

	if server.options.Once {
		log.Printf("Once option is provided, accepting only one client")
	}

	hostPort := net.JoinHostPort(server.options.Address, server.options.Port)
	listener, err := net.Listen("tcp", hostPort)
	if err != nil {
		return errors.Wrapf(err, "failed to listen at `%s`", hostPort)
	}

	scheme := "http"
	log.Printf("HTTP server is listening at: %s", scheme+"://"+hostPort+path)

	srvErr := make(chan error, 1)
	go func() {
		srvErr <- srv.Serve(listener)
	}()

	go func() {
		select {
		case <-opts.gracefullCtx.Done():
			srv.Shutdown(context.Background())
		case <-cctx.Done():
		}
	}()

	select {
	case err = <-srvErr:
		if err == http.ErrServerClosed { // by gracefull ctx
			err = nil
		} else {
			cancel()
		}
	case <-cctx.Done():
		srv.Close()
		err = cctx.Err()
	}

	conn := counter.count()
	if conn > 0 {
		log.Printf("Waiting for %d connections to be closed", conn)
	}
	counter.wait()

	return err
}

func (server *Server) setupHandlers(ctx context.Context, cancel context.CancelFunc,
	pathPrefix string, counter *counter) *mux.Router {

	staticFileHandler := http.FileServer(
		&assetfs.AssetFS{Asset: Asset, AssetDir: AssetDir, Prefix: "static"},
	)

	var siteMux = mux.NewRouter()
	siteMux.HandleFunc(pathPrefix, server.handleListContainers)
	siteMux.PathPrefix("/js").Handler(http.StripPrefix("/", staticFileHandler))
	siteMux.PathPrefix("/css").Handler(http.StripPrefix("/", staticFileHandler))
	siteMux.Handle("/favicon.png", http.StripPrefix("/", staticFileHandler))

	siteMux.HandleFunc("/auth_token.js", server.handleAuthToken)
	siteMux.HandleFunc("/config.js", server.handleConfig)

	siteMux.HandleFunc(pathPrefix+"{id}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.String()+"/", 301)
	})
	siteMux.HandleFunc(pathPrefix+"{id}/", server.handleIndex)
	siteMux.HandleFunc(pathPrefix+"{id}/"+"ws", server.generateHandleWS(ctx, cancel, counter))

	return siteMux
}

func (server *Server) setupHTTPServer(handler http.Handler) (*http.Server, error) {
	srv := &http.Server{
		Handler: handler,
	}

	if server.options.EnableTLSClientAuth {
		tlsConfig, err := server.tlsConfig()
		if err != nil {
			return nil, errors.Wrapf(err, "failed to setup TLS configuration")
		}
		srv.TLSConfig = tlsConfig
	}

	return srv, nil
}

func (server *Server) tlsConfig() (*tls.Config, error) {
	caFile := homedir.Expand(server.options.TLSCACrtFile)
	caCert, err := ioutil.ReadFile(caFile)
	if err != nil {
		return nil, errors.New("could not open CA crt file " + caFile)
	}
	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, errors.New("could not parse CA crt file data in " + caFile)
	}
	tlsConfig := &tls.Config{
		ClientCAs:  caCertPool,
		ClientAuth: tls.RequireAndVerifyClientCert,
	}
	return tlsConfig, nil
}
