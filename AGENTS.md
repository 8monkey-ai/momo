## Communication
- Always talk in ASD-STE100 Simplified Technical English. Applies to documentation and comments as well.

## Design
Apply to every non-trivial change. These constrain design, not formatting.

- **Single Responsibility** — one reason to change per module. Split modules and functions that mix unrelated responsibilities, e.g. business rules with IO, formatting, or framework glue. Things that change together live together; things that change for different reasons live apart. "A new variant exists" is a reason to change like any other, and it belongs to the variant alone: a module edited every time a variant is added has taken on a responsibility that is not its own. Everything that changes when one variant changes lives with that variant — its name, its own settings parsing, its registration.
- **Open/Closed** — extend by adding implementations, not by editing code that enumerates cases. Apply only where variation already exists or is concretely requested; do not pre-build plugin points for imagined futures. Where a variation point does exist, a new variant is a purely additive change, and nothing that already works is edited to accommodate it. Where the language cannot link a variant nothing references, the single permitted reference is a declaration of intent — an import, a manifest entry — with no logic in it to review.
- **Liskov Substitution** — an implementation must be usable wherever its abstraction is expected: no strengthened preconditions, no weakened postconditions, no not-supported members, no caller type-checking the abstraction.
- **Interface Segregation** — narrow, client-specific interfaces. Clients must not depend on members they don't call.
- **Missing capability belongs to the abstraction** — when an implementation needs something the abstraction does not offer, add it to the abstraction so every implementation has it and the caller drives it. Do not satisfy the need privately inside one implementation: the next implementation has the same need and no reason to solve it the same way, and the two are now impossible to drive together.
- **Lifecycle belongs to the owner of the process** — startup, shutdown, signals, and cleanup are decided once by the code that assembles the system, and reach an implementation through the abstraction. An implementation that watches signals or ends the process on its own has taken a responsibility that is not its own.
- **Dependency Inversion** — high-level policy must not depend on low-level detail. Source dependencies point from IO-near code (UI, HTTP, filesystem, database, SDKs, clock, randomness, env) inward toward IO-far policy, and the abstraction is owned by the high-level side. This governs the set of implementations as well as their behavior: the abstraction owns the registry variants enter and answers what is available, so policy asks it what exists rather than naming variants itself, and the dependency runs from variant to abstraction only. A module that assembles the system may reference a variant in order to link it, never in order to know it. Self-announcement trades discoverability and startup-time error reporting for additivity; a central enumeration is a deliberate exception, taken when the set of variants is closed, and stated as such.

## Writing Code
- Do not preserve backward compatibility. Remove obsolete paths instead of adding compatibility layers, fallbacks, or migrations.
- When the correct change lies outside the scope you were given, say so and stop. Do not build a local workaround that avoids touching the shared abstraction. A workaround that keeps the seam untouched is more expensive than the change it avoided, because it hides the need and every later implementation repeats it.
- Choose the simplest implementation that fully meets the current requirements. Avoid speculative abstractions, configuration, and indirection. Omit steps that aren't needed: do not compute, read, or store values that nothing depends on. Prefer simple, correct approaches over premature optimization when the input is small.
- Grow the system in layers. Start from the smallest version that works end to end, and add each new capability on top of a product that already works. Never trade a working product for unfinished complexity.
- Keep components modular and concerns clearly separated.
- Before writing code by hand, check whether a language built-in, the standard library, an installed dependency, or a well-maintained package already does it. Read its docs and types instead of assuming what it cannot do.
- Adding a dependency to delete boilerplate is a good trade, even when the hand-written version would only be a few lines. Small amounts of custom code still have to be read, tested, and maintained; a library that is already solving this problem for others does not.
- Question every hardcoded module-level constant. If someone deploying or running the system might reasonably want a different value, make it a configuration option with the current value as the default.
- Question whether a new type, wrapper, or class is needed at all. If the same behavior is expressible with a plain value, an existing type, or a function, do that instead. Introduce a struct only when it removes real ceremony rather than adding it.
- Make architectural decisions for the long term. Do not accept a stopgap that only works for now and is meant to be replaced later.
- Write the test first for the observable behavior. It must fail for a plausible wrong implementation. Then write only enough production code to pass it.
- Tests state expected values as literals. A test that compares against the production constant passes even when the value is wrong.
- Work in small, reviewable increments. Do not mix behavior change with refactoring in the same step.
- Names state intent. Rename when a better name clarifies a responsibility.
- Keep comments minimal. Code should be self-explanatory. Comment only to explain why, not what. If code needs a comment to be understood, rewrite it to be clearer instead. Aim for the smallest changeset that solves the problem.
- Repeating text is not the problem DRY solves. Before finishing a change, look for the same decision written in more than one place and unify it: if changing it in one place and not the other would be a bug, it belongs in one place. Code that merely looks alike, but changes for different reasons, stays separate.
- Do not name a constant for a value used in one place. A literal at its one call site is clearer than a name defined elsewhere. Introduce the constant on the second use, or when the name explains something the value cannot.
- Keep functions small enough to hold in one's head and files small enough to review in one sitting. When a function accumulates branches or a file outgrows a review-sized unit, split along responsibility lines, not by line count.
- Do not export symbols that are only used internally.
- Let the language's type inference do the work where it can. Omit explicit type annotations when inference yields the same type.
- Do not leak persistence shapes, DTOs, framework types, or transport formats across a boundary. Convert at the boundary.
- Adding a variant must not touch a config schema, a route table, a shared switch, or a validation list. If it does, the seam is in the wrong place. Before calling the work done, name what an unrelated variant would have to edit.
- Keep IO-near adapters as thin shells with no decision logic, so core behavior is testable without UI, network, filesystem, or devices.
- Modules expose only what callers need. Representation, IO details, and invariant enforcement stay hidden.
- Do not create import cycles.

## Structure
- Treat the current file and directory layout as a proposal, not a given. When a change makes the existing structure awkward, say so and propose a better one instead of forcing the change into the wrong place.
- Do not reorganize the project without approval. Suggest the move, name what it improves, and wait.

## Verification
Nothing is done until it has been reviewed and checked. Review first, then run the checks.

### Run the checks
Before reporting work done — and before any commit — run format, lint (including complexity and duplication rules), type check, then tests, and fix what they report. Never report completion with a known failing check or a known unaddressed violation. If a check cannot run in this environment, say so instead of skipping it silently.
