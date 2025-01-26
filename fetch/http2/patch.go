package http2

import (
	"bufio"
	"context"
	cryptotls "crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/textproto"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	tls "github.com/refraction-networking/utls"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2/hpack"
)

const (
	defaultMaxStreams = 250 // TODO: make this 100 as the GFE seems to?
)

var hackField = map[string]uintptr{}
var done = atomic.Bool{}

func init() {
	done.Store(true)
	t := reflect.TypeOf(new(cryptotls.Conn)).Elem()
	for _, name := range []string{"conn", "config", "clientProtocol", "isHandshakeComplete"} {
		field, ok := t.FieldByName(name)
		if ok {
			hackField[name] = field.Offset
		}
	}
}

// hackTlsConn make a hack, set private field
func hackTlsConn(uConn *tls.UConn) net.Conn {
	state := uConn.ConnectionState()
	if state.NegotiatedProtocol != NextProtoTLS {
		return uConn
	}
	ret := new(cryptotls.Conn)

	for field, offset := range hackField {
		ptr := unsafe.Pointer(uintptr(unsafe.Pointer(ret)) + offset)
		switch field {
		case "conn":
			*(*net.Conn)(ptr) = uConn
		case "config":
			*(**cryptotls.Config)(ptr) = &cryptotls.Config{}
		case "clientProtocol":
			*(*string)(ptr) = state.NegotiatedProtocol
		case "isHandshakeComplete":
			*(*atomic.Bool)(ptr) = done
		}
	}
	return ret
}

type Options struct {
	// HeaderOrder is for ResponseWriter.Header map keys
	// that, if present, defines a header order that will be used to
	// write the headers onto wire. The order of the slice defined how the headers
	// will be sorted. A defined Key goes before an undefined Key.
	//
	// This is the only way to specify some order, because maps don't
	// have a stable iteration order. If no order is given, headers will
	// be sorted lexicographically.
	//
	// According to RFC2616 it is good practice to send general-header fields
	// first, followed by request-header or response-header fields and ending
	// with entity-header fields.
	HeaderOrder []string

	// PHeaderOrder is for setting http2 pseudo header order.
	// If is nil it will use regular GoLang header order.
	// Valid fields are :authority, :method, :path, :scheme
	PHeaderOrder []string

	// Settings frame, the client informs the server about its HTTP/2 preferences.
	// if nil, will use default settings
	Settings []Setting

	// WindowSizeIncrement optionally specifies an upper limit for the
	// WINDOW_UPDATE frame. If zero, the default value of 2^30 is used.
	WindowSizeIncrement uint32

	// PriorityParams specifies the sender-advised priority of a stream.
	// if nil, will not send.
	PriorityParams map[uint32]PriorityParam

	// GetTlsClientHelloSpec returns the TLS spec to use with
	// tls.UClient.
	// If nil, the default configuration is used.
	GetTlsClientHelloSpec func() *tls.ClientHelloSpec
}

