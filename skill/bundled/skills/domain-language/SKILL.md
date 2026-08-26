---
name: domain-language
description: Use when deciding what to call something, when the same concept is going by several words, or when working out what the things in this domain are and what each term means. Covers finding the word the project already uses before inventing another one, where a term comes from when no domain expert is available to ask, splitting a word that is doing two jobs, why an implementation word names nothing about this business, why a name that no longer describes the behaviour is a defect and has to be renamed, where the glossary lives so it stays true, and an audit that fails the build when the code and the agreed vocabulary drift apart.
metadata:
  flynnhq.com/title: Domain language
  flynnhq.com/tags: '["naming","domain-model","glossary","vocabulary","readability"]'
---

# Domain language

## Names are an input, not decoration

Code says what it does twice: in its structure, which is binding, and in its
identifiers, which are not. A reader who has both uses both, and the measurements say
the second channel carries more than anyone designing a glossary assumes.

An October 2025 study took working code and rewrote only the identifiers, leaving
behaviour untouched, in four ways: renamed to `var1`, renamed to visually
indistinguishable tokens, renamed into the vocabulary of an unrelated field, and
renamed to imply the wrong behaviour. Asked to describe what a class does, models fell
from 87.3% to 58.7% (GPT-4o) and from 86.2% to 66.4% (Llama 4 Maverick). The result
that matters more is on predicting what the code outputs, a question structure alone
answers: pass@1 fell from 82.9% to 68.7%, and for one model from 80.2% to 56.4%, on
code whose behaviour had not changed at all.

Two things follow. Whoever maintains this next, person or model, is reading your names
as evidence about behaviour, and structure does not rescue them from a name that
misleads. And the same measurement puts a price on a wrong name: names carrying the
wrong meaning cost about as much as having no names at all.

So treat the vocabulary as part of the program's interface to its future readers,
which means it is designed, written down, and checked, rather than settled per commit
by whoever is typing.

## You will not guess the word this project already uses

Ask 334 developers to name the same thing and the median chance that two of them
choose the same name is 6.9%. That is the finding of a 2021 study across 47 naming
instances, and its companion result is the useful one: a name somebody else chose is
usually understood by the majority, even though almost nobody would have chosen it.

Read those together and the value of a name is agreement, not quality. Agreement does
not arise by coincidence, and it is not a thing you can reason your way to from the
concept. It comes from looking.

This is the mechanism behind a codebase where one thing has three names. Nobody
decided to add a synonym. Each name was chosen well, separately, by someone who did
not look first.

A run is worse placed than a person here, and it is measured: given the full context a
generation needs, models call between 63% and 69% of the dependencies they were
handed, and the authors record that they sometimes reimplement what was already in
front of them. Having the vocabulary in the context window is not the same as using
it.

So the first move on any new noun is a search, not a choice:

- Search the identifiers for the concept under at least three plausible words,
  including the one you would have picked and the two nearest synonyms.
- Search the schema and the stored records. A column name outlives every refactor
  above it.
- Search what the product shows a customer, and the strings it sends them.
- Search the tests. A test name often carries the domain word that the code under it
  has lost.

If a word already exists, use it. Preferring your own is how the third name arrives.
If nothing exists, say so explicitly before naming, because that sentence is the
evidence that the search happened.

## Where the word comes from when nobody is in the room

The standard advice is to get the term from a domain expert. Take that when it is
available. It usually is not: the run is at 2am, the expert is a calendar invite three
days out, and the code needs a name now.

Rank the sources by how expensive they are to contradict, and take the highest one
present:

1. What the customer is shown or signs. The invoice, the contract, the statutory or
   regulatory term, the label on the button, the subject line of the mail the system
   sends. These are already the words the business is held to.
2. The durable record. Column names, event types in the stored history, fields other
   systems already read. Wrong or not, they are what the data means, and renaming them
   is a migration.
3. The team's own speech, in tickets, review comments, and support threads, where the
   same concept is being discussed by people who are not writing the code.
4. Your own invention. Last, and marked as such, so the next reader knows it was never
   confirmed by anybody.

Write where the term came from next to its definition. A term with a source can be
checked by someone who knows the business; a term without one is a preference, and the
next person to have a preference will overwrite it.

When the customer-facing word and the stored-record word disagree, resist picking a
winner. Two authorities using different words is usually the signal that there are two
concepts, which is the next section.

## One word, one meaning: the split test

Write the definition in one sentence. If it needs "or", "depending on", "usually", or
a parenthesis to be true, the term is doing two jobs and the ambiguity is now in every
piece of code that uses it.

What it looks like in practice:

| Symptom | The two jobs |
|---|---|
| A status field where some values are stages of a lifecycle and one is a failure | Where it got to, and what went wrong |
| A boolean whose meaning depends on which caller set it | Two questions sharing a field |
| A column that is null for two unrelated reasons | Not applicable, and not yet known |
| A word meaning one thing in billing and another in fulfilment | Two concepts that happen to share a spelling |
| A type carrying fields half the callers must ignore | The thing, and the thing plus its context |

The last row of that table is not always a fault. The same word legitimately means
different things in different areas, and forcing one meaning across a whole system
produces a type that satisfies nobody. What is never acceptable is the collision being
undeclared. Say in the glossary that the word is scoped, and to what.

