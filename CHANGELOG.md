# Changelog

## Unreleased

- Confidential issues and internal comments are readable only when gbb runs
  with a login (`--auth`). Without one they are placeholders: the issue's
  number and state with a note on how to read it, the comment's date.
  Searches and filters leave confidential issues out. Existing caches are
  re-indexed once.
- `gbb demo` works with a relative directory (`gbb demo sample`).

## v0.1.2 (2026-11-19)

First public release.
