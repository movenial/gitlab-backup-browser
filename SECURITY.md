# Security

gbb serves the full content of a GitLab backup, private repositories
included, so please report vulnerabilities privately: use GitHub's
**Report a vulnerability** button (Security tab of this repository), not a
public issue.

Useful to include: the gbb version (`gbb version`), how it was started
(flags, `--listen`, `--auth`), and the request or file that triggers the
problem. A backup is rarely needed: a small crafted archive or dump is
usually enough, and the fuzz targets show how to build one.

Fixes go into the next release; there is no support for older versions.

What gbb protects against, and what it does not, is described in
[docs/how-it-works.md](docs/how-it-works.md#security).
