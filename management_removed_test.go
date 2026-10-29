package main

import (
	"encoding/json"
	"testing"
)

func TestRegistrationAdvertisesQuotaWithoutManagement(t *testing.T) {
	raw, err := dispatch("plugin.register", nil)
	if err != nil {
		t.Fatal(err)
	}
	var reg registrationResponse
	if err := json.Unmarshal(decodeEnvelope(t, raw).Result, &reg); err != nil {
		t.Fatal(err)
	}
	if !reg.Capabilities.QuotaProvider {
		t.Fatal("quota_provider must be advertised")
	}
	capabilities, _ := json.Marshal(reg.Capabilities)
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(capabilities, &fields)
	if _, exists := fields["management_api"]; exists {
		t.Fatal("removed management_api must not be advertised")
	}
}

func TestRemovedManagementMethods(t *testing.T) {
	for _, method := range []string{"management.register", "management.handle"} {
		raw, err := dispatch(method, nil)
		if err != nil {
			t.Fatal(err)
		}
		env := decodeEnvelope(t, raw)
		if env.OK || env.Error == nil || env.Error.Code != "unknown_method" {
			t.Fatalf("%s is still handled: %+v", method, env)
		}
	}
}
