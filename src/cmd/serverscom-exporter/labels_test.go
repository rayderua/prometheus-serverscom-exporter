package main

import (
	"strings"
	"testing"

	"github.com/rayderua/prometheus-serverscom-exporter/src/internal/collector"
)

func TestValidateExtraLabels(t *testing.T) {
	reserved := collector.New(nil, nil, nil).LabelNames()

	if err := validateExtraLabels(map[string]string{"account": "prod", "env": "dev", "Region_1": "eu"}, reserved); err != nil {
		t.Fatalf("valid labels rejected: %v", err)
	}
	for name, why := range map[string]string{
		"host_id":   "already use", // every host metric
		"location":  "already use",
		"segment":   "already use", // L2 metrics
		"loop":      "already use", // refresher
		"endpoint":  "already use", // API client
		"version":   "already use", // build info
		"label_env": "host labels",
		"__name__":  "reserved",
		"1account":  "invalid",
		"acc-ount":  "invalid",
	} {
		err := validateExtraLabels(map[string]string{name: "x"}, reserved)
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%q: err = %v, want %q", name, err, why)
		}
	}
}
