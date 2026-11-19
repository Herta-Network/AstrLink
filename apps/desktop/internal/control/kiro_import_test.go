package control

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestReadKiroFile(t *testing.T) {
	entry := `{"email":"test@example.org","provider":"Google","accessToken":"old-access","refreshToken":"mock-refresh"}`
	for _, input := range []string{entry, "[" + entry + "]"} {
		grant, err := readKiroFile(strings.NewReader(input))
		if err != nil || grant.RefreshToken != "mock-refresh" {
			t.Fatal("valid social grant rejected", err)
		}
		b, _ := json.Marshal(grant)
		if strings.Contains(string(b), "old-access") || strings.Contains(string(b), "test@example.org") {
			t.Fatal("extra fields forwarded")
		}
	}
	for _, input := range []string{"[]", "[" + entry + "," + entry + "]", strings.Replace(entry, "Google", "BuilderID", 1), strings.Repeat(" ", 1<<20) + entry, "null", "{}"} {
		if _, err := readKiroFile(strings.NewReader(input)); err == nil || strings.Contains(err.Error(), "mock-refresh") {
			t.Fatal("bad grant accepted or echoed")
		}
	}
	s := Services{PickKiroFile: func() (string, error) { return "", nil }}
	result, err := s.ImportKiroFile(context.Background(), "service_test", nil)
	if result != nil || err != nil {
		t.Fatal("cancelling the picker should do nothing")
	}
}
