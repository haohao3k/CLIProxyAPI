package config

import (
	"encoding/json"
	"strings"
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
	// No section at all: the effective default is reported, and saving does not add a section.
	absent, err := ParseConfigBytes([]byte("config-version: 8\n"))
	if err != nil || !absent.ModelExposure.AcceptBareIDs {
		t.Fatalf("config without model-exposure: accept-bare-ids=%v err=%v, want true", absent.ModelExposure.AcceptBareIDs, err)
	}
	if out, _ := yaml.Marshal(struct {
		M ModelExposureConfig `yaml:"model-exposure,omitempty"`
	}{absent.ModelExposure}); strings.Contains(string(out), "model-exposure") {
		t.Fatalf("default section is written on save: %s", out)
	}
	cfg, err := ParseConfigBytes([]byte("config-version: 8\nmodel-exposure:\n  enabled: true\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !cfg.ModelExposure.AcceptBareIDs {
		t.Fatal("full config load: accept-bare-ids omitted decoded as false")
	}
}
