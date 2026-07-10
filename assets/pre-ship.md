# Pre-ship checklist (for AI agents)

Follow this before committing or pushing code in this repository. It exists
because docs, numbers, and examples drift silently unless refreshing them is
part of shipping.

**Verify the change actually works.** `go build ./...`, `go vet ./...`, and
`go test ./...` must pass. If the change touches the wire, the tool loop, cost
accounting, or a provider, run a small live check through one of the runners
(`examples/toroid-cli -plain '<prompt>'`) against a real model — unit tests use
the scripted FauxStep and cannot catch wire regressions. Keep live spend small
(haiku-class models, short prompts).

**Update the README before every push.** It states facts that go stale: the
line count at the top, the runner binary-size table, the provider table, the
verified-models matrix, and the feature list. Recount LoC
(`find . llm tools -maxdepth 1 -name "*.go" ! -name "*_test.go" | xargs cat | wc -l`)
and rebuild the runners (`-trimpath -ldflags="-s -w"`, plus upx for the
compressed column) whenever the kernel changed materially. If a feature was
added, removed, or renamed, the README must say what the code does now — not
what it used to do.

**Update CHANGELOG.md** with a short entry under `## Unreleased` describing
what changed and why it matters to a host embedding the kernel. Keep the docs
in `assets/` (ARCHITECTURE.md, terminology.md) consistent with any renamed
files, removed config fields, or changed behaviour — a doc that references a
deleted symbol is worse than no doc.

**Scrub before pushing.** No API keys, tokens, or company-internal hostnames
in code, docs, examples, or test fixtures — use `example.com` placeholders.
Delete scratch files, built binaries, and anything created only for testing.
The repository ships one end-to-end test (`e2e_test.go`); development probes
belong in a scratch directory outside the repo, not in the tree.
