package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeDownloadRailway(t *testing.T, mode string) (railway, string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote")
	bin := filepath.Join(root, "bin")
	for _, dir := range []string{remote, bin, filepath.Join(root, "home")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launcher := "#!/bin/sh\nexec " + quote(executable) + " -test.run=^TestRailwayDownloadCLIProcess$ -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "railway"), []byte(launcher), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("AXON_HOME", filepath.Join(root, "axon"))
	t.Setenv("GLIA_HOME", filepath.Join(root, "glia"))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("COPYFILE_DISABLE", "1")
	t.Setenv("GLIA_DOWNLOAD_TEST_PROCESS", "1")
	t.Setenv("GLIA_DOWNLOAD_TEST_REMOTE", remote)
	t.Setenv("GLIA_DOWNLOAD_TEST_MODE", mode)
	return railway{config{Project: "local-project", Environment: "local-environment"}}, remote
}

func TestRailwayDownloadCLIProcess(t *testing.T) {
	if os.Getenv("GLIA_DOWNLOAD_TEST_PROCESS") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	args = args[1:]
	if len(args) < 2 || args[0] != "sandbox" || args[1] != "exec" {
		os.Exit(90)
	}
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) != 2 {
		os.Exit(91)
	}
	root := os.Getenv("GLIA_DOWNLOAD_TEST_REMOTE")
	script := strings.ReplaceAll(args[1], "/root/.axon", filepath.Join(root, ".axon"))
	script = strings.ReplaceAll(script, "/root/glia", filepath.Join(root, "glia"))
	if strings.Contains(script, "/root/") {
		os.Exit(92)
	}
	mode := os.Getenv("GLIA_DOWNLOAD_TEST_MODE")
	if mode == "cli-failure" {
		fmt.Fprintln(os.Stdout, "UNTRUSTED_TRANSFER_CONTENT")
		fmt.Fprintln(os.Stderr, "UNTRUSTED_TRANSFER_CONTENT")
		os.Exit(17)
	}
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Dir = root
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			os.Exit(exit.ExitCode())
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(93)
	}
	data := out.Bytes()
	kind := "exec"
	if bytes.Contains(data, []byte(`"size"`)) {
		kind = "manifest"
	} else if bytes.Contains(data, []byte(`"data"`)) {
		kind = "chunk"
	}
	if kind == "manifest" {
		if err := os.WriteFile(filepath.Join(root, "last-manifest"), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(root, "requests")
	previous, _ := os.ReadFile(logPath)
	if err := os.WriteFile(logPath, append(previous, []byte(kind+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if kind == "manifest" && mode == "concurrent" {
		marker, err := os.OpenFile(filepath.Join(root, "manifest-ready"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			marker.Close()
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(filepath.Join(root, "release-manifest")); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("timed out waiting for the other fetch")
				}
				time.Sleep(5 * time.Millisecond)
			}
		}
	}
	mutate := !strings.HasSuffix(mode, "-once") || !bytes.Contains(previous, []byte("chunk\n"))
	mode = strings.TrimSuffix(mode, "-once")
	switch {
	case mode == "clip" && len(data) > 64*1024:
		data = data[:64*1024]
	case kind == "exec" && mode == "exec-empty":
		data = nil
	case kind == "manifest" && strings.HasPrefix(mode, "manifest-"):
		var manifest struct {
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256"`
		}
		if err := json.Unmarshal(data, &manifest); err != nil {
			t.Fatal(err)
		}
		switch mode {
		case "manifest-empty":
			data = nil
		case "manifest-malformed":
			data = []byte(`{"size":`)
		case "manifest-missing-size":
			data = []byte(`{"sha256":"` + manifest.SHA256 + `"}`)
		case "manifest-null-size":
			data = []byte(`{"size":null,"sha256":"` + manifest.SHA256 + `"}`)
		case "manifest-negative-size":
			data = []byte(`{"size":-1,"sha256":"` + manifest.SHA256 + `"}`)
		case "manifest-overflow-size":
			data = []byte(`{"size":9223372036854775808,"sha256":"` + manifest.SHA256 + `"}`)
		case "manifest-bad-digest":
			data = []byte(fmt.Sprintf(`{"size":%d,"sha256":"xyz"}`, manifest.Size))
		case "manifest-overflow":
			data = bytes.Repeat([]byte("x"), 2048)
		default:
			t.Fatalf("unknown mode %s", mode)
		}
	case kind == "manifest" && mode == "file-changed":
		file := filepath.Join(root, "glia", "download.bin")
		original, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, bytes.Repeat([]byte("!"), len(original)), 0o600); err != nil {
			t.Fatal(err)
		}
	case kind == "chunk" && mutate:
		var chunk struct {
			Data   string `json:"data"`
			SHA256 string `json:"sha256"`
		}
		if err := json.Unmarshal(data, &chunk); err != nil {
			t.Fatal(err)
		}
		switch mode {
		case "chunk-empty":
			data = nil
		case "chunk-truncated":
			data = data[:len(data)-4]
		case "chunk-overflow":
			data = bytes.Repeat([]byte("x"), 128*1024)
		case "chunk-invalid-base64", "chunk-corrupt", "chunk-short", "chunk-dropped":
			switch mode {
			case "chunk-invalid-base64":
				chunk.Data = "!" + chunk.Data[1:]
			case "chunk-corrupt":
				decoded, err := base64.StdEncoding.DecodeString(chunk.Data)
				if err != nil {
					t.Fatal(err)
				}
				decoded[0] ^= 0xff
				chunk.Data = base64.StdEncoding.EncodeToString(decoded)
			case "chunk-short":
				decoded, err := base64.StdEncoding.DecodeString(chunk.Data)
				if err != nil {
					t.Fatal(err)
				}
				decoded = decoded[:len(decoded)-1]
				chunk.Data = base64.StdEncoding.EncodeToString(decoded)
				sum := sha256.Sum256(decoded)
				chunk.SHA256 = hex.EncodeToString(sum[:])
			case "chunk-dropped":
				chunk.Data = chunk.Data[4:]
			}
			var err error
			data, err = json.Marshal(chunk)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	os.Stdout.Write(data)
	os.Exit(0)
}

func writeDownloadTestFile(t *testing.T, name string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func downloadRequestCount(t *testing.T, remote, kind string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(remote, "requests"))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, line := range strings.Split(string(data), "\n") {
		if line == kind {
			count++
		}
	}
	return count
}

func TestRailwayDownload(t *testing.T) {
	for _, size := range []int{0, downloadChunkSize, 2*downloadChunkSize + 17} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			r, remote := fakeDownloadRailway(t, "clip")
			content := make([]byte, size)
			for i := range content {
				content[i] = byte(i * 31)
			}
			name := "out ' \" ; $(touch should-not-exist) \n.bin"
			writeDownloadTestFile(t, filepath.Join(remote, "glia", name), content)
			got, err := r.download("local-sandbox", "/root/glia/"+name)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, content) {
				t.Fatalf("download differs: got %d bytes, want %d", len(got), len(content))
			}
			want := (size + downloadChunkSize - 1) / downloadChunkSize
			if got := downloadRequestCount(t, remote, "chunk"); got != want {
				t.Fatalf("chunk requests = %d, want %d", got, want)
			}
			if _, err := os.Stat(filepath.Join(remote, "should-not-exist")); !os.IsNotExist(err) {
				t.Fatalf("shell metacharacters executed: %v", err)
			}
		})
	}
}

