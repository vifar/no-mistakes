---
title: Fork self-update releases
description: Release channel publication and source-build version ordering.
---

The [update command](/no-mistakes/reference/cli/#no-mistakes-update) follows this
fork's release channels. This fork currently has no channel manifest or release
and channel-publishing workflows. Self-update requires maintainers to first
provide a publisher and a `channels` release containing `channels.json`, backed
by published binary archives and `checksums.txt`. Until those prerequisites
exist, the updater fails closed rather than using another repository's releases.
The stable channel requires a published stable release. `update --beta` also
considers published prereleases.

The channel's release version must be newer than the installed binary's version
for an update to occur. `make build` uses the version of the `.` package in
`.release-please-manifest.json`. The canonical baseline has no build metadata.
At the root of a Git checkout, the build appends `+g<shortSHA>` and `.dirty`
when tracked files have changes, and records the source commit separately.
This metadata does not change version ordering. The build ignores imported
upstream tags. Release or development builds can set `VERSION` explicitly;
without that override, an unreadable or invalid manifest stops the build.
Keep published fork channels consistent with this version baseline;
a fork release below an installed build's version will not replace that build.
