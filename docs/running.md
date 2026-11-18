# Running gbb permanently

Ready-to-use files are in [`contrib/`](../contrib): a systemd service for a
server or NAS, a nightly check with a timer, and a Termux:Boot script for a
phone.

## On a server (systemd)

### The web UI

```sh
sudo install -m 755 gbb-linux-amd64 /usr/local/bin/gbb
sudo install -m 644 contrib/systemd/gbb.service /etc/systemd/system/

# A login, needed to use the action buttons from other machines.
sudo install -d -m 700 /etc/gbb
echo 'GBB_AUTH=admin:a-long-password' | sudo tee /etc/gbb/env >/dev/null
sudo chmod 600 /etc/gbb/env

sudo systemctl daemon-reload
sudo systemctl enable --now gbb
journalctl -u gbb -f        # import progress, then "open http://…"
```

The service:

- lists the backups of `/var/opt/gitlab/backups` (pick one in the browser;
  edit `--backups` for another directory, or give a backup path to open one
  at start);
- keeps its index, issues and unbundled repositories in `/var/lib/gbb`
  (created by systemd);
- extracts attachments and the LFS files of default branches (`--lfs=head
  --uploads`; drop them to save space);
- is sandboxed: read-only system and backups, writes only to `/var/lib/gbb`,
  no access to home directories, hard memory ceiling of 1 GiB (gbb itself
  stays near its 256 MiB soft limit).

**Permissions.** GitLab writes backups as `git:git` with mode `0600`, so the
unit runs as `git`; that is the simplest. To use a dedicated user, let
GitLab write group-readable backups and give that user the `git` group:

```ruby
# /etc/gitlab/gitlab.rb, then: sudo gitlab-ctl reconfigure
gitlab_rails['backup_archive_permissions'] = 0640
gitlab_rails['manage_backup_path'] = false   # so the directory mode below stays
```

```sh
sudo chmod 750 /var/opt/gitlab/backups
sudo useradd --system --home /var/lib/gbb --groups git gbb
```

then set `User=gbb` and `Group=gbb` in the unit. (POSIX ACLs do not help
here: with mode `0600`, the ACL mask leaves other users no access.)

**HTTPS.** gbb serves plain HTTP. Behind a reverse proxy (Caddy, nginx,
Traefik), keep `--listen 127.0.0.1:8080`: on the loopback, the action buttons
work and the proxy can add TLS and its own login. The proxy must pass the
original `Host` header (Caddy does; with nginx add
`proxy_set_header Host $host;`), since action buttons check that requests come
from the same site.

### The nightly check

```sh
sudo install -m 755 contrib/systemd/gbb-verify-latest /usr/local/bin/
sudo install -m 644 contrib/systemd/gbb-verify.service contrib/systemd/gbb-verify.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now gbb-verify.timer
systemctl list-timers gbb-verify     # next run
sudo systemctl start gbb-verify      # run it now
journalctl -u gbb-verify             # the report
```

`gbb-verify-latest` runs `gbb verify --deep --max-age 26h` on the newest
backup, compared with the one before it (`--previous`), and writes the
metrics for node_exporter's textfile collector (`PROM` in the unit; remove the
line if you do not use Prometheus). A backup with errors exits with status 2,
so the unit shows as failed: `systemctl --failed`, or alert on
`gbb_verify_status` (see [monitoring.md](monitoring.md)).

Set the timer after GitLab's own backup job (`OnCalendar=`), and keep
`--max-age` a bit above the time between backups.

## On a phone (Termux:Boot)

Termux:Boot starts scripts when the phone boots. Install it from the same
source as Termux (F-Droid or GitHub, not mixed), open it once, then:

```sh
mkdir -p ~/.termux/boot
cp contrib/termux/start-gbb ~/.termux/boot/
chmod +x ~/.termux/boot/start-gbb
```

The script takes a wake lock, starts gbb on `:8080` with
`--pause-on-battery`, lists the backups in `Download`, restarts gbb if it ever
stops, and logs to `~/gbb.log`. Edit the `GBB` path to where the binary is,
and uncomment `GBB_AUTH` if other devices connect to it.

Android may still stop background apps:

- in the system settings, set Termux's battery usage to *Unrestricted*;
- keep Termux's notification (it is what keeps it alive);
- after an update of Android or Termux, open Termux once.

To stop it: `pkill -f 'gbb serve'` stops gbb, but the script restarts it ten
seconds later; `pkill -f start-gbb` first stops the script itself.
