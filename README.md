# ppz

> Pipes for agents — private, instant, durable interprocess and internet
> communications.

ppz is an open-source tool for streaming pipes between agents,
processes, and machines. It pairs a CLI + daemon on the user's
side with a server that runs NATS / JetStream as the transport.

Hosted ppz is available at [pipescloud.io](https://pipescloud.io).
It's built on this Apache-2.0 core and adds proprietary hosted-only
features (security, scalability, billing, team management, ops
tooling). The OSS code here is fully self-hostable.

## Install

**Quick (linux / macOS, amd64 / arm64):**

```bash
curl -fsSL https://raw.githubusercontent.com/pipescloud/ppz/main/install.sh | bash
```

Detects your OS + arch, downloads the matching pre-built tarball
from the latest [GitHub Release](https://github.com/pipescloud/ppz/releases),
verifies the sha256, and drops the binaries into `~/.local/bin`. No
Go toolchain required.

You get the full bundle: the **CLI** `ppz`, plus `ppz-server` and
`ppz-natsbootstrap` for anyone who wants to bring up their own
local/self-hosted server.

Knobs: `PPZ_VERSION=v0.17.0` to pin a tag, `PPZ_INSTALL_DIR=/usr/local/bin`
to change the target directory.

Upgrade an installed CLI in place with:

```bash
ppz upgrade
```

`ppz upgrade` reuses the same installer and target directory defaults as
the curl one-liner. Release builds of `ppz login`, `ppz status`, and
`ppz version` also check a lightweight manifest on GitHub and print a
non-blocking update notice when a newer CLI is available. Set
`PPZ_UPDATE_CHECK=0` to suppress those notices. The manifest fetch is
bounded by a 2s deadline; on a high-latency link where that is too tight
(the notice silently won't appear), widen it with
`PPZ_UPDATE_TIMEOUT=5s` (any Go duration).

**From source (requires Go 1.22+):**

```bash
git clone https://github.com/pipescloud/ppz
cd ppz
make build      # binaries → ./bin/
make install    # ./bin/* → ~/.local/bin/ (same path as install.sh)
```

`make install` lands in `~/.local/bin` to match the `install.sh` curl
one-liner — running either flow always overwrites the same files, so
there's no shadowing risk if you mix them. Override with
`INSTALL_BIN=/path/to/dir`.

## What's in the box

| Binary | Audience | Purpose |
|---|---|---|
| `ppz`               | CLI users | The user-facing CLI (`ppz source create`, `ppz send`, `ppz read`, `ppz pipe …`). |
| `ppz-server`        | Self-hosters | Hosts the account/source/pipe state and embeds a NATS server. pipescloud.io runs one. |
| `ppz-natsbootstrap` | Self-hosters | One-shot helper that mints an ephemeral NATS NSC chain (operator + account JWTs) for a fresh server. Production usually pulls these from a secret manager instead. |
| `ppz-seed`          | Source / e2e only | Populates the OSS test fixtures (`foo`/`bar` users, `alpha`/`beta` accounts). Built from source by the compose harness — not published in release tarballs. |

## Using ppz from agents

ppz keeps **current-handle** state per shell session, keyed off the calling
tty. For interactive use that's transparent — open a terminal, run
`ppz source create alpha`, every subsequent `ppz` call in the same window
sees `alpha` as current.

For agents that run each command as a fresh subprocess (most agent harnesses
do — Claude Code's Bash tool, OpenAI's code interpreter, container `exec`
flows), there's no shared tty across calls, so each invocation gets its own
session id. `ppz source create alpha` in one subprocess won't be visible
to the next, and `ppz send … --request-ack` will trip `E_NO_CURRENT_SOURCE`.

The fix is one line at the agent's lifecycle level — pin a stable session id
once and every call inherits it:

```bash
export PPZ_SESSION="agent-${AGENT_NAME}"
ppz source create alpha
ppz send beta "ping" --request-ack    # sees alpha as current; ack routes back
```

Without `PPZ_SESSION`, plain `ppz send <handle> <payload>` still works for
delivery but lands with empty `sender` attribution; `--request-ack` (which
requires a current handle on the publish side) will reject.

Urgent sends can jump a busy recipient's queue: `ppz send beta "stop" \
--priority high` delivers ahead of medium/low messages in the recipient's
next inbox drain (FIFO within a tier; see `ppz help send` and
[`docs/WIRE.md`](docs/WIRE.md) §8 for the ordering rules).

## Docs

- [`docs/AUTH-V2.md`](docs/AUTH-V2.md) — auth design (GitHub OAuth + per-org NATS account JWTs)
- [`docs/WIRE.md`](docs/WIRE.md) — wire protocol reference (subjects, error codes, retention semantics)
- [`docs/ERRORS.md`](docs/ERRORS.md) — error code catalogue
- [`docs/self-hosting.md`](docs/self-hosting.md) — full self-hosting guide (setup paths, env vars, auth, gotchas)

See **[Self-hosting](#self-hosting)** below to run the full stack
(server + embedded NATS + Postgres) on your own machine, or
**[`docs/self-hosting.md`](docs/self-hosting.md)** for the long-form guide.

## Tests

```bash
make test           # Go unit tests
make e2e            # full Docker-Compose integration suite
make e2e-filter F='broadcast/*'   # subset
```

## Self-hosting

The OSS core here is fully self-hostable — `ppz-server` embeds a NATS /
JetStream node and keeps account / source / pipe state in Postgres.
`pipescloud.io` runs the same binary.

### Quickstart

`scripts/ppz-local-server.sh` is an interactive helper that provisions
everything a local server needs and writes its config to `.ppz-local/`
(gitignored):

```bash
# the install one-liner already ships ppz-server + ppz-natsbootstrap
# (or build from source: make build)
curl -fsSL https://raw.githubusercontent.com/pipescloud/ppz/main/install.sh | bash

scripts/ppz-local-server.sh          # answer the prompts → setup
scripts/ppz-local-server.sh --start  # load the config + run ppz-server
ppz login http://localhost:8080      # point the CLI at your server
```

It asks a handful of questions and then sets up:

- **Postgres** — a script-managed `postgres:16-alpine` container, or a
  connection URL you supply.
- **NATS trust root** — runs `ppz-natsbootstrap` to mint the operator /
  system JWTs and caches them. `ppz-server` *requires* these
  (`PPZ_NATS_OPERATOR_SEED`); without them the embedded NATS can't
  authenticate any client.
- **JetStream** store directory for durable pipes.
- **Auth** — GitHub OAuth (real `ppz login` device flow) or dev-login
  (`/dev/login?user=…`, no GitHub; for tests / CI).
- A session key and, optionally, the seed demo users (`foo`/`bar`,
  accounts `alpha`/`beta`).

Run it again with no arguments and it **reports** the current config
instead of reconfiguring.

> **Warning** — reconfiguring and regenerating the NATS credentials mints
> a new operator key, which invalidates every login already issued: all
> clients must `ppz login` again. Changing the DB / ports / auth alone
> does not.

### Under the hood

The helper just wires up what the server expects in its environment. To
run it by hand:

```bash
eval "$(ppz-natsbootstrap)"   # exports PPZ_NATS_OPERATOR_JWT / _SEED / _SYSTEM_ACCOUNT_JWT
export PPZ_DB_URL=postgres://postgres:ppz@localhost:5432/ppz?sslmode=disable
export PPZ_JETSTREAM_STORE_DIR=$HOME/.local/share/ppz/jetstream
ppz-server                    # runs migrations on boot, then serves :8080 + NATS :4222
```

`ppz-natsbootstrap` mints a *fresh* operator key on each run, so cache
its output (as the helper does) — regenerating it invalidates
previously-issued client JWTs.

## Releasing

Tags are minted automatically on each merge to `main` from
[Conventional Commits](https://www.conventionalcommits.org/) in the PR
title / commit subjects:

| Prefix                           | Bump   |
|---                               |---     |
| `feat:` / `feat(scope):`         | minor  |
| `fix:` / `fix(scope):`           | patch  |
| `feat!:` / `<type>!:` / `BREAKING CHANGE:` in body | major |
| `chore:`, `docs:`, `refactor:`, `test:`, `ci:`, etc. | no tag |

The highest bump level seen in the new commit range wins. Tagging is
just tagging — it does **not** publish binaries. To cut a distribution,
manually dispatch the **Release** workflow (Actions → Release → Run
workflow) and pick the tag. That's when goreleaser builds the matrix
and attaches the archives to the GitHub Release.

Manual tagging via `make tag {patch|minor|major}` is still available
for special cases.

## License

Apache 2.0. See [`LICENSE`](LICENSE).
