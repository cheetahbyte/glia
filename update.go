package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strings"
)

const (
	modulePath  = "github.com/cheetahbyte/glia"
	packagePath = modulePath
	// pseudoHash is the length of the commit at the end of a pseudo-version.
	pseudoHash = 12
)

const shortSHA = 7

func updateCommand(args []string) error {
	if len(args) > 0 {
		return errors.New("usage: glia update")
	}
	return update()
}

func versionCommand([]string) error {
	commit, modified := buildCommit()
	suffix := ""
	if modified {
		suffix = " (modified)"
	}
	fmt.Println("glia @", short(commit)+suffix)
	return nil
}

// update installs the latest commit of the default branch with go install
// into a directory next to the running binary, then renames the new binary
// over it, so a running instance keeps its file and a failure changes nothing.
func update() error {
	env, err := goEnv()
	if err != nil {
		return err
	}
	latest, err := goRun(env, "", "list", "-m", "-f", "{{.Version}}", modulePath+"@latest")
	if err != nil {
		return err
	}
	current, modified := buildCommit()
	next := versionCommit(latest)
	if next == "" {
		next = latest
	}
	if !modified && (mainVersion() == latest || current != "" && sameCommit(current, next)) {
		fmt.Println("glia is up to date @", short(next))
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	bin, err := os.MkdirTemp(filepath.Dir(exe), ".glia-update-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(bin)
	if _, err := goRun(append(env, "GOBIN="+bin), bin, "install", "-ldflags=-s -w", packagePath+"@"+latest); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(bin, "glia"), exe); err != nil {
		return err
	}
	fmt.Printf("glia updated %s to %s\n", short(current), short(next))
	return nil
}

// goEnv is the environment for the go commands. The module is added to
// GOPRIVATE to skip the public proxy, whose answer for the latest commit lags.
func goEnv() ([]string, error) {
	private, err := goRun(os.Environ(), "", "env", "GOPRIVATE")
	if err != nil {
		return nil, err
	}
	if private != "" {
		private += ","
	}
	return append(os.Environ(), "GOPRIVATE="+private+modulePath), nil
}

func goRun(env []string, dir string, args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	cmd.Env, cmd.Dir = env, dir
	if dir == "" {
		cmd.Dir = os.TempDir()
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go %s: %v\n%s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// buildCommit is the commit this binary was built from, and whether its
// checkout had uncommitted changes: the VCS stamp of a build in a checkout,
// or the commit in the pseudo-version of a go install.
func buildCommit() (commit string, modified bool) {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			commit = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if commit != "" {
		return commit, modified
	}
	return versionCommit(info.Main.Version), false
}

func mainVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		return info.Main.Version
	}
	return ""
}

// versionCommit is the commit of a pseudo-version such as
// v0.0.0-20261114120000-8df2afb1c2d3, or "" for any other version.
func versionCommit(version string) string {
	version, _, _ = strings.Cut(version, "+")
	i := strings.LastIndexByte(version, '-')
	if i < 0 || len(version)-i-1 != pseudoHash {
		return ""
	}
	return version[i+1:]
}

func sameCommit(a, b string) bool {
	n := min(len(a), len(b))
	return n > 0 && a[:n] == b[:n]
}

func short(commit string) string {
	switch {
	case commit == "":
		return "dev"
	case strings.HasPrefix(commit, "v"):
		return commit
	}
	return commit[:min(shortSHA, len(commit))]
}
