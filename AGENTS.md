# Model Router (Go rewrite) — agent instructions

These instructions apply when a user asks an agent to install or operate this
repository. This is the **Go rewrite fork**: one self-contained Go binary plus
a native macOS app. There is no Node.js runtime, no LiteLLM/Python gateway,
no install.sh, no Homebrew formula, and no dsh integration — if a user asks
for those, say they were removed with the rewrite.

## What gets installed

- `Model Router.app` in `~/Applications`: windowed macOS app (Dock icon,
  menu-bar quick view). The Go service binary is embedded in the bundle and
  runs as a supervised child — open the app and the service starts, quit the
  app and it dies with it (red X only closes the window). Launch-at-login is
  a settings toggle (SMAppService), not launchd.
- Optionally the `codex-router` CLI binary in the user's own bin directory
  (commonly `~/bin`). The app and the CLI never fight: the port is unique and
  the later starter exits.
- Codex integration: two marked blocks in `~/.codex/config.toml`
  (`openai_base_url` + `model_catalog_json` + the `model_providers`
  entry) plus the merged model catalog in the state directory.
  Everything else in that file is the user's and must stay untouched.

## Install procedure

1. Preconditions (read-only checks): macOS, Go 1.22+, Xcode command line
   tools. Codex itself must be installed. Node/Python are NOT needed.
2. Build and install the app:
   `./scripts/build-macos-tray-app.sh` then `open ~/Applications/"Model Router.app"`.
3. Publish the Codex integration: `./codex-router install` (idempotent;
   rewrites only the marked blocks, keeps the caller key stable).
4. Credentials live in `~/.codex-router/config.toml`:
   `[provider] api_key = "..."`, editable by hand, by the app's settings
   page, or via `./codex-router control credential PROVIDER` (hidden stdin
   prompt — pipe the value, never take it through chat). `{VAR}` values
   expand environment variables at read time.
5. Verify: `./codex-router doctor`. Core config, catalog, credentials
   config, and service reachability must be OK.
6. Tell the user to fully quit and reopen Codex, then pick a routed model.
   Never quit Codex for them.

## Safety rules (unchanged in spirit from the original project)

- Never ask the user to paste an API key or token into chat, command
  arguments, logs, or tracked files. The credential file and the hidden
  stdin prompt are the only entrances.
- Do not print the full managed base URL (it embeds the caller key);
  use the redacted output that doctor/status already print.
- Do not read or print credential-file contents; status commands report
  presence and source only.
- Do not kill unknown processes on port 4202, and do not restart or quit
  the Codex app from an installation task.
- `uninstall` preserves the state directory by default; `--purge` is an
  explicit destructive opt-in. The app bundle is the user's to delete.
- The service is owned by the app. `control service start` exists as a
  terminal escape hatch (detached spawn), but the normal way to run the
  router is simply having the app open.

## Operating surface

`./codex-router control …` is the full management plane (the app's buttons
call exactly these): `--json` snapshot, `service`, `providers`, `credential`,
`config init`, `reload` (re-read config + refresh catalog without restart),
`presence set`, `account --json`, `provider-usage --json`, `vision-bridge`,
`local-runtime`. Providers are a closed set: `zai-coding` and `opencode-go`
plus native Codex passthrough. Adding a protocol means implementing
`internal/wire`'s Protocol interface in a new package — see README.

Design baseline and migration history: `GO-REWRITE-PLAN.md`.
