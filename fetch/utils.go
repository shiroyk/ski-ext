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

// DecodeReader decode Content-Encoding from HTTP header (gzip, deflate, br) encodings.
func DecodeReader(encoding string, reader io.Reader) (io.Reader, error) {
	// In the order decompressed
	var bodyReader = reader
	var err error
	for _, encode := range strings.Split(encoding, ",") {
		switch strings.TrimSpace(encode) {
		case "deflate":
			bodyReader, err = zlib.NewReader(reader)
		case "gzip":
			bodyReader, err = gzip.NewReader(reader)
		case "br":
			bodyReader = brotli.NewReader(reader)
		default:
			err = fmt.Errorf("unsupported compression type %s", encode)
		}
		if err != nil {
			return nil, err
		}
	}
	return bodyReader, nil
}
