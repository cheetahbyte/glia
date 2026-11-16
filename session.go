package main

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

func axonHome() string {
	if h := os.Getenv("AXON_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".axon")
}

// sessionDirName is axon's folder name for the sessions of one working directory.
func sessionDirName(cwd string) string {
	sep := string(filepath.Separator)
	return "--" + strings.ReplaceAll(strings.TrimPrefix(filepath.Clean(cwd), sep), sep, "-") + "--"
}

// findSession returns the file name and id of a session of cwd: the one with
// the given id, or the newest when id is empty.
func findSession(cwd, id string) (file, sessionID string, err error) {
	dir := filepath.Join(axonHome(), "sessions", sessionDirName(cwd))
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for i := len(names) - 1; i >= 0; i-- {
		_, found, ok := strings.Cut(strings.TrimSuffix(names[i], ".jsonl"), "_")
		if ok && (id == "" || found == id) {
			return names[i], found, nil
		}
	}
	if id != "" {
		return "", "", fmt.Errorf("no axon session %s for %s", id, cwd)
	}
	return "", "", fmt.Errorf("no axon session for %s", cwd)
}

// sessionPaths lists what belongs to a session, relative to axon's sessions folder.
func sessionPaths(cwd, file, id string) []string {
	dir := sessionDirName(cwd)
	return []string{filepath.Join(dir, file), filepath.Join(dir, id+".blobs"), filepath.Join(dir, id+".handoff.md")}
}

// fetchSessions brings the sandbox's axon sessions for the task's directory
// back. The sandbox is not trusted with the local history: a session that
// exists locally is replaced only by one that continues it unchanged.
// It returns the id of the newest session the sandbox holds.
func fetchSessions(t task, r railway) (newest string, err error) {
	if t.Dir == "" {
		return "", nil
	}
	dir := sessionDirName(t.Dir)
	out, err := r.exec(t.Sandbox, nil, "cd /root/.axon/sessions 2>/dev/null || exit 0\n[ -d "+quote("./"+dir)+" ] || exit 0\ntar -cf - "+quote("./"+dir)+" | base64 -w0")
	if err != nil {
		return "", err
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return "", nil
	}
	archive, err := base64.StdEncoding.DecodeString(out)
	if err != nil {
		return "", fmt.Errorf("the session archive arrived damaged: %w", err)
	}
	var newestFile string
	localDir := filepath.Join(axonHome(), "sessions", dir)
	reader := tar.NewReader(bytes.NewReader(archive))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return newest, nil
		}
		if err != nil {
			return newest, err
		}
		name := path.Clean(header.Name)
		rel, inside := strings.CutPrefix(name, dir+"/")
		if header.Typeflag != tar.TypeReg || !inside || !filepath.IsLocal(rel) {
			continue
		}
		content, err := io.ReadAll(reader)
		if err != nil {
			return newest, err
		}
		target := filepath.Join(localDir, filepath.FromSlash(rel))
		existing, readErr := os.ReadFile(target)
		parts := strings.Split(rel, "/")
		switch {
		case len(parts) == 1 && strings.HasSuffix(rel, ".jsonl"):
			_, id, _ := strings.Cut(strings.TrimSuffix(rel, ".jsonl"), "_")
			if rel > newestFile {
				newestFile, newest = rel, id
			}
			switch {
			case readErr != nil:
				if !quiet {
					fmt.Printf("new axon session from the sandbox: axon --resume %s\n", id)
				}
			case bytes.Equal(existing, content):
				continue
			case bytes.HasPrefix(content, existing):
				if !quiet {
					fmt.Printf("axon session continued in the sandbox: axon --resume %s\n", id)
				}
			default:
				target = filepath.Join(home(), "tasks", t.ID+"-sessions", rel)
				fmt.Printf("session %s also changed here, so the sandbox's copy was kept apart: %s\n", id, target)
			}
		case len(parts) > 1 && strings.HasSuffix(parts[0], ".blobs"), len(parts) == 1 && strings.HasSuffix(rel, ".handoff.md"):
			if readErr == nil {
				continue
			}
		default:
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return newest, err
		}
		if err := os.WriteFile(target, content, 0o600); err != nil {
			return newest, err
		}
	}
}
