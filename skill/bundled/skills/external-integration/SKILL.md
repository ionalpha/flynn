---
name: external-integration
description: Use when your code depends on a system you do not run: a remote API you did not write, a vendor SDK, a payment, mail, storage or model provider, or a webhook one of them sends you. Covers reading their current documentation for the version you are actually on instead of writing the call from memory, proving it with one real response before building on it, translating their payloads into your own types so their next release is not your rewrite, turning a failure that comes back into a reaction your code can act on, deciding whether repeating a request is safe, how often and in which layer to retry, what your callers get while they are unavailable, and which hosts and credentials the integration may use.
metadata:
  flynnhq.com/title: External integration
  flynnhq.com/tags: '["integration","http","reliability","retries","errors"]'
---

# External integration

## Read the current document, then make one real call

An integration written from memory is a set of guesses about someone else's system,
and the guesses are wrong at a rate somebody has measured. Over eight Python libraries
and eleven models, code generated for an API that changed after the models were
trained ran in the target environment 43% of the time. With the current documentation
page in the prompt, that rose to 66%.

Read both halves of that. Fetching the page is the largest single improvement
available, and it still leaves a third of the code broken. Among the calls that stayed
wrong with the page in context, one in six was the call that page marked deprecated.

So a citation is not a verification, and the finishing move is a response. Before the
client, the types, or the tests exist, send one request to the real service or its
sandbox and keep what came back: the status, the headers you are going to depend on,
the body. A curl, a scratch script, one test against the sandbox. Everything written
after that is written against something observed; everything written before it is a
hypothesis with a URL attached.

## The version is part of the API, and memory averages the versions

Find the version twice, because there are two of them and they move separately. The
SDK version is in the dependency file and the lockfile. The API version is whatever
the vendor pins it with: a header, a segment in the path, a date, or a default sitting
on the account. Set it explicitly in the code. A version that lives on the account is
one someone else can change on a Tuesday without touching your repository.

Then fetch the page for that version, not the one search returns, and read the
deprecation notes twice. When the vendor publishes their own instructions for agents
alongside their documentation, fetch those first: they are maintained by the people
who make the changes.

## Their shapes stop at one file

The response arrives as their shape and gets translated into yours at the adapter,
which is the one file allowed to mention their field names. What matters is how the
translation behaves when their shape moves, and it will, because your integration
sits on the far side of a release you do not schedule.

- Ignore fields you do not know. A parser that rejects an unrecognised field turns
  their additive change, the safest one they can make, into your outage.
- Treat every enumerated value as open. They will add a status, and the day they do,
  the code either maps it to a named unrecognised case that a human can see or it
  panics on a string.
- Keep units and precision as documented. Money in minor units stays in minor units,
  timestamps keep their offset, identifiers stay strings even when they look numeric.
- A field documented as optional will be missing eventually. Decide now what your
  type does about it rather than finding out through a nil.

Translate the parts your own logic reads, and let the rest go unread. Handing their
object to code that makes a decision is how their next release becomes your rewrite.

## Every failure gets a class, and the default is do not retry

The adapter is the only place that saw everything: the transport error, the status,
the headers, the body. Classify there, and hand the rest of the program a reaction
rather than an error to interpret. Anything deeper that tries to work out whether a
failure was transient is doing it from less information than the code that received it.

The classes are reactions, not causes. Worth waiting and trying again. Will fail the
same way at three in the morning. Needs a person. Was cancelled by us. In this runtime
they are `fault.Class` values, and an error that reaches `Classify` without one is
treated as terminal, which is the default worth copying anywhere: retry is opted into
per failure and never inherited. An unclassified error that gets retried is a bug that
only shows up as load.

Two failures get miscategorised more than any others. A rate limit is transient and
carries a schedule, so it is the one failure that tells you exactly how long to wait.
An authorisation failure is terminal and permanent until a human acts: a token missing
a scope will not gain one by being asked again, and retrying it just spends the
account's quota on a guaranteed refusal.

`references/failures.md` catalogues what each failure looks like on the wire, per
transport, and which class it maps to.

## What you parsed may not have come from them

Between your code and their service sit load balancers, proxies, gateways, and
whatever your own platform puts in front of you. Each answers in its own shape. An
HTML error page decoded as their JSON becomes "unknown error" in your logs, and the
outage reads as a bug in your parser.

Branch on the transport status first and treat the body as detail. The problem details
format used by many APIs says as much about itself: generic HTTP software knows
nothing of a status carried inside a body, so the two can disagree, and the transport
status is the one the network acted on. Where an API instead reports failure inside a
200, that is part of its contract and belongs in the adapter with everything else.

When a body will not decode, log its content type and its first line before discarding
it. That one line is what tells the next person the response was an HTML challenge
page from a firewall.

## Retry safety is a property of the request, not of the failure

The failure tells you whether waiting could help. It says nothing about whether
repeating is safe, and those are separate questions that get answered as one.