// Transport is an HTTP/2 Transport.
//
// A Transport internally caches connections to servers. It is safe
// for concurrent use by multiple goroutines.
type Transport struct {
	// DialTLSContext specifies an optional dial function with context for
	// creating TLS connections for requests.
	//
	// If DialTLSContext and DialTLS is nil, tls.Dial is used.
	//
	// If the returned net.Conn has a ConnectionState method like tls.Conn,
	// it will be used to set http.Response.TLS.
	DialTLSContext func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error)

	// DialTLS specifies an optional dial function for creating
	// TLS connections for requests.
	//
	// If DialTLSContext and DialTLS is nil, tls.Dial is used.
	//
	// Deprecated: Use DialTLSContext instead, which allows the transport
	// to cancel dials as soon as they are no longer needed.
	// If both are set, DialTLSContext takes priority.
	DialTLS func(network, addr string, cfg *tls.Config) (net.Conn, error)

	// TLSClientConfig specifies the TLS configuration to use with
	// tls.Client. If nil, the default configuration is used.
	TLSClientConfig *tls.Config

	// ConnPool optionally specifies an alternate connection pool to use.
	// If nil, the default is used.
	ConnPool ClientConnPool

	// DisableCompression, if true, prevents the Transport from
	// requesting compression with an "Accept-Encoding: gzip"
	// request header when the Request contains no existing
	// Accept-Encoding value. If the Transport requests gzip on
	// its own and gets a gzipped response, it's transparently
	// decoded in the Response.Body. However, if the user
	// explicitly requested gzip it is not automatically
	// uncompressed.
	DisableCompression bool

	// AllowHTTP, if true, permits HTTP/2 requests using the insecure,
	// plain-text "http" scheme. Note that this does not enable h2c support.
	AllowHTTP bool

	// MaxHeaderListSize is the http2 SETTINGS_MAX_HEADER_LIST_SIZE to
	// send in the initial settings frame. It is how many bytes
	// of response headers are allowed. Unlike the http2 spec, zero here
	// means to use a default limit (currently 10MB). If you actually
	// want to advertise an unlimited value to the peer, Transport
	// interprets the highest possible value here (0xffffffff or 1<<32-1)
	// to mean no limit.
	MaxHeaderListSize uint32

	// MaxReadFrameSize is the http2 SETTINGS_MAX_FRAME_SIZE to send in the
	// initial settings frame. It is the size in bytes of the largest frame
	// payload that the sender is willing to receive. If 0, no setting is
	// sent, and the value is provided by the peer, which should be 16384
	// according to the spec:
	// https://datatracker.ietf.org/doc/html/rfc7540#section-6.5.2.
	// Values are bounded in the range 16k to 16M.
	MaxReadFrameSize uint32

	// MaxDecoderHeaderTableSize optionally specifies the http2
	// SETTINGS_HEADER_TABLE_SIZE to send in the initial settings frame. It
	// informs the remote endpoint of the maximum size of the header compression
	// table used to decode header blocks, in octets. If zero, the default value
	// of 4096 is used.
	MaxDecoderHeaderTableSize uint32

	// MaxEncoderHeaderTableSize optionally specifies an upper limit for the
	// header compression table used for encoding request headers. Received
	// SETTINGS_HEADER_TABLE_SIZE settings are capped at this limit. If zero,
	// the default value of 4096 is used.
	MaxEncoderHeaderTableSize uint32

	// StrictMaxConcurrentStreams controls whether the server's
	// SETTINGS_MAX_CONCURRENT_STREAMS should be respected
	// globally. If false, new TCP connections are created to the
	// server as needed to keep each under the per-connection
	// SETTINGS_MAX_CONCURRENT_STREAMS limit. If true, the
	// server's SETTINGS_MAX_CONCURRENT_STREAMS is interpreted as
	// a global limit and callers of RoundTrip block when needed,
	// waiting for their turn.
	StrictMaxConcurrentStreams bool

	// IdleConnTimeout is the maximum amount of time an idle
	// (keep-alive) connection will remain idle before closing
	// itself.
	// Zero means no limit.
	IdleConnTimeout time.Duration

	// ReadIdleTimeout is the timeout after which a health check using ping
	// frame will be carried out if no frame is received on the connection.
	// Note that a ping response will is considered a received frame, so if
	// there is no other traffic on the connection, the health check will
	// be performed every ReadIdleTimeout interval.
	// If zero, no health check is performed.
	ReadIdleTimeout time.Duration

	// PingTimeout is the timeout after which the connection will be closed
	// if a response to Ping is not received.
	// Defaults to 15s.
	PingTimeout time.Duration

	// WriteByteTimeout is the timeout after which the connection will be
	// closed no data can be written to it. The timeout begins when data is
	// available to write, and is extended whenever any bytes are written.
	WriteByteTimeout time.Duration

	// CountError, if non-nil, is called on HTTP/2 transport errors.
	// It's intended to increment a metric for monitoring, such
	// as an expvar or Prometheus metric.
	// The errType consists of only ASCII word characters.
	CountError func(errType string)

	// t1, if non-nil, is the standard library Transport using
	// this transport. Its settings are used (but not its
	// RoundTrip method, etc).
	t1 *http.Transport

	connPoolOnce  sync.Once
	connPoolOrDef ClientConnPool // non-nil version of ConnPool

	*transportTestHooks

	opt Options
}

