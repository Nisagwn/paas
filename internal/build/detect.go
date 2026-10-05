package build

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Port is the platform contract: every app listens on $PORT, which is 8080.
const Port = 8080

// Generated images always run as a numeric non-root user (node, python, ruby,
// java = 1000, distroless nonroot = 65532, nginx-unprivileged = 101 set by its
// base image), so the kubelet can enforce runAsNonRoot, which it cannot do for
// "USER node".
const appUser = "1000:1000"

// Kinds of project Detect recognizes.
const (
	KindDockerfile = "dockerfile" // the repo brings its own Dockerfile
	KindNode       = "node"
	KindGo         = "go"
	KindPython     = "python"
	KindRuby       = "ruby"
	KindJava       = "java"
	KindStatic     = "static"
)

// Plan says how to build a checked-out repo.
type Plan struct {
	Kind string
	// Framework is the display name of what was detected, e.g. "Next.js",
	// "Django" or "Go". Build logs show it and deployments record it.
	Framework string
	// Summary is a one-line human description for the build log.
	Summary string
	// Dockerfile is the generated Dockerfile, or "" for KindDockerfile.
	Dockerfile string
}

// ErrUnknownProject means none of the supported project types matched.
var ErrUnknownProject = errors.New("could not detect project type: add a Dockerfile, " +
	"package.json, go.mod, requirements.txt, pyproject.toml, Gemfile, pom.xml, build.gradle " +
	"or index.html to the repository root (or set the root directory in the project settings)")

// Detect inspects the project in dir with default settings.
func Detect(dir string) (Plan, error) { return DetectWith(dir, Options{}) }

// DetectWith inspects the project in dir (the repository, or its root
// directory setting) and generates a Dockerfile that honours o.
//
// Without a framework setting a Dockerfile always wins, so any language
// works as long as the repo brings its own. Java, Rack/Rails and Django
// projects are checked before package.json because they often carry one for
// frontend tooling; a Node app rarely carries a pom.xml, config.ru or
// manage.py. A Gemfile alone (e.g. Jekyll) does not outrank a static
// index.html.
func DetectWith(dir string, o Options) (Plan, error) {
	if o.Framework != "" {
		return detectForced(dir, o)
	}
	_, hasWeb := procfileWeb(dir)
	switch {
	case exists(dir, "Dockerfile"):
		return Plan{Kind: KindDockerfile, Framework: "Dockerfile", Summary: "Dockerfile found in repository"}, nil
	case javaBuildTool(dir) != "":
		return detectJava(dir, o)
	case exists(dir, "Gemfile") && exists(dir, "config.ru"):
		return detectRuby(dir, o)
	case isPython(dir) && exists(dir, "manage.py"):
		return detectPython(dir, o)
	case exists(dir, "package.json"):
		return detectNode(dir, o)
	case exists(dir, "go.mod"):
		return detectGo(dir, o)
	case isPython(dir):
		return detectPython(dir, o)
	case exists(dir, "Gemfile") && hasWeb:
		return detectRuby(dir, o)
	case exists(dir, "index.html"), exists(dir, "public", "index.html"),
		o.OutputDirectory != "" && isDir(dir, o.OutputDirectory):
		return detectStatic(dir, o)
	case exists(dir, "Gemfile"):
		return detectRuby(dir, o) // explains what is missing
	}
	return Plan{}, ErrUnknownProject
}

// detectForced builds the kind or preset the framework setting names, even
// when detection would pick something else (or ignore a Dockerfile).
func detectForced(dir string, o Options) (Plan, error) {
	missing := func(what string) (Plan, error) {
		return Plan{}, fmt.Errorf("the framework setting is %q, but the project has no %s",
			FrameworkName(o.Framework), what)
	}
	switch o.Framework {
	case KindDockerfile:
		if !exists(dir, "Dockerfile") {
			return missing("Dockerfile")
		}
		return Plan{Kind: KindDockerfile, Framework: "Dockerfile", Summary: "Dockerfile found in repository"}, nil
	case KindGo:
		if !exists(dir, "go.mod") {
			return missing("go.mod")
		}
		return detectGo(dir, o)
	case KindPython:
		if !isPython(dir) {
			return missing("requirements.txt or pyproject.toml")
		}
		return detectPython(dir, o)
	case KindRuby:
		if !exists(dir, "Gemfile") {
			return missing("Gemfile")
		}
		return detectRuby(dir, o)
	case KindJava:
		if javaBuildTool(dir) == "" {
			return missing("pom.xml or build.gradle")
		}
		return detectJava(dir, o)
	case KindStatic:
		return detectStatic(dir, o)
	}
	if FrameworkName(o.Framework) == "" {
		return Plan{}, fmt.Errorf("unknown framework setting %q", o.Framework)
	}
	if !exists(dir, "package.json") {
		return missing("package.json")
	}
	return detectNode(dir, o)
}

// ---- Procfile ----

var procfileLine = regexp.MustCompile(`^web\s*:\s*(.+)$`)

// procfileWeb returns the command of the "web:" process in a Heroku-style
// Procfile. Go and static images have no shell, so only the other kinds use it.
func procfileWeb(dir string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "Procfile"))
	if err != nil {
		return "", false
	}
	for _, l := range strings.Split(string(b), "\n") {
		if m := procfileLine.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			return strings.TrimSpace(m[1]), true
		}
	}
	return "", false
}

// startCommand is the start command setting, else the Procfile's web
// process; how describes it for the summary.
func startCommand(dir string, o Options) (cmd, how string) {
	if o.StartCommand != "" {
		return o.StartCommand, "start command: " + o.StartCommand
	}
	if web, ok := procfileWeb(dir); ok {
		return web, "Procfile web: " + web
	}
	return "", ""
}

