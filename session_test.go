package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFetchSessionsRecoversClippedRailwayOutput(t *testing.T) {
	for _, mode := range []string{"clip", "chunk-truncated-once"} {
		t.Run(mode, func(t *testing.T) {
			r, remote := fakeDownloadRailway(t, mode)
			task := task{ID: "clipped", Sandbox: "local-sandbox", Dir: "/work/repo"}
			name := "2026-01-01T00-00-00_session-id.jsonl"
			content := bytes.Repeat([]byte("{\"type\":\"message\",\"text\":\"session data\"}\n"), 5000)
			rel := filepath.Join("sessions", sessionDirName(task.Dir), name)
			writeDownloadTestFile(t, filepath.Join(remote, ".axon", rel), content)
			newest, err := fetchSessions(task, r)
			if err != nil {
				t.Fatalf("fetch clipped archive: %v", err)
			}
			if newest != "session-id" {
				t.Fatalf("newest = %q, want session-id", newest)
			}
			got, err := os.ReadFile(filepath.Join(axonHome(), rel))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, content) {
				t.Fatalf("session content differs: got %d bytes, want %d", len(got), len(content))
			}
			manifest, err := os.ReadFile(filepath.Join(remote, "last-manifest"))
			if err != nil {
				t.Fatal(err)
			}
			var archive struct {
				Size int `json:"size"`
			}
			if err := json.Unmarshal(manifest, &archive); err != nil {
				t.Fatal(err)
			}
			want := (archive.Size + downloadChunkSize - 1) / downloadChunkSize
			if mode == "chunk-truncated-once" {
				want++
			}
			if got := downloadRequestCount(t, remote, "chunk"); got != want {
				t.Fatalf("chunk requests = %d, want %d", got, want)
			}
		})
	}
}

func TestFetchSessionsRejectsDamageBeforeWriting(t *testing.T) {
	for _, mode := range []string{"chunk-truncated", "chunk-dropped", "chunk-corrupt"} {
		t.Run(mode, func(t *testing.T) {
			r, remote := fakeDownloadRailway(t, mode)
			task := task{ID: "damaged", Sandbox: "local-sandbox", Dir: "/work/repo"}
			rel := filepath.Join("sessions", sessionDirName(task.Dir), "2026-01-01_session-id.jsonl")
			original := []byte("original local history\n")
			writeDownloadTestFile(t, filepath.Join(axonHome(), rel), original)
			writeDownloadTestFile(t, filepath.Join(remote, ".axon", rel), append(bytes.Clone(original), bytes.Repeat([]byte("continued\n"), 5000)...))
			newest, err := fetchSessions(task, r)
			if err == nil || !strings.Contains(err.Error(), "failed after 3 attempts") || newest != "" {
				t.Fatalf("fetch = %q, %v; want an integrity error and no session", newest, err)
			}
			got, err := os.ReadFile(filepath.Join(axonHome(), rel))
			if err != nil || !bytes.Equal(got, original) {
				t.Fatalf("local session changed after a damaged transfer: %q, %v", got, err)
			}
			if got := downloadRequestCount(t, remote, "chunk"); got != 3 {
				t.Fatalf("chunk requests = %d, want 3", got)
			}
		})
	}
}

