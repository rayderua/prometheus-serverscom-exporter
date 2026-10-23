package collector

import (
	"slices"
	"strings"
)

// labelMapper converts free-form Servers.com labels into Prometheus label
// names, similar to kube-state-metrics' kube_*_labels metrics.
type labelMapper struct {
	allowAll bool
	allowed  map[string]bool
}

func newLabelMapper(allowed []string) *labelMapper {
	m := &labelMapper{allowed: map[string]bool{}}
	for _, key := range allowed {
		key = strings.TrimSpace(key)
		switch key {
		case "":
		case "*":
			m.allowAll = true
		default:
			m.allowed[key] = true
		}
	}
	return m
}

// labelNames maps the Servers.com label keys used in one scrape to
// Prometheus label names.
type labelNames struct {
	keys      []string // Servers.com keys, sorted
	promNames []string // label_<sanitized key>, same order
}

// names computes the label set for a scrape. All series of a metric family
// must share the same label names, so the set is the union over all hosts;
// hosts without a label get an empty value, which Prometheus treats as absent.
func (m *labelMapper) names(all []map[string]string) labelNames {
	seen := map[string]bool{}
	var keys []string
	for _, labels := range all {
		for key := range labels {
			if !seen[key] && (m.allowAll || m.allowed[key]) {
				seen[key] = true
				keys = append(keys, key)
			}
		}
	}
	slices.Sort(keys)

	// Two keys may sanitize to the same name ("app.kubernetes" and
	// "app_kubernetes"); the first one in sorted order wins.
	var result labelNames
	used := map[string]bool{}
	for _, key := range keys {
		name := HostLabelPrefix + sanitizeLabelName(key)
		if used[name] {
			continue
		}
		used[name] = true
		result.keys = append(result.keys, key)
		result.promNames = append(result.promNames, name)
	}
	return result
}

// values returns label values of one host in the order of n.promNames.
func (n labelNames) values(labels map[string]string) []string {
	values := make([]string, len(n.keys))
	for i, key := range n.keys {
		values[i] = labels[key]
	}
	return values
}

// sanitizeLabelName replaces characters not allowed in Prometheus label
// names with underscores.
func sanitizeLabelName(key string) string {
	var b strings.Builder
	for _, r := range key {
		if r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}
