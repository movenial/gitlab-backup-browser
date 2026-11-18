# Running on an Android phone

gbb runs in [Termux](https://termux.dev) (or from `adb shell`) as a single
static binary; no root, no extra packages.

```sh
termux-setup-storage            # gives Termux access to shared storage
curl -fLO https://github.com/movenial/gitlab-backup-browser/releases/latest/download/gbb-android-arm64
chmod +x gbb-android-arm64
./gbb-android-arm64 serve --listen :8080 --pause-on-battery \
  ~/storage/shared/backup.tar
```

Open `http://localhost:8080` on the phone, or `http://<phone-ip>:8080` from
another device. When listening on the network, add `--auth user:password`.

Without the backup argument, gbb opens on a Backups page listing the archives
in `~/storage/shared/Download` (and `--backups DIR` if they are elsewhere):
copy a new backup there and open it from the browser, no need to restart.
The Open, Extract and cache buttons need `--auth` when listening on `:8080`.

## Battery

`--pause-on-battery` holds background work (database import, `--prefetch`)
while the phone is unplugged; browsing stays available. The battery state is
read from `/sys/class/power_supply/*/status`, which Android exposes to Termux
and to `adb shell` alike; `termux-battery-status` (Termux:API) is used as a
fallback. "Not charging" (charge limit reached while plugged in) counts as
plugged in.

Android may kill Termux in the background: run `termux-wake-lock` while the
server should stay up, `termux-wake-unlock` afterwards. To start gbb when the
phone boots, see [running.md](running.md#on-a-phone-termuxboot).

## Memory

Android kills apps that grow too much, even briefly, so gbb keeps both its
usual footprint and its peaks small:

- pack indexes are memory-mapped, not loaded on the heap;
- all repositories share one object cache (`--cache`, 24 MiB by default);
- at most `--max-open-repos` repositories (6) stay open; others, and any
  repository unused for 3 minutes, are closed and their memory handed back;
- the soft memory limit is 128 MiB on Android, 256 MiB elsewhere
  (`--memory-limit`, or `GOMEMLIMIT`);
- issues, merge requests and comments are streamed to disk during the import;
- unbundling a repository needs memory proportional to its *largest object*,
  not to its size or history: about 2.3× the largest file (delta bases are
  released as soon as possible, leaf objects are hashed without being built,
  and memory is reclaimed right after a large file is dropped).

The one thing that can still cost a lot is the **first visit of a repository
holding very large files**. To know in advance, run `gbb verify --deep -v` on
a computer: it prints the peak memory of every repository, and the five
heaviest at the end. A running server shows its usage on the *About this
backup* page.

### Measured on a Pixel 10a (Android 17, real backup)

GitLab 19.5.3 backup, 28.8 GB, 657 projects; gbb started from `adb shell`.

| Situation | Result |
|---|---|
| first import (`verify`) | 1 min 40 |
| `serve` idle, then after opening a project and cloning it | 18 MB, then 36 MB |
| pages | 13 – 600 ms |
| clone of a mid-sized project | 2.3 s |
| first visit of a large mirror (323k objects) | ~2 min, then ~100 MB |
| first visit of a project with 64 MiB binary blobs | 300 MB peak (225 MB with `--memory-limit 100`) |
| code search, 6 projects / 7,846 files | 8.8 s (one project: 0.85 s) |

The two first-visit figures were measured before the latest memory work
(leaf hashing, earlier collection, 128 MiB default on Android), which brings a
40 MiB binary in 12 versions from 174 MB to 94 MB on a laptop. The waiting page
now shows the unbundling progress.

### Measured on a laptop (synthetic backups)

| Situation | Peak |
|---|---|
| browsing 20 repositories of 28 MiB each in one session | ~82 MB |
| `verify`, 650 projects, 60k MRs, 400k comments | ~70 MB, 3.7 s |
| first visit of a repository with a 40 MiB binary in 12 versions | ~94 MB |
| code search over 651 repositories (30k files, 257 MiB) | ~70 MB, 2 s |

## Storage

The backup can stay on shared storage. gbb writes its index and unbundled
repositories under `--data` (by default `~/.cache/gitlab-backup-browser`);
unbundled repositories take roughly the size of their bundles.

`--lfs` and `--uploads` extract `lfs.tar.gz` and `uploads.tar.gz` there too,
which needs about their size in free space (LFS objects rarely compress). gbb
refuses to start an extraction that would not fit. Extraction streams and
does not use more memory.

On a phone, prefer `--lfs=head`: only the LFS files of each project's
default branch are extracted (on a real backup, 3.1 GiB out of 11.5 GiB).
Older versions of a file then show a button to extract everything.