Splitting is three steps in one change: name both halves, define both, and move the
code onto the new words. A split recorded in the glossary but not carried into the
identifiers has made the drift worse rather than better. Which package may then reach
which is a separate question, and `structural-boundaries` owns it.

## A name that lies is a defect

A wrong name is an instruction to the next reader, and the measurements above say it
will be followed. That puts it in the same class as a wrong condition, and out of the
class of things worth doing when there is time.

The ones that recur:

- A reader that writes. `get`, `find`, `check` and `is` all promise no side effects.
- A validator that mutates, normalises, or persists on the way through.
- A duration, size or amount whose units live only in the author's head.
- A name from before the last rename, still describing a concept the business dropped.
- A flag whose name states the positive while the code branches on the negative.
- A name that describes what an early caller wanted rather than what the thing does.

The rule is that the rename ships in the change that made the old name wrong. Deferred,
it never happens: the next person to touch the code reads the name, believes it, and
writes around it.

One exception, and it is a hard one. When the name is on a published surface (an
exported symbol, an endpoint, a payload field, a stored record) the rename is a
compatibility decision, not an editorial one. Add the new name, have the old one
delegate to it, deprecate with a date, and let `contract-design` own the rest. Renaming
a field others read is breaking whatever the old name deserved.

## Implementation words say nothing about this system

`Manager`, `Handler`, `Processor`, `Helper`, `Util`, `Data`, `Info`, `Item`, and the
class named after the verb it happens to perform: each of these names the shape of the
code rather than the thing the code is about.

Two tests, and both are quick:

**The transplant test.** Paste the name into an unrelated product. If it fits there
unchanged, it says nothing about yours. `SubscriptionRenewal` fails the transplant.
`RequestProcessor` fits anywhere, which is the problem.

**The recognition test.** Would someone who does this job for a living read the name
and agree it is what they call it? If they would have to be taught the word first, it
came from the code, not the domain.

Two further constraints on the string itself:

Length is bought, not saved. Abbreviate only what the domain itself abbreviates, and
put the abbreviation in the glossary when you do. A shortened word is a second name for
the same concept, which is the failure this whole skill is about.

Two identifiers in one scope must differ by more than a character or a case. That is
the arm of the study above that did the most damage, taking one model from 80.2% to
56.4%, and it is the one people dismiss as pedantry.

## Put the glossary where it stays true

Two surfaces, holding different things.

**The terms go in the repository**, in one file, in the language of the business. They
have to travel with the code, be readable by anyone who checks it out, and be visible
to a run that has no access to this workspace at all. That is worth more than being
maintained, because a term nobody can reach is not a term.

**The reasoning goes in a durable note**: what the alternatives were, who or what
confirmed the word, what was rejected and why. Notes are superseded when they change
and diffs are not, and none of this belongs in a file the audit has to parse.

One entry is four things: the term, one or two sentences on what it *is* (not what it
does), the words it displaces, and where the word came from. The list of displaced
words looks like editorial opinion and is not: it is the input the audit greps for, and
a term without one is unenforceable.

What does not belong: types, table names, class names, or any general programming
concept. A glossary that defines "retry" and "cache" has become documentation of the
code, and it will be wrong within a month.

## The audit is the only part of this that fails loudly

Everything above is advice, and advice is skipped under time pressure without leaving a
trace. The drift is silent by construction: nothing errors when a second name appears,
no test goes red, and the cost arrives months later as two subsystems that disagree
about what a word means. So make it fail. The check reads the glossary, and exits
non-zero when the code contradicts it:

- A displaced word appears as an identifier. The glossary said `Customer` displaces
  `Client`, and `ClientRecord` is in the source.
- A term is spelled a way the entry does not list, across case conventions, so
  `purchase_order`, `PurchaseOrder` and `purchaseOrder` count as one and `purchOrd`
  does not.
- A term in the glossary appears nowhere in the code, which usually means the code was
  renamed and the glossary was not.

It reports file, line, and the term it violates, and it runs in the build beside the
tests. `references/audit.md` has the shape, including the two things that decide
whether anyone keeps it: an ignore list for vendored, generated and third-party code,
and word-boundary matching, without which the first false positive gets the whole check
switched off.

Start with one term. The one the project already argues about is worth more than a
complete glossary nobody enforces.

## What to say when you hand it over

The terms you added or changed, each with where the word came from. "Taken from the
column name" and "invented, unconfirmed" are different claims and the reviewer needs to
know which one they are reading.

Which words you searched before naming something new, and that the search came back
empty. Three words tried and none found is the evidence that a new name was necessary.

Any rename, with its reach: private to this package, repository-wide with the callers
updated in the same change, or published, in which case say which compatibility path
you took.

What the audit said. A clean run is a fact. A run that was not attempted is also a
fact, and the reviewer needs it stated rather than absent.

## Refusals

- No new name for a concept this codebase already names, however much better yours is.
- No name chosen without searching for an existing one first, and no claim that none
  exists without saying which words were tried.
- No term in the glossary without where the word came from.
- No definition that needs "or" to be true.
- No name left in place once the behaviour it describes has changed.
- No rename of a published name as an editorial change.
- No general programming concept in a domain glossary.
- No abbreviation the domain does not itself use.
- No claim that the vocabulary is consistent without the audit's output to show for it.
