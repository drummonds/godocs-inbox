# godocs-inbox

Keyboard-driven document triage web UI for godocs.

## Architecture

Single-binary Go web server. Two modes:
- **Server mode**: connects to a godocs API server, operates on real documents
- **Demo mode** (`-demo`): uses local filesystem with sample data, no server needed

All code is in `main.go`. HTML templates are embedded via `//go:embed`.

## Key paths

- `main.go` - all application code (config, API client, HTTP handlers)
- `templates/` - HTML templates (embedded at build time)
- `godocs-inbox.yaml` - runtime config (not committed)

## Build & run

```bash
task build        # build binary
task run          # build and run (server mode)
task run:demo     # build and run (demo mode)
task check        # fmt + vet + test
```
