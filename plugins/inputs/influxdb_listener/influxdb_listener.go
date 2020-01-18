package influxdb_listener

import (
	"bytes"
	"compress/gzip"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/internal"
	tlsint "github.com/influxdata/telegraf/internal/tls"
	"github.com/influxdata/telegraf/plugins/inputs"
	"github.com/influxdata/telegraf/plugins/parsers/influx"
	"github.com/influxdata/telegraf/selfstat"
)

const (
	// DEFAULT_MAX_BODY_SIZE is the default maximum request body size, in bytes.
	// if the request body is over this size, we will return an HTTP 413 error.
	// 500 MB
	DEFAULT_MAX_BODY_SIZE = 500 * 1024 * 1024

	// MAX_LINE_SIZE is the maximum size, in bytes, that can be allocated for
	// a single InfluxDB point.
	// 64 KB
	DEFAULT_MAX_LINE_SIZE = 64 * 1024
)

type TimeFunc func() time.Time

type InfluxDBListener struct {
	ServiceAddress string `toml:"service_address"`
	// Port gets pulled out of ServiceAddress
	Port int
	tlsint.ServerConfig

	ReadTimeout   internal.Duration `toml:"read_timeout"`
	WriteTimeout  internal.Duration `toml:"write_timeout"`
	MaxBodySize   internal.Size     `toml:"max_body_size"`
	MaxLineSize   internal.Size     `toml:"max_line_size"`
	BasicUsername string            `toml:"basic_username"`
	BasicPassword string            `toml:"basic_password"`
	DatabaseTag   string            `toml:"database_tag"`

	TimeFunc

	mu sync.Mutex
	wg sync.WaitGroup

	listener net.Listener

	handler *influx.MetricHandler
	parser  *influx.Parser
	acc     telegraf.Accumulator
	pool    *pool

	bytesRecv       selfstat.Stat
	requestsServed  selfstat.Stat
	writesServed    selfstat.Stat
	queriesServed   selfstat.Stat
	pingsServed     selfstat.Stat
	requestsRecv    selfstat.Stat
	notFoundsServed selfstat.Stat
	buffersCreated  selfstat.Stat
	authFailures    selfstat.Stat

	Log telegraf.Logger

	longLines selfstat.Stat

	mux        http.ServeMux
}

const sampleConfig = `
  ## Address and port to host InfluxDB listener on
  service_address = ":8186"

  ## maximum duration before timing out read of the request
  read_timeout = "10s"
  ## maximum duration before timing out write of the response
  write_timeout = "10s"

  ## Maximum allowed HTTP request body size in bytes.
  ## 0 means to use the default of 524,288,000 bytes (500 mebibytes)
  max_body_size = "500MiB"

  ## Maximum line size allowed to be sent in bytes.
  ## 0 means to use the default of 65536 bytes (64 kibibytes)
  max_line_size = "64KiB"
  

  ## Optional tag name used to store the database. 
  ## If the write has a database in the query string then it will be kept in this tag name.
  ## This tag can be used in downstream outputs.
  ## The default value of nothing means it will be off and the database will not be recorded.
  # database_tag = ""

  ## Set one or more allowed client CA certificate file names to
  ## enable mutually authenticated TLS connections
  tls_allowed_cacerts = ["/etc/telegraf/clientca.pem"]

  ## Add service certificate and key
  tls_cert = "/etc/telegraf/cert.pem"
  tls_key = "/etc/telegraf/key.pem"

  ## Optional username and password to accept for HTTP basic authentication.
  ## You probably want to make sure you have TLS configured above for this.
  # basic_username = "foobar"
  # basic_password = "barfoo"
`

func (h *InfluxDBListener) SampleConfig() string {
	return sampleConfig
}

func (h *InfluxDBListener) Description() string {
	return "InfluxDB listener"
}

func (h *InfluxDBListener) Gather(_ telegraf.Accumulator) error {
	h.buffersCreated.Set(h.pool.ncreated())
	return nil
}

