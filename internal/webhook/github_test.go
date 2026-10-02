package webhook

import (
	"errors"
	"testing"
)

func TestVerifySignature(t *testing.T) {
	body := []byte(`{"hello":"world"}`)
	good := Sign("s3cret", body)

	if err := VerifySignature("s3cret", body, good); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	for name, c := range map[string]struct{ secret, header string }{
		"wrong secret": {"other", good},
		"no prefix":    {"s3cret", good[len("sha256="):]},
		"not hex":      {"s3cret", "sha256=zzzz"},
		"empty":        {"s3cret", ""},
		"empty secret": {"", good},
	} {
		if err := VerifySignature(c.secret, body, c.header); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s: got %v, want ErrBadSignature", name, err)
		}
	}
	if err := VerifySignature("s3cret", []byte(`{"hello":"w0rld"}`), good); err == nil {
		t.Error("tampered body accepted")
	}
}

func TestParsePush(t *testing.T) {
	p, err := ParsePush([]byte(`{
		"ref": "refs/heads/feature/login",
		"after": "a3f9c1e8b2d4f6a8c0e2b4d6f8a0c2e4b6d8f0a2",
		"repository": {"full_name": "nisagwn/blog"},
		"head_commit": {"message": "Add login\n\nlong body"}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	want := Push{Repo: "nisagwn/blog", Branch: "feature/login",
		SHA: "a3f9c1e8b2d4f6a8c0e2b4d6f8a0c2e4b6d8f0a2", Message: "Add login"}
	if p != want {
		t.Fatalf("got %+v, want %+v", p, want)
	}
}

func TestParsePushIgnored(t *testing.T) {
	cases := map[string]string{
		"tag":     `{"ref":"refs/tags/v1","after":"a3f9c1e8b2d4f6a8c0e2b4d6f8a0c2e4b6d8f0a2","repository":{"full_name":"a/b"}}`,
		"deleted": `{"ref":"refs/heads/x","deleted":true,"after":"0000000000000000000000000000000000000000","repository":{"full_name":"a/b"}}`,
	}
	for name, body := range cases {
		if _, err := ParsePush([]byte(body)); !errors.Is(err, ErrIgnored) {
			t.Errorf("%s: got %v, want ErrIgnored", name, err)
		}
	}
	if _, err := ParsePush([]byte(`{"ref":"refs/heads/main","after":"nope","repository":{"full_name":"a/b"}}`)); err == nil || errors.Is(err, ErrIgnored) {
		t.Errorf("bad sha: got %v, want a validation error", err)
	}
}