// ConfigureTransport configures a net/http HTTP/1 Transport to use HTTP/2.
// It returns an error if t1 has already been HTTP/2-enabled.
//
// Use ConfigureTransports instead to configure the HTTP/2 Transport.
func ConfigureTransport(t1 *http.Transport, opt ...Options) error {
	_, err := ConfigureTransports(t1, opt...)
	return err
}

// ConfigureTransports configures a net/http HTTP/1 Transport to use HTTP/2.
// It returns a new HTTP/2 Transport for further configuration.
// It returns an error if t1 has already been HTTP/2-enabled.
func ConfigureTransports(t1 *http.Transport, opt ...Options) (*Transport, error) {
	return configureTransports(t1, opt...)
}

func configureTransports(t1 *http.Transport, opt ...Options) (*Transport, error) {
	connPool := new(clientConnPool)
	t2 := &Transport{
		AllowHTTP: true,
		ConnPool:  noDialClientConnPool{connPool},
		t1:        t1,
	}
	if len(opt) > 0 {
		t2.opt = opt[0]
	}
	connPool.t = t2
	t1.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		conn, err := t2.dialTLSWithContext(ctx, network, addr, t2.newTLSConfig(host))
		if err != nil {
			return nil, err
		}
		// set the utls.Conn to cryptotls.NetConn
		return hackTlsConn(conn), nil
	}
	if err := registerHTTPSProtocol(t1, noDialH2RoundTripper{t2}); err != nil {
		return nil, err
	}
	if t1.TLSClientConfig == nil {
		t1.TLSClientConfig = new(cryptotls.Config)
	}
	if !strSliceContains(t1.TLSClientConfig.NextProtos, "h2") {
		t1.TLSClientConfig.NextProtos = append([]string{"h2"}, t1.TLSClientConfig.NextProtos...)
	}
	if !strSliceContains(t1.TLSClientConfig.NextProtos, "http/1.1") {
		t1.TLSClientConfig.NextProtos = append(t1.TLSClientConfig.NextProtos, "http/1.1")
	}
	upgradeFn := func(scheme, authority string, c net.Conn) http.RoundTripper {
		addr := authorityAddr(scheme, authority)
		if used, err := connPool.addConnIfNeeded(addr, t2, c); err != nil {
			go c.Close()
			return erringRoundTripper{err}
		} else if !used {
			// Turns out we don't need this c.
			// For example, two goroutines made requests to the same host
			// at the same time, both kicking off TCP dials. (since protocol
			// was unknown)
			go c.Close()
		}
		if scheme == "http" {
			return (*unencryptedTransport)(t2)
		}
		return t2
	}
	if t1.TLSNextProto == nil {
		t1.TLSNextProto = make(map[string]func(string, *cryptotls.Conn) http.RoundTripper)
	}
	t1.TLSNextProto[NextProtoTLS] = func(authority string, c *cryptotls.Conn) http.RoundTripper {
		// get the utls.Conn
		return upgradeFn("https", authority, c.NetConn())
	}
	// The "unencrypted_http2" TLSNextProto key is used to pass off non-TLS HTTP/2 conns.
	t1.TLSNextProto[nextProtoUnencryptedHTTP2] = func(authority string, c *cryptotls.Conn) http.RoundTripper {
		nc, err := unencryptedNetConnFromTLSConn(c.NetConn())
		if err != nil {
			go c.Close()
			return erringRoundTripper{err}
		}
		return upgradeFn("http", authority, nc)
	}
	return t2, nil
}

var zeroDialer net.Dialer

