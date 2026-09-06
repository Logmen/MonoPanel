package store

import (
	"encoding/json"
	"time"
)

// Time is time.Time that marshals as RFC 3339 and tolerates the zero value.
type Time time.Time

// MarshalJSON renders RFC 3339 UTC or null for the zero time.
func (t Time) MarshalJSON() ([]byte, error) {
	tt := time.Time(t)
	if tt.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(tt.UTC().Format(time.RFC3339))
}

// UnmarshalJSON parses RFC 3339.
func (t *Time) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*t = Time{}
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	tt, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return err
	}
	*t = Time(tt)
	return nil
}

// Std converts back to time.Time.
func (t Time) Std() time.Time { return time.Time(t) }
