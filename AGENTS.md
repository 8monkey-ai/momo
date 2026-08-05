## Design
Apply to every non-trivial change. These constrain design, not formatting.

- **Single Responsibility** — one reason to change per module. Split modules and functions that mix unrelated responsibilities, e.g. business rules with IO, formatting, or framework glue. Things that change together live together; things that change for different reasons live apart. "A new variant exists" is a reason to change like any other, and it belongs to the variant alone: a module edited every time a variant is added has taken on a responsibility that is not its own. Everything that changes when one variant changes lives with that variant — its name, its own settings parsing, its registration.
- **Open/Closed** — extend by adding implementations, not by editing code that enumerates cases. Apply only where variation already exists or is concretely requested; do not pre-build plugin points for imagined futures. Where a variation point does exist, a new variant is a purely additive change, and nothing that already works is edited to accommodate it. Where the language cannot link a variant nothing references, the single permitted reference is a declaration of intent — an import, a manifest entry — with no logic in it to review.
- **Liskov Substitution** — an implementation must be usable wherever its abstraction is expected: no strengthened preconditions, no weakened postconditions, no not-supported members, no caller type-checking the abstraction.
- **Interface Segregation** — narrow, client-specific interfaces. Clients must not depend on members they don't call.
- **Dependency Inversion** — high-level policy must not depend on low-level detail. Source dependencies point from IO-near code (UI, HTTP, filesystem, database, SDKs, clock, randomness, env) inward toward IO-far policy, and the abstraction is owned by the high-level side. This governs the set of implementations as well as their behavior: the abstraction owns the registry variants enter and answers what is available, so policy asks it what exists rather than naming variants itself, and the dependency runs from variant to abstraction only. A module that assembles the system may reference a variant in order to link it, never in order to know it. Self-announcement trades discoverability and startup-time error reporting for additivity; a central enumeration is a deliberate exception, taken when the set of variants is closed, and stated as such.

## Writing Code
- Do not preserve backward compatibility. Remove obsolete paths instead of adding compatibility layers, fallbacks, or migrations.
- Choose the simplest implementation that fully meets the current requirements. Avoid speculative abstractions, configuration, and indirection. Omit steps that aren't needed: do not compute, read, or store values that nothing depends on. Prefer simple, correct approaches over premature optimization when the input is small.
- Grow the system in layers. Start from the smallest version that works end to end, and add each new capability on top of a product that already works. Never trade a working product for unfinished complexity.
- Keep components modular and concerns clearly separated.
- Prefer established, well-maintained libraries when they reduce overall complexity or improve reliability. Do not reimplement common functionality without a clear reason.
- Lean on the dependencies already in the project before writing your own implementation or adding packages. Do not assume a library lacks a capability without checking its documentation and types.
- Make architectural decisions for the long term. Do not accept a stopgap that only works for now and is meant to be replaced later.
- Always use the /solid skill before starting to write code.
- Write the test first for the observable behavior. It must fail for a plausible wrong implementation. Then write only enough production code to pass it.
- Work in small, reviewable increments. Do not mix behavior change with refactoring in the same step.
- Names state intent. Rename when a better name clarifies a responsibility.
- Keep comments minimal. Code should be self-explanatory. Comment only to explain why, not what. If code needs a comment to be understood, rewrite it to be clearer instead. Aim for the smallest changeset that solves the problem.
- Remove duplication of knowledge, not duplication of text. Coincidentally similar code with different reasons to change stays separate.
- Keep functions small enough to hold in one's head and files small enough to review in one sitting. When a function accumulates branches or a file outgrows a review-sized unit, split along responsibility lines, not by line count.
- Do not export symbols that are only used internally.
- Let the language's type inference do the work where it can. Omit explicit type annotations when inference yields the same type.
- Do not leak persistence shapes, DTOs, framework types, or transport formats across a boundary. Convert at the boundary.
- Adding a variant must not touch a config schema, a route table, a shared switch, or a validation list. If it does, the seam is in the wrong place. Before calling the work done, name what an unrelated variant would have to edit.
- Keep IO-near adapters as thin shells with no decision logic, so core behavior is testable without UI, network, filesystem, or devices.
- Modules expose only what callers need. Representation, IO details, and invariant enforcement stay hidden.
- Do not create import cycles.

## Verification
Nothing is done until it has been reviewed and checked. Review first, then run the checks.

### Run the checks
Before reporting work done — and before any commit — run format, lint (including complexity and duplication rules), type check, then tests, and fix what they report. Never report completion with a known failing check or a known unaddressed violation. If a check cannot run in this environment, say so instead of skipping it silently.
