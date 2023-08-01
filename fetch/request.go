package fetch

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
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

	for k, v := range DefaultHeaders {
		if _, ok := req.Header[k]; !ok {
			req.Header.Set(k, v)
		}
	}

	return req, nil
}
