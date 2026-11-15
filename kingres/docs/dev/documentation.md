<!-- SPDX-License-Identifier: AGPL-3.0-only -->
# Documentation rules

All documentation is in English, in Markdown, in plain prose without marketing language. File and directory names are English, lower case, with hyphens.

## What goes where

| Document | Holds | Changes when |
|---|---|---|
| `kingres/README.md` | What the project is, repository map, status, links | The map or the status changes |
| `kingres/docs/architecture.md` | The design, its evidence and the open decisions. The source of truth for design. It separates sourced facts, **[Recommendation]** items and **[Needs verification]** items | Only its owner edits it. Others propose changes through the work log or an ADR |
| `kingres/docs/research/` | Research notes behind the architecture document | New research is added; existing notes are not rewritten to match later decisions |
| `kingres/docs/adr/` | One architecture decision record per decision (context, decision, consequences, alternatives, status, source) | A decision is made or replaced. An accepted ADR is not edited except for its status and a pointer to its replacement |
| `kingres/docs/dev/` | How to work on the project: setup, standards, layout, patches, sync, testing, documentation | The way of working changes |
| `kingres/docs/worklog.md` | Dated record of what was done, how it was verified, and what is still open | Every completed unit of work |
| `contrib/kingres_*/README.md` | User-facing description of one extension: SQL interface, GUCs, limits | The extension interface changes |
| `kingres/api/openapi.yaml` | The REST contract | An endpoint changes (before the code does) |

Rules:

- Say each thing once. Link instead of copying. If two documents say different things, one of them is wrong; fix it and note it in the work log.
- Keep the architecture document's three kinds of statements apart in anything derived from it: sourced fact, **[Recommendation]**, **[Needs verification]**. Do not turn a recommendation into a fact by restating it without the label. A recommendation becomes a decision when an ADR records it. A "needs verification" item becomes a fact when someone checks it and the work-log entry records how.
- Do not state commands, version numbers or tool behaviour that have not been checked. Write "to be confirmed" instead.
- Use relative links between documents, and check that they resolve.
- Do not use emojis or non-English text.
- Describe the current state in present tense. History belongs in the work log and in ADR statuses.

## Architecture decision records

- One decision per ADR. Number sequentially from `0001`; the template is `docs/adr/0000-template.md`; the index and the procedure are in `docs/adr/README.md`.
- An ADR gives the real reasoning and the alternatives, cites the section of `architecture.md` it comes from, lists the consequences honestly, including costs, and lists any **[Needs verification]** item the decision depends on.
- A decision is replaced by a new ADR that says "Supersedes ADR NNNN"; the old one gets status "Superseded by ADR NNNN".

## Work-log entries

`docs/worklog.md` is the dated record. Entries are added at the end of a unit of work, newest where the file's own header says (the file is owned separately and states its own order). Every entry has:

1. **Date** (`YYYY-MM-DD`) and a short title.
2. **What was done.** Concrete: files, commands, components. Link to commits or documents when they exist.
3. **How it was verified.** The exact checks run and their results (test names, command and outcome). "Not verified" is a valid answer and must be written as such.
4. **Needs-verification items.** Which items from `architecture.md` were checked, and what the result was.
5. **Decisions and deviations.** Anything decided that is not yet an ADR, and any difference from the architecture document.
6. **Still open.** Known gaps, follow-up work, questions for the project owner.
7. **Core patches.** IDs added or changed, or "none".

## Code documentation

The rules for comments and doc comments are in [coding-standards.md](coding-standards.md). Comments explain why. User-facing documentation for each extension is in its README; the REST API is documented by the OpenAPI file.
