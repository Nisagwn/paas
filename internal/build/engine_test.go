package build

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestBuildKitArgs(t *testing.T) {
	s := Spec{
		ContextDir: filepath.FromSlash("/w/src"),
		Dockerfile: filepath.FromSlash("/w/paas.Dockerfile"),
		Image:      "localhost:5000/blog:abc",
		Platform:   "linux/arm64",
		CacheRef:   "localhost:5000/blog:buildcache",
		BuildArgs:  map[string]string{"B": "2", "A": "1"},
	}
	got := BuildKit{Addr: "tcp://bk:1234", Insecure: true}.args(s, "meta.json")
	want := []string{
		"--addr", "tcp://bk:1234", "build",
		"--frontend", "dockerfile.v0",
		"--local", "context=" + s.ContextDir,
		"--local", "dockerfile=" + filepath.Dir(s.Dockerfile),
		"--opt", "filename=paas.Dockerfile",
		"--output", "type=image,name=localhost:5000/blog:abc,push=true,registry.insecure=true",
		"--progress", "plain",
		"--metadata-file", "meta.json",
		"--opt", "platform=linux/arm64",
		"--opt", "build-arg:A=1",
		"--opt", "build-arg:B=2",
		"--export-cache", "type=registry,ref=localhost:5000/blog:buildcache,registry.insecure=true,mode=max",
		"--import-cache", "type=registry,ref=localhost:5000/blog:buildcache,registry.insecure=true",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args:\n got %q\nwant %q", got, want)
	}
}

func TestDockerArgs(t *testing.T) {
	got := Docker{}.args(Spec{ContextDir: "src", Dockerfile: "Dockerfile", Image: "r/app:sha"}, "m.json")
	want := []string{"buildx", "build", "--progress", "plain", "--file", "Dockerfile",
		"--tag", "r/app:sha", "--push", "--metadata-file", "m.json", "src"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args:\n got %q\nwant %q", got, want)
	}
}

func TestReadDigest(t *testing.T) {
	p := filepath.Join(t.TempDir(), "meta.json")
	os.WriteFile(p, []byte(`{"containerimage.digest":"sha256:abc","image.name":"x"}`), 0o644)
	if got := readDigest(p); got != "sha256:abc" {
		t.Fatalf("got %q", got)
	}
	if got := readDigest(filepath.Join(t.TempDir(), "missing")); got != "" {
		t.Fatalf("got %q for a missing file", got)
	}
}

func TestLineWriter(t *testing.T) {
	var lines []string
	w := newLineWriter(func(s string) { lines = append(lines, s) })
	w.Write([]byte("one\r\ntw"))
	w.Write([]byte("o\n\nthree"))
	w.Write([]byte("\x00bad\xff\n"))
	w.Write([]byte(strings.Repeat("x", maxLineLen+10)))
	w.Write([]byte("tail"))
	w.Flush()

	if len(lines) != 5 {
		t.Fatalf("got %d lines: %q", len(lines), lines)
	}
	if lines[0] != "one" || lines[1] != "two" || lines[2] != "threebad�" {
		t.Fatalf("got %q", lines[:3])
	}
	if !strings.HasSuffix(lines[3], "(truncated)") || lines[4] != "tail" {
		t.Fatalf("long line not split: %q / %q", lines[3][len(lines[3])-20:], lines[4])
	}
}