// dialTLSWithContext uses tls.Dialer, added in Go 1.15, to open a TLS
// connection.
func (t *Transport) dialTLSWithContext(ctx context.Context, network, addr string, cfg *tls.Config) (tlsConn *tls.UConn, err error) {
	var conn net.Conn
	if t.t1 != nil && t.t1.DialContext != nil {
		conn, err = t.t1.DialContext(ctx, network, addr)
	} else {
		conn, err = zeroDialer.DialContext(ctx, network, addr)
	}

	if err != nil {
		return
	}

	if t.opt.GetTlsClientHelloSpec != nil {
		tlsConn = tls.UClient(conn, cfg, tls.HelloCustom)
		if err = tlsConn.ApplyPreset(t.opt.GetTlsClientHelloSpec()); err != nil {
			go conn.Close()
			return
		}
	} else {
		tlsConn = tls.UClient(conn, cfg, tls.HelloGolang)
	}

	if err = tlsConn.HandshakeContext(ctx); err != nil {
		go conn.Close()
		return
	}
	return
}

func (t *Transport) newClientConn(c net.Conn, singleUse bool) (*ClientConn, error) {
	conf := configFromTransport(t)
	cc := &ClientConn{
		t:                           t,
		tconn:                       c,
		readerDone:                  make(chan struct{}),
		nextStreamID:                1,
		maxFrameSize:                16 << 10, // spec default
		initialWindowSize:           65535,    // spec default
		initialStreamRecvWindowSize: conf.MaxUploadBufferPerStream,
		maxConcurrentStreams:        initialMaxConcurrentStreams, // "infinite", per spec. Use a smaller value until we have received server settings.
		peerMaxHeaderListSize:       0xffffffffffffffff,          // "infinite", per spec. Use 2^64-1 instead.
		streams:                     make(map[uint32]*clientStream),
		singleUse:                   singleUse,
		seenSettingsChan:            make(chan struct{}),
		wantSettingsAck:             true,
		readIdleTimeout:             conf.SendPingTimeout,
		pingTimeout:                 conf.PingTimeout,
		pings:                       make(map[[8]byte]chan struct{}),
		reqHeaderMu:                 make(chan struct{}, 1),
		lastActive:                  t.now(),
	}

	// Start the idle timer after the connection is fully initialized.
	if d := t.idleConnTimeout(); d != 0 {
		cc.idleTimeout = d
		cc.idleTimer = t.afterFunc(d, cc.onIdleTimeout)
	}

	var group synctestGroupInterface
	if t.transportTestHooks != nil {
		t.markNewGoroutine()
		t.transportTestHooks.newclientconn(cc)
		c = cc.tconn
		group = t.group
	}

	cc.cond = sync.NewCond(&cc.mu)
	cc.flow.add(int32(initialWindowSize))

	// TODO: adjust this writer size to account for frame size +
	// MTU + crypto/tls record padding.
	cc.bw = bufio.NewWriter(stickyErrWriter{
		group:   group,
		conn:    c,
		timeout: conf.WriteByteTimeout,
		err:     &cc.werr,
	})
	cc.br = bufio.NewReader(c)
	cc.fr = NewFramer(cc.bw, cc.br)
	cc.fr.SetMaxReadFrameSize(conf.MaxReadFrameSize)
	if t.CountError != nil {
		cc.fr.countError = t.CountError
	}

	if t.AllowHTTP {
		cc.nextStreamID = 3
	}

	cc.henc = hpack.NewEncoder(&cc.hbuf)
	cc.henc.SetMaxDynamicTableSizeLimit(conf.MaxEncoderHeaderTableSize)
	cc.peerMaxHeaderTableSize = initialHeaderTableSize

	if cs, ok := c.(connectionStater); ok {
		state := cs.ConnectionState()
		//cc.tlsState = state
		// if not HTTP/2
		if state.NegotiatedProtocol != NextProtoTLS {
			return cc, nil
		}
	}

	maxHeaderTableSize := conf.MaxDecoderHeaderTableSize
	var settings []Setting
	if len(t.opt.Settings) == 0 {
		settings = []Setting{
			{ID: SettingEnablePush, Val: 0},
			{ID: SettingInitialWindowSize, Val: uint32(cc.initialStreamRecvWindowSize)},
		}
		settings = append(settings, Setting{ID: SettingMaxFrameSize, Val: conf.MaxReadFrameSize})
		if max := t.maxHeaderListSize(); max != 0 {
			settings = append(settings, Setting{ID: SettingMaxHeaderListSize, Val: max})
		}
		if maxHeaderTableSize != initialHeaderTableSize {
			settings = append(settings, Setting{ID: SettingHeaderTableSize, Val: maxHeaderTableSize})
		}
	} else {
		settings = t.opt.Settings
		settingVal := make([]uint32, 7)
		for _, setting := range settings {
			if err := setting.Valid(); err != nil {
				return nil, err
			}
			settingVal[setting.ID] = setting.Val
		}
		if v := settingVal[SettingHeaderTableSize]; v > 0 {
			t.MaxEncoderHeaderTableSize = v
			t.MaxDecoderHeaderTableSize = v
			maxHeaderTableSize = v
		}
		if v := settingVal[SettingMaxConcurrentStreams]; v > 0 {
			cc.maxConcurrentStreams = v
		}
		if v := settingVal[SettingInitialWindowSize]; v > 0 {
			cc.initialWindowSize = v
		}
		if v := settingVal[SettingMaxFrameSize]; v > 0 {
			t.MaxReadFrameSize = v
			cc.maxFrameSize = v
		}
		if v := settingVal[SettingMaxHeaderListSize]; v > 0 {
			t.MaxHeaderListSize = v
		}
	}

	cc.fr.ReadMetaHeaders = hpack.NewDecoder(maxHeaderTableSize, nil)
	cc.fr.MaxHeaderListSize = t.maxHeaderListSize()

	cc.bw.Write(clientPreface)
	cc.fr.WriteSettings(settings...)
	if t.opt.WindowSizeIncrement > 0 {
		cc.fr.WriteWindowUpdate(0, t.opt.WindowSizeIncrement)
		cc.inflow.init(int32(t.opt.WindowSizeIncrement + initialWindowSize))
	} else {
		cc.fr.WriteWindowUpdate(0, transportDefaultConnFlow)
		cc.inflow.init(transportDefaultConnFlow + initialWindowSize)
	}
	if len(t.opt.PriorityParams) > 0 {
		for id, frame := range t.opt.PriorityParams {
			cc.fr.WritePriority(id, frame)
		}
	}
	cc.bw.Flush()
	if cc.werr != nil {
		cc.Close()
		return nil, cc.werr
	}

	// Start the idle timer after the connection is fully initialized.
	if d := t.idleConnTimeout(); d != 0 {
		cc.idleTimeout = d
		cc.idleTimer = t.afterFunc(d, cc.onIdleTimeout)
	}

	go cc.readLoop()
	return cc, nil
}

