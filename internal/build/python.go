package build

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	defaultPythonVersion = "3.12"
	uvImage              = "ghcr.io/astral-sh/uv:0.12"
)

func isPython(dir string) bool {
	return exists(dir, "requirements.txt") || exists(dir, "pyproject.toml")
}

// pythonInstaller installs dependencies into the virtualenv /opt/venv.
type pythonInstaller struct {
	name      string
	manifests []string
	steps     string // Dockerfile lines, run with the venv on PATH
	add       string // command prefix to add a missing server package
}

func detectPythonInstaller(dir string) pythonInstaller {
	switch {
	case exists(dir, "uv.lock"):
		return pythonInstaller{"uv", []string{"pyproject.toml", "uv.lock"},
			"COPY --from=" + uvImage + " /uv /usr/local/bin/uv\n" +
				"ENV UV_PROJECT_ENVIRONMENT=/opt/venv UV_LINK_MODE=copy UV_PYTHON_DOWNLOADS=never\n" +
				"RUN --mount=type=cache,target=/root/.cache/uv uv sync --frozen --no-dev --no-install-project\n",
			"RUN --mount=type=cache,target=/root/.cache/uv uv pip install"}
	case exists(dir, "poetry.lock"):
		// Poetry itself stays in the build stage; it installs into the active venv.
		return pythonInstaller{"poetry", []string{"pyproject.toml", "poetry.lock"},
			"RUN --mount=type=cache,target=/root/.cache/pip /usr/local/bin/pip install poetry\n" +
				"RUN python -m venv /opt/venv\n" +
				"RUN --mount=type=cache,target=/root/.cache/pypoetry poetry install --only main --no-root --no-interaction\n",
			"RUN --mount=type=cache,target=/root/.cache/pip pip install"}
	case exists(dir, "requirements.txt"):
		return pythonInstaller{"pip", []string{"requirements.txt"},
			"RUN python -m venv /opt/venv\n" +
				"RUN --mount=type=cache,target=/root/.cache/pip pip install -r requirements.txt\n",
			"RUN --mount=type=cache,target=/root/.cache/pip pip install"}
	}
	// pyproject.toml without a lockfile: install [project].dependencies only,
	// so the app itself need not be an installable package.
	return pythonInstaller{"pip", []string{"pyproject.toml"},
		"RUN python -m venv /opt/venv\n" +
			`RUN python -c "import tomllib; print('\n'.join(tomllib.load(open('pyproject.toml', 'rb'))` +
			`.get('project', {}).get('dependencies', [])))" > /tmp/requirements.txt` + "\n" +
			"RUN --mount=type=cache,target=/root/.cache/pip pip install -r /tmp/requirements.txt\n",
		"RUN --mount=type=cache,target=/root/.cache/pip pip install"}
}

var (
	pythonVersionRe = regexp.MustCompile(`\b(3\.\d+)`)
	djangoSettings  = regexp.MustCompile(`DJANGO_SETTINGS_MODULE['"]\s*,\s*['"]([\w.]+)\.settings['"]`)
	asgiApp         = regexp.MustCompile(`(?m)^(\w+)\s*(?::[^=\n]+)?=\s*(?:fastapi\.|starlette\.applications\.)?(?:FastAPI|Starlette)\(`)
	flaskApp        = regexp.MustCompile(`(?m)^(\w+)\s*(?::[^=\n]+)?=\s*(?:flask\.)?Flask\(`)
)

// pythonModules are the files scanned for an ASGI or Flask app, with their
// import paths.
var pythonModules = []struct{ file, module string }{
	{"main.py", "main"}, {"app.py", "app"}, {"server.py", "server"},
	{"app/main.py", "app.main"}, {"src/main.py", "src.main"},
}

