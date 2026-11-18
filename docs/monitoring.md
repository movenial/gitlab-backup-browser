# Monitoring nightly backups

`gbb verify` checks a backup without restoring it, compares it with the
previous one, and can feed Prometheus. It is meant to run right after
GitLab's nightly backup.

```sh
LATEST=$(ls -t /var/opt/gitlab/backups/*_gitlab_backup.tar | sed -n 1p)
PREVIOUS=$(ls -t /var/opt/gitlab/backups/*_gitlab_backup.tar | sed -n 2p)
gbb verify --deep --jobs 0 --max-age 26h --previous "$PREVIOUS" \
  --prom /var/lib/node_exporter/textfile/gbb.prom "$LATEST"
```

## Levels of checking

| Command | What it checks | Cost |
|---|---|---|
| `gbb verify backup.tar` | the database dump is readable, every bundle header is readable, incremental chains have their full bundle | seconds |
| `gbb verify --deep backup.tar` | every repository is fully restored and every object checked; every LFS object and attachment the database lists is in `lfs.tar.gz` / `uploads.tar.gz` | minutes (≈4 min for 650 repositories, plus decompressing the two archives) |

`--deep` also lists the five repositories that need the most memory to
unbundle (the first-visit cost in `gbb serve`); `-v` prints time, object count
and peak memory for every repository.

On a server or NAS, `--jobs N` restores N repositories at once (`--jobs 0`:
one per CPU). It divides the time of `--deep` roughly by the number of cores,
and the peak memory can grow up to N times the heaviest repository, so keep
the default of 1 on a phone. Per-repository memory figures are only measured
with `--jobs 1` (the heap is shared); with more jobs the slowest repositories
are listed instead. The report is identical whatever the number of jobs.

### LFS objects and attachments

With `--deep`, the names inside `lfs.tar.gz` and `uploads.tar.gz` are
listed (the archives are decompressed as a stream, nothing is written) and
compared with the database tables `lfs_objects` and the attachment tables
(`uploads` up to GitLab 17; from GitLab 18 one table per kind:
`project_uploads`, `namespace_uploads`, `user_uploads`, ...; gbb reads every
table that has the attachment columns):

- objects GitLab keeps in **object storage** are never in backups: they are
  counted (`object_storage`), not reported as missing;
- a file listed on disk but absent from the archive is an error
  (`lfs_missing`, `uploads_missing`, with the first ones named);
- an archive absent although files are expected is an error
  (`lfs_archive_missing`), or a warning when `backup_information.yml` says it
  was skipped (`SKIP=lfs`);
- attachments of uploaders whose layout gbb does not know are counted as
  `unchecked`;
- an archive that holds files while the database lists none to compare
  with is a warning (`lfs_unchecked`, `uploads_unchecked`): a check that
  compares with nothing must not pass silently.

Prometheus: `gbb_backup_assets{archive="lfs|uploads",state="found|missing|object_storage"}`.

## Exit status

| Code | Meaning |
|---|---|
| `0` | ok |
| `1` | warnings only |
| `2` | errors, including when the check itself cannot run (missing backup, bad arguments) |

**Errors:** unreadable or corrupt bundle, incremental chain without its full
bundle, missing or unreadable database dump, backup older than `--max-age`.

## Comparing with the previous backup

`--previous <older backup>` adds these **warnings**:

- projects that disappeared, projects that lost their repository bundle;
- deleted branches or tags, moved tags. A branch deleted after its merge
  request was merged or closed is normal GitLab behaviour: it is reported as
  `deleted-merged`, not as a warning;
- fewer issues or merge requests, in total and per project;
- a backup that is not newer than the previous one.

Renamed and added projects are listed but are not warnings.

With `--deep`, every branch that moved is classified by looking at the new
repository's history:

| Change | Meaning | Reported |
|---|---|---|
| `fast-forward` | normal push | no |
| `deleted-merged` | source branch removed after its MR was merged or closed | no |
| `rewound` | moved back to an older commit | warning |
| `diverged` | force-pushed | warning |
| `rewritten` | the old commit is not in the backup at all | warning |

Rewound or rewritten branches are the typical sign of a truncated backup.
Without `--deep`, moved branches are only counted as `moved`.

