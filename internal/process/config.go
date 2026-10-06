package process

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"
)

// ConfigFiles are the process configuration files, in the order they are
// looked for. A repository may have only one of them.
var ConfigFiles = []string{"paas.yaml", "paas.yml", "paas.json"}

// maxConfigSize bounds the configuration file read into memory.
const maxConfigSize = 64 << 10

// Config is the process configuration of a checkout.
type Config struct {
	Set Set
	// WebDeclared: the file names the web process, with a command or as
	// "none". Without it the build looks for a start command and, if there
	// is none and the set has workers, deploys without a web process.
	WebDeclared bool
	// Notes are lines for the build log (ignored Procfile entries etc.).
	Notes []string
}

// Load reads the process configuration of the project in dir (the
// repository, or its root directory setting):
//
//  1. paas.yaml, paas.yml or paas.json, if present (only one may exist);
//  2. otherwise a Procfile: "web:" is the web command (the build already
//     uses it as the start command), every other line a worker with one
//     replica; "release:" is ignored (there is no release phase);
//  3. otherwise the default set: the web process only.
//
// Anything invalid is an error that fails the build.
func Load(dir string) (Config, error) {
	var found []string
	for _, f := range ConfigFiles {
		if fi, err := os.Stat(filepath.Join(dir, f)); err == nil && !fi.IsDir() {
			found = append(found, f)
		}
	}
	if len(found) > 1 {
		return Config{}, fmt.Errorf("found %s: keep only one process configuration file", strings.Join(found, " and "))
	}
	if len(found) == 1 {
		c, err := loadFile(dir, found[0])
		if err != nil {
			return Config{}, fmt.Errorf("%s: %w", found[0], err)
		}
		if fi, err := os.Stat(filepath.Join(dir, "Procfile")); err == nil && !fi.IsDir() {
			c.Notes = append(c.Notes, "Procfile ignored: "+found[0]+" defines the processes")
		}
		return c, nil
	}
	c, err := loadProcfile(dir)
	if err != nil {
		return Config{}, fmt.Errorf("Procfile: %w", err)
	}
	return c, nil
}

func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxConfigSize+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxConfigSize {
		return nil, fmt.Errorf("larger than %d KiB", maxConfigSize>>10)
	}
	return b, nil
}

// ---- paas.yaml ----

type fileSpec struct {
	Processes map[string]json.RawMessage `json:"processes"`
	Crons     []cronSpec                 `json:"crons"`
}

type processSpec struct {
	Command  command `json:"command"`
	Replicas *int    `json:"replicas"`
	Previews *bool   `json:"previews"`
}

type cronSpec struct {
	Name     string  `json:"name"`
	Schedule string  `json:"schedule"`
	Command  command `json:"command"`
	Previews *bool   `json:"previews"`
}

// command is a string (run by the image's shell) or a list of strings
// (run as is, without a shell).
type command struct {
	str  string
	list []string
}

func (c *command) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '[' {
		if err := json.Unmarshal(b, &c.list); err != nil {
			return errors.New("command must be a string or a list of strings")
		}
		if len(c.list) == 0 {
			return errors.New("command list is empty")
		}
		for _, a := range c.list {
			if a == "" {
				return errors.New("command list has an empty item")
			}
		}
		c.str = strings.Join(c.list, " ")
		return nil
	}
	if err := json.Unmarshal(b, &c.str); err != nil {
		return errors.New("command must be a string or a list of strings")
	}
	return nil
}

// decodeStrict decodes JSON rejecting unknown fields.
func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after the value")
	}
	return nil
}