func detectPython(dir string, o Options) (Plan, error) {
	version := defaultPythonVersion
	for _, f := range []string{".python-version", "runtime.txt"} {
		if b, err := os.ReadFile(filepath.Join(dir, f)); err == nil {
			if m := pythonVersionRe.FindStringSubmatch(string(b)); m != nil {
				version = m[1]
				break
			}
		}
	}
	inst := detectPythonInstaller(dir)
	if o.InstallCommand != "" {
		// A custom install step may need any file of the project; it runs
		// with the virtualenv on PATH.
		inst = pythonInstaller{"custom install", nil,
			"COPY . .\nRUN python -m venv /opt/venv\n" +
				"RUN --mount=type=cache,target=/root/.cache/pip " + o.InstallCommand + "\n",
			"RUN --mount=type=cache,target=/root/.cache/pip pip install"}
	}

	server := ""  // package the start command needs
	command := "" // shell command, run via sh -c
	exec := ""    // or exec-form CMD
	how := ""     // for the summary
	if start, startHow := startCommand(dir, o); start != "" {
		command, how = start, startHow
	} else if exists(dir, "manage.py") {
		project, err := djangoProject(dir)
		if err != nil {
			return Plan{}, err
		}
		server = "gunicorn"
		command = fmt.Sprintf("exec gunicorn %s.wsgi --bind 0.0.0.0:$PORT", project)
		how = "Django: gunicorn " + project + ".wsgi"
	} else if mod, app, ok := findPythonApp(dir, asgiApp); ok {
		server = "uvicorn"
		command = fmt.Sprintf("exec uvicorn %s:%s --host 0.0.0.0 --port $PORT", mod, app)
		how = fmt.Sprintf("ASGI: uvicorn %s:%s", mod, app)
	} else if mod, app, ok := findPythonApp(dir, flaskApp); ok {
		server = "gunicorn"
		command = fmt.Sprintf("exec gunicorn %s:%s --bind 0.0.0.0:$PORT", mod, app)
		how = fmt.Sprintf("Flask: gunicorn %s:%s", mod, app)
	} else {
		for _, f := range []string{"main.py", "app.py", "server.py"} {
			if exists(dir, f) {
				exec, how = fmt.Sprintf("CMD [%q, %q]\n", "python", f), "python "+f
				break
			}
		}
		if exec == "" {
			return Plan{}, errors.New("python project has no Procfile web process, manage.py, " +
				"FastAPI/Flask app or main.py/app.py; add a Procfile with a \"web:\" line or set the start command")
		}
	}

	var df strings.Builder
	fmt.Fprintf(&df, "# generated by paas: python %s (%s)\n", version, inst.name)
	fmt.Fprintf(&df, "FROM python:%s-slim AS build\n", version)
	df.WriteString("ENV VIRTUAL_ENV=/opt/venv PATH=/opt/venv/bin:$PATH PIP_DISABLE_PIP_VERSION_CHECK=1\nWORKDIR /app\n")
	if inst.manifests != nil {
		// Manifests first, so the install layers are cached until dependencies change.
		fmt.Fprintf(&df, "COPY %s ./\n", strings.Join(inst.manifests, " "))
	}
	df.WriteString(inst.steps)
	if server != "" {
		add := true
		if inst.manifests != nil {
			// The last manifest is the lockfile, if there is one; it is what gets installed.
			depsFile := inst.manifests[len(inst.manifests)-1]
			deps, _ := os.ReadFile(filepath.Join(dir, depsFile))
			add = !pythonDependsOn(depsFile, deps, server)
		}
		if add {
			fmt.Fprintf(&df, "%s %s\n", inst.add, server)
			how += " (" + server + " added)"
		}
	}
	fmt.Fprintf(&df, "FROM python:%s-slim\n", version)
	df.WriteString("ENV VIRTUAL_ENV=/opt/venv PATH=/opt/venv/bin:$PATH PYTHONUNBUFFERED=1 PYTHONDONTWRITEBYTECODE=1 HOME=/tmp\n")
	df.WriteString("COPY --from=build /opt/venv /opt/venv\nWORKDIR /app\nCOPY . .\n")
	if o.BuildCommand != "" {
		// e.g. collectstatic: runs where the app runs, before dropping root.
		fmt.Fprintf(&df, "RUN %s\n", o.BuildCommand)
		how += ", build command: " + o.BuildCommand
	}
	fmt.Fprintf(&df, "ENV PORT=%d\nEXPOSE %d\nUSER %s\n", Port, Port, appUser)
	if exec != "" {
		df.WriteString(exec)
	} else {
		df.WriteString(shellCmd(command))
	}
	return Plan{Kind: KindPython, Framework: pythonFramework(dir), Dockerfile: df.String(),
		Summary: fmt.Sprintf("python %s (%s, %s)", version, inst.name, how)}, nil
}

// pythonFramework names the web framework for the build log.
func pythonFramework(dir string) string {
	if exists(dir, "manage.py") {
		return "Django"
	}
	if _, _, ok := findPythonApp(dir, asgiApp); ok {
		return "FastAPI"
	}
	if _, _, ok := findPythonApp(dir, flaskApp); ok {
		return "Flask"
	}
	return "Python"
}

// pythonDependsOn reports whether the manifest that gets installed names pkg.
// Lockfiles list packages as name = "pkg", so an extra like
// "uvicorn[standard]" inside another package's entry does not count.
func pythonDependsOn(file string, manifest []byte, pkg string) bool {
	q := regexp.QuoteMeta(pkg)
	re := `(?mi)^\s*name\s*=\s*"` + q + `"`
	if !strings.HasSuffix(file, ".lock") {
		// requirements.txt lines, [tool.poetry] keys or PEP 621 strings.
		re = `(?mi)^\s*` + q + `([\s\[<>=~!;]|$)|["']` + q + `[\s\[<>=~!;,"']`
	}
	return regexp.MustCompile(re).Match(manifest)
}

// djangoProject finds the Django project package holding wsgi.py: the one
// manage.py points DJANGO_SETTINGS_MODULE at, else the only */wsgi.py.
func djangoProject(dir string) (string, error) {
	if b, err := os.ReadFile(filepath.Join(dir, "manage.py")); err == nil {
		if m := djangoSettings.FindSubmatch(b); m != nil {
			return string(m[1]), nil
		}
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "*", "wsgi.py"))
	if len(matches) == 1 {
		return filepath.Base(filepath.Dir(matches[0])), nil
	}
	return "", errors.New("manage.py found but no single <project>/wsgi.py; add a Procfile with a \"web:\" line")
}

// findPythonApp looks for an application object created at module level.
func findPythonApp(dir string, re *regexp.Regexp) (module, app string, ok bool) {
	for _, m := range pythonModules {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(m.file)))
		if err != nil {
			continue
		}
		if sm := re.FindSubmatch(b); sm != nil {
			return m.module, string(sm[1]), true
		}
	}
	return "", "", false
}
