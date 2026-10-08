#!/usr/bin/env python3
"""Tests for check-doc-comment-owner.py.

The checker gates `make test` for the whole repository, so a false positive
breaks CI for every future PR and a false negative silently stops catching the
defect it exists for. It had no test; this is that test.

Each case is a synthetic Go file written to a temp dir, so the fixtures are
readable in one screen and none of them depends on the real tree.

Run: python3 hack/check_doc_comment_owner_test.py
"""
import os
import sys
import tempfile
import unittest

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
checker = __import__("check-doc-comment-owner")

HEADER = "package fixture\n\n"


def hits(src):
    """Run the checker's per-file logic over one synthetic file."""
    with tempfile.TemporaryDirectory() as d:
        p = os.path.join(d, "x.go")
        with open(p, "w", encoding="utf-8", newline="") as fh:
            fh.write(HEADER + src)
        return checker.check(p)


class TestFlagsTheDefect(unittest.TestCase):
    def test_function_inserted_under_an_existing_doc_comment(self):
        # THE DEFECT. inserted's doc is the second block; owner's doc has been
        # stranded on it, and owner is declared below.
        got = hits(
            "// owner does the thing that this file is about.\n"
            "// inserted was added later and took the comment above.\n"
            "func inserted() {}\n"
            "\n"
            "func owner() {}\n"
        )
        self.assertEqual(len(got), 1, "the stranded comment was not flagged: %r" % (got,))
        _, word, sits_on = got[0]
        self.assertEqual(word, "owner")
        self.assertEqual(sits_on, "inserted")

    def test_type_declaration_too(self):
        got = hits(
            "// Owner is the type this comment describes.\n"
            "// Inserted is a different type.\n"
            "type Inserted struct{}\n"
            "\n"
            "type Owner struct{}\n"
        )
        self.assertEqual(len(got), 1, "a stranded TYPE comment was not flagged")
        self.assertEqual(got[0][1], "Owner")

    def test_method_with_a_pointer_receiver(self):
        got = hits(
            "// owner does a thing.\n"
            "// helper is a method and should not have taken this comment.\n"
            "func (t *T) helper() {}\n"
            "\n"
            "func owner() {}\n"
        )
        self.assertEqual(len(got), 1, "a receiver in the signature hid the declaration name")
        self.assertEqual(got[0][2], "helper")


class TestDoesNotFlagLegitimateComments(unittest.TestCase):
    def test_section_banner_naming_an_earlier_type(self):
        # THE REAL FALSE POSITIVE this check shipped with, found by auditing
        # the baseline: `// Datastore operations` above a method, with the type
        # declared ABOVE it. A banner is not a stranded doc comment.
        self.assertEqual(hits(
            "type Datastore struct{}\n"
            "\n"
            "// Datastore operations\n"
            "func (d *Datastore) PoolSet() {}\n"
        ), [], "a section banner naming an EARLIER declaration must not be flagged")

    def test_backward_cross_reference(self):
        self.assertEqual(hits(
            "func helper() {}\n"
            "\n"
            "// helper is called by this one, which is why it is mentioned first.\n"
            "func caller() {}\n"
        ), [], "a backward cross-reference must not be flagged")

    def test_comment_opening_with_its_own_name(self):
        self.assertEqual(hits(
            "// owner does the thing.\n"
            "func owner() {}\n"
            "\n"
            "// other does another thing.\n"
            "func other() {}\n"
        ), [], "correctly attributed comments must not be flagged")

    def test_comment_opening_with_a_word_that_is_not_a_declaration(self):
        self.assertEqual(hits(
            "// Returns the thing, eventually.\n"
            "func owner() {}\n"
            "\n"
            "func Returns() {}\n"
        ), [], "a prose opener is only a convention miss, not a misattribution")

    def test_declaration_with_no_doc_comment(self):
        self.assertEqual(hits("func owner() {}\n\nfunc other() {}\n"), [])


class TestDoesNotCrash(unittest.TestCase):
    """A check that panics in CI is worse than no check."""

    def test_empty_file(self):
        self.assertEqual(hits(""), [])

    def test_comment_only_file(self):
        self.assertEqual(hits("// just a comment, no declarations\n"), [])

    def test_bare_comment_marker(self):
        # `//` with nothing after it: the first-word split must not IndexError.
        self.assertEqual(hits("//\nfunc owner() {}\n"), [])

    def test_generic_type(self):
        got = hits(
            "// Owner is the type.\n"
            "// Box is generic.\n"
            "type Box[T any] struct{ v T }\n"
            "\n"
            "type Owner struct{}\n"
        )
        self.assertEqual(len(got), 1, "a generic type parameter list broke the matcher")
        self.assertEqual(got[0][2], "Box")

    def test_grouped_declaration_block(self):
        # `var (` / `const (` have no name on the keyword line. The matcher
        # must simply not match, rather than capture "(" or crash.
        self.assertEqual(hits(
            "// Owner is the type.\n"
            "var (\n"
            "\ta = 1\n"
            ")\n"
            "\n"
            "type Owner struct{}\n"
        ), [], "a grouped var block should not be treated as a named declaration")

    def test_crlf_line_endings(self):
        # The working tree is CRLF; the checker normalises. Without that the
        # comment walk-back fails and the defect goes unreported.
        got = hits(
            "// owner does the thing.\r\n"
            "// inserted took the comment.\r\n"
            "func inserted() {}\r\n"
            "\r\n"
            "func owner() {}\r\n"
        )
        self.assertEqual(len(got), 1, "CRLF input was not handled")


class TestBaselineKeying(unittest.TestCase):
    """The baseline must not hide a NEW defect in an already-baselined file.

    Measured by a reviewer against the two-element key: appending a fresh
    misattribution to a baselined file, stranding the SAME name on a different
    declaration, matched the entry and exited 0 with the counter moving 12->13.
    The triple is what fixes it, and this is the regression test.
    """

    def test_same_file_and_word_stranded_on_a_different_declaration(self):
        got = hits(
            "// owner does the thing.\n"
            "// firstVictim took the comment.\n"
            "func firstVictim() {}\n"
            "\n"
            "// owner does the thing, again.\n"
            "// secondVictim took it too.\n"
            "func secondVictim() {}\n"
            "\n"
            "func owner() {}\n"
        )
        self.assertEqual(len(got), 2, "both strandings should be reported: %r" % (got,))
        owners = sorted(o for _, _, o in got)
        self.assertEqual(owners, ["firstVictim", "secondVictim"])
        # The triple distinguishes them; a (file, word) key would collapse
        # these two into one and let the second pass as "already baselined".
        keys = {(w, o) for _, w, o in got}
        self.assertEqual(len(keys), 2, "the two hits share a key, so a baseline "
                                      "entry for one would silence the other")

    def test_the_shipped_baseline_matches_the_tree_exactly(self):
        """Every baselined triple still matches, and nothing else is flagged.

        This is what keeps the list honest: a fixed entry becomes stale and a
        new defect becomes new, and either fails.
        """
        import os as _os
        root = _os.path.dirname(_os.path.dirname(_os.path.abspath(__file__)))
        seen = set()
        extra = []
        for path in checker.go_files():
            rel = _os.path.relpath(path, root).replace(_os.sep, "/")
            for _, word, owner in checker.check(path):
                key = (rel, word, owner)
                seen.add(key)
                if key not in checker.BASELINE:
                    extra.append(key)
        self.assertEqual(extra, [], "new misattributions in the tree")
        self.assertEqual(sorted(checker.BASELINE - seen), [],
                         "baseline entries that no longer match; remove them")


if __name__ == "__main__":
    unittest.main(verbosity=2)
