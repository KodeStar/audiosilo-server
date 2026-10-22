package discovery

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// readLimited reads at most limit bytes, errTooLarge past them.
func readLimited(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errTooLarge
	}
	return data, nil
}

// oneJSONDocument reports whether body is exactly one JSON value, then only
// whitespace to its end.
func oneJSONDocument(body []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	var v json.RawMessage
	if dec.Decode(&v) != nil {
		return false
	}
	_, err := dec.Token()
	return errors.Is(err, io.EOF)
}
