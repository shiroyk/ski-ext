package fetch

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"
)

// A Cache interface is used to store bytes.
type Cache interface {
	Get(ctx context.Context, key string) ([]byte, error)
	Set(ctx context.Context, key string, value []byte, timeout time.Duration) error
	Del(ctx context.Context, key string) error
}

// (This implementation code copyright geziyor authors: https://github.com/geziyor/geziyor)

// Policy has no awareness of any HTTP Cache-Control directives.
type Policy string

const (
	// This policy has no awareness of any HTTP Cache-Control directives.
	// Every request and its corresponding response are cached.
	// When the same request is seen again, the response is returned without transferring anything from the Internet.

	// Dummy policy is useful for testing spiders faster (without having to wait for downloads every time)
	// and for trying your spider offline, when an Internet connection is not available.
	// The goal is to be able to “replay” a spider run exactly as it ran before.
	Dummy Policy = "dummy"

	// RFC2616 This policy provides a RFC2616 compliant HTTP cache, i.e. with HTTP Cache-Control awareness,
	// aimed at production and used in continuous runs to avoid downloading unmodified data
	// (to save bandwidth and speed up crawls).
	RFC2616 Policy = "rfc2616"

	// XFromCache is the header added to responses that are returned from the cache
	XFromCache = "X-From-Cache"
)

const (
	stale = iota
	fresh
	transparent
)

// CacheTransport is an implementation of http.RoundTripper that will return values from a cache
// where possible (avoiding a network request) and will additionally add validators (etag/if-modified-since)
// to repeated requests allowing servers to return 304 / Not Modified
type CacheTransport struct {
	Policy Policy
	// The RoundTripper interface actually used to make requests
	// If nil, http.DefaultTransport is used
	Transport http.RoundTripper
	Cache     Cache
	// If true, responses returned from the cache will be given an extra header, X-From-Cache
	MarkCachedResponses bool
}

// cacheKey returns the cache key for req.
func cacheKey(req *http.Request) string {
	if req.Method == http.MethodGet {
		return req.URL.String()
	}
	return req.Method + " " + req.URL.String()
}

// cachedResponse returns the cached http.Response for req if present, and nil
// otherwise.
func cachedResponse(c Cache, req *http.Request) (resp *http.Response, err error) {
	cachedVal, err := c.Get(req.Context(), cacheKey(req))
	if err != nil {
		return nil, err
	}

	b := bytes.NewBuffer(cachedVal)
	return http.ReadResponse(bufio.NewReader(b), req)
}

// NewCacheTransport returns new CacheTransport with the
// provided Cache implementation and MarkCachedResponses set to true
func NewCacheTransport(c Cache) *CacheTransport {
	return &CacheTransport{
		Policy:              RFC2616,
		Cache:               c,
		MarkCachedResponses: true,
	}
}

// varyMatches will return false unless all the cached values for the headers listed in Vary
// match the new request
func varyMatches(cachedResp *http.Response, req *http.Request) bool {
	for _, header := range headerAllCommaSepValues(cachedResp.Header, "vary") {
		header = http.CanonicalHeaderKey(header)
		if header != "" && req.Header.Get(header) != cachedResp.Header.Get("X-Varied-"+header) {
			return false
		}
	}
	return true
}

// RoundTrip is a wrapper for caching requests.
// If there is a fresh Response already in cache, then it will be returned without connecting to
// the server.
func (t *CacheTransport) RoundTrip(req *http.Request) (resp *http.Response, err error) {
	if t.Policy == Dummy {
		return t.RoundTripDummy(req)
	}
	return t.RoundTripRFC2616(req)
}

// RoundTripDummy has no awareness of any HTTP Cache-Control directives.
// Every request and its corresponding response are cached.
// When the same request is seen again, the response is returned without transferring anything from the Internet.
func (t *CacheTransport) RoundTripDummy(req *http.Request) (resp *http.Response, err error) {
	cacheKey := cacheKey(req)
	cacheable := (req.Method == "GET" || req.Method == "HEAD") && req.Header.Get("range") == ""
	var cachedResp *http.Response
	if cacheable {
		cachedResp, err = cachedResponse(t.Cache, req)
	} else {
		// Need to invalidate an existing value
		_ = t.Cache.Del(req.Context(), cacheKey)
	}

	transport := t.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}

	if cacheable && cachedResp != nil && err == nil {
		if t.MarkCachedResponses {
			cachedResp.Header.Set(XFromCache, "1")
		}
		return cachedResp, nil
	}
	resp, err = transport.RoundTrip(req)
	if err != nil {
		return nil, err
	}

	if cacheable {
		respBytes, err := httputil.DumpResponse(resp, true)
		if err == nil {
			_ = t.Cache.Set(req.Context(), cacheKey, respBytes, 0)
		}
	} else {
		_ = t.Cache.Del(req.Context(), cacheKey)
	}
	return resp, nil
}

