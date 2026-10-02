# Release notes

One file per release, named for its tag: `v0.2.0.md`. The release workflow puts the
file above the changelog goreleaser generates from commit subjects, and above the
API compatibility report, so a reader meets what changed for them before the list of
commits that changed it.

A tag with no notes file fails the release before anything is built or published. A
release candidate uses the notes of the version it is a candidate for, so
`v0.2.0-rc.1` reads `v0.2.0.md`.

Write the file in the pull request that prepares the release, from the merged pull
requests since the last tag. Group by what a user or an importer can now do, name the
command or the flag, and state every break to the stable API under **For hosts** with
what an implementer has to change.
