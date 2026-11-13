package logger

import (
	"bytes"
	"strings"
	"testing"
)

func TestScrubRemovesLongSecrets(t *testing.T) {
	var buf bytes.Buffer
	base, err := New(&buf, "info")
	if err != nil {
		t.Fatal(err)
	}
	const secret = "0123456789abcdef0123456789abcdef"
	log := Scrub(base, []string{secret, "short"})
	log.Info("open "+secret, "target", "user-"+secret)
	if strings.Contains(buf.String(), secret) || !strings.Contains(buf.String(), "[redacted]") {
		t.Fatalf("log = %s", buf.String())
	}
}