func (h *InfluxDBListener) routes() {
	h.mux.HandleFunc("/write", h.handleAuth(h.handleWrite()))
	h.mux.HandleFunc("/query", h.handleAuth(h.handleQuery()))
	h.mux.HandleFunc("/ping", h.handlePing())
	h.mux.HandleFunc("/", h.handleAuth(h.handleDefault()))
}

func (h *InfluxDBListener) Init() error {
	tags := map[string]string{
		"address": h.ServiceAddress,
	}
	h.bytesRecv = selfstat.Register("influxdb_listener", "bytes_received", tags)
	h.requestsServed = selfstat.Register("influxdb_listener", "requests_served", tags)
	h.writesServed = selfstat.Register("influxdb_listener", "writes_served", tags)
	h.queriesServed = selfstat.Register("influxdb_listener", "queries_served", tags)
	h.pingsServed = selfstat.Register("influxdb_listener", "pings_served", tags)
	h.requestsRecv = selfstat.Register("influxdb_listener", "requests_received", tags)
	h.notFoundsServed = selfstat.Register("influxdb_listener", "not_founds_served", tags)
	h.buffersCreated = selfstat.Register("influxdb_listener", "buffers_created", tags)
	h.authFailures = selfstat.Register("influxdb_listener", "auth_failures", tags)
	h.longLines = selfstat.Register("influxdb_listener", "long_lines", tags)
	h.routes()

	if h.MaxBodySize.Size == 0 {
		h.MaxBodySize.Size = DEFAULT_MAX_BODY_SIZE
	}
	if h.MaxLineSize.Size == 0 {
		h.MaxLineSize.Size = DEFAULT_MAX_LINE_SIZE
	}

	if h.ReadTimeout.Duration < time.Second {
		h.ReadTimeout.Duration = time.Second * 10
	}
	if h.WriteTimeout.Duration < time.Second {
		h.WriteTimeout.Duration = time.Second * 10
	}

	return nil
}

// Start starts the InfluxDB listener service.
func (h *InfluxDBListener) Start(acc telegraf.Accumulator) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.acc = acc
	h.pool = NewPool(200, int(h.MaxLineSize.Size))

	tlsConf, err := h.ServerConfig.TLSConfig()
	if err != nil {
		return err
	}

	server := &http.Server{
		Addr:         h.ServiceAddress,
		Handler:      h,
		ReadTimeout:  h.ReadTimeout.Duration,
		WriteTimeout: h.WriteTimeout.Duration,
		TLSConfig:    tlsConf,
	}

	var listener net.Listener
	if tlsConf != nil {
		listener, err = tls.Listen("tcp", h.ServiceAddress, tlsConf)
	} else {
		listener, err = net.Listen("tcp", h.ServiceAddress)
	}
	if err != nil {
		return err
	}
	h.listener = listener
	h.Port = listener.Addr().(*net.TCPAddr).Port

	h.handler = influx.NewMetricHandler()
	h.parser = influx.NewParser(h.handler)

	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		server.Serve(h.listener)
	}()

	h.Log.Infof("Started HTTP listener service on %s", h.ServiceAddress)

	return nil
}

// Stop cleans up all resources
func (h *InfluxDBListener) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.listener.Close()
	h.wg.Wait()

	h.Log.Infof("Stopped HTTP listener service on %s", h.ServiceAddress)
}

func (h *InfluxDBListener) ServeHTTP(res http.ResponseWriter, req *http.Request) {
	h.requestsRecv.Incr(1)
	h.mux.ServeHTTP(res, req)
	h.requestsServed.Incr(1)
}

func (h *InfluxDBListener) handleStats(pre selfstat.Stat, post selfstat.Stat, f http.HandlerFunc) http.HandlerFunc {
	return func(res http.ResponseWriter, req *http.Request) {
		pre.Incr(1)
		f(res, req)
		post.Incr(1)
	}
}

func (h *InfluxDBListener) handlePostStat(post selfstat.Stat, f http.HandlerFunc) http.HandlerFunc {
	return func(res http.ResponseWriter, req *http.Request) {
		f(res, req)
		post.Incr(1)
	}
}

