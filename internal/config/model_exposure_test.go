package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

// A hand-written model-exposure section that omits accept-bare-ids must keep bare-id clients working.
func TestModelExposureAcceptBareIDsDefaultsToTrue(t *testing.T) {
	for _, tc := range []struct {
		yamlDoc, jsonDoc string
		want             bool
	}{
		{"enabled: true\n", `{"enabled":true}`, true},
		{"enabled: true\naccept-bare-ids: false\n", `{"enabled":true,"accept-bare-ids":false}`, false},
	} {
		var fromYAML, fromJSON ModelExposureConfig
		if err := yaml.Unmarshal([]byte(tc.yamlDoc), &fromYAML); err != nil {
			t.Fatalf("yaml: %v", err)
		}
		if err := json.Unmarshal([]byte(tc.jsonDoc), &fromJSON); err != nil {
			t.Fatalf("json: %v", err)
		}
		if fromYAML.AcceptBareIDs != tc.want || fromJSON.AcceptBareIDs != tc.want || !fromYAML.Enabled || !fromJSON.Enabled {
			t.Fatalf("%q: yaml=%+v json=%+v, want accept-bare-ids=%v", tc.yamlDoc, fromYAML, fromJSON, tc.want)
		}
	}
	cfg, err := ParseConfigBytes([]byte("config-version: 8\nmodel-exposure:\n  enabled: true\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cfg.ModelExposure.AcceptBareIDs {
		t.Fatal("full config load: accept-bare-ids omitted decoded as false")
	}
}
