# Decisions

Rulings made while building plait, in the format
`Ruling: <what> — <why> — <cost if wrong>`.

## Scope and language

- Ruling: providers are in-process Go plugins behind a request/response interface (`internal/provider.Provider`), not gRPC — the brief allowed either; in-process keeps the build dependency-free and the tests fast, and every request/response type is plain data (JSON-serializable `value.Value`s) so an RPC transport could wrap it later — no crash isolation between engine and provider, and third parties cannot ship providers as separate binaries.
- Ruling: the HCL subset covers blocks, attributes, all operators, conditionals, string templates with `${}` interpolation, heredocs (`<<EOT`, `<<-EOT`), splats, for-expressions, function calls with `...` expansion, and 39 built-in functions; it omits template directives (`%{if}`/`%{for}`), `dynamic` blocks, modules, data sources, provisioners and `object()`/`tuple()` type constraints — these are the pieces a mini engine can drop without changing the plan/apply model — configurations that need them must be rewritten with for-expressions and conditionals.
- Ruling: a resource's provider is inferred from its type prefix (`local_file` → `local`) and provider aliases are rejected at load time — keeps the config model and the state small — multi-region/multi-account style configurations are not expressible.
- Ruling: relative paths (the `local` provider's paths, `file()`, the default provider root) resolve against the configuration directory and `path.module` is `"."` — the result is the same no matter which directory `plait` is run from — differs from Terraform, where local paths are relative to the working directory.
- Ruling: the module is declared `go 1.24` although development used Go 1.27 — keeps the language level conservative for CI and other toolchains — none known; nothing newer than 1.22 features is used.

## Planning semantics

- Ruling: `count` and `for_each` must be known at plan time, as in Terraform — the set of instances is part of the reviewed plan — a count derived from a not-yet-created resource attribute is an error with an explanatory message instead of a two-phase apply.
- Ruling: `for_each` over a list requires strings and is treated as a set (de-duplicated, sorted) — matches `toset()` semantics and gives stable instance keys — lists of non-strings must be converted to a map first.
- Ruling: unknown values are never equal to anything, including another unknown — two unknowns may resolve differently — a few comparisons collapse to unknown that a smarter analysis could decide.
- Ruling: a replacement is planned as a fresh create (the provider is asked to plan with a null prior) — so the provider, not the engine, decides which values of the new object are known — one extra provider call per replaced instance.
- Ruling: plan refreshes by default but never persists the refreshed state; only `plait refresh` and `plait apply` write it — a plan has no side effects — the "objects have changed outside of plait" note can repeat until the next apply/refresh.
- Ruling: pending (interrupted) creates are resolved even with `-refresh=false` — planning against an object whose existence is unknown would be wrong either way — `-refresh=false` still makes one provider call per pending object.
- Ruling: plan rendering orders attributes alphabetically — deterministic golden output without depending on schema order — differs cosmetically from Terraform, which uses schema order.
- Ruling: mixing a delete-then-create replacement of a dependency with a create_before_destroy replacement of its dependent is reported as an ordering error with a hint, instead of Terraform's automatic propagation of create_before_destroy to dependencies — explicit and simple — users must set `create_before_destroy` on both resources themselves.

## Apply and crash safety

- Ruling: before every create the engine persists the object as `pending` with a fresh idempotency token, and providers must accept that token and be able to look objects up by it — this write-ahead record is what lets a SIGKILL at any point be repaired by the next refresh — providers whose APIs have no idempotency/lookup mechanism must fall back to a natural key (the local provider uses the path).
- Ruling: a create whose outcome is unknown (error without an object) stays `pending` instead of being dropped — the request may have committed (e.g. a 500 after commit) — the next plan always pays one lookup per such object.
- Ruling: an abandoned pending create (lookup found nothing) is re-created with the same idempotency token — a request still in flight from the crashed run would otherwise land later and duplicate the object (observed while building the demo) — if the configuration changed in between, the re-create may return the original object and the apply reports an inconsistency for that instance, which the next plan corrects.
- Ruling: a failed create_before_destroy create leaves the old object deposed and the new one pending/tainted rather than restoring the old object as current — keeps the crash-safety rule (never forget an object that may exist) uniform — the next plan shows a deposed delete plus a recreate instead of "no changes".
- Ruling: at apply time each instance's configuration is re-evaluated with real dependency values and re-planned; any value that was known in the reviewed plan must be unchanged ("inconsistent final plan") — the reviewed plan is a promise — providers with non-deterministic planning cannot be used.
- Ruling: when an apply step fails, only its transitive dependents are skipped and every unrelated step still runs — partial progress is persisted step by step anyway — a failing apply can make more changes than a stop-on-first-error policy would.
- Ruling: outputs that cannot be evaluated after a partial apply keep their previous value (with a warning) — avoids wiping outputs because an unrelated step failed — an output may be stale until the next successful apply.

## State and locking

- Ruling: state is a JSON file next to the configuration (`plait.state.json`), written by write-to-temp + fsync + rename with a `.backup` of the previous version, and every write increments `serial` — no torn writes, cheap history of one — no remote backends.
- Ruling: locking is an `O_EXCL` lock file containing holder metadata; a lock whose holder PID is dead on the same host is broken automatically, others need `plait force-unlock ID` — makes crash recovery hands-off on a single machine — on shared filesystems a dead holder on another host still needs a manual unlock.
- Ruling: a saved plan embeds the refreshed prior state, the variable values and a SHA-256 digest of the configuration files; applying it is refused if the state lineage/serial or the digest changed — apply does exactly what was reviewed — any intervening apply invalidates saved plans.

## Fake cloud

- Ruling: the fake cloud models the things that make IaC hard rather than many resource kinds: referential integrity (409 when deleting an attached network), globally unique names, immutable fields, IP allocation, idempotency keys with lookup, and fault injection (fail before commit, fail after commit, latency) — these are what exercise ordering, retries and crash recovery — it is not a model of any real cloud's API.
- Ruling: injected latency is applied after the request body is read, so a client that dies mid-request does not cancel the server-side work — models an asynchronous backend and makes the crash demo deterministic — other failure shapes (connection reset before the body is read) are covered only by the provider's retry tests.
