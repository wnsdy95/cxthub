## Problem and user impact

Describe the problem, who encounters it, and why it matters.

Closes #

## Solution

Explain the behavior change and the important implementation choices.

## Compatibility and data integrity

Describe any effect on snapshot identity, natural or overlay parents, graph
reachability, synchronization, provider materialization, API/schema contracts,
authentication, authorization, or migrations. Write `None` when not applicable.

## Verification

List exact commands and focused scenarios. Attach redacted screenshots or logs
when useful.

- [ ] Added or updated regression tests
- [ ] `make test`
- [ ] `make typecheck` when TypeScript changed
- [ ] `make e2e` or a focused equivalent when behavior changed
- [ ] `make public-check`
- [ ] OpenAPI/schema changes include drift-guard updates
- [ ] Korean and English UI locale keys remain aligned

## AI assistance

- [ ] No material AI assistance
- [ ] AI assistance was used and reviewed; describe the scope below

## Safety and scope

- [ ] The pull request contains one focused change
- [ ] The title follows `type(optional-scope): imperative summary`
- [ ] Linked or discussed the issue first when the change is non-trivial
- [ ] No credentials, private context, personal data, local stores, or
      production inputs are included
- [ ] I have the right to submit this work under Apache License 2.0