// requires cc.wmu be held.
func (cc *ClientConn) encodeHeaders(req *http.Request, addGzipHeader bool, trailers string, contentLength int64) ([]byte, error) {
	cc.hbuf.Reset()
	if req.URL == nil {
		return nil, errNilRequestURL
	}

	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	host, err := httpguts.PunycodeHostPort(host)
	if err != nil {
		return nil, err
	}
	if !httpguts.ValidHostHeader(host) {
		return nil, errors.New("http2: invalid Host header")
	}

	var path string
	if !isNormalConnect(req) {
		path = req.URL.RequestURI()
		if !validPseudoPath(path) {
			orig := path
			path = strings.TrimPrefix(path, req.URL.Scheme+"://"+host)
			if !validPseudoPath(path) {
				if req.URL.Opaque != "" {
					return nil, fmt.Errorf("invalid request :path %q from URL.Opaque = %q", orig, req.URL.Opaque)
				} else {
					return nil, fmt.Errorf("invalid request :path %q", orig)
				}
			}
		}
	}

	// Check for any invalid headers+trailers and return an error before we
	// potentially pollute our hpack state. (We want to be able to
	// continue to reuse the hpack encoder for future requests)
	if err := validateHeaders(req.Header); err != "" {
		return nil, fmt.Errorf("invalid HTTP header %s", err)
	}
	if err := validateHeaders(req.Trailer); err != "" {
		return nil, fmt.Errorf("invalid HTTP trailer %s", err)
	}

	// PATCH START
	enumerateHeaders := func(f func(name, value string)) {
		// 8.1.2.3 Request Pseudo-Header Fields
		// The :path pseudo-header field includes the path and query parts of the
		// target URI (the path-absolute production and optionally a '?' character
		// followed by the query production, see Sections 3.3 and 3.4 of
		// [RFC3986]).
		if len(cc.t.opt.PHeaderOrder) > 0 {
			for _, p := range cc.t.opt.PHeaderOrder {
				switch p {
				case ":authority":
					f(":authority", host)
				case ":method":
					m := req.Method
					if m == "" {
						m = http.MethodGet
					}
					f(":method", m)
				case ":path":
					if req.Method != "CONNECT" {
						f(":path", path)
					}
				case ":scheme":
					if req.Method != "CONNECT" {
						f(":scheme", req.URL.Scheme)
					}
				}
			}
		} else {
			f(":authority", host)
			m := req.Method
			if m == "" {
				m = http.MethodGet
			}
			f(":method", m)
			if !isNormalConnect(req) {
				f(":path", path)
				f(":scheme", req.URL.Scheme)
			}
		}

		if trailers != "" {
			f("trailer", trailers)
		}

		var didUA bool
		var kvs []keyValues

		if len(cc.t.opt.HeaderOrder) > 0 {
			kvs = sortedKeyValuesBy(req.Header, cc.t.opt.HeaderOrder)
		} else {
			kvs = sortedKeyValues(req.Header)
		}

		for _, kv := range kvs {
			if asciiEqualFold(kv.key, "host") || asciiEqualFold(kv.key, "content-length") {
				// Host is :authority, already sent.
				// Content-Length is automatic, set below.
				continue
			} else if asciiEqualFold(kv.key, "connection") ||
				asciiEqualFold(kv.key, "proxy-connection") ||
				asciiEqualFold(kv.key, "transfer-encoding") ||
				asciiEqualFold(kv.key, "upgrade") ||
				asciiEqualFold(kv.key, "keep-alive") {
				// Per 8.1.2.2 Connection-Specific Header
				// Fields, don't send connection-specific
				// fields. We have already checked if any
				// are error-worthy so just ignore the rest.
				continue
			} else if asciiEqualFold(kv.key, "user-agent") {
				// Match Go's http1 behavior: at most one
				// User-Agent. If set to nil or empty string,
				// then omit it. Otherwise if not mentioned,
				// include the default (below).
				didUA = true
				if len(kv.values) < 1 {
					continue
				}
				kv.values = kv.values[:1]
				if kv.values[0] == "" {
					continue
				}
			} else if asciiEqualFold(kv.key, "cookie") {
				// Per 8.1.2.5 To allow for better compression efficiency, the
				// Cookie header field MAY be split into separate header fields,
				// each with one or more cookie-pairs.
				for _, v := range kv.values {
					for {
						p := strings.IndexByte(v, ';')
						if p < 0 {
							break
						}
						f("cookie", v[:p])
						p++
						// strip space after semicolon if any.
						for p+1 <= len(v) && v[p] == ' ' {
							p++
						}
						v = v[p:]
					}
					if len(v) > 0 {
						f("cookie", v)
					}
				}
				continue
			}

			for _, v := range kv.values {
				f(kv.key, v)
			}
		}

		// PATCH END
		if shouldSendReqContentLength(req.Method, contentLength) {
			f("content-length", strconv.FormatInt(contentLength, 10))
		}
		if addGzipHeader {
			f("accept-encoding", "gzip")
		}
		if !didUA {
			f("user-agent", defaultUserAgent)
		}
	}

	// Do a first pass over the headers counting bytes to ensure
	// we don't exceed cc.peerMaxHeaderListSize. This is done as a
	// separate pass before encoding the headers to prevent
	// modifying the hpack state.
	hlSize := uint64(0)
	enumerateHeaders(func(name, value string) {
		hf := hpack.HeaderField{Name: name, Value: value}
		hlSize += uint64(hf.Size())
	})

	if hlSize > cc.peerMaxHeaderListSize {
		return nil, errRequestHeaderListSize
	}

	// Header list size is ok. Write the headers.
	enumerateHeaders(func(name, value string) {
		name, ascii := lowerHeader(name)
		if !ascii {
			// Skip writing invalid headers. Per RFC 7540, Section 8.1.2, header
			// field names have to be ASCII characters (just as in HTTP/1.x).
			return
		}
		cc.writeHeader(name, value)
	})

	return cc.hbuf.Bytes(), nil
}