// shellCmd runs command through sh so $PORT and other variables expand.
func shellCmd(command string) string {
	return fmt.Sprintf("CMD [\"sh\", \"-c\", %q]\n", command)
}

// ---- static ----

// nginx-unprivileged runs as non-root and listens on 8080 out of the box.
const nginxImage = "nginxinc/nginx-unprivileged:1.27-alpine"

// detectStatic serves the output directory setting, the root or public/.
func detectStatic(dir string, o Options) (Plan, error) {
	root, what := "", ""
	switch {
	case o.OutputDirectory != "":
		if !isDir(dir, o.OutputDirectory) {
			return Plan{}, fmt.Errorf("output directory %q not found in the project", o.OutputDirectory)
		}
		root, what = o.OutputDirectory, o.OutputDirectory+"/ (output directory setting)"
	case exists(dir, "index.html"):
		root, what = ".", "index.html"
	case exists(dir, "public", "index.html"):
		root, what = "public", "public/index.html"
	default:
		return Plan{}, errors.New("static site has no index.html or public/index.html; set the output directory")
	}
	return Plan{Kind: KindStatic, Framework: "Static", Summary: "static site (" + what + ")",
		Dockerfile: staticDockerfile(root)}, nil
}

func staticDockerfile(root string) string {
	src := "."
	if root != "." {
		src = root + "/"
	}
	return fmt.Sprintf(`# generated by paas: static site
FROM %s
COPY %s /usr/share/nginx/html/
EXPOSE %d
`, nginxImage, src, Port)
}

// ---- go ----

const defaultGoVersion = "1.24"

var goDirective = regexp.MustCompile(`^go\s+(\d+\.\d+)`)

// detectGo builds a static binary and runs it on distroless. A build command
// setting replaces `go build` and must write the binary to /out/app; a start
// command runs without a shell (distroless has none), split on spaces.
func detectGo(dir string, o Options) (Plan, error) {
	version := defaultGoVersion
	f, err := os.Open(filepath.Join(dir, "go.mod"))
	if err != nil {
		return Plan{}, err
	}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := goDirective.FindStringSubmatch(strings.TrimSpace(sc.Text())); m != nil {
			version = m[1]
			break
		}
	}
	f.Close()

	build := ""
	how := ""
	if o.BuildCommand != "" {
		build = o.BuildCommand
		how = "build command: " + build
	} else {
		pkg, err := goMainPackage(dir)
		if err != nil {
			return Plan{}, err
		}
		build = `CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ` + pkg
		how = "package " + pkg
	}

	manifests := "go.mod"
	if exists(dir, "go.sum") {
		manifests += " go.sum"
	}
	var df strings.Builder
	fmt.Fprintf(&df, "# generated by paas: go %s\n", version)
	fmt.Fprintf(&df, "FROM golang:%s-alpine AS build\nWORKDIR /src\n", version)
	if o.InstallCommand != "" {
		// A custom install step may need any file of the project.
		df.WriteString("COPY . .\n")
		fmt.Fprintf(&df, "RUN --mount=type=cache,target=/go/pkg/mod %s\n", o.InstallCommand)
	} else {
		fmt.Fprintf(&df, "COPY %s ./\n", manifests)
		df.WriteString("RUN --mount=type=cache,target=/go/pkg/mod go mod download\n")
		df.WriteString("COPY . .\n")
	}
	fmt.Fprintf(&df, "RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \\\n    %s\n", build)
	if o.BuildCommand != "" {
		df.WriteString(`RUN test -x /out/app || { echo "the build command must write the binary to /out/app" >&2; exit 1; }` + "\n")
	}
	df.WriteString("FROM gcr.io/distroless/static:nonroot\nCOPY --from=build /out/app /app\n")
	fmt.Fprintf(&df, "ENV PORT=%[1]d\nEXPOSE %[1]d\nUSER 65532:65532\n", Port)
	if o.StartCommand != "" {
		args, _ := json.Marshal(strings.Fields(o.StartCommand))
		fmt.Fprintf(&df, "ENTRYPOINT %s\n", args)
		how += ", start command: " + o.StartCommand
	} else {
		df.WriteString(`ENTRYPOINT ["/app"]` + "\n")
	}
	return Plan{Kind: KindGo, Framework: "Go", Summary: fmt.Sprintf("go %s (%s)", version, how),
		Dockerfile: df.String()}, nil
}

// goMainPackage finds the package to build: the repo root if it is a main
// package, otherwise the only directory under cmd/.
func goMainPackage(dir string) (string, error) {
	if isMainPackage(dir) {
		return ".", nil
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "cmd"))
	var mains []string
	for _, e := range entries {
		if e.IsDir() && isMainPackage(filepath.Join(dir, "cmd", e.Name())) {
			mains = append(mains, "./cmd/"+e.Name())
		}
	}
	sort.Strings(mains)
	switch len(mains) {
	case 1:
		return mains[0], nil
	case 0:
		return "", errors.New("no main package found in the repository root or cmd/*")
	}
	return "", fmt.Errorf("several main packages found (%s); add a Dockerfile or set the build command to choose one",
		strings.Join(mains, ", "))
}

var packageMain = regexp.MustCompile(`(?m)^package\s+main\b`)

func isMainPackage(dir string) bool {
	files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		if b, err := os.ReadFile(f); err == nil && packageMain.Match(b) {
			return true
		}
	}
	return false
}

func exists(dir string, elem ...string) bool {
	_, err := os.Stat(filepath.Join(append([]string{dir}, elem...)...))
	return err == nil
}

// isDir reports whether rel ("a/b", slash-separated) is a directory under dir.
func isDir(dir, rel string) bool {
	fi, err := os.Stat(filepath.Join(dir, filepath.FromSlash(rel)))
	return err == nil && fi.IsDir()
}
