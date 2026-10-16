// Package strictjson decodes bytes the gateway did not write. See ../README.md.
package strictjson

import (
	stdjson "encoding/json"
	"errors"

	json "github.com/goccy/go-json"
)

// ErrInvalid is what Unmarshal answers for bytes that are not one valid JSON value.
var ErrInvalid = errors.New("invalid JSON")

// Unmarshal hands goccy only what the standard library accepts as valid JSON, so a malformed payload is refused before it reaches a decoder that can read past it.
func Unmarshal(data []byte, target any) error {
	if !stdjson.Valid(data) {
		return ErrInvalid
	}
	return json.Unmarshal(data, target)
}
