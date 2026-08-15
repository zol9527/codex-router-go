# Security model

The router has its own trust root, separate from Codex's: a random caller key
in the managed URL path, a private state directory, and per-provider
credentials in a single TOML file. The service binds `127.0.0.1` only.

## Credential separation

- Codex's own ChatGPT authentication is used only on the native passthrough
  path, and only for requests that carried it or fell back to the local
  Codex login session (same user, same machine). The token never leaves the
  process, is never logged, and stops being injected two minutes before its
  `exp` claim.
- Provider API keys are resolved per request in a fixed order — environment
  > `~/.codex-router/config.toml` > `*.secret` files > macOS Keychain — and
  each key is sent only to its own provider's endpoint. `{VAR}` references in
  the config file expand environment variables at read time, so a key can
  live in a secret manager instead of the file.
- No credential is written to the model registry, the published catalog,
  `~/.codex/config.toml`, logs, or health responses. The managed base URL
  embeds the random caller key and is treated as a local secret; status and
  doctor output print it redacted.

## Local authentication

The caller key in the URL path is the entire authentication model: anything
that can reach `127.0.0.1:4202` and knows the key can spend the configured
providers. Treat the managed URL accordingly (see above). The state
directory is `0700` with `0600` files; `config.toml` is rewritten atomically
(tmp + rename) and its parser is fail-closed — a file it cannot parse
yields "unconfigured", never a guessed key.

## Uninstall

`codex-router uninstall` removes the Codex integration blocks and the CLI
artifacts but preserves the state directory (credentials, usage history) by
default; `--purge` destroys state as an explicit opt-in.
