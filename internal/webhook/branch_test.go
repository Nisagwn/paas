package webhook

import (
	"errors"
	"testing"
)

func TestParsePushBranchDeleted(t *testing.T) {
	for name, body := range map[string]string{
		"deleted flag": `{"ref":"refs/heads/feature/login","deleted":true,
			"before":"a3f9c1e8b2d4f6a8c0e2b4d6f8a0c2e4b6d8f0a2","after":"0000000000000000000000000000000000000000",
			"repository":{"full_name":"nisagwn/blog"},"head_commit":null}`,
		"zero after only": `{"ref":"refs/heads/feature/login","after":"0000000000000000000000000000000000000000",
			"repository":{"full_name":"nisagwn/blog"}}`,
	} {
		p, err := ParsePush([]byte(body))
		if !errors.Is(err, ErrBranchDeleted) || !errors.Is(err, ErrIgnored) {
			t.Errorf("%s: got %v, want ErrBranchDeleted (and ErrIgnored)", name, err)
		}
		if p.Repo != "nisagwn/blog" || p.Branch != "feature/login" || p.SHA != "" {
			t.Errorf("%s: got %+v", name, p)
		}
	}
	// A deleted tag is just ignored.
	_, err := ParsePush([]byte(`{"ref":"refs/tags/v1","deleted":true,"after":"0000000000000000000000000000000000000000",
		"repository":{"full_name":"a/b"}}`))
	if !errors.Is(err, ErrIgnored) || errors.Is(err, ErrBranchDeleted) {
		t.Errorf("deleted tag: got %v, want plain ErrIgnored", err)
	}
}

func prBody(action, head, headRepo, base string) []byte {
	repo := `null`
	if headRepo != "" {
		repo = `{"full_name":"` + headRepo + `"}`
	}
	return []byte(`{"action":"` + action + `","number":7,"pull_request":{"merged":true,
		"head":{"ref":"` + head + `","repo":` + repo + `},
		"base":{"ref":"main","repo":{"full_name":"` + base + `"}}}}`)
}

func TestParseClosedPR(t *testing.T) {
	pr, err := ParseClosedPR(prBody("closed", "feature/login", "NisaGwn/Blog", "nisagwn/blog"))
	if err != nil {
		t.Fatal(err)
	}
	want := ClosedPR{Repo: "nisagwn/blog", Branch: "feature/login", Number: 7, Merged: true}
	if pr != want {
		t.Fatalf("got %+v, want %+v", pr, want)
	}

	for name, body := range map[string][]byte{
		"opened":       prBody("opened", "feature/login", "nisagwn/blog", "nisagwn/blog"),
		"synchronize":  prBody("synchronize", "feature/login", "nisagwn/blog", "nisagwn/blog"),
		"fork":         prBody("closed", "feature/login", "someone/blog", "nisagwn/blog"),
		"deleted fork": prBody("closed", "feature/login", "", "nisagwn/blog"),
	} {
		if _, err := ParseClosedPR(body); !errors.Is(err, ErrIgnored) {
			t.Errorf("%s: got %v, want ErrIgnored", name, err)
		}
	}
	if _, err := ParseClosedPR([]byte(`{"action":"closed","pull_request":{}}`)); err == nil || errors.Is(err, ErrIgnored) {
		t.Errorf("empty payload: got %v, want a validation error", err)
	}
}
