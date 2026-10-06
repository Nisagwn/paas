package build

import (
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/nisagwn/paas/internal/store"
)

// Faz 16: project settings (Vercel's "Build & Development Settings") and
// framework presets.

// Framework presets of Node projects. The kinds (KindNode, KindGo, …) are
// valid Framework settings too.
const (
	FrameworkNext           = "nextjs"
	FrameworkVite           = "vite"
	FrameworkCreateReactApp = "create-react-app"
	FrameworkAstro          = "astro"
	FrameworkSvelteKit      = "sveltekit"
	FrameworkNuxt           = "nuxt"
	FrameworkRemix          = "remix"
	FrameworkReactRouter    = "react-router"
	FrameworkNestJS         = "nestjs"
	FrameworkExpress        = "express"
)

// Framework is a value of the "framework" setting.
type Framework struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Frameworks lists every value the framework setting accepts besides ""
// (auto-detect), in the order a settings form shows them.
var Frameworks = []Framework{
	{FrameworkNext, "Next.js"},
	{FrameworkVite, "Vite"},
	{FrameworkCreateReactApp, "Create React App"},
	{FrameworkAstro, "Astro"},
	{FrameworkSvelteKit, "SvelteKit"},
	{FrameworkNuxt, "Nuxt"},
	{FrameworkRemix, "Remix"},
	{FrameworkReactRouter, "React Router"},
	{FrameworkNestJS, "NestJS"},
	{FrameworkExpress, "Express"},
	{KindNode, "Node.js"},
	{KindGo, "Go"},
	{KindPython, "Python"},
	{KindRuby, "Ruby"},
	{KindJava, "Java"},
	{KindStatic, "Static"},
	{KindDockerfile, "Dockerfile"},
}

// FrameworkName is the display name of a framework id ("" for unknown ids).
func FrameworkName(id string) string {
	for _, f := range Frameworks {
		if f.ID == id {
			return f.Name
		}
	}
	return ""
}

// Options are the build settings Detect honours. RootDirectory is not here:
// the caller passes the root directory itself as Detect's dir.
type Options struct {
	Framework       string
	InstallCommand  string
	BuildCommand    string
	StartCommand    string
	OutputDirectory string
	NodeVersion     string

	// noProcfile: a paas.yaml defines the processes, so the Procfile's web
	// line is not the start command (Faz 20).
	noProcfile bool
}

// OptionsFrom converts stored settings.
func OptionsFrom(s store.BuildSettings) Options {
	return Options{
		Framework: s.Framework, InstallCommand: s.InstallCommand, BuildCommand: s.BuildCommand,
		StartCommand: s.StartCommand, OutputDirectory: s.OutputDirectory, NodeVersion: s.NodeVersion,
	}
}

// overridesCommands reports whether any setting changes the generated Dockerfile.
func (o Options) overridesCommands() bool {
	return o.InstallCommand != "" || o.BuildCommand != "" || o.StartCommand != "" ||
		o.OutputDirectory != "" || o.NodeVersion != ""
}

const (
	maxDirLen     = 255
	maxCommandLen = 1024
)

var (
	dirSegment  = regexp.MustCompile(`^[A-Za-z0-9._@+-]+$`)
	nodeVersion = regexp.MustCompile(`^(\d{1,2}(\.\d{1,3}){0,2}|lts)$`)
)

// NormalizeSettings validates settings and returns them cleaned up: spaces
// trimmed, "./web/" → "web", "." → "". The error text is meant for the user.
func NormalizeSettings(s store.BuildSettings) (store.BuildSettings, error) {
	var err error
	if s.RootDirectory, err = cleanDir("root_directory", s.RootDirectory); err != nil {
		return s, err
	}
	if s.OutputDirectory, err = cleanDir("output_directory", s.OutputDirectory); err != nil {
		return s, err
	}
	s.Framework = strings.TrimSpace(s.Framework)
	if s.Framework != "" && FrameworkName(s.Framework) == "" {
		ids := make([]string, len(Frameworks))
		for i, f := range Frameworks {
			ids[i] = f.ID
		}
		return s, fmt.Errorf("framework must be empty (auto-detect) or one of: %s", strings.Join(ids, ", "))
	}
	for _, c := range []struct {
		name string
		v    *string
	}{{"install_command", &s.InstallCommand}, {"build_command", &s.BuildCommand}, {"start_command", &s.StartCommand}} {
		*c.v = strings.TrimSpace(*c.v)
		if err := checkCommand(c.name, *c.v); err != nil {
			return s, err
		}
	}
	s.NodeVersion = strings.TrimPrefix(strings.TrimSpace(s.NodeVersion), "v")
	if s.NodeVersion != "" && !nodeVersion.MatchString(s.NodeVersion) {
		return s, errors.New(`node_version must look like "22", "20.18" or "lts"`)
	}
	return s, nil
}

// cleanDir validates a directory relative to the repository (or root directory).
func cleanDir(name, dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", nil
	}
	switch {
	case len(dir) > maxDirLen:
		return "", fmt.Errorf("%s is longer than %d characters", name, maxDirLen)
	case strings.HasPrefix(dir, "/"):
		return "", fmt.Errorf("%s must be relative (no leading \"/\")", name)
	case strings.Contains(dir, `\`):
		return "", fmt.Errorf(`%s must use "/" as the separator`, name)
	}
	for _, seg := range strings.Split(dir, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%s must stay inside the repository (no \"..\")", name)
		}
	}
	dir = path.Clean(dir)
	if dir == "." {
		return "", nil
	}
	for _, seg := range strings.Split(dir, "/") {
		if !dirSegment.MatchString(seg) {
			return "", fmt.Errorf("%s may only contain letters, digits and . _ @ + -", name)
		}
	}
	return dir, nil
}

// checkCommand keeps a command on one Dockerfile line: a newline or a
// trailing backslash would let it add instructions of its own.
func checkCommand(name, cmd string) error {
	if len(cmd) > maxCommandLen {
		return fmt.Errorf("%s is longer than %d characters", name, maxCommandLen)
	}
	for _, r := range cmd {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("%s must be a single line of printable characters", name)
		}
	}
	if strings.HasSuffix(cmd, `\`) {
		return fmt.Errorf("%s must not end with a backslash", name)
	}
	return nil
}
