#!/usr/bin/env python3
"""Fail when a doc comment describes a different declaration than it sits on.

THE DEFECT THIS CATCHES, which has landed four times in one PR series. A new
function is inserted immediately above an existing one, and the blank line
between the existing function's doc comment and its own `func` is not kept:

    // varNameAt reads the variable name of a reference ...
    // startsDelimitedRef reports whether the "$" at i opens ...
    func startsDelimitedRef(s string, i int) bool { ... }

    func varNameAt(...)   // now has no doc comment at all

Both declarations still compile, godoc renders the wrong text for one of them,
and the other loses its documentation. In a codebase where the comments are
the design record, that is worse than a missing comment: it reads as
authoritative and describes something else.

HOW IT DECIDES, and why it is narrow on purpose. A doc comment is flagged ONLY
when its first word names a different top-level declaration IN THE SAME FILE.
That is the signature of the insertion mistake and essentially cannot false
positive. Deliberately NOT flagged:

  * a doc comment that opens with something other than any declaration's name
    (a heading, an article, a sentence) -- Go convention prefers the name, but
    enforcing that is a different and much noisier check, and `revive`'s
    `exported` rule is the tool for it;
  * a comment naming a declaration in another file, which is an ordinary
    cross-reference.

Exit 1 with the file, line and both names on any hit, so the fix is obvious:
insert the missing blank line, or move the comment back onto its owner.
"""
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SKIP_DIRS = {".git", "bin", "vendor", "node_modules", "testdata"}

# PRE-EXISTING, and this list may only ever SHRINK.
#
# Twelve of these were already in the tree when the check was written, in
# files unrelated to the change that added it. Fixing them is a blank line
# each and belongs in its own pass rather than inside a parser PR touching ten
# other packages. They are baselined by (file, misattributed name) and not by
# line number, so the entry survives the file moving around and stops matching
# the moment someone fixes it.
#
# Adding an entry here is not a way to pass the check. A NEW hit means a doc
# comment has just been separated from its declaration; put the blank line
# back.
BASELINE = {
    ("internal/datastore/datastore.go", "Datastore"),
    ("internal/engines/analyzers/external/analyzer.go", "Analyzer"),
    ("internal/engines/analyzers/throughput/analyzer.go", "groupByVariant"),
    ("internal/engines/variantmeta/discovery.go", "Discover"),
    ("internal/engines/variantmeta/discovery.go", "resolveScaleTarget"),
    ("internal/metrics/metrics.go", "SetModelsProcessed"),
    ("internal/signals/floor/floor.go", "median"),
    ("internal/warmpool/policy/policy.go", "roomiestFreePod"),
    ("internal/warmpool/pool/adapter.go", "capacityOf"),
    ("internal/warmpool/pool/shape.go", "parallelism"),
    ("test/e2e/fixtures/warm_pool_builder.go", "CreateWarmPool"),
    ("test/utils/pod_scraping_test_helpers.go", "PodScrapingTestConfig"),
}

# A top-level declaration whose name we can take: func, type, var, const, and
# methods (whose receiver is skipped so the NAME is captured).
DECL = re.compile(
    r"^(?:func\s+(?:\([^)]*\)\s*)?(?P<fn>[A-Za-z_]\w*)"
    r"|type\s+(?P<ty>[A-Za-z_]\w*)"
    r"|var\s+(?P<va>[A-Za-z_]\w*)"
    r"|const\s+(?P<co>[A-Za-z_]\w*))"
)


def decl_name(line):
    m = DECL.match(line)
    if not m:
        return None
    return m.group("fn") or m.group("ty") or m.group("va") or m.group("co")


def go_files():
    for base, dirs, files in os.walk(ROOT):
        dirs[:] = [d for d in dirs if d not in SKIP_DIRS]
        for f in files:
            if f.endswith(".go") and not f.endswith("_test.go"):
                yield os.path.join(base, f)


def check(path):
    with open(path, encoding="utf-8", newline="") as fh:
        lines = fh.read().replace("\r\n", "\n").split("\n")

    # Every top-level declaration name in this file.
    names = set()
    for line in lines:
        n = decl_name(line)
        if n:
            names.add(n)

    hits = []
    for i, line in enumerate(lines):
        owner = decl_name(line)
        if not owner:
            continue
        # Walk back over the contiguous comment block directly above it.
        j = i
        while j > 0 and lines[j - 1].lstrip().startswith("//"):
            j -= 1
        if j == i:
            continue  # no doc comment
        first = lines[j].lstrip()[2:].strip()
        word = first.split()[0].rstrip(":,.") if first.split() else ""
        if word and word != owner and word in names:
            hits.append((j + 1, word, owner))
    return hits


def main():
    bad = 0
    baselined = 0
    seen = set()
    for path in sorted(go_files()):
        rel = os.path.relpath(path, ROOT).replace(os.sep, "/")
        for line, word, owner in check(path):
            seen.add((rel, word))
            if (rel, word) in BASELINE:
                baselined += 1
                continue
            print("%s:%d: doc comment opens with %r but sits on %r" % (rel, line, word, owner))
            print("    both are declared in this file, so this is the "
                  "missing-blank-line mistake: %r has lost its doc comment." % word)
            bad += 1

    # A baseline entry that no longer matches is a fixed one. Say so, so the
    # list is pruned as the debt is paid rather than growing stale.
    stale = sorted(BASELINE - seen)
    if stale:
        print("%d baseline entr%s no longer misattributed -- remove from BASELINE:"
              % (len(stale), "y is" if len(stale) == 1 else "ies are"))
        for rel, word in stale:
            print("    %s: %s" % (rel, word))
        return 1

    if bad:
        print()
        print("%d NEW misattributed doc comment(s). Put the blank line back." % bad)
        return 1
    print("doc comment owners OK (%d baselined, 0 new)" % baselined)
    return 0


if __name__ == "__main__":
    sys.exit(main())
