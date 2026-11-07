package winc

import (
	"strings"
	"testing"
)

func TestBYOHHostFactsSafeDiagnostics(t *testing.T) {
	const sensitive = "private-material-must-not-escape"
	facts := byohHostFacts{UUID: "18d74027-cdc4-4347-8b32-81dd83a83001", GUID: "18d74027-cdc4-4347-8b32-81dd83a83002", Addresses: []string{"192.0.2.1"}, DNS: "host.example.test", PublicKey: testHostPublicKey(t)}
	valid := mustBYOHJSON(facts)
	for _, test := range []struct{ name, input, category string }{
		{"syntax", `{"` + sensitive + `":!}`, "json-syntax"},
		{"incomplete", `{"uuid":"` + sensitive, "json-incomplete"},
		{"empty", "", "json-incomplete"},
		{"bom", "\xef\xbb\xbf" + valid, "json-syntax"},
		{"scalar-address", strings.Replace(valid, `["192.0.2.1"]`, `"`+sensitive+`"`, 1), "json-type, field addresses"},
		{"type", `{"publicKey":{"` + sensitive + `":true}}`, "json-type, field publicKey"},
		{"root-array", `[{"` + sensitive + `":true}]`, "json-type, field schema"},
		{"unknown-field", `{"` + sensitive + `":"` + sensitive + `"}`, "json-unknown-field"},
		{"trailing-value", valid + ` {"` + sensitive + `":true}`, "json-trailing-value"},
		{"trailing-syntax", valid + sensitive, "json-trailing-syntax"},
		{"size", strings.Repeat(sensitive, 2048), "size"},
		{"capability", `{"failure":"hostkey-reparse"}`, "capability: hostkey-reparse"},
		{"unknown-capability", `{"failure":"` + sensitive + `"}`, "capability-unknown"},
		{"invalid-identity", strings.Replace(valid, facts.UUID, sensitive, 1), "invalid host system UUID"},
		{"invalid-key", strings.Replace(valid, facts.PublicKey, sensitive, 1), "missing or invalid public host key"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeBYOHHostFacts(test.input, true)
			if err == nil || !strings.Contains(err.Error(), test.category) {
				t.Fatalf("expected safe category %q, got %v", test.category, err)
			}
			if len(err.Error()) > 160 || strings.Contains(err.Error(), sensitive) || strings.Contains(err.Error(), facts.PublicKey) {
				t.Fatal("diagnostic leaked untrusted content or exceeded bound")
			}
		})
	}
	for _, output := range []string{valid, "\r\n" + valid + "\r\n"} {
		if _, err := decodeBYOHHostFacts(output, true); err != nil {
			t.Fatalf("canonical array facts rejected: %v", err)
		}
	}
}
