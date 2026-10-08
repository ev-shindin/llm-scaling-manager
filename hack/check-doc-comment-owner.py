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

HOW IT DECIDES, and why it is narrow on purpose. A doc comment is flagged only
when its first word names a different top-level declaration in the same file
AND that declaration appears LATER in the file.

The "later" half is load-bearing, and it was missing from the first version.
An insertion pushes the comment's real owner DOWN, so the owner is always
below the declaration the comment has been stranded on. Without that
condition the check also flags a SECTION BANNER whose first word happens to be
a type name -- `// Datastore operations` above a method, with `type Datastore`
declared earlier in the file. That was a real false positive in this
repository, and it was in the baseline until an audit of all twelve entries
found it.

Deliberately NOT flagged:

  * a doc comment that opens with something other than any declaration's name
    (a heading, an article, a sentence) -- Go convention prefers the name, but
    enforcing that is a different and much noisier check, and `revive`'s
    `exported` rule is the tool for it;
  * a comment naming a declaration in another file, which is an ordinary
    cross-reference;
  * a comment naming a declaration EARLIER in the same file, which is a
    backward reference or a section banner, not a stranded doc comment.

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
# Eleven of these were already in the tree when the check was written, in
# files unrelated to the change that added it. A twelfth entry was here and
# was a FALSE POSITIVE -- `// Datastore operations`, a section banner above a
# method, with `type Datastore` declared earlier in the file. Auditing all
# twelve rather than the two originally sampled is what found it, and the
# "declared later" condition in check() is what stops it being flagged at all. Fixing them is a blank line
# each and belongs in its own pass rather than inside a parser PR touching ten
# other packages. They are baselined by (file, misattributed name, the
# declaration it is stranded on) and not by line number, so an entry survives
# edits ABOVE it and stops matching the moment someone fixes it.
#
# It does NOT survive the file being renamed or moved -- the path is part of
# the key, so a `git mv` makes the entry stale and the hit new. An earlier
# version of this comment claimed otherwise, which a reviewer disproved with a
# `git mv`. Renaming one of these files means updating its entry in the same
# commit; three of them sit in packages a planned split will touch.
#
# Adding an entry here is not a way to pass the check. A NEW hit means a doc
# comment has just been separated from its declaration; put the blank line
# back.
BASELINE = {
    ("internal/engines/analyzers/external/analyzer.go", "Analyzer", "SeriesGate"),
    ("internal/engines/analyzers/throughput/analyzer.go", "groupByVariant", "ownReplicasOnly"),
    ("internal/engines/variantmeta/discovery.go", "Discover", "ObserveAccelerators"),
    ("internal/engines/variantmeta/discovery.go", "resolveScaleTarget", "resolveAccelerator"),
    ("internal/metrics/metrics.go", "SetModelsProcessed", "SetUnattributedGPUs"),
    ("internal/signals/floor/floor.go", "median", "priceable"),
    ("internal/warmpool/policy/policy.go", "roomiestFreePod", "roomiestFreePodWithout"),
    ("internal/warmpool/pool/adapter.go", "capacityOf", "groupSizeOf"),
    ("internal/warmpool/pool/shape.go", "parallelism", "nodeCount"),
    ("test/e2e/fixtures/warm_pool_builder.go", "CreateWarmPool", "poolGPUResource"),
    ("test/utils/pod_scraping_test_helpers.go", "PodScrapingTestConfig", "controllerNS"),
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

    # Every top-level declaration name in this file, with the line it is
    # declared on. The LINE is what separates a stranded doc comment from a
    # backward reference: a stranded owner is always below the declaration its
    # comment got attached to.
    declared_at = {}
    for idx, line in enumerate(lines):
        n = decl_name(line)
        if n and n not in declared_at:
            declared_at[n] = idx

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
        if not word or word == owner:
            continue
        where = declared_at.get(word)
        # Declared later than the declaration this comment sits on: the owner
        # was pushed down, which is the defect. Declared earlier (or not at
        # all): a reference or a banner, not a stranded comment.
        if where is None or where <= i:
            continue
        # AND the block must contain the stranded declaration's OWN doc line.
        # The defect is two doc comments merged, so the block opens with the
        # pushed-down owner's name and goes on to open a line with the name of
        # the declaration it is now sitting on. Without this, a prose comment
        # whose first word happens to match some later declaration -- "// Record
        # the start time." above a function, with `type Record` below -- is
        # flagged for a convention miss rather than a misattribution. A test
        # case caught that.
        if not any(
            (ln.lstrip()[2:].strip().split() or [""])[0].rstrip(":,.") == owner
            for ln in lines[j + 1:i]
        ):
            continue
        hits.append((j + 1, word, owner))
    return hits


def main():
    new = []
    baselined = 0
    seen = set()
    for path in sorted(go_files()):
        rel = os.path.relpath(path, ROOT).replace(os.sep, "/")
        for line, word, owner in check(path):
            # KEYED ON THE TRIPLE. With (file, word) alone, a NEW
            # misattribution in an already-baselined file that happened to
            # strand the same name matched the entry and passed -- measured,
            # 12 -> 13 "baselined" and exit 0. The declaration the comment got
            # stranded ON is what separates the old hit from a new one.
            key = (rel, word, owner)
            seen.add(key)
            if key in BASELINE:
                baselined += 1
                continue
            print("%s:%d: doc comment opens with %r but sits on %r" % (rel, line, word, owner))
            print("    both are declared in this file, so this is the "
                  "missing-blank-line mistake: %r has lost its doc comment." % word)
            new.append(key)

    # A baseline entry that no longer matches is a fixed one. Say so, so the
    # list is pruned as the debt is paid rather than growing stale.
    #
    # Reported AFTER the new hits, not instead of them: an earlier version
    # returned here first, so a run with both printed the per-hit lines and
    # then only the stale message.
    stale = sorted(BASELINE - seen)
    if new:
        print()
        print("%d NEW misattributed doc comment(s). Put the blank line back." % len(new))
    if stale:
        print()
        print("%d baseline entr%s no longer misattributed -- remove from BASELINE:"
              % (len(stale), "y is" if len(stale) == 1 else "ies are"))
        for rel, word, owner in stale:
            print("    %s: %s (was stranded on %s)" % (rel, word, owner))
    if new or stale:
        return 1
    # Both figures measured, not typed: a literal "0 new" here would be the
    # hardcoded-status-message mistake this repo has been bitten by.
    print("doc comment owners OK (%d baselined, %d new)" % (baselined, len(new)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
