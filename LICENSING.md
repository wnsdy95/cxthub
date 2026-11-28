# Licensing

CXTHub is developed by [@wnsdy95](https://github.com/wnsdy95) at
<https://github.com/wnsdy95/cxthub>. The copyright stays with its authors.
This page explains which license applies to which part, what is free, and when
a company needs a commercial license. Apart from the grant under
[Commercial license for sponsors](#commercial-license-for-sponsors), it is an
explanation: the license texts decide.

## Which license applies

| Part | Paths | License |
|---|---|---|
| `cxt` command-line client, agent integrations, schemas, documentation, scripts | everything not listed in the next row | [Apache License 2.0](LICENSES/Apache-2.0.txt) |
| Server, web application, deployment files | `backend/`, `frontend/`, `deploy/` | [PolyForm Small Business 1.0.0](LICENSES/PolyForm-Small-Business-1.0.0.md) or [PolyForm Noncommercial 1.0.0](LICENSES/PolyForm-Noncommercial-1.0.0.md), at your option. Other use needs a commercial license. |

The license of a file is the one in the `LICENSE` file of its nearest
enclosing directory. Third-party components keep their own licenses; see
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).

## The client is open source

Everything you install on a developer machine is under the Apache License 2.0:
the `cxt` CLI, the Claude Code and Codex integrations, and the schemas. Anyone
may use it, at a company of any size, without asking and without paying.

## The server and web application are source-available

The source of the server and the web application is public, and you may read,
run, change and redistribute it within the limits of the two PolyForm licenses.
It is not open source in the Open Source Initiative's sense, because large
companies need a commercial license.

It is free when either license permits your use:

- **Noncommercial use.** Personal use such as study, research and hobby
  projects, and use by charitable organizations, educational institutions,
  public research organizations, public safety or health organizations,
  environmental protection organizations and government institutions
  (PolyForm Noncommercial, sections "Noncommercial Purposes", "Personal Uses"
  and "Noncommercial Organizations").
- **Small businesses.** Use for the benefit of a company that has fewer than
  100 people working for it, counting employees and independent contractors,
  and had less than 1,000,000 USD (2019 dollars, adjusted for inflation) total
  revenue in the prior tax year (PolyForm Small Business, section "Small
  Business"). Companies under common control count together.

Using the hosted CXTHub service does not need a license to this source code.

## Larger companies need a commercial license

A company that neither license covers must have a commercial license before it
uses the software in `backend/`, `frontend/` or `deploy/`, for example to run
its own CXTHub server. This does not apply to the Apache-licensed client.

### Commercial license for sponsors

A commercial license comes with sponsoring the project.

1. Sponsor at a tier that says it includes a commercial license:
   <https://github.com/sponsors/wnsdy95>.
2. If your company needs a signed agreement or an invoice instead, open a
   thread in [Discussions](https://github.com/wnsdy95/cxthub/discussions).

While your organization's sponsorship at such a tier is active, the copyright
holder grants your company the licenses of the PolyForm Small Business License
1.0.0 for the software in `backend/`, `frontend/` and `deploy/` without the
headcount and revenue limits of its "Small Business" section. Every other term
of that license applies unchanged. The grant ends when the sponsorship ends.

## Attribution

Whoever uses CXTHub code in their own work must say where it came from.

- **Apache-licensed parts.** Keep the license and the [NOTICE](NOTICE) file
  with any copy or derivative work you distribute, as section 4 of the Apache
  License 2.0 requires.
- **Server, web application, deployment files.** Anyone who gets a copy of any
  part of them from you must also get the license terms and this line:

  ```text
  Required Notice: Copyright (c) 2026 wnsdy95 (https://github.com/wnsdy95/cxthub)
  ```

The CXTHub name and logos are covered by [TRADEMARKS.md](TRADEMARKS.md), not by
these licenses.

## Earlier versions

The whole repository as published up to and including release v0.2.0 was
licensed under the Apache License 2.0. Those versions remain available under
that license. The terms on this page apply to later versions.

## Contributions

[CONTRIBUTING.md](CONTRIBUTING.md) describes the license of contributions.

## Questions

Ask in [Discussions](https://github.com/wnsdy95/cxthub/discussions) if you are
unsure whether your use is covered.