func (h *InfluxDBListener) handleQuery() http.HandlerFunc {
	return func(res http.ResponseWriter, req *http.Request) {
		defer h.queriesServed.Incr(1)
		// Deliver a dummy response to the query endpoint, as some InfluxDB
		// clients test endpoint availability with a query
		res.Header().Set("Content-Type", "application/json")
		res.Header().Set("X-Influxdb-Version", "1.0")
		res.WriteHeader(http.StatusOK)
		res.Write([]byte("{\"results\":[]}"))
	}
}

func (h *InfluxDBListener) handlePing() http.HandlerFunc {
	return func(res http.ResponseWriter, req *http.Request) {
		defer h.pingsServed.Incr(1)
		verbose := req.URL.Query().Get("verbose")

		// respond to ping requests
		if verbose != "" && verbose != "0" && verbose != "false" {
			res.WriteHeader(http.StatusOK)
			b, _ := json.Marshal(map[string]string{"version": "1.0"}) // based on header set above
			res.Write(b)
		} else {
			res.WriteHeader(http.StatusNoContent)
		}
	}
}

func (h *InfluxDBListener) handleDefault() http.HandlerFunc {
	return func(res http.ResponseWriter, req *http.Request) {
		defer h.notFoundsServed.Incr(1)
		http.NotFound(res, req)
	}
}

func (h *InfluxDBListener) handleWrite() http.HandlerFunc {
	return func(res http.ResponseWriter, req *http.Request) {
		defer h.writesServed.Incr(1)
		// Check that the content length is not too large for us to handle.
		if req.ContentLength > h.MaxBodySize.Size {
			tooLarge(res)
			return
		}
		now := h.TimeFunc()

		precision := req.URL.Query().Get("precision")
		db := req.URL.Query().Get("db")

		// Handle gzip request bodies
		body := req.Body
		if req.Header.Get("Content-Encoding") == "gzip" {
			var err error
			body, err = gzip.NewReader(req.Body)
			if err != nil {
				h.Log.Debug(err.Error())
				badRequest(res, err.Error())
				return
			}
			defer body.Close()
		}
		body = http.MaxBytesReader(res, body, h.MaxBodySize.Size)

		var return400 bool
		var hangingBytes bool
		buf := h.pool.get()
		defer h.pool.put(buf)
		bufStart := 0
		for {
			n, err := io.ReadFull(body, buf[bufStart:])
			if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
				h.Log.Debug(err.Error())
				// problem reading the request body
				badRequest(res, err.Error())
				return
			}
			h.bytesRecv.Incr(int64(n))

			if err == io.EOF {
				if return400 {
					badRequest(res, "")
				} else {
					res.WriteHeader(http.StatusNoContent)
				}
				return
			}

			if hangingBytes {
				i := bytes.IndexByte(buf, '\n')
				if i == -1 {
					// still didn't find a newline, keep scanning
					continue
				}
				// rotate the bit remaining after the first newline to the front of the buffer
				i++ // start copying after the newline
				bufStart = len(buf) - i
				if bufStart > 0 {
					copy(buf, buf[i:])
				}
				hangingBytes = false
				continue
			}

			if err == io.ErrUnexpectedEOF {
				// finished reading the request body
				err = h.parse(buf[:n+bufStart], now, precision, db)
				if err != nil {
					h.Log.Debugf("%s: %s", err.Error(), bufStart+n)
					if strings.HasPrefix(err.Error(), "partial write:") {
						partialWrite(res, err.Error())
						return
					}
					return400 = true
				}
				if return400 {
					if err != nil {
						badRequest(res, err.Error())
					} else {
						badRequest(res, "")
					}
				} else {
					res.WriteHeader(http.StatusNoContent)
				}
				return
			}

			// if we got down here it means that we filled our buffer, and there
			// are still bytes remaining to be read. So we will parse up until the
			// final newline, then push the rest of the bytes into the next buffer.
			i := bytes.LastIndexByte(buf, '\n')
			if i == -1 {
				h.longLines.Incr(1)
				// drop any line longer than the max buffer size
				h.Log.Debugf("Influxdb_listener received a single line longer than the maximum of %d bytes",
					len(buf))
				hangingBytes = true
				return400 = true
				bufStart = 0
				continue
			}
			if err := h.parse(buf[:i+1], now, precision, db); err != nil {
				h.Log.Debug(err.Error())
				return400 = true
			}
			// rotate the bit remaining after the last newline to the front of the buffer
			i++ // start copying after the newline
			bufStart = len(buf) - i
			if bufStart > 0 {
				copy(buf, buf[i:])
			}
		}
	}
}

