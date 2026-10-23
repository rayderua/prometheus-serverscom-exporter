package main

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// otherLabelNames are label names of metrics outside the collector package:
// API client, refresher, build info, Go runtime, histograms and summaries.
var otherLabelNames = []string{
	"endpoint", "code", "loop", "resource",
	"version", "revision", "branch", "goversion", "goos", "goarch", "tags",
	"le", "quantile",
}

var labelNameRE = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// validateExtraLabels checks the --label flags: valid names that do not
// collide with labels of the exporter's metrics, which would make those
// metrics fail to render.
func validateExtraLabels(labels map[string]string, reserved []string) error {
	for name := range labels {
		switch {
		case !labelNameRE.MatchString(name):
			return fmt.Errorf("--label %q: invalid label name", name)
		case strings.HasPrefix(name, "__"):
			return fmt.Errorf("--label %q: names starting with __ are reserved by Prometheus", name)
		case strings.HasPrefix(name, "label_"):
			return fmt.Errorf("--label %q: label_* names are used for Servers.com host labels", name)
		case slices.Contains(reserved, name) || slices.Contains(otherLabelNames, name):
			return fmt.Errorf("--label %q: the exporter's metrics already use this label", name)
		}
	}
	return nil
}
