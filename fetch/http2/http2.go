package http2

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"golang.org/x/net/http/httpguts"
)

const (
	// ClientPreface is the string that must be sent by new
	// connections from clients.
	ClientPreface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

	// NextProtoTLS is the NPN/ALPN protocol negotiated during
	// HTTP/2's TLS setup.
	NextProtoTLS = "h2"

	// https://httpwg.org/specs/rfc7540.html#SettingValues
	initialHeaderTableSize = 4096

	initialWindowSize = 65535 // 6.9.2 Initial Flow Control Window Size
)

var (
	clientPreface = []byte(ClientPreface)
)

// Setting is a setting parameter: which setting it is, and its value.
type Setting struct {
	// ID is which setting is being set.
	// See https://httpwg.org/specs/rfc7540.html#SettingFormat
	ID SettingID

	// Val is the value.
	Val uint32
}

func (s Setting) String() string {
	return fmt.Sprintf("[%v = %d]", s.ID, s.Val)
}

// Valid reports whether the setting is valid.
func (s Setting) Valid() error {
	// Limits and error codes from 6.5.2 Defined SETTINGS Parameters
	switch s.ID {
	case SettingEnablePush:
		if s.Val != 1 && s.Val != 0 {
			return ConnectionError(ErrCodeProtocol)
		}
	case SettingInitialWindowSize:
		if s.Val > 1<<31-1 {
			return ConnectionError(ErrCodeFlowControl)
		}
	case SettingMaxFrameSize:
		if s.Val < 16384 || s.Val > 1<<24-1 {
			return ConnectionError(ErrCodeProtocol)
		}
	}
	return nil
}

// A SettingID is an HTTP/2 setting as defined in
// https://httpwg.org/specs/rfc7540.html#iana-settings
type SettingID uint16

const (
	SettingHeaderTableSize      SettingID = 0x1
	SettingEnablePush           SettingID = 0x2
	SettingMaxConcurrentStreams SettingID = 0x3
	SettingInitialWindowSize    SettingID = 0x4
	SettingMaxFrameSize         SettingID = 0x5
	SettingMaxHeaderListSize    SettingID = 0x6
)

var settingName = map[SettingID]string{
	SettingHeaderTableSize:      "HEADER_TABLE_SIZE",
	SettingEnablePush:           "ENABLE_PUSH",
	SettingMaxConcurrentStreams: "MAX_CONCURRENT_STREAMS",
	SettingInitialWindowSize:    "INITIAL_WINDOW_SIZE",
	SettingMaxFrameSize:         "MAX_FRAME_SIZE",
	SettingMaxHeaderListSize:    "MAX_HEADER_LIST_SIZE",
}

func (s SettingID) String() string {
	if v, ok := settingName[s]; ok {
		return v
	}
	return fmt.Sprintf("UNKNOWN_SETTING_%d", uint16(s))
}

// validWireHeaderFieldName reports whether v is a valid header field
// name (key). See httpguts.ValidHeaderName for the base rules.
//
// Further, http2 says:
//
//	"Just as in HTTP/1.x, header field names are strings of ASCII
//	characters that are compared in a case-insensitive
//	fashion. However, header field names MUST be converted to
//	lowercase prior to their encoding in HTTP/2. "
func validWireHeaderFieldName(v string) bool {
	if len(v) == 0 {
		return false
	}
	for _, r := range v {
		if !httpguts.IsTokenRune(r) {
			return false
		}
		if 'A' <= r && r <= 'Z' {
			return false
		}
	}
	return true
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
	New: func() interface{} { return new(headerSorter) },
}

func sortedKeyValues(header http.Header) (kvs []keyValues) {
	sorter := headerSorterPool.Get().(*headerSorter)
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
