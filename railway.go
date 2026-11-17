package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// railway runs the railway command against one project and environment.
type railway struct{ c config }

func (r railway) args(sub []string, rest ...string) []string {
	a := append([]string{"sandbox"}, sub...)
	a = append(a, "-p", r.c.Project, "-e", r.c.Environment)
	return append(a, rest...)
}

func (r railway) run(stdin io.Reader, args []string) (string, error) {
	cmd := exec.Command("railway", args...)
	cmd.Stdin = stdin
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("railway %s: %s", strings.Join(args[:2], " "), lastLines(errOut.String()+out.String(), 15))
	}
	return out.String(), nil
}

// create starts a sandbox on the soma service's private network. Railway
// resolves the token and the address itself, so glia never reads them.
func (r railway) create() (string, error) {
	created, err := r.run(nil, r.args([]string{"create"},
		"--private-network",
		"--idle-timeout-minutes", fmt.Sprint(r.c.IdleMinutes),
		"--variable", "SOMA_TOKEN="+r.c.SomaService+".SOMA_TOKEN",
		"--variable", "SOMA_HOST="+r.c.SomaService+".RAILWAY_PRIVATE_DOMAIN",
		"--json"))
	if err != nil {
		return "", err
	}
	var sandbox struct {
		ID string `json:"id"`
	}
	begin := strings.Index(created, "{")
	if begin < 0 || json.Unmarshal([]byte(created[begin:]), &sandbox) != nil || sandbox.ID == "" {
		return "", fmt.Errorf("unexpected answer from sandbox create: %s", lastLines(created, 6))
	}
	return sandbox.ID, nil
}

func (r railway) waitRunning(id string) error {
	var err error
	for range 45 {
		if _, err = r.exec(id, nil, "true"); err == nil {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("sandbox %s did not start: %w", id, err)
}

func (r railway) exec(id string, stdin io.Reader, script string) (string, error) {
	return r.run(stdin, r.args([]string{"exec", "--id", id}, "--", script))
}

const downloadChunkSize = 32 * 1024

func (r railway) download(id, remotePath string) ([]byte, error) {
	manifestScript := `import hashlib, json, sys
h = hashlib.sha256()
size = 0
with open(sys.argv[1], "rb") as f:
    for chunk in iter(lambda: f.read(65536), b""):
        h.update(chunk)
        size += len(chunk)
print(json.dumps({"size": size, "sha256": h.hexdigest()}))
`
	out, err := r.downloadOutput(id, "python3 -c "+quote(manifestScript)+" "+quote(remotePath), 1024)
	if err != nil {
		return nil, fmt.Errorf("download %q: read manifest: %w", remotePath, err)
	}
	var manifest struct {
		Size   *int64 `json:"size"`
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal([]byte(out), &manifest); err != nil || manifest.Size == nil || *manifest.Size < 0 {
		return nil, fmt.Errorf("download %q: invalid size/checksum manifest", remotePath)
	}
	expected, err := hex.DecodeString(manifest.SHA256)
	if err != nil || len(expected) != sha256.Size {
		return nil, fmt.Errorf("download %q: invalid SHA-256 manifest", remotePath)
	}
	chunkScript := `import base64, hashlib, json, sys
with open(sys.argv[1], "rb") as f:
    f.seek(int(sys.argv[2]))
    chunk = f.read(int(sys.argv[3]))
print(json.dumps({"data": base64.b64encode(chunk).decode("ascii"), "sha256": hashlib.sha256(chunk).hexdigest()}))
`
	var data bytes.Buffer
	for offset := int64(0); offset < *manifest.Size; {
		size := int(min(int64(downloadChunkSize), *manifest.Size-offset))
		script := "python3 -c " + quote(chunkScript) + " " + quote(remotePath) + fmt.Sprintf(" %d %d", offset, size)
		var chunk []byte
		var chunkErr error
		for range 3 {
			out, chunkErr = r.downloadOutput(id, script, 2*downloadChunkSize)
			if chunkErr == nil {
				chunk, chunkErr = decodeDownloadChunk(out, size)
			}
			if chunkErr == nil {
				break
			}
		}
		if chunkErr != nil {
			return nil, fmt.Errorf("download %q: chunk at byte %d failed after 3 attempts: %w", remotePath, offset, chunkErr)
		}
		data.Write(chunk)
		offset += int64(size)
	}
	actual := sha256.Sum256(data.Bytes())
	if !bytes.Equal(actual[:], expected) {
		return nil, fmt.Errorf("download %q: SHA-256 mismatch; the file changed or the transfer was corrupted", remotePath)
	}
	return data.Bytes(), nil
}

func decodeDownloadChunk(out string, size int) ([]byte, error) {
	var response struct {
		Data   string `json:"data"`
		SHA256 string `json:"sha256"`
	}
	if err := json.Unmarshal([]byte(out), &response); err != nil {
		return nil, fmt.Errorf("invalid chunk response")
	}
	chunk, err := base64.StdEncoding.Strict().DecodeString(response.Data)
	if err != nil {
		return nil, fmt.Errorf("invalid chunk base64")
	}
	if len(chunk) != size {
		return nil, fmt.Errorf("short or oversized chunk: got %d bytes, want %d", len(chunk), size)
	}
	expected, err := hex.DecodeString(response.SHA256)
	actual := sha256.Sum256(chunk)
	if err != nil || !bytes.Equal(actual[:], expected) {
		return nil, fmt.Errorf("chunk SHA-256 mismatch")
	}
	return chunk, nil
}

type downloadOutput struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
}

func (out *downloadOutput) Write(p []byte) (int, error) {
	n := len(p)
	keep := min(n, out.limit-out.buffer.Len())
	out.buffer.Write(p[:keep])
	out.overflow = out.overflow || keep < n
	return n, nil
}

func (r railway) downloadOutput(id, script string, limit int) (string, error) {
	cmd := exec.Command("railway", r.args([]string{"exec", "--id", id}, "--", script)...)
	out := downloadOutput{limit: limit}
	cmd.Stdout, cmd.Stderr = &out, io.Discard
	err := cmd.Run()
	if out.overflow {
		return "", fmt.Errorf("railway sandbox exec output exceeded %d bytes", limit)
	}
	if err != nil {
		return "", fmt.Errorf("railway sandbox exec: %w", err)
	}
	return out.buffer.String(), nil
}

func lastLines(s string, n int) string {
	var keep []string
	for _, l := range strings.Split(strings.TrimSpace(s), "\n") {
		if !strings.HasPrefix(l, "Warning: Railway sandboxes are experimental") {
			keep = append(keep, l)
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return strings.Join(keep, "\n")
}
