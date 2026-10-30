# Release notes

One file per release, named for its tag: `v0.2.0.md`. The release workflow puts the
file above the changelog goreleaser generates from commit subjects, and above the
API compatibility report, so a reader meets what changed for them before the list of
commits that changed it.

A tag with no notes file fails the release before anything is built or published. A
release candidate uses the notes of the version it is a candidate for, so
`v0.2.0-rc.1` reads `v0.2.0.md`.

The plain install line and `flynn upgrade` skip candidates, so a tester who is sent
only the README's first install line gets the previous final release without a word
of warning. The notes for a version that has a candidate therefore carry a **Testing
a release candidate** section with the install command for each OS, and the pull
request that cuts the final release deletes it.

## Cutting a candidate

1. Merge the notes file, including the **Testing a release candidate** section.
2. Tag `vX.Y.Z-rc.N`. The workflow marks it a pre-release, so it does not move
   "latest".
3. Send testers the candidate command, never the plain install line:
   `curl -fsSL https://raw.githubusercontent.com/ionalpha/flynn/main/install.sh | FLYNN_PRERELEASE=1 sh`,
   or on Windows
   `$env:FLYNN_PRERELEASE = '1'; irm https://raw.githubusercontent.com/ionalpha/flynn/main/install.ps1 | iex`.
   For a tester who already has Flynn, it is `flynn upgrade --pre`. Ask them to
   reply with the output of `flynn version`, which shows whether they got the candidate.
4. Before the final tag, remove the tester section from the notes file.

Write the file in the pull request that prepares the release, from the merged pull
requests since the last tag. Group by what a user or an importer can now do, name the
command or the flag, and state every break to the stable API under **For hosts** with
what an implementer has to change.
