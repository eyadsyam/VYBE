#!/usr/bin/env python3
"""Traceability gate for VYBE specs.

Master Prompt v2 §13.3 makes the Definition of Done machine-verifiable, and the
spec-driven workflow requires that every functional requirement has at least one
acceptance criterion and that every acceptance criterion references a
requirement.

Both directions matter, and they catch different defects:

  FR with no AC   -> a requirement nobody will ever verify. It will be
                     implemented from someone's memory of a conversation.
  AC with no FR   -> either a requirement is missing from the spec, or the
                     criterion is testing something nobody asked for.

Usage:
    python tools/spec/check_traceability.py specs/001-vertical-slice/spec.md
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

# Requirement ids look like FR-12, NFR-3, AC-27, EC-5, OS-11.
ID = re.compile(r"\b(FR|NFR|AC|EC|OS)-(\d+)\b")

# A definition is an id in a table cell or heading emphasised with **…**, which
# is how this spec declares them: | **FR-12** | The system MUST … |
DEFINITION = re.compile(r"\*\*(FR|NFR|AC|EC|OS)-(\d+)\*\*")

# Given/When/Then is required for acceptance criteria — §5 of the workflow.
# An AC that is prose is not machine-verifiable and will be interpreted.
GWT = re.compile(r"\bGiven\b.*?\bWhen\b.*?\bThen\b", re.IGNORECASE | re.DOTALL)

# Language that cannot be turned into an assertion. An AC containing these is a
# preference, not a criterion.
VAGUE = [
    "should work", "works well", "user-friendly", "fast enough",
    "as appropriate", "etc.", "and so on", "reasonable", "intuitive",
    "look good", "properly",
]


def main(argv: list[str]) -> int:
    if len(argv) != 2:
        print(__doc__)
        return 2

    path = Path(argv[1])
    if not path.is_file():
        print(f"error: {path} not found")
        return 2

    text = path.read_text(encoding="utf-8")
    errors: list[str] = []
    warnings: list[str] = []

    defined: dict[str, set[int]] = {k: set() for k in ("FR", "NFR", "AC", "EC", "OS")}
    for kind, num in DEFINITION.findall(text):
        defined[kind].add(int(num))

    for kind in defined:
        if not defined[kind] and kind in ("FR", "AC"):
            errors.append(f"no {kind}-* requirements defined — is the spec format correct?")

    # --- Split the document into per-AC blocks -----------------------------
    # An AC block runs from its own definition to the next id definition.
    ac_blocks: dict[int, str] = {}
    positions = [(m.start(), m.group(1), int(m.group(2))) for m in DEFINITION.finditer(text)]
    for i, (start, kind, num) in enumerate(positions):
        end = positions[i + 1][0] if i + 1 < len(positions) else len(text)
        if kind == "AC":
            ac_blocks[num] = text[start:end]

    # --- Every AC must reference at least one FR or NFR --------------------
    referenced_by_ac: set[tuple[str, int]] = set()
    for num, block in sorted(ac_blocks.items()):
        refs = {
            (k, int(n))
            for k, n in ID.findall(block)
            if k in ("FR", "NFR")
        }
        if not refs:
            errors.append(
                f"AC-{num} references no FR-* or NFR-*. "
                f"An orphaned criterion means either a requirement is missing "
                f"or the criterion is unnecessary."
            )
        referenced_by_ac |= refs

        # Dangling references are worse than missing ones: they look correct.
        for kind, n in refs:
            if n not in defined[kind]:
                errors.append(f"AC-{num} references {kind}-{n}, which is not defined in this spec.")

        if not GWT.search(block):
            warnings.append(
                f"AC-{num} is not in Given/When/Then form; it may not be directly testable."
            )

        low = block.lower()
        for phrase in VAGUE:
            if phrase in low:
                warnings.append(f"AC-{num} contains vague language: {phrase!r}")

    # --- Every FR/NFR must be covered by at least one AC -------------------
    # The spec's §9 traceability table may cover a block of requirements by
    # range (e.g. "FR-1 – FR-18"); honour that so a deliberate group coverage
    # statement is not reported as a gap.
    covered = set(referenced_by_ac)
    for a, b in re.findall(r"\b(?:FR|NFR)-(\d+)\s*(?:–|-|to)\s*(?:FR|NFR)-(\d+)\b", text):
        for n in range(int(a), int(b) + 1):
            covered.add(("FR", n))
            covered.add(("NFR", n))

    for kind in ("FR", "NFR"):
        for n in sorted(defined[kind]):
            if (kind, n) not in covered:
                errors.append(
                    f"{kind}-{n} has no acceptance criterion. "
                    f"A requirement nobody verifies will be implemented from memory."
                )

    # --- Report ------------------------------------------------------------
    print(f"spec: {path}")
    for kind in ("FR", "NFR", "AC", "EC", "OS"):
        print(f"  {kind:<4} defined: {len(defined[kind]):>3}")

    if warnings:
        print(f"\n{len(warnings)} warning(s):")
        for w in warnings:
            print(f"  ! {w}")

    if errors:
        print(f"\n{len(errors)} error(s):")
        for e in errors:
            print(f"  x {e}")
        print("\nTRACEABILITY FAILED")
        return 1

    print("\nTRACEABILITY OK — every FR/NFR is covered and every AC traces back.")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
