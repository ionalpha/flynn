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

## Supported versions

Until a 1.0 release, only the latest release and `main` receive security fixes.

## Scope

This repository is the open Flynn. Issues in a connected commercial host belong to
that system, not here.
