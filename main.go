package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const usage = `usage: glia <command> [arguments]

commands:
  start [-c | --session <id>] [--name <name>] [--model <provider/model>] [prompt]
        Copy the current repository into a new sandbox. With a prompt, axon
        works on it there without you; without one, axon opens for you.
        -c continues this directory's latest axon session in the sandbox.
  list                 Show known tasks.
  status [id]          Show what the task is doing and what it changed.
  attach [id]          Open axon in the sandbox; detaching leaves it running.
  preview [id] [port]  Forward sandbox ports to this machine.
  fetch [id]           Bring the work back as branch glia/<id>, and the axon
                       session with it. Checks out nothing.
  apply [id]           Fetch, then put the sandbox's changes into the working tree.
  stop [--discard] [id]
                       Fetch, then destroy the sandbox.
  remote [--session <id>] [prompt]
                       What axon's /remote runs: continue a session in a
                       sandbox, or return to the sandbox that already has it.
  back [id]            Apply the work, destroy the sandbox, and open axon here
                       on the session. What /local in the sandbox does.
  update               Install the latest commit of the default branch.
  version              Print the commit this glia was built from.

Without an id, a command uses the newest task.
`

type config struct {
	Project     string            `json:"project"`
	Environment string            `json:"environment"`
	SomaService string            `json:"somaService"`
	AxonRepo    string            `json:"axonRepo"`
	Providers   map[string]string `json:"providers"`
	Model       string            `json:"model"`
	IdleMinutes int               `json:"idleMinutes"`
}

// repoConfig is the optional .glia.json at a repository's root.
type repoConfig struct {
	Setup string   `json:"setup"`
	Copy  []string `json:"copy"`
	Ports []string `json:"ports"`
}

type task struct {
	ID      string    `json:"id"`
	Sandbox string    `json:"sandbox"`
	Repo    string    `json:"repo"`
	Dir     string    `json:"dir"`
	Base    string    `json:"base"`
	Session string    `json:"session,omitempty"`
	Prompt  string    `json:"prompt,omitempty"`
	Created time.Time `json:"created"`
	Fetched time.Time `json:"fetched,omitzero"`
	Applied string    `json:"applied,omitempty"`
	Stopped bool      `json:"stopped,omitempty"`
}

// Tasks from before sessions were carried over kept the repository at /root/work.
func (t task) remoteRepo() string {
	if t.Dir == "" {
		return "/root/work"
	}
	return t.Repo
}

func (t task) remoteCwd() string {
	if t.Dir == "" {
		return "/root/work"
	}
	return t.Dir
}

func (t task) branch() string    { return "glia/" + t.ID }
func (t task) baseRef() string   { return "refs/glia/" + t.ID + "/base" }
func (t task) resultRef() string { return "refs/glia/" + t.ID + "/result" }

const (
	remoteDir  = "/root/glia"
	remoteAxon = "/root/bin/axon"
)