// RoundTripRFC2616 provides a RFC2616 compliant HTTP cache, i.e. with HTTP Cache-Control awareness,
// aimed at production and used in continuous runs to avoid downloading unmodified data
// (to save bandwidth and speed up crawls).
//
// If there is a stale Response, then any validators it contains will be set on the new request
// to give the server a chance to respond with NotModified. If this happens, then the cached Response
// will be returned.
//
//nolint:funlen,gocognit,cyclop
func (t *CacheTransport) RoundTripRFC2616(req *http.Request) (resp *http.Response, err error) {
	cacheKey := cacheKey(req)
	cacheable := (req.Method == "GET" || req.Method == "HEAD") && req.Header.Get("range") == ""
	var cachedResp *http.Response
	if cacheable {
		cachedResp, err = cachedResponse(t.Cache, req)
	} else {
		// Need to invalidate an existing value
		_ = t.Cache.Del(req.Context(), cacheKey)
	}

	transport := t.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}

	if cacheable && cachedResp != nil && err == nil { //nolint:nestif
		if t.MarkCachedResponses {
			cachedResp.Header.Set(XFromCache, "1")
		}

		if varyMatches(cachedResp, req) {
			// Can only use cached value if the new request doesn't Vary significantly
			freshness := getFreshness(cachedResp.Header, req.Header)
			if freshness == fresh {
				return cachedResp, nil
			}

			if freshness == stale {
				var req2 *http.Request
				// Add validators if caller hasn't already done so
				etag := cachedResp.Header.Get("etag")
				if etag != "" && req.Header.Get("etag") == "" {
					req2 = cloneRequest(req)
					req2.Header.Set("if-none-match", etag)
				}
				lastModified := cachedResp.Header.Get("last-modified")
				if lastModified != "" && req.Header.Get("last-modified") == "" {
					if req2 == nil {
						req2 = cloneRequest(req)
					}
					req2.Header.Set("if-modified-since", lastModified)
				}
				if req2 != nil {
					req = req2
				}
			}
		}

		resp, err = transport.RoundTrip(req)
		switch {
		case err == nil && req.Method == "GET" && resp.StatusCode == http.StatusNotModified:
			endToEndHeaders := getEndToEndHeaders(resp.Header)
			for _, header := range endToEndHeaders {
				cachedResp.Header[header] = resp.Header[header]
			}

			if err = resp.Body.Close(); err != nil {
				return nil, err
			}
			resp = cachedResp
		case (err != nil || resp.StatusCode >= 500) &&
			req.Method == "GET" && canStaleOnError(cachedResp.Header, req.Header):
			if resp != nil && resp.Body != nil {
				if err = resp.Body.Close(); err != nil {
					return nil, err
				}
			}
			return cachedResp, nil
		default:
			if err != nil || resp.StatusCode != http.StatusOK {
				_ = t.Cache.Del(req.Context(), cacheKey)
			}
			if err != nil {
				return nil, err
			}
		}
	} else {
		reqCacheControl := parseCacheControl(req.Header)
		if _, ok := reqCacheControl["only-if-cached"]; ok {
			resp = newGatewayTimeoutResponse(req)
		} else {
			resp, err = transport.RoundTrip(req)
			if err != nil {
				return nil, err
			}
		}
	}

	if cacheable && canStore(parseCacheControl(req.Header), parseCacheControl(resp.Header)) {
		for _, varyKey := range headerAllCommaSepValues(resp.Header, "vary") {
			varyKey = http.CanonicalHeaderKey(varyKey)
			fakeHeader := "X-Varied-" + varyKey
			reqValue := req.Header.Get(varyKey)
			if reqValue != "" {
				resp.Header.Set(fakeHeader, reqValue)
			}
		}
		switch req.Method {
		case http.MethodGet:
			// Delay caching until EOF is reached.
			resp.Body = &cachingReadCloser{
				R: resp.Body,
				OnEOF: func(r io.Reader) {
					resp := *resp
					resp.Body = io.NopCloser(r)
					respBytes, err := httputil.DumpResponse(&resp, true)
					if err == nil {
						_ = t.Cache.Set(req.Context(), cacheKey, respBytes, 0)
					}
				},
			}
		default:
			respBytes, err := httputil.DumpResponse(resp, true)
			if err == nil {
				_ = t.Cache.Set(req.Context(), cacheKey, respBytes, 0)
			}
		}
	} else {
		_ = t.Cache.Del(req.Context(), cacheKey)
	}
	return resp, nil
}

