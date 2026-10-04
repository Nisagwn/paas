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
				"RUN npm run build", `CMD ["npm", "start"]`, "PORT=8080", "USER 1000:1000"},
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
				"distroless/static:nonroot", "ENV PORT=8080", "USER 65532:65532"},
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
		{
			name:  "Procfile web process overrides the node start script",
			files: map[string]string{"package.json": `{"scripts":{"build":"tsc","start":"x"}}`, "Procfile": "worker: q\nweb: node dist/main.js --port $PORT\n"},
			kind:  KindNode,
			want:  []string{"RUN npm run build", `CMD ["sh", "-c", "node dist/main.js --port $PORT"]`, "USER 1000:1000"},
			not:   []string{"npm start", "nginx"},
		},
		{
			name:  "go ignores a Procfile (no shell in distroless)",
			files: map[string]string{"go.mod": "module x\n", "main.go": "package main\n", "Procfile": "web: ./x\n"},
			kind:  KindGo,
			want:  []string{`ENTRYPOINT ["/app"]`},
			not:   []string{"sh"},
		},

		// ---- python ----
		{
			name: "fastapi with requirements.txt",
			files: map[string]string{
				"requirements.txt": "fastapi==0.115\nuvicorn[standard]\n",
				"main.py":          "from fastapi import FastAPI\n\napp = FastAPI()\n",
			},
			kind: KindPython,
			want: []string{"FROM python:3.12-slim AS build", "RUN python -m venv /opt/venv",
				"COPY requirements.txt ./", "--mount=type=cache,target=/root/.cache/pip pip install -r requirements.txt",
				"COPY --from=build /opt/venv /opt/venv", "PATH=/opt/venv/bin:$PATH", "ENV PORT=8080", "USER 1000:1000",
				`CMD ["sh", "-c", "exec uvicorn main:app --host 0.0.0.0 --port $PORT"]`},
			not: []string{"pip install uvicorn"},
		},
		{
			name: "typed FastAPI app in app/main.py",
			files: map[string]string{
				"requirements.txt": "fastapi\n",
				"app/main.py":      "import fastapi\napi: fastapi.FastAPI = fastapi.FastAPI(title='x')\n",
			},
			kind: KindPython,
			want: []string{"pip install uvicorn", "exec uvicorn app.main:api"},
		},
		{
			name:  "flask gets gunicorn added",
			files: map[string]string{"requirements.txt": "Flask\n", "app.py": "from flask import Flask\napp = Flask(__name__)\n"},
			kind:  KindPython,
			want:  []string{"pip install gunicorn", "exec gunicorn app:app --bind 0.0.0.0:$PORT"},
		},
		{
			name: "django beats package.json and uses the settings package",
			files: map[string]string{
				"requirements.txt": "django\ngunicorn\n",
				"package.json":     `{"scripts":{"build":"tailwind"}}`,
				"manage.py":        "os.environ.setdefault('DJANGO_SETTINGS_MODULE', 'mysite.settings')\n",
				"mysite/wsgi.py":   "",
			},
			kind: KindPython,
			want: []string{"exec gunicorn mysite.wsgi --bind 0.0.0.0:$PORT"},
			not:  []string{"pip install gunicorn", "npm"},
		},
		{
			name:  "django project found by its wsgi.py",
			files: map[string]string{"requirements.txt": "django\n", "manage.py": "", "proj/wsgi.py": ""},
			kind:  KindPython,
			want:  []string{"pip install gunicorn", "exec gunicorn proj.wsgi"},
		},
		{
			name:  "pyproject.toml without a lockfile installs project dependencies",
			files: map[string]string{"pyproject.toml": "[project]\ndependencies=[]\n", "main.py": "print(1)\n"},
			kind:  KindPython,
			want:  []string{"COPY pyproject.toml ./", "import tomllib", "pip install -r /tmp/requirements.txt", `CMD ["python", "main.py"]`},
		},
		{
			name: "uv lockfile",
			files: map[string]string{
				"pyproject.toml": "[project]\n", "uv.lock": "name = \"uvicorn\"\n",
				"main.py": "from fastapi import FastAPI\napp = FastAPI()\n",
			},
			kind: KindPython,
			want: []string{"COPY pyproject.toml uv.lock ./", "COPY --from=" + uvImage, "UV_PROJECT_ENVIRONMENT=/opt/venv",
				"--mount=type=cache,target=/root/.cache/uv uv sync --frozen --no-dev --no-install-project"},
			not: []string{"python -m venv", "uv pip install"},
		},
		{
			name: "poetry lockfile",
			files: map[string]string{
				"pyproject.toml": "[tool.poetry]\n", "poetry.lock": "", "app.py": "import flask\napp = flask.Flask('x')\n",
			},
			kind: KindPython,
			want: []string{"COPY pyproject.toml poetry.lock ./", "pip install poetry",
				"poetry install --only main --no-root", "pip install gunicorn", "exec gunicorn app:app"},
		},
		{
			name: "python Procfile and version file",
			files: map[string]string{
				"requirements.txt": "", ".python-version": "3.11.9\n",
				"Procfile": "web: gunicorn -w 4 wsgi:application\n", "app.py": "app = Flask(__name__)\n",
			},
			kind: KindPython,
			want: []string{"FROM python:3.11-slim AS build", `CMD ["sh", "-c", "gunicorn -w 4 wsgi:application"]`},
			not:  []string{"pip install gunicorn"},
		},

		// ---- ruby ----
		{
			name: "rack app with puma beats package.json",
			files: map[string]string{
				"Gemfile": "gem 'rack'\ngem 'puma'\n", "Gemfile.lock": "GEM\n  specs:\n    puma (6.4.2)\n",
				"config.ru": "run App", "package.json": `{"scripts":{"start":"x"}}`,
			},
			kind: KindRuby,
			want: []string{"FROM ruby:3.3 AS build", "COPY Gemfile Gemfile.lock ./",
				"--mount=type=cache,target=/root/.bundle/cache bundle install", "FROM ruby:3.3-slim",
				"COPY --from=build /usr/local/bundle /usr/local/bundle", "RACK_ENV=production PORT=8080", "USER 1000:1000",
				`CMD ["sh", "-c", "exec bundle exec puma -b tcp://0.0.0.0:$PORT"]`},
			not: []string{"npm", "RAILS_ENV", "libpq5"},
		},
		{
			name:  "rack app with rackup and the Gemfile's ruby version",
			files: map[string]string{"Gemfile": "ruby '3.2.4'\ngem \"rackup\"\n", "config.ru": ""},
			kind:  KindRuby,
			want:  []string{"FROM ruby:3.2 AS build", "COPY Gemfile ./", "exec bundle exec rackup -o 0.0.0.0 -p $PORT"},
		},
		{
			name: "rails with assets and postgres",
			files: map[string]string{
				"Gemfile": "gem 'rails'\n", "Gemfile.lock": "    pg (1.5.6)\n    puma (6.4.2)\n",
				"config.ru": "", "bin/rails": "", "app/assets/x.css": "", ".ruby-version": "ruby-3.4.1\n",
			},
			kind: KindRuby,
			want: []string{"FROM ruby:3.4 AS build", "SECRET_KEY_BASE_DUMMY=1 RAILS_ENV=production bundle exec rails assets:precompile",
				"chown -R 1000:1000 tmp log", "libpq5", "RAILS_ENV=production",
				"exec bundle exec rails server -b 0.0.0.0 -p $PORT"},
		},
		{
			name:  "Gemfile with a Procfile",
			files: map[string]string{"Gemfile": "gem 'sinatra'\n", "Procfile": "web: bundle exec ruby app.rb -p $PORT\n"},
			kind:  KindRuby,
			want:  []string{`CMD ["sh", "-c", "bundle exec ruby app.rb -p $PORT"]`},
		},
		{
			name:  "Gemfile alone does not outrank a static site",
			files: map[string]string{"Gemfile": "gem 'jekyll'\n", "index.html": ""},
			kind:  KindStatic,
		},

		// ---- java ----
		{
			name:  "maven without wrapper beats package.json",
			files: map[string]string{"pom.xml": "<project/>", "package.json": `{"scripts":{"start":"x"}}`},
			kind:  KindJava,
			want: []string{"FROM " + mavenImage + " AS build", "COPY pom.xml ./",
				"--mount=type=cache,target=/root/.m2 mvn -B dependency:go-offline",
				"--mount=type=cache,target=/root/.m2 mvn -B -DskipTests package", "ls -S target/*.jar",
				"FROM " + javaJREImage, "SERVER_PORT=8080", "USER 1000:1000", `CMD ["java", "-jar", "/app/app.jar"]`},
			not: []string{"npm", "mvnw"},
		},
		{
			name:  "maven wrapper",
			files: map[string]string{"pom.xml": "", "mvnw": "", ".mvn/wrapper/maven-wrapper.properties": ""},
			kind:  KindJava,
			want: []string{"FROM " + javaJDKImage + " AS build", "COPY .mvn .mvn/", "COPY pom.xml mvnw ./",
				"chmod +x mvnw", "./mvnw -B -DskipTests package"},
		},
		{
			name: "gradle kotlin DSL with wrapper and Procfile",
			files: map[string]string{
				"build.gradle.kts": "", "settings.gradle.kts": "", "gradlew": "", "gradle/wrapper/x.properties": "",
				"Procfile": "web: java -Dserver.port=$PORT -jar build/libs/app.jar\n",
			},
			kind: KindJava,
			want: []string{"ENV GRADLE_USER_HOME=/root/.gradle", "COPY gradle gradle/",
				"COPY build.gradle.kts settings.gradle.kts gradlew ./",
				"--mount=type=cache,target=/root/.gradle ./gradlew --no-daemon assemble", "ls -S build/libs/*.jar",
				"COPY --from=build /src/build/libs/ /app/build/libs/",
				`CMD ["sh", "-c", "java -Dserver.port=$PORT -jar build/libs/app.jar"]`},
		},
		{
			name:  "gradle without wrapper",
			files: map[string]string{"build.gradle": ""},
			kind:  KindJava,
			want:  []string{"FROM " + gradleImage + " AS build", "COPY build.gradle ./", "gradle --no-daemon dependencies"},
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
			// The built-in BuildKit frontend handles cache mounts; a syntax
			// directive only adds a frontend image resolve to every build.
			if plan.Dockerfile != "" && (!strings.HasPrefix(plan.Dockerfile, "# generated by paas: ") ||
				strings.Contains(plan.Dockerfile, "# syntax=")) {
				t.Errorf("Dockerfile must start with the paas comment and have no syntax directive:\n%s", plan.Dockerfile)
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

func TestPythonDependsOn(t *testing.T) {
	tests := []struct {
		file, body, pkg string
		want            bool
	}{
		{"requirements.txt", "Flask==3.0\ngunicorn>=22 ; python_version > '3'\n", "gunicorn", true},
		{"requirements.txt", "uvicorn[standard]\n", "uvicorn", true},
		{"requirements.txt", "uvicorn-worker\n# gunicorn later\n", "uvicorn", false},
		{"requirements.txt", "uvicorn-worker\n# gunicorn later\n", "gunicorn", false},
		{"pyproject.toml", "[project]\ndependencies = [\"fastapi\", \"uvicorn>=0.30\"]\n", "uvicorn", true},
		{"pyproject.toml", "[tool.poetry.dependencies]\nuvicorn = \"^0.30\"\n", "uvicorn", true},
		{"pyproject.toml", "[project]\ndependencies = [\"fastapi\"]\n", "uvicorn", false},
		{"uv.lock", "[[package]]\nname = \"uvicorn\"\n", "uvicorn", true},
		// fastapi's extras mention uvicorn; that does not install it.
		{"poetry.lock", "[[package]]\nname = \"fastapi\"\n[package.extras]\nstandard = [\"uvicorn[standard] (>=0.12.0)\"]\n", "uvicorn", false},
	}
	for _, tt := range tests {
		if got := pythonDependsOn(tt.file, []byte(tt.body), tt.pkg); got != tt.want {
			t.Errorf("%s %q: %s = %v, want %v", tt.file, tt.body, tt.pkg, got, tt.want)
		}
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
		{"python without entry point", map[string]string{"requirements.txt": "requests\n", "lib.py": ""}, "Procfile"},
		{"django without wsgi.py", map[string]string{"requirements.txt": "", "manage.py": ""}, "wsgi.py"},
		{"Gemfile without config.ru or Procfile", map[string]string{"Gemfile": "gem 'rake'\n"}, "config.ru"},
		{"rack without a server gem", map[string]string{"Gemfile": "gem 'rack'\n", "config.ru": ""}, "add puma"},
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
