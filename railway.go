package main

import (
	"bytes"
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
