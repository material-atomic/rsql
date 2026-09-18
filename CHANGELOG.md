# Changelog

This file starts at the entry below (task 0045). Nothing before this point was
recorded, so its absence is not a claim that nothing changed before it.

## Unreleased

### Changed

- A scan or a rollup read whose `from` sorts after its `to` is refused
  instead of run. Before this change, such a declaration read back an empty
  result — no `rows`, no error, no `truncated` — indistinguishable on the
  wire from a stretch that is legitimately empty. Now it is an error:
  `code=declaration` when both ends are constants (caught when the operation
  is declared, since no argument could ever make it return a row), or
  `code=argument` when either end depends on an argument or a batch step's
  key (caught when the operation runs, since that is the first point either
  value exists). `from` is the low end of a stretch and `to` is the high end,
  in both the forward and the reverse direction — writing them the other way
  round is not a different, valid stretch, in either direction.

  This does not change what `from == to` means: two ends pinned to the same
  point, with `exclusive` telling them apart, is how a caller writes an
  empty range on purpose, and it keeps returning an empty result rather than
  an error.

  Not a migration: this package has never been tagged and carries no
  `CHANGELOG.md` entry before this one, so there is nobody upgrading from a
  released version who could be relying on the old silent-empty answer.