// snapshotScript prints a commit that holds the working tree, including
// uncommitted and untracked files, without touching the index or any branch.
const snapshotScript = `set -e
d=$(mktemp -d)
src=$(git rev-parse --git-path index)
if [ -f "$src" ]; then cp "$src" "$d/index"; fi
GIT_INDEX_FILE="$d/index" git add -A
tree=$(GIT_INDEX_FILE="$d/index" git write-tree)
rm -rf "$d"
if head=$(git rev-parse -q --verify HEAD); then
  if [ "$tree" = "$(git rev-parse 'HEAD^{tree}')" ]; then
    echo "$head"
  else
    git commit-tree "$tree" -p "$head" -m "glia: uncommitted work"
  fi
else
  git commit-tree "$tree" -m "glia: uncommitted work"
fi
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	commands := map[string]func([]string) error{
		"start":   start,
		"list":    list,
		"status":  status,
		"attach":  attach,
		"preview": preview,
		"fetch":   fetch,
		"apply":   apply,
		"stop":    stop,
		"remote":  remote,
		"back":    back,
		"update":  updateCommand,
		"version": versionCommand,
	}
	run, ok := commands[os.Args[1]]
	if !ok {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "glia:", err)
		os.Exit(1)
	}
}

func home() string {
	if h := os.Getenv("GLIA_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".glia")
}

func loadConfig() (config, error) {
	var c config
	path := filepath.Join(home(), "config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return c, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	if c.Environment == "" {
		c.Environment = "production"
	}
	if c.IdleMinutes == 0 {
		c.IdleMinutes = 120
	}
	for _, f := range []struct{ name, value string }{
		{"project", c.Project}, {"somaService", c.SomaService}, {"axonRepo", c.AxonRepo},
	} {
		if f.value == "" {
			return c, fmt.Errorf("%s: %q is not set", path, f.name)
		}
	}
	if len(c.Providers) == 0 {
		return c, fmt.Errorf("%s: \"providers\" is empty", path)
	}
	return c, nil
}

func loadRepoConfig(repo string) (repoConfig, error) {
	var rc repoConfig
	path := filepath.Join(repo, ".glia.json")
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return rc, nil
	}
	if err != nil {
		return rc, err
	}
	if err := json.Unmarshal(data, &rc); err != nil {
		return rc, fmt.Errorf("%s: %w", path, err)
	}
	return rc, nil
}

func taskPath(id string) string { return filepath.Join(home(), "tasks", id+".json") }

func saveTask(t task) error {
	if err := os.MkdirAll(filepath.Dir(taskPath(t.ID)), 0o700); err != nil {
		return err
	}
	data, _ := json.MarshalIndent(t, "", "  ")
	return os.WriteFile(taskPath(t.ID), data, 0o600)
}

func loadTasks() ([]task, error) {
	entries, err := os.ReadDir(filepath.Join(home(), "tasks"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	var tasks []task
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(home(), "tasks", e.Name()))
		if err != nil {
			return nil, err
		}
		var t task
		if err := json.Unmarshal(data, &t); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		tasks = append(tasks, t)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Created.Before(tasks[j].Created) })
	return tasks, nil
}

func pickTask(args []string) (task, error) {
	tasks, err := loadTasks()
	if err != nil {
		return task{}, err
	}
	if len(args) > 0 {
		for _, t := range tasks {
			if t.ID == args[0] {
				return t, nil
			}
		}
		return task{}, fmt.Errorf("no task %q", args[0])
	}
	if len(tasks) == 0 {
		return task{}, errors.New("no tasks yet")
	}
	return tasks[len(tasks)-1], nil
}

func taskAndConfig(args []string) (task, config, error) {
	t, err := pickTask(args)
	if err != nil {
		return t, config{}, err
	}
	if t.Stopped {
		return t, config{}, fmt.Errorf("task %s is stopped", t.ID)
	}
	c, err := loadConfig()
	return t, c, err
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func git(dir string, args ...string) (string, error) {
	return gitInput(dir, nil, args...)
}

func gitInput(dir string, stdin io.Reader, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(errOut.String()))
	}
	return strings.TrimRight(out.String(), "\n"), nil
}

func step(format string, a ...any) { fmt.Fprintf(os.Stderr, "· "+format+"\n", a...) }

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

type launchOptions struct {
	name, model, session string
	continueLast         bool
	prompt               string
	home                 bool // return to a local axon when the remote one ends
}

func start(args []string) error {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	var o launchOptions
	fs.StringVar(&o.name, "name", "", "task id, also the branch name after glia/")
	fs.StringVar(&o.model, "model", "", "model as provider/model")
	fs.BoolVar(&o.continueLast, "c", false, "continue this directory's latest axon session")
	fs.StringVar(&o.session, "session", "", "continue this axon session id")
	fs.Parse(args)
	o.prompt = strings.Join(fs.Args(), " ")
	return launch(o)
}

func launch(o launchOptions) error {
	name, model, session, continueLast, prompt := &o.name, &o.model, &o.session, &o.continueLast, o.prompt

	c, err := loadConfig()
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	cdup, err := git(cwd, "rev-parse", "--show-cdup")
	if err != nil {
		return err
	}
	repo := filepath.Clean(filepath.Join(cwd, cdup))
	rc, err := loadRepoConfig(repo)
	if err != nil {
		return err
	}
	t := task{ID: *name, Repo: repo, Dir: cwd, Prompt: prompt, Created: time.Now()}
	if t.ID == "" {
		t.ID = t.Created.Format("0102-150405")
	}
	if _, err := os.Stat(taskPath(t.ID)); err == nil {
		return fmt.Errorf("task %q already exists", t.ID)
	}
	var sessionFile string
	if *continueLast || *session != "" {
		sessionFile, t.Session, err = findSession(cwd, *session)
		if err != nil {
			return err
		}
	}
	chosenModel := *model
	if chosenModel == "" {
		chosenModel = c.Model
	}
	if chosenModel == "" {
		return errors.New("no model: pass --model or set \"model\" in the config")
	}

	step("snapshot %s", repo)
	snapshot := exec.Command("sh", "-c", snapshotScript)
	snapshot.Dir = repo
	snapshot.Stderr = os.Stderr
	out, err := snapshot.Output()
	if err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	t.Base = strings.TrimSpace(string(out))
	if _, err := git(repo, "update-ref", t.baseRef(), t.Base); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "glia-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	bundle := filepath.Join(tmp, "in.bundle")
	if _, err := git(repo, "bundle", "create", "-q", bundle, t.baseRef()); err != nil {
		return err
	}

	step("build axon for linux")
	axon := filepath.Join(home(), "axon-linux-amd64")
	build := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", axon, "./cmd/axon")
	build.Dir = c.AxonRepo
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0", "GOFLAGS=-mod=readonly")
	if msg, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("build axon: %s", lastLines(string(msg), 10))
	}

	step("create sandbox next to %s", c.SomaService)
	r := railway{c}
	t.Sandbox, err = r.create()
	if err != nil {
		return err
	}
	if err := saveTask(t); err != nil {
		return err
	}
	if err := r.waitRunning(t.Sandbox); err != nil {
		return err
	}

	step("upload axon and the repository")
	providers := map[string]any{}
	for entry, upstream := range c.Providers {
		providers[entry] = map[string]string{
			"api": "soma", "baseUrl": "http://__SOMA_HOST__:7662", "apiKey": "$SOMA_TOKEN", "upstream": upstream,
		}
	}
	models, _ := json.Marshal(map[string]any{"providers": providers})
	settings, _ := json.Marshal(map[string]string{"defaultModel": chosenModel})
	for _, f := range []struct{ path, local string }{{remoteAxon, axon}, {remoteDir + "/in.bundle", bundle}} {
		file, err := os.Open(f.local)
		if err != nil {
			return err
		}
		err = r.put(t.Sandbox, f.path, file)
		file.Close()
		if err != nil {
			return err
		}
	}
	for path, content := range map[string]string{
		"/root/.axon/models.json":   string(models),
		"/root/.axon/settings.json": string(settings),
		remoteDir + "/prompt":       prompt,
		remoteDir + "/sid":          t.Session,
	} {
		if err := r.put(t.Sandbox, path, strings.NewReader(content)); err != nil {
			return err
		}
	}
	userName, _ := git(repo, "config", "user.name")
	userEmail, _ := git(repo, "config", "user.email")
	setup := strings.Join([]string{
		"set -e",
		"chmod +x " + remoteAxon,
		`sed -i "s/__SOMA_HOST__/$SOMA_HOST/" /root/.axon/models.json`,
		"git init -q " + quote(repo),
		"cd " + quote(repo),
		"git fetch -q " + remoteDir + "/in.bundle " + t.baseRef() + ":refs/heads/" + t.branch(),
		"git checkout -q " + t.branch(),
		"git config user.name " + quote(userName),
		"git config user.email " + quote(userEmail),
		"mkdir -p " + quote(t.Dir),
	}, "\n")
	if _, err := r.exec(t.Sandbox, nil, setup); err != nil {
		return err
	}
	if len(rc.Copy) > 0 {
		step("copy %s", strings.Join(rc.Copy, ", "))
		if err := r.putTar(t.Sandbox, repo, repo, rc.Copy); err != nil {
			return err
		}
	}
	if t.Session != "" {
		step("copy axon session %s", t.Session)
		if err := r.putTar(t.Sandbox, "/root/.axon/sessions", filepath.Join(axonHome(), "sessions"), sessionPaths(cwd, sessionFile, t.Session)); err != nil {
			return err
		}
	}
	if rc.Setup != "" {
		step("set up: %s", rc.Setup)
		if _, err := r.exec(t.Sandbox, nil, "cd "+quote(repo)+" && { "+rc.Setup+"\n} >"+remoteDir+"/setup.log 2>&1 || { tail -n 15 "+remoteDir+"/setup.log >&2; exit 1; }"); err != nil {
			return fmt.Errorf("setup failed, sandbox kept as task %s: %w", t.ID, err)
		}
	}

	if prompt == "" {
		if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
			return attachTask(t, c, o.home)
		}
		fmt.Printf("task %s is ready. Work in it with: glia attach %s\n", t.ID, t.ID)
		return nil
	}
	step("start axon")
	ask := remoteAxon + " ask --json"
	if t.Session != "" {
		ask += " --resume " + quote(t.Session)
	}
	if t.Session == "" || *model != "" {
		ask += " --model " + quote(chosenModel)
	}
	work := "cd " + quote(t.Dir) + " && AXON_REMOTE=" + quote(t.ID) + " " + ask + ` "$(cat ` + remoteDir + `/prompt)" > ` + remoteDir + `/result.json 2> ` + remoteDir + `/log; echo $? > ` + remoteDir + `/exit`
	if _, err := r.run(nil, r.args([]string{"exec", "--id", t.Sandbox, "--detach"}, "--", work)); err != nil {
		return err
	}
	fmt.Printf("task %s is running in the sandbox; you can close this machine.\ncheck it with:  glia status %s\nbring it home:  glia back %s\n", t.ID, t.ID, t.ID)
	return nil
}

// remote continues an axon session in a sandbox, or returns to the sandbox
// that already holds it.
func remote(args []string) error {
	fs := flag.NewFlagSet("remote", flag.ExitOnError)
	session := fs.String("session", "", "the axon session to continue")
	fs.Parse(args)
	prompt := strings.Join(fs.Args(), " ")
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	c, err := loadConfig()
	if err != nil {
		return err
	}
	tasks, err := loadTasks()
	if err != nil {
		return err
	}
	for i := len(tasks) - 1; i >= 0; i-- {
		t := tasks[i]
		if t.Stopped || t.Dir != cwd || *session != "" && t.Session != *session {
			continue
		}
		if _, err := (railway{c}).exec(t.Sandbox, nil, "true"); err != nil {
			t.Stopped = true
			if err := saveTask(t); err != nil {
				return err
			}
			continue
		}
		step("task %s already holds this session", t.ID)
		return attachTask(t, c, true)
	}
	return launch(launchOptions{session: *session, prompt: prompt, home: true})
}

func list(args []string) error {
	tasks, err := loadTasks()
	if err != nil {
		return err
	}
	for _, t := range tasks {
		state := "running, work not fetched"
		switch {
		case t.Stopped:
			state = "stopped"
		case !t.Fetched.IsZero():
			state = "running, fetched " + t.Fetched.Format("15:04")
		}
		fmt.Printf("%s  %s  %s  %s\n", t.ID, t.Created.Format("2006-01-02 15:04"), filepath.Base(t.Repo), state)
	}
	return nil
}

func status(args []string) error {
	t, c, err := taskAndConfig(args)
	if err != nil {
		return err
	}
	script := strings.Join([]string{
		"cd " + quote(t.remoteRepo()),
		"if [ -f " + remoteDir + "/session ]; then echo 'state: axon is open (glia attach)'",
		"elif [ -f " + remoteDir + "/exit ]; then echo \"state: finished, exit $(cat " + remoteDir + "/exit)\"",
		"elif [ -s " + remoteDir + "/prompt ]; then echo 'state: working'",
		"else echo 'state: waiting for glia attach'; fi",
		"echo 'commits:'; git log --oneline " + t.Base + "..HEAD | sed 's/^/  /'",
		"echo \"uncommitted files: $(git status --short | wc -l | tr -d ' ')\"",
		"if [ -s " + remoteDir + "/log ]; then echo 'last steps:'; tail -n 5 " + remoteDir + "/log | cut -c1-160 | sed 's/^/  /'; fi",
		"if [ -s " + remoteDir + "/result.json ]; then echo 'answer:'; python3 -c 'import json;print(json.load(open(\"" + remoteDir + "/result.json\"))[\"final_output\"])' | sed 's/^/  /'; fi",
	}, "\n")
	out, err := railway{c}.exec(t.Sandbox, nil, script)
	fmt.Print(out)
	return err
}

func attach(args []string) error {
	t, c, err := taskAndConfig(args)
	if err != nil {
		return err
	}
	return attachTask(t, c, false)
}

// remoteState reports what axon does in the sandbox: "BUSY" while it works on
// a prompt without a terminal, the durable session's name while its
// interface is open, and "" otherwise.
func remoteState(t task, r railway) (string, error) {
	state, err := r.exec(t.Sandbox, nil, strings.Join([]string{
		"if [ -s " + remoteDir + "/session ]; then cat " + remoteDir + "/session",
		"elif [ -s " + remoteDir + "/prompt ] && [ ! -f " + remoteDir + "/exit ]; then echo BUSY; fi",
	}, "\n"))
	return strings.TrimSpace(state), err
}

// attachTask opens axon in the sandbox. With home set, it brings the session
// back to this machine once the remote axon has ended.
func attachTask(t task, c config, home bool) error {
	r := railway{c}
	state, err := remoteState(t, r)
	if err != nil {
		return err
	}
	if state == "BUSY" {
		return fmt.Errorf("axon is still working on the prompt; see glia status %s", t.ID)
	}
	sshArgs := r.args([]string{"ssh", "--id", t.Sandbox})
	if state != "" {
		sshArgs = append(sshArgs, "--session", state)
	} else {
		sid := remoteDir + "/sid"
		sshArgs = append(sshArgs, "--", strings.Join([]string{
			"cd " + quote(t.remoteCwd()),
			`echo "$RAILWAY_DURABLE_SESSION_NAME" > ` + remoteDir + "/session",
			"[ -s " + sid + " ] || python3 -c 'import json;print(json.load(open(\"" + remoteDir + "/result.json\"))[\"session_id\"])' > " + sid + " 2>/dev/null",
			"export AXON_REMOTE=" + quote(t.ID),
			"if [ -s " + sid + " ]; then " + remoteAxon + ` --resume "$(cat ` + sid + `)"; elif [ -n "$(ls -A /root/.axon/sessions 2>/dev/null)" ]; then ` + remoteAxon + " -c; else " + remoteAxon + "; fi",
			"rm -f " + remoteDir + "/session",
		}, "; "))
	}
	cmd := exec.Command("railway", sshArgs...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	runErr := cmd.Run()
	if !home {
		return runErr
	}
	state, err = remoteState(t, r)
	if err != nil {
		return err
	}
	if state != "" {
		fmt.Printf("axon keeps running in the sandbox as task %s.\nreturn to it:   /remote in axon, or glia attach %s\nbring it home:  /local in the sandbox, or glia back %s\n", t.ID, t.ID, t.ID)
		return nil
	}
	return backTask(t, c)
}

func back(args []string) error {
	t, c, err := taskAndConfig(args)
	if err != nil {
		return err
	}
	state, err := remoteState(t, railway{c})
	if err != nil {
		return err
	}
	switch {
	case state == "BUSY":
		return fmt.Errorf("axon is still working on the prompt; see glia status %s, or end it with glia stop %s", t.ID, t.ID)
	case state != "":
		return fmt.Errorf("axon is still open in the sandbox; use /local there, or end it with glia stop %s", t.ID)
	}
	return backTask(t, c)
}

// backTask puts the sandbox's work into the working tree, destroys the
// sandbox, and opens axon here on the session the sandbox continued.
func backTask(t task, c config) error {
	if _, err := fetchTask(&t, c); err != nil {
		return fmt.Errorf("the sandbox is kept: %w", err)
	}
	if err := applyWork(&t); err != nil {
		return fmt.Errorf("the sandbox is kept: %w", err)
	}
	r := railway{c}
	if _, err := r.run(nil, r.args([]string{"destroy", t.Sandbox})); err != nil {
		return err
	}
	t.Stopped = true
	if err := saveTask(t); err != nil {
		return err
	}
	fmt.Printf("sandbox of task %s destroyed\n", t.ID)
	axon, err := exec.LookPath("axon")
	if err != nil || t.Session == "" || !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		if t.Session != "" {
			fmt.Printf("continue here with: axon --resume %s\n", t.Session)
		}
		return nil
	}
	if err := os.Chdir(t.Dir); err != nil {
		return err
	}
	return syscall.Exec(axon, []string{axon, "--resume", t.Session}, os.Environ())
}

func preview(args []string) error {
	var id, ports []string
	for _, a := range args {
		if strings.Trim(a, "0123456789:") == "" {
			ports = append(ports, a)
		} else {
			id = append(id, a)
		}
	}
	t, c, err := taskAndConfig(id)
	if err != nil {
		return err
	}
	if len(ports) == 0 {
		rc, err := loadRepoConfig(t.Repo)
		if err != nil {
			return err
		}
		ports = rc.Ports
	}
	if len(ports) == 0 {
		return errors.New("no port: pass one, or set \"ports\" in .glia.json")
	}
	r := railway{c}
	cmd := exec.Command("railway", r.args([]string{"forward", "--id", t.Sandbox}, ports...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func fetch(args []string) error {
	t, c, err := taskAndConfig(args)
	if err != nil {
		return err
	}
	_, err = fetchTask(&t, c)
	return err
}

// fetchTask brings the sandbox's work and axon sessions back. It reports
// whether the sandbox changed anything.
func fetchTask(t *task, c config) (bool, error) {
	r := railway{c}
	step("snapshot the sandbox")
	script := "cd " + quote(t.remoteRepo()) + "\n" + "c=$(sh -c " + quote(snapshotScript) + ")\n" + strings.Join([]string{
		`[ -n "$c" ] || exit 1`,
		// Reuse the last snapshot when nothing changed, so fetching twice gives the same commit.
		`if prev=$(git rev-parse -q --verify refs/glia/out) && [ "$c" != "$(git rev-parse HEAD)" ] && [ "$(git rev-parse "$prev^")" = "$(git rev-parse HEAD)" ] && [ "$(git rev-parse "$prev^{tree}")" = "$(git rev-parse "$c^{tree}")" ]; then c=$prev; fi`,
		`git update-ref refs/glia/out "$c"`,
		`if [ "$c" = ` + t.Base + ` ]; then echo NOCHANGE; exit 0; fi`,
		"git bundle create -q " + remoteDir + "/out.bundle refs/glia/out ^" + t.Base,
		"base64 -w0 " + remoteDir + "/out.bundle",
	}, "\n")
	out, err := r.exec(t.Sandbox, nil, script)
	if err != nil {
		return false, err
	}
	out = strings.TrimSpace(out)
	changed := out != "NOCHANGE"
	if !changed {
		fmt.Println("the sandbox changed no files")
	} else if err := landWork(*t, out); err != nil {
		return false, err
	}
	newest, err := fetchSessions(*t, r)
	if err != nil {
		return changed, fmt.Errorf("the work is fetched, the axon session is not: %w", err)
	}
	if newest != "" {
		t.Session = newest
	}
	t.Fetched = time.Now()
	return changed, saveTask(*t)
}

func landWork(t task, encoded string) error {
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return fmt.Errorf("the bundle arrived damaged: %w", err)
	}
	tmp, err := os.MkdirTemp("", "glia-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	bundle := filepath.Join(tmp, "out.bundle")
	if err := os.WriteFile(bundle, data, 0o600); err != nil {
		return err
	}
	previous, _ := git(t.Repo, "rev-parse", "-q", "--verify", t.resultRef())
	if _, err := git(t.Repo, "-c", "fetch.fsckObjects=true", "fetch", "-q", bundle, "+refs/glia/out:"+t.resultRef()); err != nil {
		return err
	}
	commits, _ := git(t.Repo, "log", "--oneline", t.Base+".."+t.resultRef())
	stat, _ := git(t.Repo, "diff", "--stat", t.Base, t.resultRef())
	fmt.Printf("%s\n\n%s\n\n", commits, stat)
	// Move the branch only when it holds nothing but earlier fetched work.
	current, _ := git(t.Repo, "rev-parse", "-q", "--verify", "refs/heads/"+t.branch())
	_, notAncestor := git(t.Repo, "merge-base", "--is-ancestor", "refs/heads/"+t.branch(), t.resultRef())
	if current != "" && current != previous && notAncestor != nil {
		fmt.Printf("branch %s has commits of its own and was not moved. The work is at %s.\n", t.branch(), t.resultRef())
		return nil
	}
	if _, err := git(t.Repo, "branch", "-f", t.branch(), t.resultRef()); err != nil {
		fmt.Printf("branch %s was not moved (%v). The work is at %s.\n", t.branch(), err, t.resultRef())
		return nil
	}
	fmt.Printf("the work is on branch %s; nothing was checked out.\n", t.branch())
	return nil
}

func apply(args []string) error {
	t, c, err := taskAndConfig(args)
	if err != nil {
		return err
	}
	if _, err := fetchTask(&t, c); err != nil {
		return err
	}
	return applyWork(&t)
}

// applyWork puts what the sandbox changed since the last apply into the
// working tree.
func applyWork(t *task) error {
	result, err := git(t.Repo, "rev-parse", "-q", "--verify", t.resultRef())
	if err != nil {
		return nil
	}
	from := t.Applied
	if from == "" {
		from = t.Base
	}
	diff, err := git(t.Repo, "diff", "--binary", from, result)
	if err != nil {
		return err
	}
	if diff == "" {
		return nil
	}
	patch := diff + "\n"
	if _, err := gitInput(t.Repo, strings.NewReader(patch), "apply", "--check"); err != nil {
		return fmt.Errorf("the changes do not fit the working tree as it is now; merge branch %s instead (%v)", t.branch(), err)
	}
	if _, err := gitInput(t.Repo, strings.NewReader(patch), "apply"); err != nil {
		return err
	}
	fmt.Println("the sandbox's changes are in the working tree, uncommitted")
	t.Applied = result
	return saveTask(*t)
}

func stop(args []string) error {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	discard := fs.Bool("discard", false, "destroy the sandbox without fetching its work")
	fs.Parse(args)
	t, c, err := taskAndConfig(fs.Args())
	if err != nil {
		return err
	}
	if !*discard {
		if _, err := fetchTask(&t, c); err != nil {
			return fmt.Errorf("fetch failed, sandbox kept (use --discard to destroy it anyway): %w", err)
		}
	}
	r := railway{c}
	if _, err := r.run(nil, r.args([]string{"destroy", t.Sandbox})); err != nil {
		return err
	}
	t.Stopped = true
	if err := saveTask(t); err != nil {
		return err
	}
	fmt.Printf("sandbox of task %s destroyed\n", t.ID)
	return nil
}
