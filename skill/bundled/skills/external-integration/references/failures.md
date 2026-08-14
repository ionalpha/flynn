# What the failure looks like, and what it means

The adapter has to turn everything that can go wrong into a small set of reactions.
This is the mechanical half: what each failure looks like on the wire, which class it
belongs in, and what you know about the request afterwards.

## Three questions, in this order

1. **Did the request reach them?** A connection refused and a 500 are both failures,
   and only one of them ran your work.
2. **Can waiting help?** This is the class. It is a property of the failure.
3. **Is repeating safe?** This is a property of the request, and the answer does not
   change because the failure looked transient.

A failure you cannot answer question one about is the interesting case, and it has its
own table below.

## HTTP

| What came back | Class | Usual meaning |
|---|---|---|
| Connection refused, DNS failure, TLS handshake failure | Transient, but suspect configuration | Nothing ran. A failure that is instant and total on every host is usually yours: a wrong name, an expired certificate, a blocked egress. |
| Connection reset or timeout before the response | Unknown outcome | May have run. See the table below before retrying. |
| 400, 422 | Terminal | Your payload. Repeating sends the same payload. |
| 401 | Terminal, needs a person | Credential absent, expired, or wrong. Refresh once if the flow has a refresh step, then stop. |
| 403 | Terminal, needs a person | Authenticated and not permitted. A missing scope does not appear by retrying. |
| 404 | Terminal, with one exception | Read the vendor's consistency note: some services return it briefly after a create. |
| 405, 406, 415 | Terminal | Method, accept header, or content type wrong. A version mismatch often lands here. |
| 409 | Terminal, resolvable | A conflict with current state. Re-read, decide, and send a new request rather than the same one. |
| 413, 414 | Terminal | Too large. Split the work, do not repeat it. |
| 429 | Transient, scheduled | Wait what the header says, capped. The only failure that tells you how long. |
| 500, 502, 503, 504 | Transient | Theirs. 502 and 504 usually come from something in front of them, so the body is often not their error shape. |
| 501 | Terminal | Not implemented in this version. |

## Other transports

| Transport | Where the classification lives |
|---|---|
| gRPC | The status code carries it: `UNAVAILABLE`, `DEADLINE_EXCEEDED` and `RESOURCE_EXHAUSTED` are transient, `INVALID_ARGUMENT`, `PERMISSION_DENIED` and `FAILED_PRECONDITION` are terminal, and `ABORTED` means retry the whole operation rather than the call. |
| A vendor SDK | The exception hierarchy, which usually distinguishes throttling and service errors from client errors. Read it once and map the types; do not match on message text, which they reword without notice. |
| A message queue | Delivery is at least once, so the consumer is the idempotent one. Failure means the message returns, and the counter that matters is the redelivery count that sends it to the dead letter queue. |
| A webhook you receive | Your response is their retry signal. Return success once the event is stored, not once it is processed, and process from your own store. A 500 because your handler was slow buys you the same event again in a minute. |
| A database or cache behind the vendor | Their outage arrives as latency before it arrives as an error. A timeout you set is the only thing that turns it into a failure you can classify. |

## What you know after the failure

The column that decides whether repeating is safe.

| Failure | Did the write land? | What to do |
|---|---|---|
| Connection refused, DNS failure | No | Safe to repeat. |
| Timeout before the request finished sending | Probably not | Safe to repeat under a key. |
| Timeout after sending, no response | Unknown | Repeat under the same key, read back, or fail and say the outcome is unknown. |
| Connection reset while reading the response | Unknown, and it probably succeeded | Same three options. This is the case people assume is a failure. |
| 5xx from the service itself | Unknown | Their documentation is the only authority on whether a 500 means it did not happen. |
| 502 or 504 from a proxy | Unknown | The request may have reached the service and the answer may have been lost on the way back. |
| 429 | No | Safe to repeat after the stated wait. |
| Any 4xx other than 429 | No | Do not repeat the same request. |

## Failures that get classified wrong

- **Matching on message text.** Vendors reword messages in patch releases. Every rule
  that survives is written against a status, a code, or an exception type.
- **A blanket retry around the whole call.** It catches the parse error in your own
  code and sends the request again, three times, at every layer.
- **Treating a timeout as a failure.** It is an unknown, and the difference decides
  whether a customer is charged twice.
- **Treating 429 as an error to log.** It is a schedule. If it appears at all, either
  the concurrency is wrong or the rate limit is worth asking about.
- **Retrying a cancelled context.** The caller has already gone. Spending three
  attempts on their behalf is load with no reader.
- **One class for the whole integration.** The failure that says the payload was wrong
  and the failure that says the service is down do not deserve the same reaction, and
  collapsing them is what produces a retry loop against a permanently bad request.

## What to record when it fails

One line, and it holds the four things a person will want at three in the morning:
the operation, the status or code, the class you assigned, and the attempt number. Add
the content type and first line of a body that would not decode, and the request
identifier the vendor returned, since it is the only thing their support can act on.
Never log the credential, and never log a whole response body by default: it carries
somebody's data and it is the field most likely to be a customer's.