func TestRailwayDownloadRetriesDamagedChunks(t *testing.T) {
	for _, mode := range []string{"chunk-truncated-once", "chunk-dropped-once", "chunk-corrupt-once"} {
		t.Run(mode, func(t *testing.T) {
			r, remote := fakeDownloadRailway(t, mode)
			content := bytes.Repeat([]byte("payload"), 50)
			writeDownloadTestFile(t, filepath.Join(remote, "glia", "download.bin"), content)
			got, err := r.download("local-sandbox", "/root/glia/download.bin")
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, content) {
				t.Fatal("retry did not recover the complete original bytes")
			}
			if got := downloadRequestCount(t, remote, "chunk"); got != 2 {
				t.Fatalf("chunk requests = %d, want 2", got)
			}
		})
	}
}

func TestRailwayDownloadRejectsDamage(t *testing.T) {
	cases := []struct {
		mode   string
		want   string
		chunks int
	}{
		{"manifest-empty", "manifest", 0},
		{"manifest-malformed", "manifest", 0},
		{"manifest-missing-size", "manifest", 0},
		{"manifest-null-size", "manifest", 0},
		{"manifest-negative-size", "manifest", 0},
		{"manifest-overflow-size", "manifest", 0},
		{"manifest-bad-digest", "manifest", 0},
		{"manifest-overflow", "exceeded 1024 bytes", 0},
		{"chunk-empty", "invalid chunk response", 3},
		{"chunk-truncated", "invalid chunk response", 3},
		{"chunk-invalid-base64", "invalid chunk base64", 3},
		{"chunk-short", "got 349 bytes, want 350", 3},
		{"chunk-dropped", "got 347 bytes, want 350", 3},
		{"chunk-corrupt", "chunk SHA-256 mismatch", 3},
		{"chunk-overflow", "exceeded 65536 bytes", 3},
		{"file-changed", "the file changed or the transfer was corrupted", 1},
	}
	for _, tc := range cases {
		t.Run(tc.mode, func(t *testing.T) {
			r, remote := fakeDownloadRailway(t, tc.mode)
			writeDownloadTestFile(t, filepath.Join(remote, "glia", "download.bin"), bytes.Repeat([]byte("payload"), 50))
			got, err := r.download("local-sandbox", "/root/glia/download.bin")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
			if got != nil {
				t.Fatalf("returned %d bytes before integrity was verified", len(got))
			}
			if got := downloadRequestCount(t, remote, "chunk"); got != tc.chunks {
				t.Fatalf("chunk requests = %d, want %d", got, tc.chunks)
			}
		})
	}
}

func TestRailwayDownloadDoesNotExposeFailedCLIOutput(t *testing.T) {
	r, _ := fakeDownloadRailway(t, "cli-failure")
	got, err := r.download("local-sandbox", "/root/glia/download.bin")
	if got != nil || err == nil {
		t.Fatalf("download = %v, %v; want nil data and an error", got, err)
	}
	if strings.Contains(err.Error(), "UNTRUSTED_TRANSFER_CONTENT") {
		t.Fatalf("error exposed transfer content: %v", err)
	}
}
