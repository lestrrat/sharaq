package sharaq

import (
	"log"
	"net"
	"net/http"
	"regexp"
	"sync"

	"github.com/lestrrat/go-apache-logformat"
	"github.com/lestrrat/go-file-rotatelogs"
)

type Dispatcher struct {
	backend    Backend
	listenAddr string
	bucketName string
	whitelist  []*regexp.Regexp
	cache      *URLCache
	logConfig  *LogConfig
	guardian   *Guardian
}

type DispatcherConfig interface {
	DispatcherAddr() string
}

func NewDispatcher(s *Server, g *Guardian) (*Dispatcher, error) {
	c := s.config

	whitelist := make([]*regexp.Regexp, len(s.config.Whitelist()))
	for i, pat := range s.config.Whitelist() {
		re, err := regexp.Compile(pat)
		if err != nil {
			return nil, err
		}
		whitelist[i] = re
	}

	return &Dispatcher{
		backend:    s.backend,
		listenAddr: c.DispatcherAddr(),
		bucketName: c.BucketName(),
		cache:      s.cache,
		guardian:   g,
		logConfig:  s.config.DispatcherLog(),
		whitelist:  whitelist,
	}, nil
}

func (d *Dispatcher) Run(doneWg *sync.WaitGroup, exitCond *sync.Cond) {
	defer doneWg.Done()

	logger := apachelog.CombinedLog.Clone()
	if dl := d.logConfig; dl != nil {
		dlh := rotatelogs.NewRotateLogs(dl.LogFile)
		dlh.LinkName = dl.LinkName
		dlh.MaxAge = dl.MaxAge
		dlh.Offset = dl.Offset
		dlh.RotationTime = dl.RotationTime
		logger.SetOutput(dlh)

		log.Printf("Dispatcher logging to %s", dlh.LogFile)
	}
	srv := &http.Server{Addr: d.listenAddr, Handler: apachelog.WrapLoggingWriter(d, logger)}
	ln, err := makeListener(d.listenAddr)
	if err != nil {
		log.Printf("Error binding to listen address: %s", err)
		return
	}

	go func(ln net.Listener, exitCond *sync.Cond) {
		defer recover()
		exitCond.L.Lock()
		exitCond.Wait()
		exitCond.L.Unlock()
		ln.Close()
	}(ln, exitCond)

	log.Printf("Dispatcher listening on %s", d.listenAddr)
	srv.Serve(tcpKeepAliveListener{ln.(*net.TCPListener)})
}

func (d *Dispatcher) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		d.HandleFetch(w, r)

	default:
		http.Error(w, "What, what, what?", 400)
	}
}

// HandleFetch replies with the proper URL of the image
func (d *Dispatcher) HandleFetch(w http.ResponseWriter, r *http.Request) {
	rawValue := r.FormValue("url")
	if rawValue == "" {
		http.Error(w, "Bad url", 500)
		return
	}

	allowed := false
	if len(d.whitelist) == 0 {
		allowed = true
	} else {
		for _, pat := range d.whitelist {
			if pat.MatchString(rawValue) {
				allowed = true
				break
			}
		}
	}

	if !allowed {
		http.Error(w, "Specified url not allowed", 403)
		return
	}

	d.backend.Serve(w, r)
}
