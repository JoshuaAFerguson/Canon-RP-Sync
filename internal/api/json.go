package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// decodeJSON reads a JSON request body, rejecting oversized or malformed input
// with a message the caller can show a user.
func decodeJSON(r *http.Request, out any, limit int64) error {
	if limit <= 0 {
		limit = 1 << 20
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, limit))
	dec.DisallowUnknownFields()

	if err := dec.Decode(out); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("request body is empty")
		}
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
