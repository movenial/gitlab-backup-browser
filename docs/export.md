# Exporting all repositories

gbb has no export command on purpose: the server already makes every
repository clonable, so a few lines of shell are enough.

```sh
gbb serve backup.tar &
sleep 5   # wait for the index (or watch http://localhost:8080)

gbb list backup.tar | awk '$1=="code" {print $2}' | while read -r p; do
  git clone --mirror "http://localhost:8080/$p.git" "export/$p.git"
done
```

`gbb list` prints one line per project: `code`, `no-repo` or `unmatched`, the
project path, then `+wiki` / `archived` flags. The result is a tree of bare
repositories, with all branches and tags, laid out like the GitLab groups:

```
export/
  infrastructure/internal.git
  web/site.git
```

The first clone of each repository waits while gbb unbundles it; git shows
the progress.

## Wikis

```sh
gbb list backup.tar | awk '/\+wiki/ {print $2}' | while read -r p; do
  git clone --mirror "http://localhost:8080/$p.wiki.git" "export/$p.wiki.git"
done
```

## With a login

If the server runs with `--auth`, put the credentials in the URL
(`http://user:password@localhost:8080/...`) or in a git credential helper.

## Pushing to Forgejo or Gitea

Push each mirror to a repository created on the new server:

```sh
git -C "export/$p.git" push --mirror "https://forgejo.example/$p.git"
```

Forgejo and Gitea can also *adopt* bare repositories copied into their
repository directory (Site administration → Repositories → Unadopted
repositories).

gbb has no built-in `--push` yet.