A read is safe. A write is safe only when the service gives you something to make it
safe: an idempotency key, a conditional update against a version or an entity tag, or
a natural key it deduplicates on. Where keys are offered, generate one when the work
is decided rather than when the request is sent, so every attempt carries the same key,
and change it when the parameters change: services typically replay a stored response
for a repeated key and reject the same key sent with different parameters. Retention is
finite, often around a day, so a key is not a permanent record of what you did.

The case worth designing for is the one with no answer. A request that timed out with
no response leaves you not knowing whether it happened. There are three honest moves:
repeat it under the same idempotency key, read back to see whether it landed, or fail
the operation and report the uncertainty. Guessing is not one of them.

A retry also needs a body you can send again. A consumed stream, a one-shot reader, or
a signature computed over a nonce is a request that can be sent once, and the code
should refuse to replay it rather than sending something subtly different.

## Retry in one layer, on a budget

Retries are the mechanism by which a service having a bad minute has a bad hour, so
they get limits, and the limits are the ones operators converged on.

- Three attempts for a request, then let the failure out. A fourth attempt rarely
  succeeds and always adds load.
- One layer retries: the one immediately above the failure. If the client, the service
  calling it, and the gateway above that all retry three times, one user request became
  twenty-seven.
- Cap retries as a share of traffic, not just per request. A ceiling near one retry per
  ten requests keeps a widespread failure from turning every client into a load
  generator.
- Wait with full jitter, sleeping a random duration up to the backoff rather than the
  backoff itself. Measured against plain exponential backoff, it cut both total client
  work and time to completion, because unjittered clients retry in step.
- Honour a rate limit's stated delay, and cap how long it may hold you. A server, or
  something impersonating one, can otherwise park your run for a day.
- Retries live inside the caller's deadline. A budget that outlives the request it is
  serving is spending someone else's patience.

## Say what happens while it is down

Every integration behaves somehow when the service is unavailable, whether or not
anyone chose the behaviour. Choose it, and write it where the adapter is defined:
fail the operation with a classified error, serve a stored copy along with its age,
queue the work with a stated way of draining it, or drop to a lesser feature and say
so. Each is defensible. Discovering which one you shipped during an incident is not.

Set two timeouts, one to connect and one for the whole call, and make both shorter
than the deadline of whatever is waiting on you. A call with no deadline is a worker
holding a slot until the outage ends.

## Name what it can reach and what it may carry

The set of hosts an integration talks to is knowable, small, and almost never written
down. Write it down and let something enforce it, because the enforcement answers a
question that is otherwise a guess: what can this integration actually reach.

Two things follow from having that list. A URL that arrives in someone else's payload,
or in a webhook, cannot become a request to an address of their choosing, which is the
whole of server-side request forgery. The check belongs on the address the connection
resolved to rather than on the name, or a name that resolves to an internal address
walks straight through it. This runtime's outbound gate is default-deny and checks at
connect for that reason, and the shared transport is built on it, so an integration
starts with public addresses only and the private, loopback and cloud metadata ranges
refused.

The credential rides on the same list. Attach it only for hosts you named, so a
redirect or an attacker-supplied URL cannot carry your token somewhere else, and give
the integration the smallest role that does its job. Resolve it by name at the moment
of the call, so the secret itself never sits in a spec, a log, or a prompt.

## The vendor's specifics are the vendor's to maintain

Their error codes, their pagination, their auth dance, and their retry semantics change
on their schedule. Write those into your repository and you have adopted their
changelog for as long as the file exists, which is how integration guidance rots into
confident instructions for a version nobody runs.

The division is clean. Theirs: what the endpoint is called, what it returns, what the
codes mean, which parameters exist. Fetch it, or install the instructions they publish
for agents, and cite the page with its version. Yours: which operations you use, what
you translate them into, which class each failure maps to, whether repeating is safe,
and what happens while they are down. That part is worth writing down carefully,
because no vendor will ever write it for you.

This skill follows its own rule and names no provider. A section here about a
particular API would be out of date on their next release and would be teaching a
changelog rather than a craft.

## Leave the evidence beside the adapter

The next person to touch this call has the same problem you started with, and can be
saved most of it by three lines in the adapter's doc comment: the documentation URL
with its version, the date it was read, and what a real response looked like.

Keep that response as a dated fixture the tests read. It is what lets the next failure
be diagnosed in one step, by telling apart the two explanations that look identical
from the logs: their shape changed, or we broke it. Run the calls that touch the real
sandbox on a schedule rather than on every commit, so a vendor's change is discovered
by a red build on a quiet morning rather than by a customer.

## Refusals

- No call written from memory. The current page for the version you are on is fetched,
  or the code is marked unverified where it sits.
- No client built out before one real response has been received and recorded.
- No vendor type passed to code that decides something, and no parser that fails on a
  field it does not recognise.
- No error crossing the adapter unclassified, and nothing retried because an error was
  unrecognised.
- No repeat of a write without an idempotency key, a conditional update, or a stated
  reason it is safe.
- No retry loop without an attempt ceiling, jitter, and a deadline it lives inside, and
  none in a layer that already has one below it.
- No integration shipped without a stated behaviour for the service being down, and no
  outbound call to a host nobody listed.
- No section, file, or comment in your repository that restates a vendor's API where
  their own documentation would have been fetched.
