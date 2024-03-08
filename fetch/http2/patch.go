package http2

import (
	"bufio"
	"compress/gzip"
	"compress/zlib"
	"context"
	cryptotls "crypto/tls"
	"fmt"
	"io"
	mathrand "math/rand"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/textproto"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"
	tls "github.com/refraction-networking/utls"
	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2/hpack"
)

type WarpReadCloser struct {
	Reader io.Reader
	Closer func() error
}

func (r *WarpReadCloser) Read(p []byte) (n int, err error) { return r.Reader.Read(p) }
func (r *WarpReadCloser) Close() error                     { return r.Closer() }

var encodings = []string{"gzip", "deflate", "br"}

// DecodeResponse decode Content-Encoding from HTTP header (gzip, deflate, br) encodings.
func DecodeResponse(res *http.Response) (*http.Response, error) {
	// In the order decompressed
	encoding := res.Header.Get("Content-Encoding")
	if encoding == "" {
		return res, nil
	}
	var (
		body = res.Body
		err  error
	)
	for _, encode := range strings.Split(encoding, ",") {
		switch strings.TrimSpace(encode) {
		case "deflate":
			body, err = zlib.NewReader(body)
		case "gzip":
			body, err = gzip.NewReader(body)
		case "br":
			body = &WarpReadCloser{brotli.NewReader(body), body.Close}
		default:
			err = fmt.Errorf("unsupported compression type %s", encode)
		}
		if err != nil {
			return nil, err
		}
		res.Body = body
		res.Header.Del("Content-Encoding")
		res.Header.Del("Content-Length")
		res.ContentLength = -1
		res.Uncompressed = true
	}
	return res, nil
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
	// requesting compression with an "Accept-Encoding: gzip, deflate, br"
	// request header when the Request contains no existing
	// Accept-Encoding value. If the Transport requests compression
	// it's transparently decoded in the Response.Body. However, if the user
	// explicitly requested compression it is not automatically
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
	t1  *http.Transport
	opt Options

	connPoolOnce  sync.Once
	connPoolOrDef ClientConnPool // non-nil version of ConnPool
}

// ConfigureTransports configures a net/http HTTP/1 Transport to use HTTP/2.
// It returns a new HTTP/2 Transport.
func ConfigureTransports(t1 *http.Transport, opts ...Options) *Transport {
	connPool := new(clientConnPool)
	t2 := &Transport{
		ConnPool: noDialClientConnPool{connPool},
		t1:       t1,
	}
	if len(opts) > 0 {
		t2.opt = opts[0]
	}
	connPool.t = t2
	return t2
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Accept-Encoding") == "" &&
		req.Header.Get("Range") == "" &&
		req.Method != "HEAD" &&
		!t.disableCompression() {
		req.Header["Accept-Encoding"] = encodings
	}
	res, err := t.roundTrip(req)
	if err != nil {
		return nil, err
	}
	if !t.disableCompression() {
		return DecodeResponse(res)
	}
	return res, nil
}

func (cc *ClientConn) downgrade(req *http.Request) (*http.Response, error) {
	defer cc.decrStreamReservations()
	if cc.idleTimer != nil {
		cc.idleTimer.Reset(cc.idleTimeout)
	}
	if err := req.Write(cc.tconn); err != nil {
		return nil, err
	}
	return http.ReadResponse(cc.br, req)
}

func (t *Transport) roundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		if t.t1 == nil {
			return http.DefaultTransport.RoundTrip(req)
		}
		return t.t1.RoundTrip(req)
	}

	addr := authorityAddr(req.URL.Scheme, req.URL.Host)
	for retry := 0; ; retry++ {
		cc, err := t.connPool().GetClientConn(req, addr)
		if err != nil {
			return nil, err
		}
		if cc.tlsState != nil && cc.tlsState.NegotiatedProtocol != NextProtoTLS {
			return cc.downgrade(req)
		}
		res, err := cc.RoundTrip(req)
		if err != nil && retry <= 6 {
			if req, err = shouldRetryRequest(req, err); err == nil {
				// After the first retry, do exponential backoff with 10% jitter.
				if retry == 0 {
					continue
				}
				backoff := float64(uint(1) << (uint(retry) - 1))
				backoff += backoff * (0.1 * mathrand.Float64())
				d := time.Second * time.Duration(backoff)
				timer := time.NewTimer(d)
				select {
				case <-timer.C:
					continue
				case <-req.Context().Done():
					timer.Stop()
					err = req.Context().Err()
				}
			}
		}
		if err != nil {
			return nil, err
		}
		return res, nil
	}
}

