# How it works

```
backup.tar ──┬── db/database.sql.gz ──► streaming COPY parser ──► index.json
             │                            (no PostgreSQL)          + issues on disk
             │
             └── repositories/**/*.bundle ──► on first visit: pack + .idx ──► web UI
                    (read in place)              (pure Go, like git           + git clone
                                                  index-pack --fix-thin)        (smart HTTP)
```

1. **Scan.** The tar's headers are read and bodies are skipped by seeking, so
   repositories appear (by hash) within seconds.
2. **Database.** `db/database.sql.gz` is decompressed as a stream. Only the
   `COPY` blocks of `namespaces`, `routes`, `projects` and
   `project_repositories` are decoded, plus `issues`, `merge_requests`,
   `merge_request_diffs`, `notes`, `labels`, `label_links`, `milestones`,
   `releases`, `release_links`, `snippets`, `snippet_repositories` and
   `users` (username and display name only) unless `--no-issues` is set.
   Label links can come before or after the issues they point to in the dump,
   so they are kept as pairs of ids and filed per project at the end. Everything else (CI, events, e-mails, password
   hashes, tokens, secrets) is skipped without being parsed. Columns are found
   by name, so schema changes between GitLab versions are tolerated.
   The import runs as a pipeline: decompression, parsing and writing issues
   each have their own goroutine and exchange small blocks, so they overlap
   on a multi-core CPU (on one core they simply take turns). On a synthetic
   dump of 476 MB with 400k comments the first import takes ~3.7 s on two
   cores, where decompression alone takes 3 s.
3. **Index.** Hashed storage paths (`@hashed/ab/cd/<sha256(id)>`) are joined
   with project paths and saved as a small JSON file, reused until the backup
   file changes. Issues, merge requests and comments go to small per-project
   files.
4. **Repositories.** On first visit, a project's bundle chain (full plus any
   incremental bundles) becomes a bare repository: each bundle's pack is
   indexed (idx v2) and thin packs are completed. A built-in object reader
   serves trees, blobs and history.
5. **Clone.** Repositories are served over git's smart HTTP protocol, in its
   simplest form: gbb advertises the refs and sends the repository's packs
   as they are on disk, joined into one if there are several. Nothing is
   recompressed or recomputed, so serving a clone costs no more CPU than
   serving a file. Because of that:
   - a clone of a repository not unbundled yet starts at once, and git shows
     the unbundling progress (`remote: gbb: unbundling…`);
   - git, libgit2 (cargo, GUI clients) and other smart-only clients work;
   - every fetch gets the whole repository, whatever the client already has
     (each new backup is a new set of packs anyway);
   - `--depth` is refused with a clear message; `--filter` is not offered,
     so git ignores it and makes a full clone.

   The dumb protocol (static files) still works for clients that ask for
   it. Only branches, tags and merge-request refs are advertised; GitLab's
   internal refs (`keep-around`, `pipelines`…) are hidden, though the objects
   only they reach are still in the packs.

## Backup formats

| GitLab | Layout under `repositories/` |
|---|---|
| 13 – 15 (classic) | `[storage/]@hashed/ab/cd/<hash>.bundle`, `.wiki.bundle`, `.design.bundle` |
| 15+ (gitaly-backup) | `<storage>/@hashed/ab/cd/<hash>.git/<backup_id>/NNN.bundle` + `NNN.refs`, incremental chains |
| legacy storage | `group/project.bundle` |

Tested on a real GitLab 19.5.3 backup (28.8 GB, 657 projects).

A backup can be an uncompressed `.tar` (read in place), a `.tar.gz` (the parts
gbb needs are extracted once into the data directory) or an extracted
directory.

## Data directory

`--data` (default: the user cache directory) holds, per backup, the index,
issues and unbundled repositories. A backup's cache is kept while the backup
file exists unchanged, and removed once it is rotated away or modified.

## Git implementation

