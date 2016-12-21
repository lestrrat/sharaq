package sharaq

import (
	"net/http"
	"net/url"
	"regexp"

	"github.com/lestrrat/sharaq/internal/transformer"
	"github.com/lestrrat/sharaq/internal/urlcache"
)

type Server struct {
	backend     Backend
	config      *Config
	cache       *urlcache.URLCache
	transformer *transformer.Transformer
}

type Backend interface {
	Serve(http.ResponseWriter, *http.Request)
	StoreTransformedContent(*url.URL) error
	Delete(*url.URL) error
}

// Dispatcher is responsible for marshaling the incoming request
// to the appropriate backend.
type Dispatcher struct {
	cache      *urlcache.URLCache
	backend    Backend
	bucketName string
	guardian   *Guardian
	listenAddr string
	logConfig  *LogConfig
	whitelist  []*regexp.Regexp
}

type DispatcherConfig interface {
	DispatcherAddr() string
}