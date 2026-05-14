package dashboard

import (
	"encoding/json"
	"io"
	"testing"
)

// decodeEnvelopeData unwraps `{data: T, meta}` into target. Tests use this
// instead of decoding the raw body so each call also exercises the
// canonical envelope contract end-to-end.
func decodeEnvelopeData(t *testing.T, body io.Reader, target any) {
	t.Helper()
	var env struct {
		Data json.RawMessage `json:"data"`
		Meta map[string]any  `json:"meta"`
	}
	if err := json.NewDecoder(body).Decode(&env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if len(env.Data) == 0 {
		t.Fatalf("envelope has no data field; raw body decoded as %+v", env)
	}
	if err := json.Unmarshal(env.Data, target); err != nil {
		t.Fatalf("decode envelope data: %v", err)
	}
}