func (h *InfluxDBListener) parse(b []byte, t time.Time, precision, db string) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.handler.SetTimePrecision(getPrecisionMultiplier(precision))
	h.handler.SetTimeFunc(func() time.Time { return t })
	metrics, err := h.parser.EagerParse(b)

	for _, m := range metrics {
		// Do we need to keep the database name in the query string.
		// If a tag has been supplied to put the db in and we actually got a db query,
		// then we write it in. This overwrites the database tag if one was sent.
		// This makes it behave like the influx endpoint.
		if h.DatabaseTag != "" && db != "" {
			m.AddTag(h.DatabaseTag, db)
		}
		h.acc.AddFields(m.Name(), m.Fields(), m.Tags(), m.Time())
	}

	if err != nil {
		if len(metrics) > 0 {
			return fmt.Errorf("partial write: unable to parse: %s", err.Error())
		}
		return fmt.Errorf("unable to parse: %s", err.Error())
	}

	return nil
}

func tooLarge(res http.ResponseWriter) {
	res.Header().Set("Content-Type", "application/json")
	res.Header().Set("X-Influxdb-Version", "1.0")
	res.Header().Set("X-Influxdb-Error", "http: request body too large")
	res.WriteHeader(http.StatusRequestEntityTooLarge)
	res.Write([]byte(`{"error":"http: request body too large"}`))
}

func badRequest(res http.ResponseWriter, errString string) {
	res.Header().Set("Content-Type", "application/json")
	res.Header().Set("X-Influxdb-Version", "1.0")
	if errString == "" {
		errString = "http: bad request"
	}
	res.Header().Set("X-Influxdb-Error", errString)
	res.WriteHeader(http.StatusBadRequest)
	res.Write([]byte(fmt.Sprintf(`{"error":%q}`, errString)))
}

func partialWrite(res http.ResponseWriter, errString string) {
	res.Header().Set("Content-Type", "application/json")
	res.Header().Set("X-Influxdb-Version", "1.0")
	res.Header().Set("X-Influxdb-Error", errString)
	res.WriteHeader(http.StatusBadRequest)
	res.Write([]byte(fmt.Sprintf(`{"error":%q}`, errString)))
}

func (h *InfluxDBListener) handleAuth(f http.HandlerFunc) http.HandlerFunc {
	return func(res http.ResponseWriter, req *http.Request) {
		res.Header().Set("WWW-Authenticate", "Basic realm=\"Restricted\"")

		if h.BasicUsername != "" && h.BasicPassword != "" {
			reqUsername, reqPassword, ok := req.BasicAuth()
			if !ok ||
				subtle.ConstantTimeCompare([]byte(reqUsername), []byte(h.BasicUsername)) != 1 ||
				subtle.ConstantTimeCompare([]byte(reqPassword), []byte(h.BasicPassword)) != 1 {

				h.authFailures.Incr(1)
				http.Error(res, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}

		f(res, req)
	}
}

func getPrecisionMultiplier(precision string) time.Duration {
	d := time.Nanosecond
	switch precision {
	case "u":
		d = time.Microsecond
	case "ms":
		d = time.Millisecond
	case "s":
		d = time.Second
	case "m":
		d = time.Minute
	case "h":
		d = time.Hour
	}
	return d
}

func init() {
	// http_listener deprecated in 1.9
	inputs.Add("http_listener", func() telegraf.Input {
		return &InfluxDBListener{
			ServiceAddress: ":8186",
			TimeFunc:       time.Now,
		}
	})
	inputs.Add("influxdb_listener", func() telegraf.Input {
		return &InfluxDBListener{
			ServiceAddress: ":8186",
			TimeFunc:       time.Now,
		}
	})
}