Some refs are rewritten on purpose every day: bot branches (Renovate,
Dependabot rebase theirs) or a mirror whose upstream force-pushes. So that
the nightly alert stays meaningful, `--ignore-refs` lists refs whose
changes are still shown (`"ignored": true` in JSON, a count in the text)
but not warned about:

```sh
gbb verify --deep --previous yesterday.tar \
  --ignore-refs 'renovate/*' --ignore-refs 'dependabot/*' \
  --ignore-refs 'mirrors/upstream-project:*' today.tar
```

A pattern is a branch or tag name (`*` matches anything, slashes included),
or `group/project:pattern` for one project. Deleted branches, moved tags,
rewound, force-pushed and rewritten branches are all covered.

Restored repositories are written under the data directory (`<data>/tmp`,
not `/tmp`) and removed at the end, also on Ctrl-C or SIGTERM.

Measured on a real 657-project backup (a mini PC): `verify --previous` in 32 s
with an 87 MB peak; `--deep --jobs 4` in 102 s instead of 242 s.

The comparison needs the previous archive to still be on disk. Its index stays
cached in the data directory until that file is rotated away, so comparing is
nearly free when yesterday's backup was verified yesterday.

## Output formats

- Text summary by default.
- `--json`: the full report (summary, problems, comparison, heaviest
  repositories) on stdout; progress goes to stderr.
- `--prom FILE`: metrics for the node_exporter textfile collector, written
  atomically.

## Prometheus

Example alerts:

```yaml
groups:
- name: gitlab-backup
  rules:
  - alert: GitLabBackupTooOld
    expr: time() - gbb_backup_created_timestamp_seconds > 26 * 3600
  - alert: GitLabBackupBroken
    expr: gbb_verify_status == 2
  - alert: GitLabBackupSuspicious
    expr: gbb_verify_status == 1
  - alert: GitLabBackupLostProjects
    expr: gbb_compare_projects_removed > 0 or gbb_compare_repositories_lost > 0
  - alert: GitLabBackupHistoryRewound
    expr: sum(gbb_compare_refs{change=~"rewound|rewritten"}) > 0
  - alert: GitLabBackupCheckNotRunning
    expr: time() - gbb_verify_timestamp_seconds > 26 * 3600
```

Metrics:

| Metric | Description |
|---|---|
| `gbb_verify_status` | 0 ok, 1 warnings, 2 errors |
| `gbb_verify_errors`, `gbb_verify_warnings` | counts |
| `gbb_verify_problems{severity,kind}` | problems by kind |
| `gbb_verify_deep` | 1 when every repository was restored |
| `gbb_verify_timestamp_seconds`, `gbb_verify_duration_seconds` | when, and how long (import included) |
| `gbb_verify_import_seconds` | part spent indexing the backup (near 0 when cached) |
| `gbb_backup_info{gitlab_version,file}` | always 1 |
| `gbb_backup_created_timestamp_seconds` | when GitLab created the backup |
| `gbb_backup_size_bytes`, `gbb_backup_database_ok` | archive size, dump readable |
| `gbb_backup_projects{state}` | `with_repository` / `without_repository` |
| `gbb_backup_repositories`, `gbb_backup_repository_bytes` | bundles |
| `gbb_backup_wikis`, `gbb_backup_namespaces`, `gbb_backup_unmatched_bundles` | |
| `gbb_backup_issues`, `gbb_backup_merge_requests` | |
| `gbb_compare_available` | 1 when compared with a previous backup |
| `gbb_compare_projects_{added,removed,renamed}`, `gbb_compare_repositories_lost` | |
| `gbb_compare_refs{change}` | added, deleted, deleted-merged, moved, fast-forward, rewound, diverged, rewritten; refs matched by `--ignore-refs` excluded |
| `gbb_compare_refs_ignored{change}` | the same, for refs matched by `--ignore-refs` |
| `gbb_compare_issues_delta`, `gbb_compare_merge_requests_delta` | |

## Trying it

`gbb demo <dir>` creates two sample backups a day apart, with a deleted
project, a rewound branch, a deleted tag, a force-push and a deleted issue:

```sh
gbb demo /tmp/sample
gbb verify --deep --previous /tmp/sample/day1/*.tar /tmp/sample/day2/*.tar
```
