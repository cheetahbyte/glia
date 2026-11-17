package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

func sandboxModel(c config, requested, resumed string) (string, error) {
	chosen := requested
	if chosen == "" && hasModelRoute(c, resumed) {
		chosen = resumed
	}
	if chosen == "" {
		chosen = c.Model
	}
	if chosen == "" {
		return "", errors.New("no model: pass --model or set \"model\" in the config")
	}
	provider, model, ok := strings.Cut(chosen, "/")
	if !ok || provider == "" || model == "" {
		return "", fmt.Errorf("model %q must name a provider/model", chosen)
	}
	if !hasModelRoute(c, chosen) {
		return "", fmt.Errorf("model %q has no Soma route: add provider %q to the config's \"providers\" or choose --model with a mapped provider", chosen, provider)
	}
	return chosen, nil
}

func hasModelRoute(c config, model string) bool {
	provider, name, ok := strings.Cut(model, "/")
	return ok && provider != "" && name != "" && strings.TrimSpace(c.Providers[provider]) != ""
}

func sessionModel(file string) (string, error) {
	f, err := os.Open(file)
	if err != nil {
		return "", err
	}
	defer f.Close()
	r := bufio.NewReader(f)
	model := ""
	for {
		line, err := r.ReadBytes('\n')
		if err != nil && err != io.EOF {
			return "", err
		}
		if len(line) > 0 {
			var entry struct {
				Kind     string `json:"kind"`
				Provider string `json:"provider"`
				Model    string `json:"model"`
			}
			if parseErr := json.Unmarshal(line, &entry); parseErr != nil {
				if err == io.EOF {
					return model, nil
				}
				return "", fmt.Errorf("read session model: %w", parseErr)
			}
			if entry.Kind == "model_change" && entry.Provider != "" && entry.Model != "" {
				model = entry.Provider + "/" + entry.Model
			}
		}
		if err == io.EOF {
			return model, nil
		}
	}
}

const remoteModelScript = `import glob, json, os, sys
home, control, directory = sys.argv[1:]
def read(path):
    try:
        with open(path) as f:
            return f.read().strip()
    except FileNotFoundError:
        return ""
with open(os.path.join(home, "models.json")) as f:
    providers = json.load(f).get("providers", {})
with open(os.path.join(home, "settings.json")) as f:
    default = json.load(f).get("defaultModel", "")
sid = read(os.path.join(control, "sid"))
if not sid:
    result = read(os.path.join(control, "result.json"))
    if result:
        sid = json.loads(result).get("session_id", "")
folder = "--" + os.path.normpath(directory).lstrip("/").replace("/", "-") + "--"
files = glob.glob(os.path.join(home, "sessions", folder, "*.jsonl"))
resumed = ""
found = False
for path in sorted(files, key=os.path.getmtime, reverse=True):
    with open(path) as f:
        header = json.loads(next(f))
        if (not sid and header.get("parent")) or (sid and header.get("id") != sid):
            continue
        found = True
        for line in f:
            try:
                entry = json.loads(line)
            except json.JSONDecodeError:
                if not line.endswith("\n"):
                    break
                raise
            if entry.get("kind") == "model_change" and entry.get("provider") and entry.get("model"):
                resumed = entry["provider"] + "/" + entry["model"]
    break
if sid and not found:
    raise RuntimeError("the sandbox session was not found")
print(json.dumps({"config": {"model": default, "providers": {name: p.get("upstream", "") for name, p in providers.items() if p.get("api") == "soma"}}, "requested": read(os.path.join(control, "model")), "resumed": resumed}))
`

func remoteModel(t task, r railway) (string, error) {
	out, err := r.exec(t.Sandbox, nil, "python3 -c "+quote(remoteModelScript)+" /root/.axon "+quote(remoteDir)+" "+quote(t.remoteCwd()))
	if err != nil {
		return "", err
	}
	var state struct {
		Config    config `json:"config"`
		Requested string `json:"requested"`
		Resumed   string `json:"resumed"`
	}
	if err := json.Unmarshal([]byte(out), &state); err != nil {
		return "", fmt.Errorf("read sandbox model routing: %w", err)
	}
	chosen, err := sandboxModel(state.Config, state.Requested, state.Resumed)
	if err == nil && state.Requested == "" && state.Resumed != "" && chosen != state.Resumed {
		step("session model %s has no Soma route; using %s in the sandbox", state.Resumed, chosen)
	}
	return chosen, err
}

func remoteAsk(session, model string) string {
	command := remoteAxon + " ask --json"
	if session != "" {
		command += " --resume " + quote(session)
	}
	command += " --model " + quote(model)
	return command
}
