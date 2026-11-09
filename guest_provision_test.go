package main

import (
	"os"
	"strings"
	"testing"
)

func TestGuestPostgresIsStartedBeforeFirstSQL(t *testing.T) {
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	begin := strings.Index(text, "provision := `")
	if begin < 0 {
		t.Fatal("guest provisioning script missing")
	}
	script := text[begin:]
	for _, command := range []string{"systemctl enable postgresql", "systemctl start postgresql"} {
		index := strings.Index(script, command)
		if index < 0 || index > strings.Index(script, "psql -Atqc") {
			t.Errorf("guest-side %q must precede first SQL query", command)
		}
	}
}
