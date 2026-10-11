package metrics

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The dashboard, the alerts and their tests select only metrics that the
// tester defines.
func TestMonitoringNames(t *testing.T) {
	src, err := os.ReadFile("metrics.go")
	if err != nil {
		t.Fatal(err)
	}
	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`Name:\s+"(\w+)"`).FindAllStringSubmatch(string(src), -1) {
		defined[namespace+"_"+m[1]] = true
	}
	for _, path := range []string{"../../deploy/monitoring/dashboard.json", "../../deploy/monitoring/prometheusrule.yaml", "../../e2e/rules-test.yaml"} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range regexp.MustCompile(namespace+`_\w+`).FindAllString(string(b), -1) {
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				if trimmed := strings.TrimSuffix(name, suffix); defined[trimmed] {
					name = trimmed
				}
			}
			if !defined[name] {
				t.Errorf("%s: %s is no metric of the tester", path, name)
			}
		}
	}
}
