package config

import (
	"bytes"
	"encoding/json"
	"testing"
)

func FuzzConfigDocument(f *testing.F) {
	f.Add([]byte(`{"history_max":20,"tools":{"editor":["vi"]}}`))
	f.Add([]byte(`{"read_only":true,"read_only":false}`))
	f.Add([]byte(`{} {}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			return
		}
		if err := validateJSONDocument(data); err != nil {
			return
		}
		cfg := Default()
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&cfg); err == nil {
			_ = validate(cfg)
		}
	})
}