func (t *Transport) dialTLS(ctx context.Context, network, addr string, tlsCfg *tls.Config) (net.Conn, error) {
	if t.DialTLSContext != nil {
		return t.DialTLSContext(ctx, network, addr, tlsCfg)
	} else if t.DialTLS != nil {
		return t.DialTLS(network, addr, tlsCfg)
	}

	return t.dialTLSWithContext(ctx, network, addr, tlsCfg)
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
	cc := &ClientConn{
		t:                      t,
		tconn:                  c,
		readerDone:             make(chan struct{}),
		nextStreamID:           1,
		maxFrameSize:           16 << 10,                    // spec default
		initialWindowSize:      65535,                       // spec default
		maxConcurrentStreams:   initialMaxConcurrentStreams, // "infinite", per spec. Use a smaller value until we have received server settings.
		peerMaxHeaderTableSize: initialHeaderTableSize,
		peerMaxHeaderListSize:  0xffffffffffffffff, // "infinite", per spec. Use 2^64-1 instead.
		streams:                make(map[uint32]*clientStream),
		singleUse:              singleUse,
		wantSettingsAck:        true,
		pings:                  make(map[[8]byte]chan struct{}),
		reqHeaderMu:            make(chan struct{}, 1),
	}
	if d := t.idleConnTimeout(); d != 0 {
		cc.idleTimeout = d
		cc.idleTimer = time.AfterFunc(d, cc.onIdleTimeout)
	}

	cc.cond = sync.NewCond(&cc.mu)
	cc.flow.add(int32(initialWindowSize))

	// TODO: adjust this writer size to account for frame size +
	// MTU + crypto/tls record padding.
	cc.bw = bufio.NewWriter(stickyErrWriter{
		conn:    c,
		timeout: t.WriteByteTimeout,
		err:     &cc.werr,
	})
	cc.br = bufio.NewReader(c)
	cc.fr = NewFramer(cc.bw, cc.br)
	if t.CountError != nil {
		cc.fr.countError = t.CountError
	}

	if t.AllowHTTP {
		cc.nextStreamID = 3
	}

	if cs, ok := c.(connectionStater); ok {
		state := cs.ConnectionState()
		cc.tlsState = &state
		// if not HTTP/2
		if state.NegotiatedProtocol != NextProtoTLS {
			return cc, nil
		}
	}

	maxHeaderTableSize := t.maxDecoderHeaderTableSize()
	var settings []Setting
	if len(t.opt.Settings) == 0 {
		settings = []Setting{
			{ID: SettingEnablePush, Val: 0},
			{ID: SettingInitialWindowSize, Val: transportDefaultStreamFlow},
		}
		if max := t.maxFrameReadSize(); max != 0 {
			settings = append(settings, Setting{ID: SettingMaxFrameSize, Val: max})
		}
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

	cc.henc = hpack.NewEncoder(&cc.hbuf)
	cc.henc.SetMaxDynamicTableSizeLimit(t.maxEncoderHeaderTableSize())

	if size := t.maxFrameReadSize(); size != 0 {
		cc.fr.SetMaxReadFrameSize(t.maxFrameReadSize())
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

	go cc.readLoop()
	return cc, nil
}

// requires cc.wmu be held.
func (cc *ClientConn) encodeHeaders(req *http.Request, _ bool, trailers string, contentLength int64) ([]byte, error) {
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

	var path string
	if req.Method != "CONNECT" {
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

	// Check for any invalid headers and return an error before we
	// potentially pollute our hpack state. (We want to be able to
	// continue to reuse the hpack encoder for future requests)
	for k, vv := range req.Header {
		if !httpguts.ValidHeaderFieldName(k) {
			return nil, fmt.Errorf("invalid HTTP header name %q", k)
		}
		for _, v := range vv {
			if !httpguts.ValidHeaderFieldValue(v) {
				// Don't include the value in the error, because it may be sensitive.
				return nil, fmt.Errorf("invalid HTTP header value for header %q", k)
			}
		}
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
				default:
					continue
				}
			}
		} else {
			f(":authority", host)
			m := req.Method
			if m == "" {
				m = http.MethodGet
			}
			f(":method", m)
			if req.Method != "CONNECT" {
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

func (cc *ClientConn) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	cs := &clientStream{
		cc:                   cc,
		ctx:                  ctx,
		reqCancel:            req.Cancel,
		isHead:               req.Method == "HEAD",
		reqBody:              req.Body,
		reqBodyContentLength: actualContentLength(req),
		trace:                httptrace.ContextClientTrace(ctx),
		peerClosed:           make(chan struct{}),
		abort:                make(chan struct{}),
		respHeaderRecv:       make(chan struct{}),
		donec:                make(chan struct{}),
	}
	go cs.doRequest(req)

	waitDone := func() error {
		select {
		case <-cs.donec:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-cs.reqCancel:
			return errRequestCanceled
		}
	}

	handleResponseHeaders := func() (*http.Response, error) {
		res := cs.res
		if res.StatusCode > 299 {
			// On error or status code 3xx, 4xx, 5xx, etc abort any
			// ongoing write, assuming that the server doesn't care
			// about our request body. If the server replied with 1xx or
			// 2xx, however, then assume the server DOES potentially
			// want our body (e.g. full-duplex streaming:
			// golang.org/issue/13444). If it turns out the server
			// doesn't, they'll RST_STREAM us soon enough. This is a
			// heuristic to avoid adding knobs to Transport. Hopefully
			// we can keep it.
			cs.abortRequestBodyWrite()
		}
		res.Request = req
		// PATCH START
		if cc.tlsState != nil {
			res.TLS = &cryptotls.ConnectionState{
				Version:                     cc.tlsState.Version,
				HandshakeComplete:           cc.tlsState.HandshakeComplete,
				DidResume:                   cc.tlsState.DidResume,
				CipherSuite:                 cc.tlsState.CipherSuite,
				NegotiatedProtocol:          cc.tlsState.NegotiatedProtocol,
				NegotiatedProtocolIsMutual:  cc.tlsState.NegotiatedProtocolIsMutual,
				ServerName:                  cc.tlsState.ServerName,
				PeerCertificates:            cc.tlsState.PeerCertificates,
				VerifiedChains:              cc.tlsState.VerifiedChains,
				SignedCertificateTimestamps: cc.tlsState.SignedCertificateTimestamps,
				OCSPResponse:                cc.tlsState.OCSPResponse,
				TLSUnique:                   cc.tlsState.TLSUnique,
			}
		}
		// PATCH END
		if res.Body == noBody && actualContentLength(req) == 0 {
			// If there isn't a request or response body still being
			// written, then wait for the stream to be closed before
			// RoundTrip returns.
			if err := waitDone(); err != nil {
				return nil, err
			}
		}
		return res, nil
	}

	cancelRequest := func(cs *clientStream, err error) error {
		cs.cc.mu.Lock()
		bodyClosed := cs.reqBodyClosed
		cs.cc.mu.Unlock()
		// Wait for the request body to be closed.
		//
		// If nothing closed the body before now, abortStreamLocked
		// will have started a goroutine to close it.
		//
		// Closing the body before returning avoids a race condition
		// with net/http checking its readTrackingBody to see if the
		// body was read from or closed. See golang/go#60041.
		//
		// The body is closed in a separate goroutine without the
		// connection mutex held, but dropping the mutex before waiting
		// will keep us from holding it indefinitely if the body
		// close is slow for some reason.
		if bodyClosed != nil {
			<-bodyClosed
		}
		return err
	}

	for {
		select {
		case <-cs.respHeaderRecv:
			return handleResponseHeaders()
		case <-cs.abort:
			select {
			case <-cs.respHeaderRecv:
				// If both cs.respHeaderRecv and cs.abort are signaling,
				// pick respHeaderRecv. The server probably wrote the
				// response and immediately reset the stream.
				// golang.org/issue/49645
				return handleResponseHeaders()
			default:
				waitDone()
				return nil, cs.abortErr
			}
		case <-ctx.Done():
			err := ctx.Err()
			cs.abortStream(err)
			return nil, cancelRequest(cs, err)
		case <-cs.reqCancel:
			cs.abortStream(errRequestCanceled)
			return nil, cancelRequest(cs, errRequestCanceled)
		}
	}
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
