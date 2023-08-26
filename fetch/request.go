package fetch

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"reflect"
	"strings"
	"text/template"
	_ "unsafe"

	"github.com/shiroyk/cloudcat"
)

// NewRequest returns a new RequestConfig given a method, URL, optional body, optional headers.
func NewRequest(method, u string, body any, headers map[string]string) (*http.Request, error) {
	var reqBody io.Reader = http.NoBody
	if body != nil {
		// Convert body to io.Reader
		switch data := body.(type) {
		default:
			kind := reflect.ValueOf(body).Kind()
			if kind != reflect.Struct && kind != reflect.Map {
				break
			}

			j, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			if headers == nil {
				headers = make(map[string]string)
			}
			if _, ok := headers["Content-Type"]; !ok {
				headers["Content-Type"] = "application/json"
			}
			reqBody = bytes.NewReader(j)
		case io.Reader:
			reqBody = data
		case string:
			reqBody = bytes.NewBufferString(data)
		case []byte:
			reqBody = bytes.NewBuffer(data)
		}
	}

	req, err := http.NewRequest(method, u, reqBody)
	if err != nil {
		return nil, err
	}

	// set headers
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	return req, nil
}

// NewTemplateRequest returns a new RequestConfig given a http template with argument.
func NewTemplateRequest(funcs template.FuncMap, tpl string, arg any) (*http.Request, error) {
	tmp, err := template.New("url").Funcs(funcs).Parse(tpl)
	if err != nil {
		return nil, err
	}

	buf := new(bytes.Buffer)
	if err = tmp.Execute(buf, arg); err != nil {
		return nil, err
	}
	// https://github.com/golang/go/issues/24963
	novalue := strings.ReplaceAll(buf.String(), "<no value>", "")

	tp := newTextprotoReader(bufio.NewReader(strings.NewReader(novalue)))

	// First line: GET /index.html HTTP/1.0
	var s string
	if s, err = tp.ReadLine(); err != nil {
		return nil, err
	}
	defer func() {
		putTextprotoReader(tp)
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
	}()

	req := new(http.Request)
	var rawURI string

	req.Method, rawURI, req.Proto = parseRequestLine(s)
	if !validMethod(req.Method) {
		return nil, fmt.Errorf("invalid method %s", req.Method)
	}
	var ok bool
	if req.ProtoMajor, req.ProtoMinor, ok = http.ParseHTTPVersion(req.Proto); !ok {
		return nil, fmt.Errorf("malformed HTTP version %s", req.Proto)
	}

	if req.URL, err = url.ParseRequestURI(rawURI); err != nil {
		return nil, err
	}

	// Subsequent lines: Key: value.
	mimeHeader, err := tp.ReadMIMEHeader()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	req.Header = http.Header(mimeHeader)
	if len(req.Header["Host"]) > 1 {
		return nil, fmt.Errorf("too many Host headers")
	}

	// RFC 7230, section 5.3: Must treat
	//	GET /index.html HTTP/1.1
	//	Host: www.google.com
	// and
	//	GET http://www.google.com/index.html HTTP/1.1
	//	Host: doesntmatter
	// the same. In the second case, any Host line is ignored.
	req.Host = req.URL.Host

	fixPragmaCacheControl(req.Header)

	req.Close = shouldClose(req.ProtoMajor, req.ProtoMinor, req.Header)

	if req.Method != http.MethodHead || req.Body == nil {
		// Read body and fix content-length
		body := new(bytes.Buffer)
		if _, err = tp.R.WriteTo(body); err != nil {
			return nil, err
		}
		if body.Len() == 0 {
			req.Body = http.NoBody
		} else {
			req.ContentLength = int64(body.Len())
			req.Body = io.NopCloser(body)
		}
	}

	return req, nil
}

// DefaultTemplateFuncMap The default template function map
func DefaultTemplateFuncMap(cache cloudcat.Cache) template.FuncMap {
	return template.FuncMap{
		"get": func(key string) (ret string) {
			if v, ok := cache.Get(key); ok {
				return string(v)
			}
			return
		},
		"set": func(key string, value string) (ret string) {
			cache.Set(key, []byte(value))
			return
		},
	}
}

// parseRequestLine parses "GET /foo HTTP/1.1" into its three parts.
// Default proto HTTP/1.1.
func parseRequestLine(line string) (method, requestURI, proto string) {
	method, rest, ok1 := strings.Cut(line, " ")
	requestURI, proto, ok2 := strings.Cut(rest, " ")
	if !ok1 {
		// default GET request
		return "GET", line, "HTTP/1.1"
	}
	if !ok2 {
		return method, requestURI, "HTTP/1.1"
	}
	return method, requestURI, proto
}

//go:linkname newTextprotoReader net/http.newTextprotoReader
func newTextprotoReader(br *bufio.Reader) *textproto.Reader

//go:linkname putTextprotoReader net/http.putTextprotoReader
func putTextprotoReader(r *textproto.Reader)

//go:linkname validMethod net/http.validMethod
func validMethod(method string) bool

//go:linkname shouldClose net/http.shouldClose
func shouldClose(major, minor int, header http.Header) bool

//go:linkname fixPragmaCacheControl net/http.fixPragmaCacheControl
func fixPragmaCacheControl(header http.Header)