The only dependency is [goldmark](https://github.com/yuin/goldmark), for
Markdown. The pack reader, delta resolver, index writer and pack sender live in
`internal/gitstore`; the git HTTP protocol is in `internal/web/smart.go`. They are tested against the `git` command line:
`git index-pack` output must match byte for byte, `git fsck --strict` must pass
on every restored repository, and real `git clone` runs against the server in
the test suite.

Delta resolution releases each base as soon as it has been used and re-reads
large bases instead of holding them, so memory follows the largest object
rather than the length of delta chains.

Rough numbers on a laptop: unbundling a 28 MiB bundle with ~16k objects takes
~1 s (about 2× `git index-pack`); pages answer in a few milliseconds.

## Git LFS and attachments

GitLab stores them outside the repositories, in `lfs.tar.gz` and
`uploads.tar.gz`. Both are gzipped, so they cannot be read in place like the
main tar: with `--lfs` / `--uploads`, gbb extracts them **once**, in the
background after the index is ready (paused on battery with
`--pause-on-battery`), into the data directory. It checks the free space
first, builds the result in a temporary directory and only uses it once
complete; progress and sizes are on the *About this backup* page.

`--lfs=head` keeps only the objects that the default branch of each project
points to. The database (`lfs_objects_projects`) says which projects use
LFS; only those repositories are unbundled, their HEAD tree is walked, and
blobs small enough to be pointers (a size read from the pack header, without
inflating anything) are parsed. The extraction then skips every other
object. Asking for all objects later extracts again, while the partial copy
keeps being served.

Once extracted:

- file pages show the real file behind a Git LFS pointer (images, text,
  downloads), raw downloads and `.zip` archives contain it too;
- `git clone` with git-lfs installed fetches LFS files: gbb implements the
  download side of the Git LFS batch API;
- `/uploads/<secret>/<file>` links in issues, merge requests and comments
  point to the attachment.

Without extraction, LFS files show their pointer with an explanation, and
attachment links answer 404 with a hint. LFS objects GitLab keeps in object
storage are not part of the backup; such files stay pointers.

## Code search

`/-/code` greps the default branch of every project (or of one project), with
memory kept flat on purpose:

- repositories are searched one at a time and files one at a time; only
  matching lines are kept (at most 5 per file, 500 per search);
- files over 1 MiB, binaries, Git LFS pointers and known binary extensions are
  skipped, large ones straight from their pack header, without inflating them;
- only one search runs at a time (others wait), and a search stops when the
  page is closed;
- by default only repositories already unbundled are searched; "include not
  yet opened" unbundles the others one by one;
- results are streamed to the browser as they are found, most recently active
  projects first. Patterns are plain text or RE2 regular expressions (linear
  time, safe on any input).

Measured on a synthetic backup: 651 repositories, 30k files (257 MiB) searched
in about 2 s with a ~70 MB peak.

## Security

- Read-only for the backup: nothing is ever written to it, and git push is
  refused. The only POST endpoints are git's fetch and the Git LFS batch
  call (both reads), and the action buttons (open another
  backup found in the `--backups` directories, extract LFS or attachments,
  unbundle repositories, delete the caches of other backups). They need a
  per-process token and a same-origin request, and are disabled when gbb
  listens on the network without `--auth`.
- Listens on `127.0.0.1` by default. With `--listen :8080`, use
  `--auth user:password` (or `GBB_AUTH`), otherwise anyone on the network can
  read every repository, private ones included. git asks for the same login
  when cloning. Basic auth travels in clear text over plain HTTP: put a TLS
  reverse proxy in front on untrusted networks. Failed logins are not
  limited: use a long password, or let the proxy limit them.
- Without `--auth`, only requests addressed to an IP address, `localhost` or
  a `--host` name are answered, so a web page cannot read the backup through
  a visitor's browser by pointing its own domain at gbb (DNS rebinding).
- The cache of each backup is a directory only its owner can enter (mode
  `0700`): other users of the machine cannot read the unbundled repositories,
  issues or attachments.
- Repository content is untrusted: raw files are served with a sandboxing
  Content-Security-Policy, READMEs and comments are rendered with raw HTML
  escaped, and Git LFS pointers are never served as the file they stand for.
- Tar member paths are validated (no absolute paths, `..`, backslashes or
  NUL); nothing is written outside the data directory.
- Archives and dumps are parsed by gbb's own code, which is fuzzed weekly
  in CI: sizes and counts read from the input are checked before anything
  is allocated, so a damaged or hostile backup gives an error, not a crash.
