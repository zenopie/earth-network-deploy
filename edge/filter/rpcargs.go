package filter

// CometBFT v0.38 argument decoding, reproduced from
// rpc/jsonrpc/server/http_uri_handler.go (URI GETs) and http_json_handler.go
// with libs/json (JSON-RPC params), for the argument types of the routes this
// proxy serves. The proxy decodes each argument exactly once, the way the
// node would, checks the decoded value, and then forwards a request it wrote
// itself in one unambiguous encoding (EncodeURI / EncodeJSON). So the node
// never parses anything a client wrote: a double decoding (`0x` hex, JSON
// `\u` escapes, `%74rue`) has nothing left to hide in.

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type ArgKind int

const (
	KString   ArgKind = iota // string
	KHexBytes                // libs/bytes.HexBytes: JSON "ABCD", URI 0x.. or "raw"
	KBytes                   // []byte, types.Tx: JSON base64, URI 0x.. or "raw"
	KInt64                   // int64
	KInt64Ptr                // *int64
	KIntPtr                  // *int
	KUint                    // uint
	KBool                    // bool
)

// ArgVal is one decoded argument. set is false when the node would use the
// zero value (absent, empty, or null); a nil pointer and an absent argument
// are the same to every route.
type ArgVal struct {
	set bool
	s   string // KString
	b   []byte // KHexBytes, KBytes
	i   int64  // the integer kinds
	t   bool   // KBool
}

// reInt is http_uri_handler.go's.
var reInt = regexp.MustCompile(`^-?[0-9]+$`)

var errArg = errors.New("bad argument")

func isIntKind(k ArgKind) bool {
	return k == KInt64 || k == KInt64Ptr || k == KIntPtr || k == KUint
}

// DecodeURIArg is httpParamsToArgs for one non-empty query value:
// _nonJSONStringToArg first, then jsonStringToArg.
func DecodeURIArg(k ArgKind, arg string) (ArgVal, error) {
	isIntString := reInt.MatchString(arg)
	isQuotedString := strings.HasPrefix(arg, `"`) && strings.HasSuffix(arg, `"`)
	isHexString := strings.HasPrefix(strings.ToLower(arg), "0x")
	expectingString := k == KString
	expectingBytes := k == KHexBytes || k == KBytes

	if isIntString && isIntKind(k) {
		return DecodeJSONArg(k, []byte(`"`+arg+`"`))
	}
	if isHexString {
		if !expectingString && !expectingBytes {
			return ArgVal{}, fmt.Errorf("%w: hex string where a %s is expected", errArg, kindName(k))
		}
		v, err := hex.DecodeString(arg[2:])
		if err != nil {
			return ArgVal{}, fmt.Errorf("%w: %v", errArg, err)
		}
		if expectingString {
			return ArgVal{set: true, s: string(v)}, nil
		}
		return ArgVal{set: true, b: v}, nil
	}
	if isQuotedString && expectingBytes {
		// cmtjson.Unmarshal into a string: the JSON string's own bytes.
		var s string
		if err := json.Unmarshal([]byte(arg), &s); err != nil {
			return ArgVal{}, fmt.Errorf("%w: %v", errArg, err)
		}
		return ArgVal{set: true, b: []byte(s)}, nil
	}
	return DecodeJSONArg(k, []byte(arg))
}

