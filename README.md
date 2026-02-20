# godocs-inbox

Keyboard-driven document triage UI for [godocs](https://github.com/drummonds/godocs). Presents untagged documents one at a time and lets you tag them with single keystrokes.

## Usage

```bash
# Run with a godocs server
godocs-inbox

# Run with built-in demo data (no server needed)
godocs-inbox -demo

# Create example config file
godocs-inbox -init

# Override listen address
godocs-inbox -addr :9090
```

## Configuration

Create `godocs-inbox.yaml` (or run `godocs-inbox -init`):

```yaml
godocs_server: http://your-godocs:8000
addr: :8080
tags:
  - key: l
    tag_id: 18
  - key: m
    tag_id: 20
```

Tag IDs come from your godocs server: `GET /api/tags`.

## Building

```bash
task build
```

## License

MIT
