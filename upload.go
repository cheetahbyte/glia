package main

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// archive collects everything a new sandbox needs into one upload. Names are
// relative to the sandbox's root directory.
type archive struct {
	buf bytes.Buffer
	w   *tar.Writer
}

func newArchive() *archive {
	a := &archive{}
	a.w = tar.NewWriter(&a.buf)
	return a
}

func (a *archive) bytes(name string, data []byte) error {
	if err := a.w.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now()}); err != nil {
		return err
	}
	_, err := a.w.Write(data)
	return err
}

// tree adds a file, or a directory with the regular files below it. A
// symbolic link at local is followed; a missing local adds nothing.
func (a *archive) tree(name, local string) error {
	resolved, err := filepath.EvalSymlinks(local)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return filepath.WalkDir(resolved, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(resolved, path)
		entry := name
		if rel != "." {
			entry = name + "/" + filepath.ToSlash(rel)
		}
		if err := a.w.WriteHeader(&tar.Header{Name: entry, Mode: int64(info.Mode().Perm()), Size: info.Size(), ModTime: info.ModTime()}); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(a.w, file)
		return err
	})
}

func (a *archive) send(r railway, id string) error {
	if err := a.w.Close(); err != nil {
		return err
	}
	_, err := r.exec(id, &a.buf, "tar -C / -xf -")
	return err
}

// Settings that change how axon looks and thinks, safe to carry to another
// machine. Paths and shell settings stay behind.
var carriedSettings = []string{
	"defaultThinking", "enabledModels", "statusLine", "icons", "theme", "accent",
	"header", "showThinking", "highlight", "mermaid", "links",
}

// sandboxSettings is the settings.json for the sandbox: the look of the local
// one, with the chosen model.
func sandboxSettings(model string) []byte {
	local := map[string]json.RawMessage{}
	if data, err := os.ReadFile(filepath.Join(axonHome(), "settings.json")); err == nil {
		json.Unmarshal(data, &local)
	}
	settings := map[string]json.RawMessage{}
	for _, key := range carriedSettings {
		if value, ok := local[key]; ok {
			settings[key] = value
		}
	}
	settings["defaultModel"], _ = json.Marshal(model)
	data, _ := json.Marshal(settings)
	return data
}

// sandboxState is the state.json for the sandbox: the check approvals of
// instruction files inside the repository, which sits at the same path there.
func sandboxState(repo string) []byte {
	var local struct {
		Checks map[string]string `json:"checks"`
	}
	if data, err := os.ReadFile(filepath.Join(axonHome(), "state.json")); err == nil {
		json.Unmarshal(data, &local)
	}
	checks := map[string]string{}
	for path, command := range local.Checks {
		if strings.HasPrefix(path, repo+string(filepath.Separator)) {
			checks[path] = command
		}
	}
	data, _ := json.Marshal(map[string]any{"checks": checks})
	return data
}

// detectSetup is the setup command for a repository that names none, from
// the lock files at its root. The sandbox image has Node.js, npm, pnpm, yarn,
// Python, uv and mise.
func detectSetup(repo string) string {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(repo, name))
		return err == nil
	}
	var steps []string
	if data, err := os.ReadFile(filepath.Join(repo, "go.mod")); err == nil {
		version := ""
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == "go" && version == "" {
				version = fields[1]
			}
			if len(fields) == 2 && fields[0] == "toolchain" {
				version = strings.TrimPrefix(fields[1], "go")
			}
		}
		if version != "" {
			steps = append(steps, "mise use -g go@"+version)
		}
	}
	switch {
	case has("bun.lock") || has("bun.lockb"):
		steps = append(steps, "mise use -g bun@latest", "bun install")
	case has("pnpm-lock.yaml"):
		steps = append(steps, "pnpm install --frozen-lockfile")
	case has("yarn.lock"):
		steps = append(steps, "yarn install --frozen-lockfile")
	case has("package-lock.json"):
		steps = append(steps, "npm ci")
	}
	if has("uv.lock") {
		steps = append(steps, "uv sync")
	}
	return strings.Join(steps, " && ")
}
