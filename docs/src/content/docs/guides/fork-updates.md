---
title: Fork self-update releases
description: Release channel publication and source-build version ordering.
---

The [update command](/no-mistakes/reference/cli/#no-mistakes-update) follows this
fork's release channels. The fork must publish its own `channels` release with
the `channels.json` asset. `.github/workflows/publish-channels.yml` builds that
manifest from releases in the current GitHub repository. The release workflow
calls it after publishing all binary archives and `checksums.txt`; maintainers
can also run the publisher manually after publishing or promoting a release.
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
