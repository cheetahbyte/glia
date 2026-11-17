package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxModel(t *testing.T) {
	cases := []struct {
		name, requested, resumed, defaultModel, want string
		wantError                                    bool
	}{
		{"new session", "", "", "openai/gpt-test", "openai/gpt-test", false},
		{"unmapped resumed provider", "", "claude/fable", "openai/gpt-test", "openai/gpt-test", false},
		{"mapped resumed provider", "", "other/fable", "openai/gpt-test", "other/fable", false},
		{"explicit override", "openai/override", "other/fable", "openai/gpt-test", "openai/override", false},
		{"unmapped explicit provider", "claude/fable", "", "openai/gpt-test", "", true},
		{"unmapped default provider", "", "claude/fable", "missing/model", "", true},
		{"empty upstream", "empty/model", "", "openai/gpt-test", "", true},
		{"invalid model", "missing-provider", "", "openai/gpt-test", "", true},
		{"missing model", "", "", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := config{Model: c.defaultModel, Providers: map[string]string{"openai": "chatgpt", "other": "claude", "empty": ""}}
			got, err := sandboxModel(cfg, c.requested, c.resumed)
			if (err != nil) != c.wantError || got != c.want {
				t.Fatalf("sandboxModel = %q, %v; want %q, error %v", got, err, c.want, c.wantError)
			}
		})
	}
}

func TestSessionModelUsesLastChange(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AXON_HOME", root)
	file := filepath.Join(root, "session.jsonl")
	data := "{\"kind\":\"session\"}\n" +
		"{\"kind\":\"model_change\",\"provider\":\"openai\",\"model\":\"old\"}\n" +
		"{\"kind\":\"model_change\",\"provider\":\"claude\",\"model\":\"fable\"}\n" +
		"{\"kind\":\"model_change\",\"provider\":\"torn"
	if err := os.WriteFile(file, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := sessionModel(file)
	if err != nil || got != "claude/fable" {
		t.Fatalf("sessionModel = %q, %v; want claude/fable", got, err)
	}
}

func TestRemoteModelStateUsesSandboxRoutes(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is unavailable")
	}
	root := t.TempDir()
	t.Setenv("HOME", root)
	home, control := filepath.Join(root, "axon"), filepath.Join(root, "control")
	sessions := filepath.Join(home, "sessions", sessionDirName("/repo"))
	for _, dir := range []string{sessions, control} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(home, "models.json"):   `{"providers":{"openai":{"api":"soma","upstream":"chatgpt","apiKey":"fixture-only"},"local":{"api":"codex"}}}`,
		filepath.Join(home, "settings.json"): `{"defaultModel":"openai/gpt-test"}`,
		filepath.Join(control, "sid"):        "session-id",
		filepath.Join(sessions, "session.jsonl"): "{\"kind\":\"session\",\"id\":\"session-id\"}\n" +
			"{\"kind\":\"model_change\",\"provider\":\"openai\",\"model\":\"old\"}\n" +
			"{\"kind\":\"model_change\",\"provider\":\"claude\",\"model\":\"fable\"}\n",
	}
	for path, data := range files {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := exec.Command(python, "-c", remoteModelScript, home, control, "/repo").CombinedOutput()
	if err != nil {
		t.Fatalf("remote model metadata: %v\n%s", err, out)
	}
	var state struct {
		Config    config `json:"config"`
		Requested string `json:"requested"`
		Resumed   string `json:"resumed"`
	}
	if err := json.Unmarshal(out, &state); err != nil {
		t.Fatal(err)
	}
	if state.Resumed != "claude/fable" || len(state.Config.Providers) != 1 || state.Config.Providers["openai"] != "chatgpt" {
		t.Fatalf("wrong model state: %+v", state)
	}
	if strings.Contains(string(out), "fixture-only") {
		t.Fatal("model metadata exposed a credential field")
	}
	model, err := sandboxModel(state.Config, state.Requested, state.Resumed)
	if err != nil || model != "openai/gpt-test" {
		t.Fatalf("sandbox model = %q, %v; want openai/gpt-test", model, err)
	}
	t.Run("explicit child session", func(t *testing.T) {
		file := filepath.Join(sessions, "session.jsonl")
		child := strings.Replace(files[file], `"id":"session-id"`, `"id":"session-id","parent":"parent-id"`, 1)
		if err := os.WriteFile(file, []byte(child), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.Command(python, "-c", remoteModelScript, home, control, "/repo").CombinedOutput()
		if err != nil {
			t.Fatalf("explicit child session metadata: %v\n%s", err, out)
		}
		if err := json.Unmarshal(out, &state); err != nil || state.Resumed != "claude/fable" {
			t.Fatalf("child model metadata: %v, resumed %q", err, state.Resumed)
		}
	})
}

func TestRemoteAskRoutesResumedSession(t *testing.T) {
	command := remoteAsk("session-id", "openai/gpt-test")
	for _, argument := range []string{"--resume 'session-id'", "--model 'openai/gpt-test'"} {
		if !strings.Contains(command, argument) {
			t.Errorf("remote command %q lacks %s", command, argument)
		}
	}
}