// DecodeJSONArg is cmtjson.Unmarshal into the argument's Go type
// (libs/json/decoder.go decodeReflect).
func DecodeJSONArg(k ArgKind, bz []byte) (ArgVal, error) {
	if len(bz) == 0 {
		return ArgVal{}, fmt.Errorf("%w: empty", errArg)
	}
	if bytes.Equal(bz, []byte("null")) {
		return ArgVal{}, nil
	}
	switch k {
	case KString:
		var s string
		if err := json.Unmarshal(bz, &s); err != nil {
			return ArgVal{}, fmt.Errorf("%w: %v", errArg, err)
		}
		return ArgVal{set: true, s: s}, nil
	case KHexBytes:
		// HexBytes.UnmarshalJSON: the raw bytes between the quotes, hex,
		// with no JSON unescaping.
		if len(bz) < 2 || bz[0] != '"' || bz[len(bz)-1] != '"' {
			return ArgVal{}, fmt.Errorf("%w: invalid hex string", errArg)
		}
		v, err := hex.DecodeString(string(bz[1 : len(bz)-1]))
		if err != nil {
			return ArgVal{}, fmt.Errorf("%w: %v", errArg, err)
		}
		return ArgVal{set: true, b: v}, nil
	case KBytes:
		var v []byte
		if err := json.Unmarshal(bz, &v); err != nil {
			return ArgVal{}, fmt.Errorf("%w: %v", errArg, err)
		}
		return ArgVal{set: len(v) > 0, b: v}, nil
	case KInt64, KInt64Ptr, KIntPtr, KUint:
		// "For 64-bit integers, unwrap expected string": int and uint too.
		// A lone `"` would make cmtjson slice bz[1:0] and panic; refuse it.
		if len(bz) < 2 || bz[0] != '"' || bz[len(bz)-1] != '"' {
			return ArgVal{}, fmt.Errorf("%w: integer must be a JSON string", errArg)
		}
		inner := bz[1 : len(bz)-1]
		var i int64
		switch k {
		case KUint:
			var u uint64
			if err := json.Unmarshal(inner, &u); err != nil {
				return ArgVal{}, fmt.Errorf("%w: %v", errArg, err)
			}
			if u > 1<<62 {
				return ArgVal{}, fmt.Errorf("%w: out of range", errArg)
			}
			i = int64(u)
		case KIntPtr:
			var n int // the node's *int; 64-bit on every platform it runs on
			if err := json.Unmarshal(inner, &n); err != nil {
				return ArgVal{}, fmt.Errorf("%w: %v", errArg, err)
			}
			i = int64(n)
		default:
			if err := json.Unmarshal(inner, &i); err != nil {
				return ArgVal{}, fmt.Errorf("%w: %v", errArg, err)
			}
		}
		// A non-pointer zero is the node's default; a pointer to zero is an
		// explicit value (getHeight refuses height 0, it does not mean latest).
		return ArgVal{set: k == KInt64Ptr || k == KIntPtr || i != 0, i: i}, nil
	case KBool:
		var t bool
		if err := json.Unmarshal(bz, &t); err != nil {
			return ArgVal{}, fmt.Errorf("%w: %v", errArg, err)
		}
		return ArgVal{set: t, t: t}, nil
	}
	return ArgVal{}, fmt.Errorf("%w: unknown kind", errArg)
}

func kindName(k ArgKind) string {
	switch k {
	case KString:
		return "string"
	case KHexBytes, KBytes:
		return "bytes"
	case KBool:
		return "bool"
	}
	return "integer"
}

// EncodeURI writes v the one way this proxy forwards it in a GET: strings and
// bytes as 0x hex (the node hex-decodes any 0x value for these types, and hex
// has no escapes), integers in decimal, bools as true. Only set values are
// written.
func EncodeURI(k ArgKind, v ArgVal) string {
	switch k {
	case KString:
		return "0x" + hex.EncodeToString([]byte(v.s))
	case KHexBytes, KBytes:
		return "0x" + hex.EncodeToString(v.b)
	case KBool:
		return strconv.FormatBool(v.t)
	}
	return strconv.FormatInt(v.i, 10)
}

// EncodeJSON writes v as libs/json would marshal the Go value, which is what
// the node's JSON-RPC handler decodes.
func EncodeJSON(k ArgKind, v ArgVal) json.RawMessage {
	switch k {
	case KString:
		b, _ := json.Marshal(v.s)
		return b
	case KHexBytes:
		return json.RawMessage(`"` + strings.ToUpper(hex.EncodeToString(v.b)) + `"`)
	case KBytes:
		return json.RawMessage(`"` + base64.StdEncoding.EncodeToString(v.b) + `"`)
	case KBool:
		return json.RawMessage(strconv.FormatBool(v.t))
	}
	return json.RawMessage(`"` + strconv.FormatInt(v.i, 10) + `"`)
}