// ErrNoDateHeader indicates that the HTTP headers contained no Date header.
var ErrNoDateHeader = errors.New("no Date header")

// parserDate parses and returns the value of the Date header.
func parserDate(respHeaders http.Header) (date time.Time, err error) {
	dateHeader := respHeaders.Get("date")
	if dateHeader == "" {
		err = ErrNoDateHeader
		return
	}

	return time.Parse(time.RFC1123, dateHeader)
}

type realClock struct{}

func (c *realClock) since(d time.Time) time.Duration {
	return time.Since(d)
}

type timer interface {
	since(d time.Time) time.Duration
}

var clock timer = &realClock{}

// getFreshness will return one of fresh/stale/transparent based on the cache-control
// values of the request and the response
//
// fresh indicates the response can be returned
// stale indicates that the response needs validating before it is returned
// transparent indicates the response should not be used to fulfil the request
//
// Because this is only a private cache, 'public' and 'private' in cache-control aren't
// significant. Similarly, max-age isn't used.
func getFreshness(respHeaders, reqHeaders http.Header) (freshness int) {
	respCacheControl := parseCacheControl(respHeaders)
	reqCacheControl := parseCacheControl(reqHeaders)
	if _, ok := reqCacheControl["no-cache"]; ok {
		return transparent
	}
	if _, ok := respCacheControl["no-cache"]; ok {
		return stale
	}
	if _, ok := reqCacheControl["only-if-cached"]; ok {
		return fresh
	}

	date, err := parserDate(respHeaders)
	if err != nil {
		return stale
	}
	currentAge := clock.since(date)

	var lifetime time.Duration
	var zeroDuration time.Duration

	// If a response includes both an Expires header and a max-age directive,
	// the max-age directive overrides the Expires header, even if the Expires header is more restrictive.
	if maxAge, ok := respCacheControl["max-age"]; ok { //nolint:nestif
		lifetime, err = time.ParseDuration(maxAge + "s")
		if err != nil {
			lifetime = zeroDuration
		}
	} else {
		expiresHeader := respHeaders.Get("Expires")
		if expiresHeader != "" {
			expires, err := time.Parse(time.RFC1123, expiresHeader) //nolint:govet
			if err != nil {
				lifetime = zeroDuration
			} else {
				lifetime = expires.Sub(date)
			}
		}
	}

	if maxAge, ok := reqCacheControl["max-age"]; ok {
		// the client is willing to accept a response whose age is no greater than the specified time in seconds
		lifetime, err = time.ParseDuration(maxAge + "s")
		if err != nil {
			lifetime = zeroDuration
		}
	}
	if minfresh, ok := reqCacheControl["min-fresh"]; ok {
		//  the client wants a response that will still be fresh for at least the specified number of seconds.
		minfreshDuration, err := time.ParseDuration(minfresh + "s")
		if err == nil {
			currentAge += minfreshDuration
		}
	}

	if maxstale, ok := reqCacheControl["max-stale"]; ok {
		// Indicates that the client is willing to accept a response that has exceeded its expiration time.
		// If max-stale is assigned a value, then the client is willing to accept a response that has exceeded
		// its expiration time by no more than the specified number of seconds.
		// If no value is assigned to max-stale, then the client is willing to accept a stale response of any age.
		//
		// Responses served only because of a max-stale value are supposed to have a Warning header added to them,
		// but that seems like a  hassle, and is it actually useful? If so, then there needs to be a different
		// return-value available here.
		if maxstale == "" {
			return fresh
		}
		maxstaleDuration, err := time.ParseDuration(maxstale + "s")
		if err == nil {
			currentAge -= maxstaleDuration
		}
	}

	if lifetime > currentAge {
		return fresh
	}

	return stale
}

// Returns true if either the request or the response includes the stale-if-error
// cache control extension: https://tools.ietf.org/html/rfc5861
func canStaleOnError(respHeaders, reqHeaders http.Header) bool {
	respCacheControl := parseCacheControl(respHeaders)
	reqCacheControl := parseCacheControl(reqHeaders)

	var err error
	lifetime := time.Duration(-1)

	if staleMaxAge, ok := respCacheControl["stale-if-error"]; ok {
		if staleMaxAge != "" {
			lifetime, err = time.ParseDuration(staleMaxAge + "s")
			if err != nil {
				return false
			}
		} else {
			return true
		}
	}
	if staleMaxAge, ok := reqCacheControl["stale-if-error"]; ok {
		if staleMaxAge != "" {
			lifetime, err = time.ParseDuration(staleMaxAge + "s")
			if err != nil {
				return false
			}
		} else {
			return true
		}
	}

	if lifetime >= 0 {
		date, err := parserDate(respHeaders)
		if err != nil {
			return false
		}
		currentAge := clock.since(date)
		if lifetime > currentAge {
			return true
		}
	}

	return false
}