func TestFetchSessionsConcurrentArchives(t *testing.T) {
	r, remote := fakeDownloadRailway(t, "concurrent")
	first := task{ID: "first", Sandbox: "local-sandbox", Dir: "/work/first"}
	second := task{ID: "second", Sandbox: "local-sandbox", Dir: "/work/second"}
	for _, task := range []task{first, second} {
		file := filepath.Join(remote, ".axon", "sessions", sessionDirName(task.Dir), "2026-01-01_session-id.jsonl")
		writeDownloadTestFile(t, file, []byte(task.ID+" session\n"))
	}
	done := make(chan error, 1)
	go func() {
		_, err := fetchSessions(first, r)
		done <- err
	}()
	defer func() {
		if err := os.WriteFile(filepath.Join(remote, "release-manifest"), nil, 0o600); err != nil {
			t.Error(err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("first concurrent fetch: %v", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("first concurrent fetch did not finish")
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(remote, "manifest-ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first fetch did not reach its manifest")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := fetchSessions(second, r); err != nil {
		t.Errorf("second concurrent fetch: %v", err)
	}
}

func TestFetchSessionsRemovesTemporaryArchive(t *testing.T) {
	for _, mode := range []string{"", "chunk-corrupt"} {
		t.Run(mode, func(t *testing.T) {
			r, remote := fakeDownloadRailway(t, mode)
			task := task{ID: "cleanup", Sandbox: "local-sandbox", Dir: "/work/repo"}
			file := filepath.Join(remote, ".axon", "sessions", sessionDirName(task.Dir), "2026-01-01_session-id.jsonl")
			writeDownloadTestFile(t, file, []byte("session data\n"))
			_, err := fetchSessions(task, r)
			if (err != nil) != (mode != "") {
				t.Fatalf("fetch error = %v for mode %q", err, mode)
			}
			archives, err := filepath.Glob(filepath.Join(remote, "glia", "sessions*.tar"))
			if err != nil || len(archives) != 0 {
				t.Fatalf("temporary archives remain: %v, %v", archives, err)
			}
		})
	}
}

func TestFetchSessionsAbsent(t *testing.T) {
	r, remote := fakeDownloadRailway(t, "")
	newest, err := fetchSessions(task{Sandbox: "local-sandbox", Dir: "/work/missing"}, r)
	if err != nil || newest != "" {
		t.Fatalf("absent sessions = %q, %v", newest, err)
	}
	if got := downloadRequestCount(t, remote, "manifest"); got != 0 {
		t.Fatalf("manifest requests for absent sessions = %d", got)
	}
}

func TestFetchSessionsRequiresArchiveStatus(t *testing.T) {
	r, remote := fakeDownloadRailway(t, "exec-empty")
	task := task{Sandbox: "local-sandbox", Dir: "/work/repo"}
	writeDownloadTestFile(t, filepath.Join(remote, ".axon", "sessions", sessionDirName(task.Dir), "2026-01-01_session-id.jsonl"), []byte("session\n"))
	newest, err := fetchSessions(task, r)
	if err == nil || !strings.Contains(err.Error(), "unexpected response") || newest != "" {
		t.Fatalf("missing status = %q, %v; want an explicit error", newest, err)
	}
}

func TestFetchSessionsReportsTarFailure(t *testing.T) {
	r, remote := fakeDownloadRailway(t, "")
	task := task{Sandbox: "local-sandbox", Dir: "/work/repo"}
	writeDownloadTestFile(t, filepath.Join(remote, ".axon", "sessions", sessionDirName(task.Dir), "2026-01-01_session-id.jsonl"), []byte("session\n"))
	tar := filepath.Join(filepath.SplitList(os.Getenv("PATH"))[0], "tar")
	writeDownloadTestFile(t, tar, []byte("#!/bin/sh\nprintf partial > \"$2\"\nexit 17\n"))
	if err := os.Chmod(tar, 0o700); err != nil {
		t.Fatal(err)
	}
	newest, err := fetchSessions(task, r)
	if err == nil || !strings.Contains(err.Error(), "create the session archive") || newest != "" {
		t.Fatalf("failed tar = %q, %v; want an explicit error", newest, err)
	}
	archives, err := filepath.Glob(filepath.Join(remote, "glia", "sessions*.tar"))
	if err != nil || len(archives) != 0 {
		t.Fatalf("partial archives remain: %v, %v", archives, err)
	}
}

func TestFetchSessionsPreservesHistoryAndPaths(t *testing.T) {
	r, remote := fakeDownloadRailway(t, "")
	task := task{ID: "conflicts", Sandbox: "local-sandbox", Dir: "/work/repo ' $(touch should-not-exist)"}
	dir := sessionDirName(task.Dir)
	remoteSessions := filepath.Join(remote, ".axon", "sessions", dir)
	localSessions := filepath.Join(axonHome(), "sessions", dir)
	for name, content := range map[string]string{
		"2026-01-01_continued.jsonl": "one\ntwo\n",
		"2026-01-02_conflict.jsonl":  "remote\n",
		"2026-01-03_newest.jsonl":    "new session\n",
		"continued.blobs/existing":   "remote blob",
		"continued.blobs/new":        "new blob",
		"continued.handoff.md":       "remote handoff",
		"newest.handoff.md":          "new handoff",
		"ignored.txt":                "not session data",
	} {
		writeDownloadTestFile(t, filepath.Join(remoteSessions, name), []byte(content))
	}
	for name, content := range map[string]string{
		"2026-01-01_continued.jsonl": "one\n",
		"2026-01-02_conflict.jsonl":  "local\n",
		"continued.blobs/existing":   "local blob",
		"continued.handoff.md":       "local handoff",
	} {
		writeDownloadTestFile(t, filepath.Join(localSessions, name), []byte(content))
	}
	if err := os.Symlink(filepath.Join(remoteSessions, "2026-01-03_newest.jsonl"), filepath.Join(remoteSessions, "2026-01-04_link.jsonl")); err != nil {
		t.Fatal(err)
	}
	newest, err := fetchSessions(task, r)
	if err != nil || newest != "newest" {
		t.Fatalf("fetch = %q, %v; want newest", newest, err)
	}
	for name, content := range map[string]string{
		"2026-01-01_continued.jsonl": "one\ntwo\n",
		"2026-01-02_conflict.jsonl":  "local\n",
		"2026-01-03_newest.jsonl":    "new session\n",
		"continued.blobs/existing":   "local blob",
		"continued.blobs/new":        "new blob",
		"continued.handoff.md":       "local handoff",
		"newest.handoff.md":          "new handoff",
	} {
		got, err := os.ReadFile(filepath.Join(localSessions, name))
		if err != nil || string(got) != content {
			t.Errorf("%s = %q, %v; want %q", name, got, err, content)
		}
	}
	conflict, err := os.ReadFile(filepath.Join(home(), "tasks", task.ID+"-sessions", "2026-01-02_conflict.jsonl"))
	if err != nil || string(conflict) != "remote\n" {
		t.Errorf("conflict copy = %q, %v", conflict, err)
	}
	for _, name := range []string{"ignored.txt", "2026-01-04_link.jsonl"} {
		if _, err := os.Lstat(filepath.Join(localSessions, name)); !os.IsNotExist(err) {
			t.Errorf("untrusted archive entry %s was ingested: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(remote, "should-not-exist")); !os.IsNotExist(err) {
		t.Fatalf("session directory shell metacharacters executed: %v", err)
	}
}
