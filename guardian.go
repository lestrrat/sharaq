package sharaq

import (
	"bytes"
	"fmt"
	"hash/crc64"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/goamz/goamz/aws"
	"github.com/goamz/goamz/s3"
)

type Guardian struct {
	Bucket          *s3.Bucket
	Cache           *URLCache
	listenAddr      string
	processingMutex *sync.Mutex
	processing      map[uint64]bool
	transformer     *Transformer
}

type GuardianConfig interface {
	AccessKey() string
	BucketName() string
	GuardianAddr() string
	SecretKey() string
}

var presets = map[string]string{
	"pc-thumb":     "360x216",
	"ticket-thumb": "170x230",
	"wando-thumb":  "596x450",
	"email-thumb":  "596x450",
}

func NewGuardian(s *Server) (*Guardian, error) {
	c := s.config
	auth := aws.Auth{
		AccessKey: c.AccessKey(),
		SecretKey: c.SecretKey(),
	}

	s3o := s3.New(auth, aws.APNortheast)
	g := &Guardian{
		Bucket:          s3o.Bucket(c.BucketName()),
		Cache:           s.cache,
		listenAddr:      c.GuardianAddr(),
		processingMutex: &sync.Mutex{},
		processing:      make(map[uint64]bool),
		transformer:     s.transformer,
	}

	return g, nil
}

func (g *Guardian) Run(doneCh chan struct{}) {
	defer func() { doneCh <- struct{}{} }()
	log.Printf("Guardian listening on %s", g.listenAddr)
	http.ListenAndServe(g.listenAddr, g)
}

func (g *Guardian) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		g.HandleView(w, r)
	case "PUT":
		g.HandleStore(w, r)
	case "DELETE":
		g.HandleDelete(w, r)
	default:
		http.Error(w, "What, what, what?", 400)
	}
}

func (g *Guardian) MarkProcessing(u *url.URL) bool {
	h := crc64.New(crc64Table)
	io.WriteString(h, u.String())
	k := h.Sum64()

	g.processingMutex.Lock()
	defer g.processingMutex.Unlock()
	g.processing[k] = true
	return true
}

func (g *Guardian) UnmarkProcessing(u *url.URL) {
	h := crc64.New(crc64Table)
	io.WriteString(h, u.String())
	k := h.Sum64()

	g.processingMutex.Lock()
	defer g.processingMutex.Unlock()
	delete(g.processing, k)
}

func (g *Guardian) transformAllAndStore(u *url.URL) chan error {

	// Transformation is completely done by the transformer, so just
	// hand it over to it
	wg := &sync.WaitGroup{}
	errCh := make(chan error, len(presets))
	for preset, rule := range presets {
		wg.Add(1)
		go func(wg *sync.WaitGroup, t *Transformer, preset string, rule string, errCh chan error) {
			defer wg.Done()

			res, err := t.Transform(rule, u.String())
			if err != nil {
				errCh <- err
				return
			}

			// good, done. save it to S3
			path := "/" + preset + u.Path
			log.Printf("Sending PUT to S3 %s...", path)
			err = g.Bucket.PutReader(path, res.content, res.size, res.contentType, s3.PublicRead, s3.Options{})
			defer res.content.Close()
			if err != nil {
				errCh <- err
				return
			}
		}(wg, g.transformer, preset, rule, errCh)
	}

	wg.Wait()
	close(errCh)

	return errCh
}

func (g *Guardian) HandleView(w http.ResponseWriter, r *http.Request) {
	rawValue := r.FormValue("url")
	if rawValue == "" {
		log.Printf("URL was empty")
		http.Error(w, "Bad url", 500)
		return
	}

	u, err := url.Parse(rawValue)
	if err != nil {
		log.Printf("Parsing URL '%s' failed: %s", rawValue, err)
		http.Error(w, "Bad url", 500)
		return
	}

	vars := struct {
		Images map[string]string
	} {
		Images: make(map[string]string),
	}
	for name := range presets {
		vars.Images[name] = "http://ix.peatix.com.s3.amazonaws.com/" + name + u.Path
	}

	t, err := template.New("sharaq-view").Parse(`
<html>
<body>
<table>
{{range $name, $url := .Images}}
<tr>
    <td>{{ $name }}</td>
    <td><img src="{{ $url }}"></td>
</tr>
{{end}}
</table>
</body>
</html>`)
	if err != nil {
		log.Printf("Error parsing template: %s", err)
		http.Error(w, "Template error", 500)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf8")
	t.Execute(w, vars)
}

// HandleStore accepts PUT requests to create resized images and
// store them on S3
func (g *Guardian) HandleStore(w http.ResponseWriter, r *http.Request) {
	rawValue := r.FormValue("url")
	if rawValue == "" {
		log.Printf("URL was empty")
		http.Error(w, "Bad url", 500)
		return
	}

	u, err := url.Parse(rawValue)
	if err != nil {
		log.Printf("Parsing URL '%s' failed: %s", rawValue, err)
		http.Error(w, "Bad url", 500)
		return
	}

	// Don't process the same url while somebody else is processing it
	if !g.MarkProcessing(u) {
		log.Printf("URL '%s' is being processed", rawValue)
		http.Error(w, "url is being processed", 500)
		return
	}
	defer g.UnmarkProcessing(u)

	start := time.Now()
	errCh := g.transformAllAndStore(u)

	buf := &bytes.Buffer{}
	for err := range errCh {
		fmt.Fprintf(buf, "Err: %s\n", err)
	}

	if buf.Len() > 0 {
		log.Printf("Error detected while processing: %s", buf.String())
		http.Error(w, buf.String(), 500)
		return
	}

	w.Header().Add("X-Peatix-Elapsed-Time", fmt.Sprintf("%0.2f", time.Since(start).Seconds()))
}

// HandleDelete accepts DELETE requests to delete all known resized images from S3
func (g *Guardian) HandleDelete(w http.ResponseWriter, r *http.Request) {
	rawValue := r.FormValue("url")
	if rawValue == "" {
		http.Error(w, "Bad url", 500)
		return
	}

	u, err := url.Parse(rawValue)
	if err != nil {
		http.Error(w, "Bad url", 500)
		return
	}

	// Don't process the same url while somebody else is processing it
	if !g.MarkProcessing(u) {
		http.Error(w, "url is being processed", 500)
		return
	}
	defer g.UnmarkProcessing(u)

	log.Printf("DELETE for source image: %s\n", u.String())

	start := time.Now()
	// Transformation is completely done by the transformer, so just
	// hand it over to it
	wg := &sync.WaitGroup{}
	errCh := make(chan error, len(presets))
	for preset := range presets {
		wg.Add(1)
		go func(wg *sync.WaitGroup, preset string, errCh chan error) {
			defer wg.Done()
			path := "/" + preset + u.Path
			log.Printf(" + DELETE S3 entry %s\n", path)
			err = g.Bucket.Del(path)
			if err != nil {
				errCh <- err
			}

			// fallthrough here regardless, because it's better to lose the
			// cache than to accidentally have one linger
			g.Cache.Delete(MakeCacheKey(preset, u.String()))
		}(wg, preset, errCh)
	}

	wg.Wait()
	close(errCh)

	buf := &bytes.Buffer{}
	for err := range errCh {
		fmt.Fprintf(buf, "Err: %s\n", err)
	}

	if buf.Len() > 0 {
		http.Error(w, buf.String(), 500)
		return
	}

	w.Header().Add("X-Peatix-Elapsed-Time", fmt.Sprintf("%0.2f", time.Since(start).Seconds()))
}