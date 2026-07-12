# Security Policy

## Reporting a vulnerability

Please report security issues **privately**. Do not open a public issue or pull request.

- Use GitHub's [private vulnerability reporting](https://docs.github.com/code-security/security-advisories/guidance-on-reporting-and-writing/privately-reporting-a-security-vulnerability)
  ("Report a vulnerability" under the Security tab), or
- email **contact@ionalpha.io**.

Include a description, reproduction steps, affected version, and impact. We aim to
acknowledge within a few business days and will coordinate a fix and disclosure timeline
with you.

A machine-readable contact is published at [`.well-known/security.txt`](.well-known/security.txt).

## Threat model

The published [threat model](docs/THREAT_MODEL.md) describes the trust boundaries, the
classes of attack Flynn defends against, and which defense is responsible for each, with a
clear split between what is enforced today and what is planned.

## How we reach you about a vulnerability

A binary that cannot be told anything after it is installed cannot be warned. Flynn
therefore fetches a signed notice feed, which carries security advisories about Flynn
itself along with deprecations and release notices. `flynn notices` shows what it has
received; a security advisory that applies to the version you are running is printed at
startup until you move to a version that fixes it.

Here is exactly what that costs you in privacy, stated in full so you can check it against
the code in `notices/`:

- One HTTPS GET, at most once a day, for a single fixed URL. No query string, no cookies,
  no authorization header, no install id, no machine id, no telemetry of any kind. The
  user agent is the word `flynn`, deliberately without a version.
- The document that comes back is a static file, byte-identical for every user. Because
  the request carries nothing that distinguishes you and the response is the same for
  everyone, the origin cannot serve you a different answer from anyone else, whether it
  wanted to or was made to.
- Nothing in the feed can change Flynn's behaviour. It is text, it is stripped of anything
  a terminal would act on, and it can never enable a feature, open a URL, or run a
  command. It is not a feature-flag or remote-configuration channel and will not become
  one.
- The feed is a COSE_Sign1 document signed with an Ed25519 key that is compiled into the
  binary, and it carries a monotonic version and an expiry. A forged feed is refused, an
  old feed replayed over a newer one is refused, and a feed that has stopped being
  refreshed is reported as stale rather than trusted as silence.

The one thing the request does reveal is your IP address, which is true of any network
fetch. Set `FLYNN_NO_NOTICES=1` and none of it happens: no request, no notices. That
switch does exactly this one thing and nothing else.

## Supported versions

Until a 1.0 release, only the latest release and `main` receive security fixes.

## Scope

This repository is the open Flynn. Issues in a connected commercial host belong to
that system, not here.
