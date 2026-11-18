# GitLab versions

gbb reads GitLab's backup files and database dump directly, so it depends
on how GitLab lays them out. Both change between versions. This page says
what is supported, how gbb copes with differences, and how a change in a
new GitLab version gets noticed.

## Tested

| GitLab | How |
|---|---|
| 19.5 | real 28.8 GB backup (657 projects), on a server and on an Android phone |
| 19 (layout) | test backups: split attachment tables, multi-line backup date |
| 17 | test backups (the default ones) |
| 12 (layout) | test backups: legacy storage, text states, `WIP:` drafts, no topics, snippets without repositories |
| 15.11, 16.11, 17.11, newest release, `master` | weekly CI check of the database schema (see below) |

Backups from GitLab 13 to 19 are expected to work fully. Older ones work
with the fallbacks listed below; anything the dump lacks is reported.

## Differences handled

| Change | Since (approx.) | What gbb does |
|---|---|---|
| Repository bundles `<hash>.bundle` → `<hash>.git/<backup_id>/NNN.bundle` + `.refs`, incremental chains | 15+ (gitaly-backup) | both layouts read; chains joined from their full bundle |
| Legacy storage (repositories under their full path) → hashed storage | 10–13 | storage path from `project_repositories`, else hashed id, else full path |
| `issues.state`, `merge_requests.state` (text) → `state_id` (number) | 12 | either column |
| Drafts as `WIP:` titles → `merge_requests.draft` | 13–14 | column if present, else title prefix (`Draft:`, `WIP:`, `[Draft]`) |
| `notes.confidential` → `notes.internal` | 15 | either column |
| `users.bot_type` → `users.user_type` | 13 | either column |
| Project tags → `topics` / `project_topics` | 14 | topics shown when the tables exist |
| Snippet content in the database → snippet repositories (`@snippets/`) | 13 | repositories browsed and cloned; older snippets listed without files |
| `uploads` → one table per kind (`project_uploads`, `user_uploads`…) | 18 | every table with the attachment columns is read; owner type from the table name when needed; rows in both counted once |
| Group descriptions `namespaces.description` → `namespace_details.description` | 19 | either table |
| `backup_information.yml` date as text → multi-line `TimeWithZone` | 19 | both read |
| LFS objects / attachments in object storage | any | counted, never expected in the backup |

## When a GitLab version changes something

Everything gbb reads from the database is described in one place,
`internal/index/schema.go`: each table and column, the names it had in
other versions, and what goes wrong without it. The import reads columns
through that description, never by position, so:

- a **new** table or column is ignored;
- a column with an **older name** is read under that name, and noted;
- a **missing** table or column is noted, with its effect, instead of
  silently showing wrong or empty data.

These notes appear:

- on the *About this backup* page, under Database;
- in `gbb verify`: missing data gbb needs is a warning
  (`database_schema`), and every note is in the JSON report (`schema`), so a
  nightly check flags the first backup of a GitLab version that broke
  something.

The same description is checked every week in CI against GitLab's own
`db/structure.sql` (workflow `gitlab-schema` in `.github/workflows/`), for the last
release of older majors, the newest release and GitLab's `master`, so a
change is usually seen before anyone upgrades.

## Adding support for a change

1. In `internal/index/schema.go`, add the old or new column name to `Alts`
   (or a new table).
2. If the meaning changed too (text instead of numbers), check `c.via(name)`
   in `internal/index/db.go` and convert, as `issueState` does.
3. Teach the test backups that version (`internal/fixture`, `Era`), so the
   whole import and `verify --deep` run on it (`TestGitLabEras`,
   `TestVerifyEras`).
