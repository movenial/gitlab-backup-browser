# Commands and flags

```
gbb serve  [flags] [backup]   web UI and git server
gbb verify [flags] <backup>   check a backup (see monitoring.md)
gbb list   [flags] <backup>   print the projects
gbb demo   <dir>              create two small sample backups (needs git)
gbb version
```

`<backup>` is a `*_gitlab_backup.tar`, a `.tar.gz`, or an extracted backup
directory.

## Common

| Flag | Default | |
|---|---|---|
| `--data DIR` | user cache dir | index, issues and unbundled repositories |

## `serve`

| Flag | Default | |
|---|---|---|
| `--listen ADDR` | `127.0.0.1:8080` | `:8080` to allow other devices |
| `--backups DIR` | see below | directory where the Backups page looks for archives (repeatable) |
| `--auth user:password` | none | require a login (or `GBB_AUTH`) |
| `--host NAME` | none | host name the server is reached by, besides IP addresses and `localhost` (repeatable; not needed with `--auth`) |
| `--no-issues` | off | skip issues, merge requests and comments |
| `--lfs`, `--lfs=head` | off | extract `lfs.tar.gz` once to show Git LFS files and serve them to git-lfs: all objects (about the archive's size on disk), or with `head` only the files of each project's default branch |
| `--uploads` | off | extract `uploads.tar.gz` once to show images and attachments of issues, MRs and comments |
| `--prefetch` | off | unbundle every repository after indexing |
| `--pause-on-battery` | off | pause background work while not charging |
| `--memory-limit MiB` | 128 on Android, 256 elsewhere | soft memory limit (0 = none; `GOMEMLIMIT` wins) |
| `--max-open-repos N` | 6 | repositories kept open at once |
| `--cache MiB` | 24 | object cache shared by all repositories |

Without a backup argument, `gbb serve` opens on the **Backups** page, which
lists the `*_gitlab_backup.tar(.gz)` files and extracted backup directories
found in the `--backups` directories. Without `--backups` it looks in
`/var/opt/gitlab/backups`, `~/Downloads`, `~/storage/shared/Download` and
`/sdcard/Download` (Termux), and the current directory. Pick one and it is
imported in the background; switching back to a backup already indexed is
instant, because its cache in `--data` is reused. Only files found by that
scan can be opened, never an arbitrary path.

### Action buttons

The web UI has a few buttons that change what the server does:

- **Open** another backup (Backups page);
- **Extract** LFS objects or uploads when they were not enabled at start
  (About this backup page, and on an LFS file); the choice is remembered
  for that backup;
- **Unbundle the 20 most active repositories** now, so they open instantly;
- **Delete other caches**: remove the caches of backups that are not open.

They are protected by the same login as the rest of the site (`--auth`), and
use a per-process token plus an origin check against cross-site requests.
When gbb listens on the network **without** `--auth`, the buttons are
disabled: anyone on the network can browse, nobody can change anything.

### Host names

Without `--auth`, gbb answers only requests addressed to an IP address, to
`localhost`, or to a name given with `--host`: `http://192.168.1.20:8080`
works as it is, `http://nas.lan:8080` needs `--host nas.lan`. Other names get
a `421` response that says so. This stops a web page from reading the backup
through the browser of someone who has gbb open, by pointing its own domain
at gbb's address (DNS rebinding). With `--auth` every name is accepted: such
a page has no login to send.

## `verify`

| Flag | Default | |
|---|---|---|
| `--deep` | off | restore every repository; classify branch moves |
| `-v` | off | with `--deep`, one line per repository |
| `--jobs N` | 1 | with `--deep`, repositories restored in parallel (0 = one per CPU); memory grows with N |
| `--previous BACKUP` | none | compare with an older backup |
| `--ignore-refs PATTERN` | none | with `--previous`: list but do not warn about changes of these refs (`renovate/*`, `group/mirror:*`; repeatable) |
| `--max-age DURATION` | none | error when the backup is older, e.g. `26h` |
| `--json` | off | JSON report on stdout |
| `--prom FILE` | none | Prometheus textfile metrics |

Exit status: `0` ok, `1` warnings, `2` errors.
