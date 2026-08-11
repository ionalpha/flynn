# The naming audit

A glossary nothing checks is a file that was true on the day it was written. This is
the mechanical half: a format a script can read, the matching rule that keeps the
check honest, how to turn it on in a repository that already fails it, and the two
mistakes that get one of these deleted within a week.

## A format that is readable both ways

The entry has to be prose for a person and parseable for a script, so fix the shape
and keep it flat. One heading per term, three labelled lines:

```md
## Purchase order

A customer's commitment to buy, priced and accepted, before anything ships.
Instead of: order, PO, purchase request, requisition
Source: the wording on the document the customer signs
```

Three properties make this checkable. The heading is the canonical term. The
`Instead of:` line is the list of displaced spellings, which is what the audit
searches for. The `Source:` line is not machine-checked, and is required anyway,
because an entry nobody can trace is an entry the next author will overwrite.

Anything else in the file is prose the parser skips. Keep the file in the repository
root or beside the code it covers, and keep one per bounded area if a word genuinely
means two things in two places, with each file's scope stated in its first line.

## What the check asserts

Three assertions, in the order of how often they fire:

1. **A displaced spelling appears as an identifier.** The entry says `Purchase order`
   displaces `requisition`, and `requisitionID` is in the source. This is the drift
   the skill exists to catch, and it is the one that costs nothing to detect.
2. **A term is spelled a way the entry does not list.** `purchOrd`, `p_order`,
   `PurchOrderDTO`. Detected by splitting identifiers into words and comparing word
   sequences, not by string equality.
3. **A term appears nowhere in the code.** Usually means the code was renamed and the
   glossary was not, which is the same drift arriving from the other side. Report this
   one as a warning first: a term can legitimately be defined before it is built.

## Matching, which is where these go wrong

Split before you compare. An identifier is a sequence of words wearing a case
convention: `purchaseOrderID`, `purchase_order_id`, `PURCHASE_ORDER_ID` and
`purchase-order-id` are the same three words. Split on case changes, digits and
separators, lowercase the parts, then compare the resulting word lists. Comparing raw
strings means writing the same rule once per convention and missing one.

Match whole words only. A substring search for `order` fires on `reorder`,
`ordering`, `borderline` and `orderly`, and the first false positive on a legitimate
word is what gets the check switched off.

Skip what you do not own. Vendored directories, generated files, lock files, test
fixtures, migrations already applied, and anything under a build output path. A
generated client that speaks a third party's vocabulary is not your drift, and
failing on it teaches everyone to ignore the failure.

Report file, line, column, the identifier, and the term it violates, all of them, in
one run. A check that stops at the first violation costs one build per fix.

## The shape of the script

Roughly forty lines in any language with a regular expression engine. The steps:

```
parse the glossary       -> [(term_words, [displaced_words...]), ...]
walk the source tree     -> skip the ignore list
tokenise each file       -> identifiers with file, line, column
split each identifier    -> lowercase word list
for each identifier:
    if it contains a displaced word sequence -> violation, name the term
    if it near-matches a term without matching it -> violation
exit non-zero if any violation
```

A language-aware tokeniser is better than a regular expression, because it skips
strings and comments, and every ecosystem has one in its standard library or its lint
toolchain. A regular expression over identifier-shaped substrings is an acceptable
first version and will produce noise in comments; if you take that route, say so in
the failure message so the next author knows what they are reading.

Run it in the same command that runs the tests. A check with its own invocation is a
check that runs on the author's machine once.

## Turning it on where the code already fails

Every glossary written after the code will fail on its first run, often in the
hundreds. A red build nobody can fix today is a build people learn to ignore.

Baseline it. Record the current violations in an explicit exception file, keep the
check live for everything else, and add one property: the list may shrink and may
never grow. New code is held to the vocabulary from day one, the old crossings are
counted rather than forgotten, and the number going down is something a reviewer can
verify. A test asserting the exception file's length does that in three lines.

Add terms one at a time and in the order they cost you. The word two subsystems
already disagree about is worth more than the twenty nobody confuses.

## When not to have one

Skip the audit when the glossary has fewer than about five terms: read the diff
instead, the tooling costs more than it returns. Skip it in a repository whose code is
mostly generated from a schema you do not control, where the right place for the rule
is the schema. And do not extend it into a general naming linter that enforces suffixes
or forbids abbreviations wholesale. It has exactly one job, which is agreement between
the code and the words the project has already agreed on, and every rule added beyond
that is a rule someone will argue with while the real drift walks past.
