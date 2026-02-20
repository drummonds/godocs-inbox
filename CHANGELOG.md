# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/).

## [0.4.0] - 2026-02-19

- Making buttons always clickable
- Not losing place when submitting an image

## [0.3.1] - 2026-02-19

- Implmenting looking at the backlog set

## [0.3.0] - 2026-02-19

- Changing formatting

## [0.2.0] - 2026-02-17

### Changed
- Merged edit page into main inbox page — single page for document triage and tag editing
- Date provenance badge: LLM-inferred dates show warning badge, pre-existing dates show success badge
- Keyboard `d` key for done/next document, `1`/`2`/`3` for recent tag sets
- Removed `e` key (edit tags) shortcut — tag editor is now inline

### Added
- Recent tag sets: last 3 applied tag sets shown with 1/2/3 keyboard shortcuts
- `POST /done` endpoint to capture tag set and advance
- `POST /api/apply-tagset` endpoint to apply a recent tag set
- Reserved key collision warning at startup

### Removed
- Separate `/edit` page and `edit.html` template

## [0.1.0] - 2026-02-14

### Added
- Keyboard-driven inbox triage with configurable tag shortcuts
- Demo mode with sample data (no godocs server required)
- Server mode connecting to godocs API
- Document preview with thumbnail proxy
- Tag editing page with full tag management
- Undo last action support
- About page showing configuration and server tags
