# camux

Claude-Code-aware orchestration on top of [`amux`](../amux). Small Go CLI,
shells out to `amux` for tmux primitives, adds:

- State-machine-aware `ask`: refuses to submit unless Ready, waits for
  streaming to finish, emits the reply delta.
- `spawn` that launches Claude and handles the trust-folder dialog
  automatically.
- `status` / `trust` / `permit` / `interrupt` / `clear` / `slash` for
  the common Claude TUI interactions.

See [AGENTS.md](./AGENTS.md) for the full cookbook.

```bash
TARGET=$(camux spawn demo)
echo "what is 17*23?" | camux ask $TARGET        # → ⏺ 391
amux kill demo
```

## Install

```bash
git clone ... ~/dev/camux
cd ~/dev/camux && make install
```

Requires `amux` on `$PATH` (see sibling repo).

## Tests

```bash
go test ./... -v -count=1           # 8 tests, real tmux + real Claude
CAMUX_SKIP_CLAUDE=1 go test ./...   # fast path, skips Claude e2e
```
