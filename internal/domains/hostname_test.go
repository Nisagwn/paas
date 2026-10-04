package domains

import (
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	ok := map[string]string{
		"www.example.com":       "www.example.com",
		" WWW.Example.COM. ":    "www.example.com",
		"example.com":           "example.com",
		"xn--bcher-kva.example": "xn--bcher-kva.example",
		"a-b.c-d.example.org":   "a-b.c-d.example.org",
		"blog.paas.test.evil":   "blog.paas.test.evil",
	}
	for in, want := range ok {
		got, err := Normalize(in, "paas.test")
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	bad := []string{
		"", "localhost", "paas.test", "blog.paas.test", "x.y.PAAS.test",
		"https://www.example.com", "www.example.com:443", "www.example.com/x",
		"*.example.com", "bücher.example", "-a.example.com", "a-.example.com",
		"a..example.com", "1.2.3.4", "a_b.example.com",
		strings.Repeat("a", 64) + ".example.com",
		strings.Repeat("abcdefghi.", 26) + "com",
	}
	for _, in := range bad {
		if got, err := Normalize(in, "paas.test"); err == nil {
			t.Errorf("Normalize(%q) = %q, want an error", in, got)
		}
	}
}

func TestRecordsAndToken(t *testing.T) {
	tok := NewToken()
	if !strings.HasPrefix(tok, "paas-verify-") || len(tok) != len("paas-verify-")+32 || tok == NewToken() {
		t.Fatalf("token %q", tok)
	}
	rs := Records("www.example.com", "blog", "paas.test", tok)
	if rs[0].Type != "CNAME" || rs[0].Name != "www.example.com" || rs[0].Value != "blog.paas.test" {
		t.Errorf("cname record %+v", rs[0])
	}
	if rs[1].Type != "TXT" || rs[1].Name != "_paas-challenge.www.example.com" || rs[1].Value != tok {
		t.Errorf("txt record %+v", rs[1])
	}
}
