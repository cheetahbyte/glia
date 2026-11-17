# glia

Glia runs [axon](https://github.com/cheetahbyte/axon) in a Railway sandbox next to your [soma](https://github.com/cheetahbyte/soma) server. The sandbox gets a copy of your repository and a soma token, and no provider login.

This is a proof of concept. It wraps the `railway` command, which must be installed and logged in.

## Set up

Create `~/.glia/config.json`:

```json
{
  "project": "<Railway project ID of your soma service>",
  "environment": "production",
  "somaService": "<name of your soma service>",
  "axonRepo": "<path to the axon repository>",
  "providers": { "openai": "chatgpt" },
  "model": "openai/gpt-6.1-sol",
  "idleMinutes": 60
}
```

`providers` maps each provider name axon uses in the sandbox to a provider on your soma server. A continued session keeps its model when that provider has a route. Otherwise, glia uses `model` and reports the switch. `--model` overrides both choices. Glia checks that the selected provider has a route before creating a sandbox.

The first sandbox launch explicitly selects that model, including interactive and resumed sessions. Later attachments preserve the sandbox session's model when it has a route. Generated provider settings take precedence over copied home files.

`home` lists files and folders in your home directory that the sandbox's axon should also have, such as instruction files, hooks and skills: `"home": [".axon/AGENTS.md", ".axon/hooks.json", ".agents"]`. The default is `.axon/AGENTS.md`. Every command axon runs in the sandbox can read them.

`idleMinutes` is how long Railway keeps an idle sandbox before it destroys it, along with any work you haven't fetched. The default, 120, is the longest Railway allows.

Install the command:

```sh
go install github.com/cheetahbyte/glia@latest
```

`glia update` installs the latest commit of the default branch with `go install` and replaces the running binary. It needs Go. `glia version` prints the commit the binary was built from.

A repository can add a `.glia.json` at its root. Every key is optional:

```json
{
  "setup": "bun install",
  "copy": [".env.local"],
  "ports": ["3000"]
}
```

| Key | Meaning |
| --- | --- |
| `setup` | A shell command run in the sandbox's copy of the repository before axon starts. If it fails, the task stops and the sandbox is kept. Without the key, glia picks one from the lock files at the root: `bun install`, `pnpm install`, `yarn install`, `npm ci`, `uv sync`, and the Go version of `go.mod`. `""` runs nothing. |
| `copy` | Files and folders that git ignores but the work needs, relative to the repository root. They are copied into the sandbox, where every command axon runs can read them. |
| `ports` | The ports `glia preview` forwards when you name none. |

## Use from axon

| In axon | What happens |
| --- | --- |
| `/remote` | The session moves into a sandbox, and the sandbox's axon opens in the same terminal. Its status line starts with `remote <task>`. |
| `/remote <prompt>` | The session moves into a sandbox and axon works on the prompt there without a terminal. Glia shows each step as it happens. If you stay until it finishes, the work comes home as with `/local`. Ctrl+C stops the watching, not the work: you can close the machine, and `glia watch` or `glia back` picks it up later. |
| `/local` or `/quit`, inside the sandbox | The sandbox's changes land in your working tree, uncommitted. The sandbox is destroyed, and axon opens on your machine on the same session. |
| `/detach` or closing the terminal, inside the sandbox | You leave; axon keeps running in the sandbox, also in the middle of a run. `/remote` in a local axon in that directory returns to it. |

If the changes don't fit your working tree any more, the sandbox is kept and glia tells you to merge branch `glia/<id>` instead.

## Use from the shell

Run these in the repository you want to work on.

| Command | What it does |
| --- | --- |
| `glia start "<prompt>"` | Copies the repository, including uncommitted and untracked files, into a new sandbox and lets axon work on the prompt without you. |
| `glia start` | Copies the repository and opens axon in the sandbox. |
| `glia start -c ["<prompt>"]` | The same, but continues this directory's latest axon session, so the sandbox knows the conversation so far. Quit the local axon first. `--session <id>` picks another session. |
| `glia status` | Shows what the task is doing and what it changed. |
| `glia watch` | Follows a task that works on a prompt until it finishes. Ctrl+C stops the watching, not the work. |
| `glia attach` | Opens axon in the sandbox. Closing the terminal leaves it running; `glia attach` again returns to it. |
| `glia preview [port]` | Forwards sandbox ports to this machine until you press Ctrl+C. |
| `glia fetch` | Brings the work back as branch `glia/<id>`, and the axon session with it. It checks out nothing. |
| `glia apply` | Fetches, then puts the sandbox's changes into your working tree, uncommitted. |
| `glia back` | What `/local` does: applies the work, destroys the sandbox, and opens axon here on the session. It refuses while axon still works or is open in the sandbox. |
| `glia remote` | What `/remote` runs. |
| `glia stop` | Fetches, then destroys the sandbox. `--discard` skips the fetch. |
| `glia list` | Lists running tasks; `--all` includes stopped ones. |

`start` also takes `--name <id>` and `--model <provider/model>`. The other commands take a task ID and default to the newest running task of the directory you are in.

## How it works

- `start` stores your working tree as a commit under `refs/glia/<id>/base`. Your branch, index and files stay as they are.
- The sandbox holds the repository at the same path as on your machine, so a continued axon session finds its files.
- The sandbox's axon gets the look of your `settings.json`, and the check approvals of the repository from your `state.json`, so project checks run there too.
- The sandbox joins the private network of your soma service. Railway fills in the token and the address by reference, so glia never reads them.
- `fetch` stores the sandbox's working tree, committed or not, under `refs/glia/<id>/result` and moves branch `glia/<id>` to it.
- Git bundles and session archives download in bounded chunks. Glia verifies their lengths and SHA-256 checksums before importing them. Damaged transfers fail without accepting partial data.
- `apply` applies the difference between the two to your working tree. It refuses when the changes don't fit, and you merge the branch instead.
- A session that came back replaces your local copy only when it continues it unchanged. If you also continued it locally, the sandbox's copy is saved under `~/.glia/tasks/<id>-sessions/`.

## Limits

- Nothing collects the work for you. If you don't fetch within `idleMinutes` after axon finishes, Railway destroys the sandbox and the work is lost.
- The sandbox's `models.json` holds only the providers from the config. An unmapped session model needs a routed fallback in `model` or `--model`.
- Tasks leave `refs/glia/<id>/` and branch `glia/<id>` in your repository. Delete them yourself when you're done.