func loadFile(dir, name string) (Config, error) {
	raw, err := readLimited(filepath.Join(dir, name))
	if err != nil {
		return Config{}, err
	}
	data := raw
	if !strings.HasSuffix(name, ".json") {
		// Duplicate keys are an error; YAML-only types become JSON.
		if data, err = yaml.YAMLToJSONStrict(raw); err != nil {
			return Config{}, cleanYAMLError(err)
		}
	}
	if len(bytes.TrimSpace(data)) == 0 || string(bytes.TrimSpace(data)) == "null" {
		return Config{}, errors.New("the file is empty")
	}
	var spec fileSpec
	if err := decodeStrict(data, &spec); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %v (expected top-level keys: processes, crons)", err)
	}

	c := Config{Set: Set{Source: name}}
	names := make([]string, 0, len(spec.Processes))
	for n := range spec.Processes {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		v := bytes.TrimSpace(spec.Processes[n])
		if n == Web {
			c.WebDeclared = true
			if err := c.webSpec(v); err != nil {
				return Config{}, err
			}
			continue
		}
		var ps processSpec
		if len(v) > 0 && v[0] == '"' {
			if err := json.Unmarshal(v, &ps.Command); err != nil {
				return Config{}, fmt.Errorf("process %s: %w", n, err)
			}
		} else if err := decodeStrict(v, &ps); err != nil {
			return Config{}, fmt.Errorf("process %s: %v (allowed keys: command, replicas, previews)", n, err)
		}
		w := Worker{Name: n, Command: ps.Command.str, Exec: ps.Command.list, Replicas: 1}
		if ps.Replicas != nil {
			w.Replicas = *ps.Replicas
		}
		if ps.Previews != nil {
			w.Previews = *ps.Previews
		}
		c.Set.Workers = append(c.Set.Workers, w)
	}
	for i, cs := range spec.Crons {
		if cs.Name == "" {
			return Config{}, fmt.Errorf("cron #%d: name is required", i+1)
		}
		cr := Cron{Name: cs.Name, Schedule: strings.TrimSpace(cs.Schedule), Command: cs.Command.str, Exec: cs.Command.list}
		if cs.Previews != nil {
			cr.Previews = *cs.Previews
		}
		c.Set.Crons = append(c.Set.Crons, cr)
	}
	sort.Slice(c.Set.Crons, func(i, j int) bool { return c.Set.Crons[i].Name < c.Set.Crons[j].Name })
	if err := c.Set.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// webSpec reads processes.web: "none", a command, or {command: …}.
func (c *Config) webSpec(v []byte) error {
	if string(v) == "null" {
		return errors.New(`process web: empty; write "web: none" for an app without an HTTP process`)
	}
	if len(v) > 0 && v[0] == '"' {
		var s string
		json.Unmarshal(v, &s)
		if strings.TrimSpace(s) == "none" {
			c.Set.NoWeb = true
			return nil
		}
		v = []byte(`{"command":` + string(v) + `}`)
	}
	var spec struct {
		Command  *command        `json:"command"`
		Replicas json.RawMessage `json:"replicas"`
		Previews json.RawMessage `json:"previews"`
	}
	if err := decodeStrict(v, &spec); err != nil {
		return fmt.Errorf("process web: %v (allowed key: command)", err)
	}
	if spec.Replicas != nil || spec.Previews != nil {
		return errors.New("process web: replicas and previews cannot be set; the web process runs " +
			"in every deployment and scales to zero when idle")
	}
	if spec.Command == nil {
		return nil // web: {} is the detected start command
	}
	c.Set.WebCommand = spec.Command.str
	c.Set.WebExec = spec.Command.list
	if strings.TrimSpace(c.Set.WebCommand) == "none" && spec.Command.list == nil {
		c.Set.WebCommand, c.Set.NoWeb = "", true
	}
	return nil
}

// sigs.k8s.io/yaml errors start with "error converting YAML to JSON: ".
func cleanYAMLError(err error) error {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "error converting YAML to JSON: ")
	return errors.New("invalid YAML: " + msg)
}

// ---- Procfile ----

var procfileLine = regexp.MustCompile(`^([A-Za-z0-9_-]+)\s*:\s*(.*)$`)

func loadProcfile(dir string) (Config, error) {
	raw, err := readLimited(filepath.Join(dir, "Procfile"))
	if errors.Is(err, os.ErrNotExist) {
		return Config{Set: Default()}, nil
	}
	if err != nil {
		return Config{}, err
	}
	c := Config{Set: Set{Source: SourceProcfile}}
	seen := map[string]bool{}
	for i, l := range strings.Split(string(raw), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		m := procfileLine.FindStringSubmatch(l)
		if m == nil {
			return Config{}, fmt.Errorf("line %d: want \"<name>: <command>\"", i+1)
		}
		name, cmd := m[1], strings.TrimSpace(m[2])
		if seen[name] {
			return Config{}, fmt.Errorf("line %d: process %q is defined twice", i+1, name)
		}
		seen[name] = true
		switch name {
		case Web:
			c.WebDeclared = true
			c.Set.WebCommand = cmd
		case "release":
			c.Notes = append(c.Notes, "Procfile: \"release\" ignored (there is no release phase; "+
				"run migrations at start or as a cron in paas.yaml)")
		default:
			c.Set.Workers = append(c.Set.Workers, Worker{Name: name, Command: cmd, Replicas: 1})
		}
	}
	sort.Slice(c.Set.Workers, func(i, j int) bool { return c.Set.Workers[i].Name < c.Set.Workers[j].Name })
	if c.Set.Empty() && c.Set.WebCommand != "" {
		// A Procfile with only a web line: nothing new, keep the default set
		// but show where the command came from.
		return c, nil
	}
	if err := c.Set.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}
