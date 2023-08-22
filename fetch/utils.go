package fetch

import (
	"compress/gzip"
	"compress/zlib"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/shiroyk/cloudcat"
)

// DoString do request and read response body as string.
func DoString(fetch cloudcat.Fetch, req *http.Request) (string, error) {
	body, err := DoByte(fetch, req)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// DoByte do request and read response body.
func DoByte(fetch cloudcat.Fetch, req *http.Request) ([]byte, error) {
	res, err := fetch.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}

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
			body = io.NopCloser(brotli.NewReader(body))
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