// foreachHeaderElement splits v according to the "#rule" construction
// in RFC 7230 section 7 and calls fn for each non-empty element.
func foreachHeaderElement(v string, fn func(string)) {
	v = textproto.TrimString(v)
	if v == "" {
		return
	}
	if !strings.Contains(v, ",") {
		fn(v)
		return
	}
	for _, f := range strings.Split(v, ",") {
		if f = textproto.TrimString(f); f != "" {
			fn(f)
		}
	}
}

type keyValues struct {
	key    string
	values []string
}

// A headerSorter implements sort.Interface by sorting a []keyValues
// by the given order, if not nil, or by Key otherwise.
// It's used as a pointer, so it can fit in a sort.Interface
// value without allocation.
type headerSorter struct {
	kvs   []keyValues
	order map[string]int
}

func (s *headerSorter) Len() int      { return len(s.kvs) }
func (s *headerSorter) Swap(i, j int) { s.kvs[i], s.kvs[j] = s.kvs[j], s.kvs[i] }
func (s *headerSorter) Less(i, j int) bool {
	// If the order isn't defined, sort lexicographically.
	if len(s.order) == 0 {
		return s.kvs[i].key < s.kvs[j].key
	}
	si, iok := s.order[strings.ToLower(s.kvs[i].key)]
	sj, jok := s.order[strings.ToLower(s.kvs[j].key)]
	if !iok && !jok {
		return s.kvs[i].key < s.kvs[j].key
	} else if !iok && jok {
		return false
	} else if iok && !jok {
		return true
	}
	return si < sj
}

var headerSorterPool = sync.Pool{
	New: func() any { return new(headerSorter) },
}

func sortedKeyValues(header http.Header) (kvs []keyValues) {
	sorter := headerSorterPool.Get().(*headerSorter)
	defer headerSorterPool.Put(sorter)

	if cap(sorter.kvs) < len(header) {
		sorter.kvs = make([]keyValues, 0, len(header))
	}

	kvs = sorter.kvs[:0]
	for k, vv := range header {
		kvs = append(kvs, keyValues{k, vv})
	}

	sorter.kvs = kvs
	sort.Sort(sorter)
	return kvs
}

func sortedKeyValuesBy(header http.Header, headerOrder []string) (kvs []keyValues) {
	sorter := headerSorterPool.Get().(*headerSorter)
	defer headerSorterPool.Put(sorter)

	if cap(sorter.kvs) < len(header) {
		sorter.kvs = make([]keyValues, 0, len(header))
	}
	kvs = sorter.kvs[:0]
	for k, vv := range header {
		kvs = append(kvs, keyValues{k, vv})
	}
	sorter.kvs = kvs
	sorter.order = make(map[string]int)
	for i, v := range headerOrder {
		sorter.order[v] = i
	}
	sort.Sort(sorter)
	return kvs
}