func getEndToEndHeaders(respHeaders http.Header) []string {
	// These headers are always hop-by-hop
	hopByHopHeaders := map[string]struct{}{
		"Connection":          {},
		"Keep-Alive":          {},
		"Proxy-Authenticate":  {},
		"Proxy-Authorization": {},
		"Te":                  {},
		"Trailers":            {},
		"Transfer-Encoding":   {},
		"Upgrade":             {},
	}

	for _, extra := range strings.Split(respHeaders.Get("connection"), ",") {
		// any header listed in connection, if present, is also considered hop-by-hop
		if strings.Trim(extra, " ") != "" {
			hopByHopHeaders[http.CanonicalHeaderKey(extra)] = struct{}{}
		}
	}
	var endToEndHeaders []string
	for respHeader := range respHeaders {
		if _, ok := hopByHopHeaders[respHeader]; !ok {
			endToEndHeaders = append(endToEndHeaders, respHeader)
		}
	}
	return endToEndHeaders
}

func canStore(reqCacheControl, respCacheControl cacheControl) (canStore bool) {
	if _, ok := respCacheControl["no-store"]; ok {
		return false
	}
	if _, ok := reqCacheControl["no-store"]; ok {
		return false
	}
	return true
}

func newGatewayTimeoutResponse(req *http.Request) *http.Response {
	var braw bytes.Buffer
	braw.WriteString("HTTP/1.1 504 Gateway Timeout\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(&braw), req)
	if err != nil {
		panic(err)
	}
	return resp
}

// cloneRequest returns a clone of the provided *http.Request.
// The clone is a shallow copy of the struct and its Header map.
// (This function copyright goauth2 authors: https://code.google.com/p/goauth2)
func cloneRequest(r *http.Request) *http.Request {
	// shallow copy of the struct
	r2 := new(http.Request)
	*r2 = *r
	// deep copy of the Header
	r2.Header = make(http.Header)
	for k, s := range r.Header {
		r2.Header[k] = s
	}
	return r2
}

type cacheControl map[string]string

func parseCacheControl(headers http.Header) cacheControl {
	cc := cacheControl{}
	ccHeader := headers.Get("Cache-Control")
	for _, part := range strings.Split(ccHeader, ",") {
		part = strings.Trim(part, " ")
		if part == "" {
			continue
		}
		if strings.ContainsRune(part, '=') {
			keyval := strings.Split(part, "=")
			cc[strings.Trim(keyval[0], " ")] = strings.Trim(keyval[1], ",")
		} else {
			cc[part] = ""
		}
	}
	return cc
}

// headerAllCommaSepValues returns all comma-separated values (each
// with whitespace trimmed) for header name in headers. According to
// Section 4.2 of the HTTP/1.1 spec
// (http://www.w3.org/Protocols/rfc2616/rfc2616-sec4.html#sec4.2),
// values from multiple occurrences of a header should be concatenated, if
// the header's value is a comma-separated list.
func headerAllCommaSepValues(headers http.Header, name string) []string {
	var vals []string
	for _, val := range headers[http.CanonicalHeaderKey(name)] {
		fields := strings.Split(val, ",")
		for i, f := range fields {
			fields[i] = strings.TrimSpace(f)
		}
		vals = append(vals, fields...)
	}
	return vals
}

// cachingReadCloser is a wrapper around ReadCloser R that calls OnEOF
// handler with a full copy of the content read from R when EOF is
// reached.
type cachingReadCloser struct {
	// Underlying ReadCloser.
	R io.ReadCloser
	// OnEOF is called with a copy of the content of R when EOF is reached.
	OnEOF func(io.Reader)

	buf bytes.Buffer // buf stores a copy of the content of R.
}

// Read reads the next len(p) bytes from R or until R is drained. The
// return value n is the number of bytes read. If R has no data to
// return, err is io.EOF and OnEOF is called with a full copy of what
// has been read so far.
func (r *cachingReadCloser) Read(p []byte) (n int, err error) {
	n, err = r.R.Read(p)
	r.buf.Write(p[:n])
	if errors.Is(err, io.EOF) {
		r.OnEOF(bytes.NewReader(r.buf.Bytes()))
	}
	return n, err
}

// Close the io.ReadCloser
func (r *cachingReadCloser) Close() error {
	return r.R.Close()
}
