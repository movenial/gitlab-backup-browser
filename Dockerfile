# The gbb release binary in an otherwise empty image, for hosts that run it from
# cron with docker (e.g. a nightly `gbb verify` next to GitLab). Built by CI from
# the cross-compiled release binary, nothing else: gbb is static and needs no libc,
# no git and no certificates to read a backup.
FROM scratch
COPY dist/gbb-linux-amd64 /gbb
ENTRYPOINT ["/gbb"]
# Set up front so gbb does not re-execute itself to set it (see cmd/gbb/godebug_linux.go),
# and give the default cache dir a home; hosts pass --data anyway.
ENV GODEBUG=madvdontneed=1 HOME=/data
