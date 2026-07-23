# Shared helpers for ppz e2e test scenarios.
#
# Each scenario sources nothing — it inherits this via run.sh wrapper conventions.
# But for explicit use, scenarios MAY `. /tests/lib/common.sh` themselves.

set -u
set -o pipefail

# Pin a generous terminal width for `ppz ls` / `ppz --help` so the
# adaptive-truncation introduced in v0.31.5 doesn't shrink the payload
# column or wrap help lines against test fixtures that were written
# assuming a wide terminal. Exported so child run.sh invocations
# inherit it.
export COLUMNS=200

: "${PPZ_SERVER_URL:=http://ppz-server:8080}"
: "${PPZ_DAEMON_A_HOME:=/tmp/a}"
: "${PPZ_DAEMON_B_HOME:=/tmp/b}"
: "${PPZ_DAEMON_A_SOCK:=$PPZ_DAEMON_A_HOME/daemon.sock}"
: "${PPZ_DAEMON_B_SOCK:=$PPZ_DAEMON_B_HOME/daemon.sock}"
: "${PPZ_API_KEY_ALPHA_FILE:=/seed/key-alpha.txt}"
: "${PPZ_API_KEY_ALPHA2_FILE:=/seed/key-alpha2.txt}"
: "${PPZ_API_KEY_BETA_FILE:=/seed/key-beta.txt}"

# Run ppz CLI against daemon A.
ppz_a() {
  PPZ_IPC_SOCKET="$PPZ_DAEMON_A_SOCK" ppz "$@"
}

# Run ppz CLI against daemon B.
ppz_b() {
  PPZ_IPC_SOCKET="$PPZ_DAEMON_B_SOCK" ppz "$@"
}

# Read a seeded plaintext api key.
key_alpha()  { cat "$PPZ_API_KEY_ALPHA_FILE";  }
key_alpha2() { cat "$PPZ_API_KEY_ALPHA2_FILE"; }
key_beta()   { cat "$PPZ_API_KEY_BETA_FILE";   }

# Curl the server GUI; -s silent, -L follow redirects. If a test has
# called auth_as / auth_as_foo, $PPZ_TEST_COOKIE_JAR is set and we
# automatically inject `-b $jar` so subsequent calls flow as that user.
curl_server() {
  if [[ -n "${PPZ_TEST_COOKIE_JAR:-}" ]]; then
    curl -sS -L "$PPZ_SERVER_URL$1" -b "$PPZ_TEST_COOKIE_JAR" "${@:2}"
  else
    curl -sS -L "$PPZ_SERVER_URL$1" "${@:2}"
  fi
}

# auth_as <username> mints a session via the test-only /dev/login
# endpoint and stores the cookie in PPZ_TEST_COOKIE_JAR for the rest
# of the scenario. Tests that need to remain anonymous simply don't
# call this. Auth V2 introduced this — required for any test that
# hits a session-gated GUI route.
auth_as() {
  local user="$1"
  PPZ_TEST_COOKIE_JAR=$(mktemp)
  export PPZ_TEST_COOKIE_JAR
  curl -sS "$PPZ_SERVER_URL/dev/login?user=$user" -X POST -c "$PPZ_TEST_COOKIE_JAR" -o /dev/null
}

auth_as_foo() { auth_as foo; }
auth_as_bar() { auth_as bar; }

# Wait until a command succeeds (rc=0) or `n` attempts of 0.1s have elapsed.
# Usage: wait_for 20 'ppz_a status | grep -q current'
#
# The eval runs in a subshell with `pipefail` disabled. Many call sites
# end with `… | grep -q PATTERN`; under pipefail, a successful grep -q
# closes its stdin and the upstream commands see SIGPIPE on their next
# write — propagating 141 through the pipeline and making wait_for
# loop until timeout even when the condition was met. Disabling
# pipefail just for the polled expression keeps the legitimate
# semantics ("did this match?") without that false negative.
wait_for() {
  local n="$1"; shift
  local i
  for ((i = 0; i < n; i++)); do
    if (set +o pipefail; eval "$@"); then return 0; fi
    sleep 0.1
  done
  return 1
}

# `grep -q` for the tail of a pipeline: same question ("did anything
# match?"), same exit status, but it reads stdin to EOF instead of
# exiting on the first hit.
#
#   ppz_a ls | ls_normalize | matches '^chat.heartbeat'
#   printf '%s' "$page" | matches -F "$needle"
#
# Flags and patterns pass straight through to grep.
#
# Why this exists: `grep -q` short-circuits, and whatever is still
# writing upstream then takes SIGPIPE — awk/sed inside ls_normalize, or
# a Go CLI, whose os.Stdout is unbuffered and so writes one syscall per
# line. Under this file's `set -o pipefail` that 141 becomes the
# pipeline's status, so the assertion reads as a miss even though the
# pattern matched. It is a race (does grep exit before upstream's next
# write?), so it shows up as a rare CI flake, and it is worst when the
# match is on an early line: measured ~3% of runs for the first row of
# a six-row `ppz ls`, 0% for the last. That was PR #184; wait_for
# documents the same trap above.
#
# Reading to EOF removes the cause rather than masking the status, so
# call sites keep their literal pipeline (no subshell, no eval quoting).
#
# The rule is therefore just: never end a pipeline in `grep -q`. No
# judgement call about which upstreams are "safe enough" — a builtin
# looks immune only while the data fits the 64K pipe buffer, and bash's
# printf returns nonzero on EPIPE like anything else. Every piped site
# in tests/ uses `matches`.
#
# `grep -q` is still right where nothing can be signalled — a file
# argument, `grep -q x "$err"` — and on a producer that never closes
# stdout, where short-circuiting is the point and `matches` would block
# forever. wait_for keeps its own guard because it evals arbitrary
# expressions.
matches() { grep -c "$@" >/dev/null; }

# Print only the last broadcast payload visible to daemon A for handle $1.
# Post-v0.34: NAMESPACE column owns field index 1; PIPE moved to $2.
# The header's PIPE field is the literal "PIPE", which won't match a
# real handle, so the awk guard skips it without an explicit header check.
last_payload_a() {
  ppz_a ls | awk -v h="$1" '$2 == h { for (i=5; i<=NF; i++) printf "%s%s", $i, (i==NF?"":" ") }'
}

# Normalise `ppz ls` output for diff-based assertions. Strips the
# NAMESPACE header row, FUSES the leading NAMESPACE cell into the PIPE
# cell so downstream filters see the pre-v0.34 `<manifold>.<pipe>`
# combined form (root → just `<pipe>`, manifold → `<manifold>.<pipe>`);
# callers that need to assert NAMESPACE as its own field use raw
# `ppz ls` instead. Collapses runs of whitespace (the table renderer
# pads columns to varying widths), and replaces "just now" / "N
# seconds ago" / "5 minutes ago" / etc. with the literal token
# RELATIVE so tests don't drift with wall-clock time. Apply BEFORE
# grep — the fused field is what downstream `grep '^<pipe>'` patterns
# expect.
ls_normalize() {
  awk '
    $1 == "NAMESPACE" { next }
    {
      ns = $1
      $1 = ""
      sub(/^[ \t]+/, "")
      if (ns == "-") print $0
      else           print ns "." $0
    }
  ' \
    | sed -E 's/[[:space:]]+/ /g' \
    | sed -E 's/(just now|[0-9]+ (seconds?|minutes?|hours?|days?) ago)/RELATIVE/'
}
