# Capsule conformance

Results of the hostile test capsules (Capsules Spec, Conformance suite) per backend.
Each row is an attack a capsule tries; "blocked" means the test passes.
Run them with `go test ./internal/capsule/...` on the target OS.

| Attack | Linux | Windows | macOS | Test |
| --- | --- | --- | --- | --- |
| Read `~` / `~/.ssh` | blocked | — | — | `sandbox.TestIsolation` |
| Read the host's token store | blocked | — | — | `sandbox.TestIsolation` |
| Read another capsule's `data/` | blocked | — | — | `sandbox.TestIsolation` |
| Write outside its own `data/` | blocked | — | — | `sandbox.TestIsolation` |
| Execute a file it wrote | blocked | — | — | `sandbox.TestIsolation` |
| Write a single mounted file, or see the folder around it (`runtime = "luna"`) | blocked | — | — | `sandbox.TestFileMount` |
| TCP connection with `net = "none"` | blocked | — | — | `sandbox.TestIsolation` |
| Reach a non-allowlisted host | n/a: allowlists not implemented, such packages are refused | — | — | `capsule.CanRun` |
| Allocate past the memory cap | killed, cause `oom` | — | — | `sandbox.TestLimits` |
| Fork bomb past `limits.pids` | blocked | — | — | `sandbox.TestLimits` |
| Open files past `limits.files` | blocked | — | — | `sandbox.TestLimits` |
| See processes outside its tree | blocked (sees only itself) | — | — | `sandbox.TestIsolation` |
| Signal a process outside its tree | blocked | — | — | `sandbox.TestIsolation` |
| Leave a child running after it stops | blocked | — | — | `sandbox.TestNoOrphans` |
| New user namespace, `mount` | blocked | — | — | `sandbox.TestIsolation` |
| Connect to another capsule's link socket directly | blocked | — | — | `sandbox.TestIsolation` |
| Spoof the caller ID on a link call | overwritten by the broker | — | — | `capsule.TestLinks` |
| Send a 2 MiB frame | connection closed | — | — | `capsule.TestLinks`, `capsule.TestFrameTooBig` |
| Hang without `hello` | killed, cause `handshake` | — | — | `capsule.TestHandshakeAndHealth` |
| Stop answering pings | killed, cause `unhealthy` | — | — | `capsule.TestHandshakeAndHealth` |

Linux results are from kernel 7.2 with Landlock ABI 10 and a systemd user session.
Without a systemd user session the memory cap falls back to a 1 s RSS watchdog and
`limits.pids` to `RLIMIT_NPROC`; `luna capsule doctor` reports which applies.
