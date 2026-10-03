package build

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree creates files (path → content) under a new temp dir.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDetect(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		kind  string
		want  []string // substrings of the generated Dockerfile
		not   []string
	}{
		{
			name:  "own Dockerfile wins over package.json",
			files: map[string]string{"Dockerfile": "FROM scratch", "package.json": `{}`},
			kind:  KindDockerfile,
		},
		{
			name: "node server with npm lockfile and build step",
			files: map[string]string{
				"package.json":      `{"scripts":{"build":"tsc","start":"node dist/server.js"}}`,
				"package-lock.json": `{}`,
			},
			kind: KindNode,
			want: []string{"COPY package.json package-lock.json ./", "--mount=type=cache,target=/root/.npm npm ci",
				"RUN npm run build", `CMD ["npm", "start"]`, "PORT=8080", "USER node"},
			not: []string{"nginx"},
		},
		{
			name: "vite-style SPA with pnpm is served by nginx",
			files: map[string]string{
				"package.json":   `{"scripts":{"build":"vite build"}}`,
				"pnpm-lock.yaml": "",
			},
			kind: KindNode,
			want: []string{"corepack enable", "pnpm install --frozen-lockfile", "pnpm run build",
				"for d in dist build out public", "FROM " + nginxImage, "COPY --from=build /site/"},
			not: []string{"CMD"},
		},
		{
			name:  "node with yarn and only a server.js",
			files: map[string]string{"package.json": `{}`, "yarn.lock": "", "server.js": ""},
			kind:  KindNode,
			want:  []string{"yarn install --frozen-lockfile", `CMD ["node", "server.js"]`},
		},
		{
			name:  "node main field",
			files: map[string]string{"package.json": `{"main":"src/app.js"}`},
			kind:  KindNode,
			want:  []string{"COPY package.json ./", "npm install", `CMD ["node", "src/app.js"]`},
		},
		{
			name: "go with root main package",
			files: map[string]string{
				"go.mod":  "module example.com/hello\n\ngo 1.23.4\n",
				"go.sum":  "",
				"main.go": "package main\nfunc main() {}\n",
			},
			kind: KindGo,
			want: []string{"FROM golang:1.23-alpine", "COPY go.mod go.sum ./", "-o /out/app .",
				"distroless/static:nonroot", "ENV PORT=8080"},
		},
		{
			name: "go with a single cmd/ binary and no go.sum",
			files: map[string]string{
				"go.mod":               "module example.com/svc\n",
				"lib.go":               "package svc\n",
				"cmd/server/main.go":   "package main\nfunc main() {}\n",
				"cmd/server/x_test.go": "package main_test\n",
			},
			kind: KindGo,
			want: []string{"FROM golang:" + defaultGoVersion + "-alpine", "COPY go.mod ./", "-o /out/app ./cmd/server"},
		},
		{
			name:  "static site in root",
			files: map[string]string{"index.html": "<h1>hi</h1>"},
			kind:  KindStatic,
			want:  []string{"FROM " + nginxImage, "COPY . /usr/share/nginx/html/"},
		},
		{
			name:  "static site in public/",
			files: map[string]string{"public/index.html": "<h1>hi</h1>"},
			kind:  KindStatic,
			want:  []string{"COPY public/ /usr/share/nginx/html/"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Detect(writeTree(t, tt.files))
			if err != nil {
				t.Fatal(err)
			}
			if plan.Kind != tt.kind {
				t.Fatalf("kind = %q, want %q", plan.Kind, tt.kind)
			}
			if tt.kind == KindDockerfile && plan.Dockerfile != "" {
				t.Fatal("must not generate a Dockerfile when the repo has one")
			}
			for _, w := range tt.want {
				if !strings.Contains(plan.Dockerfile, w) {
					t.Errorf("Dockerfile lacks %q:\n%s", w, plan.Dockerfile)
				}
			}
			for _, n := range tt.not {
				if strings.Contains(plan.Dockerfile, n) {
					t.Errorf("Dockerfile should not contain %q:\n%s", n, plan.Dockerfile)
				}
			}
		})
	}
}

func TestDetectErrors(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		msg   string
	}{
		{"empty repo", map[string]string{"README.md": "hi"}, "could not detect"},
		{"bad package.json", map[string]string{"package.json": "{"}, "package.json"},
		{"node without entry point", map[string]string{"package.json": `{"scripts":{"test":"x"}}`}, `"start"`},
		{"go without main", map[string]string{"go.mod": "module x\n", "x.go": "package x\n"}, "no main package"},
		{"go with two binaries", map[string]string{
			"go.mod":        "module x\n",
			"cmd/a/main.go": "package main\n",
			"cmd/b/main.go": "package main\n",
		}, "./cmd/a, ./cmd/b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Detect(writeTree(t, tt.files))
			if err == nil || !strings.Contains(err.Error(), tt.msg) {
				t.Fatalf("err = %v, want it to mention %q", err, tt.msg)
			}
		})
	}
	if _, err := Detect(t.TempDir()); !errors.Is(err, ErrUnknownProject) {
		t.Fatalf("err = %v, want ErrUnknownProject", err)
	}
}
