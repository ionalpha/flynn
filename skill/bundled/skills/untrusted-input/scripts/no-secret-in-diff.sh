#!/bin/sh
# Refuse a change that adds a credential literal.
#
# This is the mechanical half of the skill's check and it is deliberately the easy
# half: it reads added lines only, and it looks for the one pattern that is almost
# never anything else, which is a credential-shaped name assigned a long literal.
# The other half of the check (every new external input crosses a conversion that can
# fail) is a judgement a reader makes, not a grep.
#
# It reports and exits 1 on a hit. A hit is a credential to rotate, not a line to
# delete: once the value has been committed anywhere it has to be assumed published.
#
# Scope, stated so nobody reads more into a pass than it means. Base64 blobs,
# credentials split across lines, values loaded from a file the change also adds, and
# anything not named like a secret all pass this and are still secrets. A green run
# says the obvious case is absent.

set -u

# Default to the staged change; a caller may pass any git diff arguments instead.
if [ "$#" -gt 0 ]; then
	diff=$(git diff "$@")
else
	diff=$(git diff --cached)
fi

added=$(printf '%s\n' "$diff" | grep '^+' | grep -v '^+++')

# A credential-shaped name, an assignment, then a quoted literal of some length.
assigned='(secret|passwd|password|token|api[_-]?key|apikey|access[_-]?key|client[_-]?secret|private[_-]?key|auth)[a-z0-9_]*[[:space:]]*[:=][[:space:]]*.?["'"'"'`][^"'"'"'`]{8,}'
# A private key block, which is a credential whatever it is called.
block='BEGIN [A-Z ]*PRIVATE KEY'
# The same name assigned a bare literal, as a config file writes it. Filtered below
# for anything that looks like an expression, since a value read from a function call
# or an interpolation is the correct pattern rather than the one being refused.
bare='(secret|passwd|password|token|api[_-]?key|apikey|access[_-]?key|client[_-]?secret|private[_-]?key)[a-z0-9_]*[[:space:]]*[:=][[:space:]]*[A-Za-z0-9+/_.=-]{12,}[[:space:]]*$'

hits=$(printf '%s\n' "$added" | grep -nEi "$assigned|$block" || true)
hits=$(printf '%s\n%s\n' "$hits" "$(printf '%s\n' "$added" | grep -nEi "$bare" | grep -vE '[(${<]' || true)" | grep -v '^$' | sort -u -t: -k1,1n || true)

if [ -n "$hits" ]; then
	echo "a credential literal is present in the added lines:"
	printf '%s\n' "$hits"
	echo
	echo "read it from the credential source instead, and rotate the value: it is published."
	exit 1
fi

exit 0
